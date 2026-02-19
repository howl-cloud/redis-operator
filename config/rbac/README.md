# config/rbac

RBAC manifests for the operator and instance manager.

## Operator (controller-manager)

| Resource | Verbs | Purpose |
|----------|-------|---------|
| `redisclusters`, `redisbackups`, `redisscheduledbackups` | `*` | Manage CRs |
| `redisclusters/status` | `get`, `patch`, `update` | Update cluster status |
| `pods`, `pvcs`, `services`, `configmaps`, `secrets` | `*` | Manage cluster resources |
| `events` | `create`, `patch` | Emit Kubernetes events |
| `leases` | `*` | Leader election |

## Instance Manager (per-pod ServiceAccount)

The operator creates a dedicated `ServiceAccount` and `Role`/`RoleBinding` per `RedisCluster`, scoped to the cluster's namespace.

| Resource | Verbs | Purpose |
|----------|-------|---------|
| `redisclusters` | `get`, `watch` | Read own cluster spec |
| `redisclusters/status` | `patch` | Report instance status |
| `configmaps`, `secrets` | `get`, `watch` | Read redis.conf, TLS certs |
| `events` | `create` | Emit pod-level events |
