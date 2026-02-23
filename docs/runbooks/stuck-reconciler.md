# Runbook: Stuck reconciler

**Severity**: P1  
**Estimated time**: 10-15 minutes

Use this when a `RedisCluster` is not converging and reconcile loops are not making visible progress.

## Symptoms

- `status.phase` remains unchanged (for example `Degraded`) for multiple reconcile intervals.
- Expected actions (pod recreate, service selector update, PVC creation) are not happening.
- Cluster events show repeated failures or no recent reconcile activity.

## Prerequisites

- `kubectl` access to the cluster and operator namespace.
- Shell variables:

```bash
export NS=<rediscluster-namespace>
export CLUSTER=<rediscluster-name>
```

## Diagnosis

1. Check cluster status snapshot:

```bash
kubectl get rediscluster "$CLUSTER" -n "$NS" -o yaml
```

2. Check recent events for the `RedisCluster`:

```bash
kubectl get events -n "$NS" \
  --field-selector involvedObject.kind=RedisCluster,involvedObject.name="$CLUSTER" \
  --sort-by=.lastTimestamp
```

3. Locate operator deployment and namespace:

```bash
kubectl get deploy -A -l app.kubernetes.io/name=redis-operator
```

Set:

```bash
export OP_NS=<operator-namespace>
export OP_DEPLOY=<operator-deployment-name>
```

4. Check leader election lease and operator logs:

```bash
kubectl get lease redis-operator-leader -n "$OP_NS" -o yaml
kubectl logs -n "$OP_NS" deploy/"$OP_DEPLOY" --tail=300
```

## Recovery steps

1. Force re-enqueue by touching an annotation on the `RedisCluster`:

```bash
kubectl annotate rediscluster "$CLUSTER" -n "$NS" \
  runbooks.redis.io/force-reconcile-ts="$(date +%s)" --overwrite
```

2. Watch events and object status for one or two reconcile intervals (~30-60s):

```bash
kubectl get rediscluster "$CLUSTER" -n "$NS" -w
```

3. If still stuck, restart operator deployment:

```bash
kubectl rollout restart deploy/"$OP_DEPLOY" -n "$OP_NS"
kubectl rollout status deploy/"$OP_DEPLOY" -n "$OP_NS"
```

4. Re-check operator logs for new reconcile attempts:

```bash
kubectl logs -n "$OP_NS" deploy/"$OP_DEPLOY" --tail=300
```

## Verification

```bash
kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.status.phase}{"\n"}'
kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.status.readyInstances}{"\n"}'
kubectl get events -n "$NS" \
  --field-selector involvedObject.kind=RedisCluster,involvedObject.name="$CLUSTER" \
  --sort-by=.lastTimestamp
```

Expected:
- Reconcile activity resumes in logs/events.
- Status fields begin changing again.
- Cluster trends back to `Healthy`.

## Escalation

- If reconcile restarts but repeatedly fails with the same error, escalate with:
  - `RedisCluster` YAML
  - recent events
  - operator logs around the failure
- If leader election lease is not being renewed, investigate controller-manager health and RBAC before retrying.
