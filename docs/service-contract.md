# Service & Label Integration Contract

This document is the **integration contract** between the operator and external
platforms (Hostess and similar). It describes the Services and pod labels the
operator generates for a `RedisCluster`, which of them are stable commitments,
and how to use them to route traffic and discover pods.

Platform code should depend on the names, selectors, and labels documented here
rather than reverse-engineering reconciler behavior. The contract is enforced by
`internal/controller/cluster/service_contract_test.go` — if a name or selector
below changes, that test fails and this document must change in the same commit.

> Scope: this contract covers the resources the operator creates. Connection
> credentials live in the cluster's auth Secret (`spec.authSecret`, or the
> generated `<name>-auth` if omitted); see the README for secret handling. For a
> ready-to-use, operator-maintained Secret bundling host/port/url/password and
> per-role endpoints, see [Connection Secret](connection-secret.md)
> (`spec.connectionSecret`).

---

## Generated Services by mode

All Services live in the same namespace as the `RedisCluster` and expose Redis on
TCP port **6379** (Sentinel on **26379**, cluster bus on **16379**). `<name>` is
the `RedisCluster` object name.

### `standalone` mode

| Service | Selector | Port | Use it for |
|---|---|---|---|
| `<name>-leader` | `redis.io/cluster=<name>, redis.io/role=primary` → narrows to `redis.io/instance=<primary-pod>` once a primary is known | 6379 | **Writes** (and strongly-consistent reads). Always points at the single current primary. |
| `<name>-replica` | `redis.io/cluster=<name>, redis.io/role=replica` | 6379 | **Reads** from replicas (eventually consistent). Empty endpoints if there are no replicas. |
| `<name>-any` | `redis.io/cluster=<name>, redis.io/workload=data` | 6379 | **All-pod discovery** — every data pod regardless of role. |

A single-instance standalone cluster has only the primary, so `-replica` will
have no endpoints; route reads to `-leader` or `-any` in that case.

### `sentinel` mode

Everything from standalone, **plus**:

| Service | Selector | Port | Use it for |
|---|---|---|---|
| `<name>-sentinel` | `redis.io/cluster=<name>, redis.io/role=sentinel` | 26379 | **Sentinel discovery.** Connect a Sentinel-aware client here to discover the current master and subscribe to failover events. |

In sentinel mode the operator still maintains `-leader` pointing at the current
primary, so non-Sentinel-aware clients can keep using `-leader` for writes. A
Sentinel-aware client should prefer `-sentinel` so it follows failovers natively.

### `cluster` mode

Redis Cluster shards data across nodes and clients follow `MOVED`/`ASK`
redirects, so there is **no** `-leader`/`-replica`/`-sentinel` Service. If a
cluster was migrated from another mode, the operator deletes those legacy
Services.

| Service | Selector | Ports | Use it for |
|---|---|---|---|
| `<name>-cluster` | `redis.io/cluster=<name>, redis.io/workload=data` | 6379 (client), 16379 (cluster bus) | **Cluster discovery.** Headless Service (`clusterIP: None`) returning every data pod IP. Point a cluster-aware client at it; the client learns the slot topology via the Redis Cluster protocol. |
| `<name>-any` | `redis.io/cluster=<name>, redis.io/workload=data` | 6379 | A load-balanced entry point to any data pod — useful as a bootstrap/seed address. The client still resolves shard ownership itself. |

Per-shard primary/replica roles are exposed only via pod labels (see below), not
via dedicated Services.

---

## Pod labels

Every data and sentinel pod carries these labels. Service selectors match against
them.

### Compatibility commitments

These keys and values are **stable**. Platforms may depend on them; they will not
change without a major-version contract break.

| Label | Values | Meaning |
|---|---|---|
| `redis.io/cluster` | `<name>` | The owning `RedisCluster`. Use as the primary filter for all discovery. |
| `redis.io/instance` | pod name | Identifies a specific pod. The `-leader` Service narrows to this once a primary is known. |
| `redis.io/role` | `primary`, `replica`, `sentinel` | The pod's replication role. Drives `-leader`/`-replica`/`-sentinel` selectors. |
| `redis.io/workload` | `data`, `sentinel` | Coarse workload class. `data` = a Redis data pod; `sentinel` = a Sentinel pod. Drives `-any`/`-cluster` selectors. |

Service **names** (`-leader`, `-replica`, `-any`, `-sentinel`, `-cluster`) are
also part of this commitment.

### Internal — no stability guarantee

These exist for the operator's own bookkeeping. They are visible on the objects
but **may change or disappear** between releases. Do not build platform logic on
them.

| Label / annotation | Notes |
|---|---|
| `redis.io/shard` | Cluster-mode shard index (`s0`, `s1`, …). Informational; prefer the Redis Cluster protocol for slot ownership. |
| `redis.io/shard-role` | Cluster-mode per-shard role (`primary`/`replica`). Informational. |
| `redis.io/spec-hash` (annotation) | Rolling-update bookkeeping. Internal. |

---

## Common platform tasks

### Route writes / create an external primary service

The operator's `-leader` Service is `ClusterIP` (in-cluster only). To expose the
primary externally, create your own Service that reuses the committed selector —
do **not** hardcode a pod name, so it follows failovers:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-redis-primary-ext
  namespace: default
spec:
  type: LoadBalancer        # or NodePort
  selector:
    redis.io/cluster: my-redis
    redis.io/role: primary
  ports:
    - name: redis
      port: 6379
      targetPort: 6379
```

### Port-forward to the leader

```bash
kubectl port-forward svc/my-redis-leader 6379:6379 -n default
```

### List Redis data pods

```bash
kubectl get pods -n default -l 'redis.io/cluster=my-redis,redis.io/workload=data'
```

### Find replicas

```bash
kubectl get pods -n default -l 'redis.io/cluster=my-redis,redis.io/role=replica'
```

### Find the current primary

```bash
kubectl get pods -n default -l 'redis.io/cluster=my-redis,redis.io/role=primary'
```

The authoritative primary is also published at `status.currentPrimary` on the
`RedisCluster` object:

```bash
kubectl get rediscluster my-redis -n default -o jsonpath='{.status.currentPrimary}'
```

---

## Routing cheat-sheet

| Intent | standalone / sentinel | cluster |
|---|---|---|
| Writes | `<name>-leader` | `<name>-cluster` (cluster-aware client) |
| Reads (replica) | `<name>-replica` | `<name>-cluster` with replica-read routing |
| Any data pod | `<name>-any` | `<name>-any` |
| Sentinel discovery | `<name>-sentinel` | — |
| Cluster discovery | — | `<name>-cluster` (headless) |
