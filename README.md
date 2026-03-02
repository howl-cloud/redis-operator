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

## APIs

- `RedisCluster`: cluster lifecycle, topology, resources, secrets, scheduling rules
- `RedisBackup`: one-off backup execution
- `RedisScheduledBackup`: cron-driven backup scheduling

## Docs

- Architecture and component docs: `internal/controller/cluster/README.md`, `internal/instance-manager/README.md`
- Runbooks: `docs/runbooks/index.md`
- Upgrade guidance: `docs/upgrade.md`
- Contributor guide: `CONTRIBUTING.md`

## Credits

- Horizon Web Labs.
