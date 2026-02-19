# internal/instance-manager/reconciler

`InstanceReconciler` — the in-pod watch loop.

## Responsibilities

Runs a `controller-runtime` reconciler inside the pod, watching the `RedisCluster` CR. On each reconcile cycle:

1. **Config reconciliation** — if `redis.conf` content has changed (detected via ConfigMap resource version), write new config and issue `CONFIG REWRITE` + `CONFIG SET` for live-reloadable parameters. Parameters requiring restart are handled by flagging the pod for rolling update via a pod annotation.
2. **Secret reconciliation** — compare each secret's `ResourceVersion` in `cluster.status.secretsResourceVersion` against a locally cached version. On change:
   - `authSecret` rotated → `CONFIG SET requirepass <new-password>` (no restart needed).
   - `aclConfigSecret` rotated → rewrite `/data/users.acl` and issue `ACL LOAD`.
   - `tlsSecret` / `caSecret` rotated → rewrite cert files; Redis 7.x requires a restart for TLS cert changes, so flag the pod annotation for rolling update.
3. **Role reconciliation** — compare `cluster.status.currentPrimary` to this pod's name:
   - If this pod should be primary but is a replica → `REPLICAOF NO ONE`.
   - If this pod should be a replica of a different primary → `REPLICAOF <new-primary-ip> 6379`.
4. **Fencing check** — if fenced, stop `redis-server` and return without requeue.
5. **Status reporting** — collect `INFO replication`, `INFO server`, `INFO stats` and patch `RedisCluster.status.instancesStatus[<podName>]`.
