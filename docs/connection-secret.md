# Connection Secret

The operator can publish a Kubernetes Secret containing ready-to-use Redis
connection details for a `RedisCluster`. This lets application workloads and
platforms (Hostess and similar) consume `host`, `port`, `password`, and a full
connection `url` directly, instead of reconstructing them from
[service naming conventions](service-contract.md) and the auth Secret.

It is **opt-in**: nothing is published unless you set `spec.connectionSecret`.

## Enabling it

```yaml
apiVersion: redis.io/v1
kind: RedisCluster
metadata:
  name: my-redis
  namespace: default
spec:
  mode: standalone
  instances: 2
  storage:
    size: 1Gi
  connectionSecret:
    name: my-redis-connection
```

The operator creates and maintains a Secret named `my-redis-connection` in the
cluster's namespace.

## What the Secret contains

> ⚠️ **This Secret is credential-bearing.** It embeds the Redis password (in the
> `password` key and inside every `*_url`). It lives in the same namespace and
> under the same RBAC as the cluster's auth Secret, so it carries no *additional*
> exposure — but treat it like any other credential: restrict read access with
> RBAC and avoid logging the `url` keys.

All values are stored as Secret data (base64 in the API, plaintext when mounted).
Hosts are namespace-qualified (`<service>.<namespace>.svc`), resolvable from any
namespace. The scheme is `rediss://` when TLS is enabled (`spec.tlsSecret` +
`spec.caSecret`), otherwise `redis://`.

### Always present

| Key | Example | Meaning |
|---|---|---|
| `cluster_name` | `my-redis` | The source `RedisCluster` name. |
| `namespace` | `default` | The source namespace. |
| `mode` | `standalone` | `standalone`, `sentinel`, or `cluster`. |
| `host` | `my-redis-leader.default.svc` | Primary endpoint for writes (`-cluster` headless service in cluster mode). |
| `port` | `6379` | Redis client port. |
| `username` | `default` | Redis ACL user. |
| `password` | `9f2c…` | The Redis password (omitted only if the cluster has no auth Secret). |
| `url` | `redis://default:9f2c…@my-redis-leader.default.svc:6379` | Full connection URL for writes. |

### `standalone` and `sentinel` modes

| Key | Meaning |
|---|---|
| `leader_host` / `leader_url` | The current primary (writes). Same target as `host`/`url`. |
| `replica_host` / `replica_url` | Replica read endpoint. Has no endpoints on a single-instance cluster — fall back to `host`. |

### `sentinel` mode (additional)

| Key | Meaning |
|---|---|
| `sentinel_host` | Sentinel discovery service (`<name>-sentinel.<ns>.svc`). |
| `sentinel_port` | `26379`. |
| `master_name` | The master name to ask Sentinel for — equal to the `RedisCluster` name. |

A Sentinel-aware client should connect via `sentinel_host`/`master_name` so it
follows failovers natively.

### `cluster` mode

Redis Cluster shards data and clients follow `MOVED`/`ASK` redirects, so there is
no single leader/replica endpoint. `host`/`url` point at the headless
`<name>-cluster` Service; `leader_*`/`replica_*`/`sentinel_*` keys are **not**
written. Point a cluster-aware client at `host` and let it learn the slot map.

## Lifecycle

- **Ownership.** The Secret is owned by the `RedisCluster` (owner reference), so
  it is garbage-collected automatically when the cluster is deleted.
- **Disable / rename.** If you remove `spec.connectionSecret` (or change its
  `name`), the operator deletes the previously published Secret so no
  credential-bearing projection is left behind. Only the operator's own
  projections (identified by an internal marker label and owner reference) are
  removed — Secrets you manage are never touched.
- **Updates.** It is re-rendered on every reconcile, so password rotation and
  service changes propagate without manual action (within the reconcile
  interval).
- **Collision safety.** If a Secret with the configured name already exists and
  is **not** owned by this `RedisCluster`, the operator refuses to overwrite it
  and records a `ConnectionSecretConflict` warning event. Choose a name that does
  not collide with a Secret you manage yourself.

## Consuming it from an application

Project the keys you need as environment variables:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  template:
    spec:
      containers:
        - name: app
          image: my-app:latest
          env:
            - name: REDIS_URL
              valueFrom:
                secretKeyRef:
                  name: my-redis-connection
                  key: url
            - name: REDIS_HOST
              valueFrom:
                secretKeyRef:
                  name: my-redis-connection
                  key: host
            - name: REDIS_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: my-redis-connection
                  key: password
```

## Hostess integration

Hostess can wire its `${redis.*}` placeholders straight from the operator-owned
Secret instead of rebuilding them:

| Hostess placeholder | Secret key |
|---|---|
| `${redis.host}` | `host` |
| `${redis.port}` | `port` |
| `${redis.password}` | `password` |
| `${redis.url}` | `url` |

Because the Secret tracks rotation and failover, Hostess does not need custom
adapter logic to follow auth changes or primary switchovers.
