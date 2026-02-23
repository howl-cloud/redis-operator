# Operational Runbooks

These runbooks are for incident response and production operations of `RedisCluster` resources managed by this operator.

## Runbook Index

| Scenario | Runbook |
| --- | --- |
| Manual failover when operator is unavailable | [manual-failover.md](manual-failover.md) |
| Reconciler loop appears stuck | [stuck-reconciler.md](stuck-reconciler.md) |
| All data pods lost, PVCs intact | [total-cluster-loss.md](total-cluster-loss.md) |
| Single corrupted replica PVC | [pvc-corruption.md](pvc-corruption.md) |
| Suspected split-brain (two primaries) | [split-brain-recovery.md](split-brain-recovery.md) |
| Rotate `authSecret` without pod restarts | [secret-rotation.md](secret-rotation.md) |
| Upgrade operator safely | [operator-upgrade.md](operator-upgrade.md) |

## Shared Conventions

- Set shell variables before executing commands:
  - `NS=<rediscluster namespace>`
  - `CLUSTER=<rediscluster name>`
- Prefer touching annotations to force immediate reconciliation; the controller also reconciles periodically.
- Validate the cluster has returned to `status.phase=Healthy` before closing the incident.
