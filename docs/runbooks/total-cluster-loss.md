# Runbook: Total cluster loss (pods deleted, PVCs intact)

**Severity**: P1  
**Estimated time**: 15-30 minutes

Use this when all Redis data pods are gone but the PVCs still exist. The operator should recreate pods and reattach existing PVCs.

## Symptoms

- No data pods exist for the cluster.
- PVCs named `<cluster>-data-<index>` still exist.
- `RedisCluster` is degraded or stuck in non-healthy phase.

## Prerequisites

- Operator deployment is running.
- PVCs for the cluster are intact.
- Shell variables:

```bash
export NS=<rediscluster-namespace>
export CLUSTER=<rediscluster-name>
```

## Diagnosis

1. Confirm data pods are missing:

```bash
kubectl get pods -n "$NS" -l redis.io/cluster="$CLUSTER",redis.io/workload=data
```

2. Confirm PVCs are still present:

```bash
kubectl get pvc -n "$NS" -l redis.io/cluster="$CLUSTER"
```

3. Check desired instance count and current primary:

```bash
kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.spec.instances}{"\n"}'
kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.status.currentPrimary}{"\n"}'
```

## Recovery steps

1. Do not delete PVCs.

2. Trigger immediate reconciliation by touching an annotation:

```bash
kubectl annotate rediscluster "$CLUSTER" -n "$NS" \
  runbooks.redis.io/recover-from-pod-loss-ts="$(date +%s)" --overwrite
```

3. Watch pod recreation:

```bash
kubectl get pods -n "$NS" -l redis.io/cluster="$CLUSTER",redis.io/workload=data -w
```

4. If pods are not recreated within ~60 seconds, check operator logs/events and resolve operator health first:

```bash
kubectl get events -n "$NS" \
  --field-selector involvedObject.kind=RedisCluster,involvedObject.name="$CLUSTER" \
  --sort-by=.lastTimestamp
```

5. Verify recreated pods are bound to expected PVC names:

```bash
kubectl get pod -n "$NS" -l redis.io/cluster="$CLUSTER",redis.io/workload=data \
  -o jsonpath='{range .items[*]}{.metadata.name}{" -> "}{.spec.volumes[?(@.name=="data")].persistentVolumeClaim.claimName}{"\n"}{end}'
```

## Verification

```bash
kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.status.currentPrimary}{"\n"}'
kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.status.readyInstances}{"\n"}'
kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.status.phase}{"\n"}'
kubectl get pvc -n "$NS" -l redis.io/cluster="$CLUSTER"
```

Expected:
- Data pods are recreated to match `spec.instances`.
- Pods are attached to existing `<cluster>-data-<index>` PVCs.
- `status.currentPrimary` is set and reachable.
- Cluster returns to `Healthy`.

## Escalation

- If PVCs are missing or failed to bind, escalate to storage/platform team.
- If pods recreate but Redis cannot start from PVC data, move to [pvc-corruption.md](pvc-corruption.md) for per-replica recovery.
- If no primary becomes available, follow [manual-failover.md](manual-failover.md).
