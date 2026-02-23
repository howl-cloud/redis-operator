# Runbook: Manual failover

**Severity**: P1  
**Estimated time**: 10-20 minutes

Use this when the operator is unavailable or not making failover progress, but Redis data pods are still running.

## Symptoms

- Current primary pod is unreachable or unhealthy.
- `RedisCluster` remains degraded and does not fail over automatically.
- Operator deployment is down, crashlooping, or otherwise unavailable.

## Prerequisites

- Cluster-admin or equivalent permissions to patch `RedisCluster` (including status) and Services.
- A healthy replica pod that can be promoted.
- Shell variables:

```bash
export NS=<rediscluster-namespace>
export CLUSTER=<rediscluster-name>
```

## Diagnosis

1. Inspect cluster status and current primary:

```bash
kubectl get rediscluster "$CLUSTER" -n "$NS" -o yaml
kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.status.currentPrimary}{"\n"}'
```

2. Inspect pod health and instance statuses:

```bash
kubectl get pods -n "$NS" -l redis.io/cluster="$CLUSTER",redis.io/workload=data -o wide
kubectl get rediscluster "$CLUSTER" -n "$NS" -o go-template='{{range $name,$s := .status.instancesStatus}}{{printf "%s role=%s connected=%t replOffset=%d masterLink=%s\n" $name $s.role $s.connected $s.replicationOffset $s.masterLinkStatus}}{{end}}'
```

3. Choose:
- `CURRENT_PRIMARY`: failed/stale primary pod name
- `CANDIDATE`: connected replica with the highest replication offset

## Recovery steps

1. Set working variables:

```bash
export CURRENT_PRIMARY=<failed-primary-pod>
export CANDIDATE=<replica-to-promote>
```

2. Fence the former primary first:

```bash
kubectl patch rediscluster "$CLUSTER" -n "$NS" --type=merge \
  -p "{\"metadata\":{\"annotations\":{\"redis.io/fencedInstances\":\"[\\\"$CURRENT_PRIMARY\\\"]\"}}}"
```

3. Promote the candidate directly through its instance-manager API.

Terminal A:

```bash
kubectl port-forward -n "$NS" "pod/$CANDIDATE" 18080:8080
```

Terminal B:

```bash
curl -sS -X POST http://127.0.0.1:18080/v1/promote
```

4. Patch cluster status to the new primary:

```bash
kubectl patch rediscluster "$CLUSTER" -n "$NS" --subresource=status --type=merge \
  -p "{\"status\":{\"currentPrimary\":\"$CANDIDATE\",\"phase\":\"FailingOver\"}}"
```

5. Patch the leader Service selector:

```bash
kubectl patch service "$CLUSTER-leader" -n "$NS" --type=merge \
  -p "{\"spec\":{\"selector\":{\"redis.io/cluster\":\"$CLUSTER\",\"redis.io/instance\":\"$CANDIDATE\"}}}"
```

6. Clear fencing and force the former primary to cold-start as a replica:

```bash
kubectl annotate rediscluster "$CLUSTER" -n "$NS" redis.io/fencedInstances-
kubectl delete pod "$CURRENT_PRIMARY" -n "$NS"
```

## Verification

```bash
kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.status.currentPrimary}{"\n"}'
kubectl get service "$CLUSTER-leader" -n "$NS" -o jsonpath='{.spec.selector.redis\.io/instance}{"\n"}'
kubectl get rediscluster "$CLUSTER" -n "$NS" -o go-template='{{range $name,$s := .status.instancesStatus}}{{printf "%s role=%s connected=%t masterLink=%s\n" $name $s.role $s.connected $s.masterLinkStatus}}{{end}}'
kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.status.phase}{"\n"}'
```

Expected:
- `status.currentPrimary` equals `CANDIDATE`.
- `-leader` Service points to `CANDIDATE`.
- Former primary reports as replica (`role=slave`).
- Cluster returns to `Healthy`.

## Escalation

- If promotion endpoint fails or pod is unreachable, choose a different replica candidate.
- If no connected replica exists, treat as data-loss/disaster recovery and escalate immediately.
- If split-brain persists, follow [split-brain-recovery.md](split-brain-recovery.md).
