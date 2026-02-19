# internal/controller/cluster

`ClusterReconciler` — the primary reconciler for `RedisCluster` resources.

## Reconciliation Flow

The `Reconcile()` function delegates to `reconcile()`, which executes ordered sub-steps:

1. **Global resources** — ServiceAccount, RBAC, ConfigMap (redis.conf template), PodDisruptionBudget
2. **Secret resolution** — resolve all secrets referenced in `ClusterSpec`, refresh `status.secretsResourceVersion`
3. **Services** — ensure `-leader`, `-replica`, and `-any` Services exist with correct selectors
4. **Status collection** — HTTP-poll each instance manager pod for live Redis status
5. **Status update** — write collected data into `cluster.status`
6. **Reachability check** — requeue if any expected instance is unreachable
7. **PVC reconciliation** — create missing PVCs, track dangling/resizing/unusable
8. **Pod reconciliation** — create primary, join replicas, scale up/down, rolling updates

## Key Files

| File | Description |
|------|-------------|
| `reconciler.go` | `ClusterReconciler` struct, `Reconcile()` and `reconcile()` |
| `secrets.go` | `reconcileSecrets()`: resolve referenced secrets, auto-generate auth secret if absent, update `status.secretsResourceVersion` |
| `pods.go` | Pod creation (primary bootstrap, replica join), deletion of terminated pods |
| `pvcs.go` | PVC creation, lifecycle tracking (healthy/dangling/resizing/unusable) |
| `pdb.go` | Create and update `PodDisruptionBudget`; `minAvailable` = `max(1, instances-1)` |
| `services.go` | Ensure `-leader`, `-replica`, `-any` Services and update selectors on failover |
| `status.go` | HTTP polling of instance managers, status and condition updates |
| `rolling_update.go` | Rolling update logic: replicas first (highest ordinal), primary last via switchover |
| `fencing.go` | Set/clear fencing annotation on `RedisCluster` to stop specific instances |

## Secret Resolution

`reconcileSecrets()` runs early in every reconcile cycle:

1. For each secret reference in `ClusterSpec` (`authSecret`, `aclConfigSecret`, `tlsSecret`, `caSecret`, `backupCredentialsSecret`), fetch the `Secret` and record its `ResourceVersion` into `status.secretsResourceVersion`.
2. If `authSecret` is not set, generate a random password, store it in `<cluster-name>-auth`, and set `spec.authSecret` to point at it.
3. Secrets are injected into pods as **projected volumes** (not env vars) so rotations reach the pod filesystem automatically.
4. The reconciler watches `Secret` objects and uses `UsesSecret(name) bool` — checking `status.secretsResourceVersion` — to decide whether to enqueue a cluster when a secret changes.

When a secret's `ResourceVersion` changes between reconcile cycles, the reconciler enqueues the cluster and the instance manager detects the new version, applying the change live (see `internal/instance-manager/reconciler/README.md`).

## Pod Lifecycle

Pods are **not** managed by a StatefulSet. The reconciler creates, updates, and deletes `Pod` objects directly. Each pod's PVC is created separately and reused across pod restarts — the pod is deleted and recreated with the same PVC when its spec needs updating.

## Failover

1. Operator detects primary is unreachable (HTTP poll timeout/error).
2. **Fence the former primary first** — set the fence annotation on `RedisCluster` for that pod before promoting anything. This prevents split-brain if the former primary recovers mid-failover.
3. Selects the replica with the smallest replication lag (`INFO replication` offset from status).
4. Issues `POST /v1/promote` to that replica's pod IP.
5. Instance manager runs `REPLICAOF NO ONE`.
6. Operator updates `-leader` Service selector to the new primary's pod labels.
7. Operator updates `cluster.status.currentPrimary`.
8. Operator removes the fence annotation from the former primary.
9. Former primary pod restarts; instance manager detects it is no longer `currentPrimary` and starts as a replica (see split-brain prevention below).

## Split-Brain Prevention

The fencing-first failover sequence (step 2 above) is the primary defence. The instance manager provides a second line of defence at startup:

On every cold start, before `redis-server` is launched, the instance manager compares `POD_NAME` against `cluster.status.currentPrimary`:
- **Match** → start as primary (no `replicaof` directive in `redis.conf`).
- **No match** → always start with `replicaof <currentPrimary-ip> 6379`, regardless of any local data state. Redis will perform a `PSYNC` (partial) or full `SYNC` as needed; any data the former primary wrote after the failover is discarded, matching CNPG's `pg_rewind` behaviour.

This ensures a recovering former primary can never self-elect: it unconditionally follows `status.currentPrimary` on boot.

## PodDisruptionBudget

`pdb.go` creates and reconciles a `PodDisruptionBudget` for every `RedisCluster` where `spec.enablePodDisruptionBudget` is `true` (the default). The PDB sets `minAvailable` to `max(1, spec.instances - 1)`, allowing at most one pod to be voluntarily disrupted at a time. The PDB is updated whenever `spec.instances` changes.
