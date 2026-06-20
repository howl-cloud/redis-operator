# Redis Kubernetes Operator

A Kubernetes operator for Redis built around the same control-plane ideas used by [CloudNativePG](https://github.com/cloudnative-pg/cloudnative-pg): direct pod/PVC management, a split control/data architecture, and deterministic failover behavior.

## What This Operator Does

- Manages Redis clusters through Kubernetes CRDs (`RedisCluster`, `RedisBackup`, `RedisScheduledBackup`)
- Handles bootstrap, scaling, rolling updates, and failover
- Exposes stable service endpoints per cluster:
  - Standalone/sentinel modes:
    - `<name>-leader` (current primary)
    - `<name>-replica` (replicas)
    - `<name>-any` (all data pods)
    - `<name>-sentinel` (sentinel mode only)
  - Cluster mode:
    - `<name>-any` (all data pods)
    - `<name>-cluster` (headless cluster discovery/bus service)
- Supports backup and scheduled backup workflows
- Includes operational runbooks for common failure scenarios

## CloudNativePG-Inspired Design

This operator follows the same core pattern as CNPG:

- No StatefulSet for data pods; the reconciler manages Pods and PVCs directly
- Single binary, two roles:
  - `redis-operator controller` (controller-manager)
  - `redis-operator instance` (in-pod instance manager as PID 1)
- Instance managers report per-pod status and execute pod-local operations over HTTP
- Controller remains authoritative for desired state via CRD status/spec

Why this matters: the operator can enforce strict pod update ordering, explicit fencing, and controlled failover/switchover sequences instead of delegating behavior to generic controllers.

## Core Behavior

- Replication topology: primary + replicas (Redis native replication)
- Failover safety:
  - Fencing-first failover flow to reduce split-brain risk
  - Boot-time split-brain guard in instance manager: non-primary pods always come up as replicas of `status.currentPrimary`
- Rolling updates:
  - Replicas updated first
  - Primary updated last with switchover sequencing
- Secrets/config:
  - If `spec.authSecret` is omitted, the operator creates `<cluster>-auth`, persists `spec.authSecret`, and enforces Redis authentication
  - Secrets are mounted via projected volumes and tracked with resource versions in status
  - Secret updates trigger reconciliation and in-pod config refresh logic

## Supported Modes

| Mode | Status |
| --- | --- |
| `standalone` | Supported |
| `sentinel` | Supported (requires at least 3 data instances) |
| `cluster` | Supported |

A running `standalone` cluster can be upgraded to `sentinel` (HA) in place by
editing `spec.mode` (and scaling `spec.instances` to at least 3) — the existing
primary and its data are preserved, with no backup/restore needed. Only
`standalone -> sentinel` is supported in place; see
[docs/runbooks/standalone-to-sentinel-migration.md](docs/runbooks/standalone-to-sentinel-migration.md).

## Quick Start

Set a release version once:

```bash
VERSION="v0.1.5"
```

### 1) Install the operator

Option A: one command with Kustomize (CRDs + RBAC + controller + webhooks).

```bash
kubectl apply -k "github.com/howl-cloud/redis-operator/config/default?ref=${VERSION}"
```

Option B: Helm OCI chart.
Charts are published to GHCR at `ghcr.io/howl-cloud/charts/redis-operator` on version tags.

Install CRDs first:

```bash
kubectl apply -f "https://raw.githubusercontent.com/howl-cloud/redis-operator/${VERSION}/config/crd/bases/redis.io_redisclusters.yaml"
kubectl apply -f "https://raw.githubusercontent.com/howl-cloud/redis-operator/${VERSION}/config/crd/bases/redis.io_redisbackups.yaml"
kubectl apply -f "https://raw.githubusercontent.com/howl-cloud/redis-operator/${VERSION}/config/crd/bases/redis.io_redisscheduledbackups.yaml"
```

Install the chart:

```bash
helm upgrade --install redis-operator oci://ghcr.io/howl-cloud/charts/redis-operator \
  --version "${VERSION#v}" \
  --namespace redis-operator-system \
  --create-namespace
```

Option C: Helm chart from local checkout.

Install CRDs first (same as above), then:

```bash
helm upgrade --install redis-operator ./charts/redis-operator \
  --namespace redis-operator-system \
  --create-namespace
```

### 2) Create a Redis cluster

```yaml
apiVersion: redis.io/v1
kind: RedisCluster
metadata:
  name: my-redis
  namespace: default
spec:
  instances: 3
  mode: standalone
  storage:
    size: 1Gi
```

```bash
kubectl apply -f rediscluster.yaml
kubectl wait --for=condition=Ready rediscluster/my-redis -n default --timeout=10m
```

### 3) Connect

```bash
PASSWORD=$(kubectl get secret my-redis-auth -n default -o jsonpath='{.data.password}' | base64 -d)
kubectl port-forward svc/my-redis-leader 6379:6379 -n default
redis-cli -a "$PASSWORD" -h 127.0.0.1 -p 6379 ping
```

## Storage

`spec.storage.type` selects how each data pod backs `/data`:

- **`pvc`** (default) — a PersistentVolumeClaim per pod. Data survives pod recreation, rescheduling, and rolling updates. `storage.size` is the PVC request and `storage.storageClassName` selects the StorageClass. This is the right choice for any cluster that holds state you care about.
- **`emptyDir`** — pod-local ephemeral storage. Use this for caches and disposable queue backends where data loss on pod recreation is acceptable. `storage.size` is applied as the emptyDir `sizeLimit` (the pod is evicted if it exceeds it); `storageClassName` is not allowed.

```yaml
spec:
  instances: 3
  mode: standalone
  storage:
    type: emptyDir
    size: 1Gi
```

**Data-loss semantics for `emptyDir`:** an `emptyDir` volume lives and dies with its pod. Any event that recreates a pod — node drain or failure, eviction (including exceeding `size`), a rolling update, or a manual delete — **permanently discards that pod's Redis data**. In sentinel mode a recreated replica re-syncs from the current primary, but if all pods are lost at once (or the standalone pod is recreated) the dataset is gone. Because the data is disposable:

- PVCs are never created, and PVC resize, dangling-PVC accounting, and PVC health status do not apply.
- `storage.type` is immutable after creation (switching backends would destroy or migrate data).
- `spec.bootstrap` (restore-from-backup) is rejected — restored data would not survive the first pod recreation.

On-demand and scheduled backups still work for `emptyDir` clusters (they snapshot a live pod), but a backup only captures the data present at backup time.

## Memory

A Kubernetes memory **limit** and Redis **`maxmemory`** are enforced by different layers. If a pod has `spec.resources.limits.memory` but Redis has no `maxmemory`, Redis keeps allocating until the kernel OOM-kills the container — an abrupt, unpredictable failure. Setting `maxmemory` makes Redis itself enforce a ceiling *below* the container limit, so it applies its eviction policy (or rejects writes) instead of being killed.

Configure this with first-class fields under `spec.memory` rather than raw `spec.redis` keys, so the operator can keep `maxmemory` consistent with the container limit and validate unsafe combinations:

```yaml
spec:
  resources:
    limits:
      memory: 1Gi
  memory:
    maxMemoryPercent: 75      # maxmemory = 75% of the container limit (768Mi)
    maxMemoryPolicy: allkeys-lru
```

- **`maxMemory`** — an explicit value (e.g. `512Mi`). Mutually exclusive with `maxMemoryPercent`.
- **`maxMemoryPercent`** — derives `maxmemory` as a percent (1–100) of `spec.resources.limits.memory`. Requires that limit. Leave headroom (≈75%) for replication buffers, copy-on-write during persistence (`BGSAVE`/AOF rewrite), and fragmentation; using the full limit risks OOM despite `maxmemory`.
- **`maxMemoryPolicy`** — the eviction policy. Defaults to **`noeviction`**, which rejects writes at the limit instead of silently dropping data. Choose `allkeys-lru`/`allkeys-lfu` for caches, or a `volatile-*` policy to evict only keys with a TTL.

Both fields are applied live (via `CONFIG SET`) and persisted to the rendered `redis.conf`. The webhook rejects setting `maxmemory`/`maxmemory-policy` in both `spec.memory` and `spec.redis`, and warns when a memory limit is set with no `maxmemory` configured. See [`docs/memory.md`](docs/memory.md) for the full interaction model.

## APIs

- `RedisCluster`: cluster lifecycle, topology, resources, secrets, scheduling rules
- `RedisBackup`: one-off backup execution (S3-compatible or Azure Blob destinations)
- `RedisScheduledBackup`: cron-driven backup scheduling

Backup destinations, credential secret keys, and example manifests are documented in `internal/controller/backup/README.md`.

## Docs

- Architecture and component docs: `internal/controller/cluster/README.md`, `internal/instance-manager/README.md`
- Memory and eviction: `docs/memory.md`
- Monitoring and alerting: `docs/monitoring.md`
- Runbooks: `docs/runbooks/index.md`
- Upgrade guidance: `docs/upgrade.md`
- Contributor guide: `CONTRIBUTING.md`

## Credits

- Horizon Web Labs.
