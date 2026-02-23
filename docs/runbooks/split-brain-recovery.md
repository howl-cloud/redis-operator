# Runbook: Split-brain recovery

**Severity**: P0/P1  
**Estimated time**: 15-30 minutes

Use this when two data pods appear to be primary (`role=master`) at the same time.

## Symptoms

- `status.instancesStatus` shows more than one `role=master`.
- Clients report inconsistent reads/writes.
- `status.currentPrimary` does not match observed write leader.

## Prerequisites

- Permissions to patch `RedisCluster` metadata and status, and patch Services.
- A chosen authoritative primary pod.
- Shell variables:

```bash
export NS=<rediscluster-namespace>
export CLUSTER=<rediscluster-name>
```

## Diagnosis

1. Capture current status:

```bash
kubectl get rediscluster "$CLUSTER" -n "$NS" -o yaml
kubectl get rediscluster "$CLUSTER" -n "$NS" -o go-template='{{range $name,$s := .status.instancesStatus}}{{printf "%s role=%s connected=%t replOffset=%d masterLink=%s\n" $name $s.role $s.connected $s.replicationOffset $s.masterLinkStatus}}{{end}}'
```

2. Identify authoritative primary:
- Prefer the primary with the highest `replicationOffset`.
- If application owners confirm a different source of truth, use that.

3. Set working variables:

```bash
export AUTHORITATIVE=<authoritative-primary-pod>
export STALE_PRIMARY=<stale-primary-pod>
```

## Recovery steps

1. Fence the stale primary first:

```bash
kubectl patch rediscluster "$CLUSTER" -n "$NS" --type=merge \
  -p "{\"metadata\":{\"annotations\":{\"redis.io/fencedInstances\":\"[\\\"$STALE_PRIMARY\\\"]\"}}}"
```

2. Point cluster status and leader service to the authoritative primary:

```bash
kubectl patch rediscluster "$CLUSTER" -n "$NS" --subresource=status --type=merge \
  -p "{\"status\":{\"currentPrimary\":\"$AUTHORITATIVE\",\"phase\":\"FailingOver\"}}"

kubectl patch service "$CLUSTER-leader" -n "$NS" --type=merge \
  -p "{\"spec\":{\"selector\":{\"redis.io/cluster\":\"$CLUSTER\",\"redis.io/instance\":\"$AUTHORITATIVE\"}}}"
```

3. Clear fencing and force stale primary cold start:

```bash
kubectl annotate rediscluster "$CLUSTER" -n "$NS" redis.io/fencedInstances-
kubectl delete pod "$STALE_PRIMARY" -n "$NS"
```

On cold start, split-brain guard uses `status.currentPrimary` and starts this pod as a replica.

## Verification

```bash
kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.status.currentPrimary}{"\n"}'
kubectl get service "$CLUSTER-leader" -n "$NS" -o jsonpath='{.spec.selector.redis\.io/instance}{"\n"}'
kubectl get rediscluster "$CLUSTER" -n "$NS" -o go-template='{{range $name,$s := .status.instancesStatus}}{{printf "%s role=%s connected=%t masterLink=%s\n" $name $s.role $s.connected $s.masterLinkStatus}}{{end}}'
kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.status.phase}{"\n"}'
```

Expected:
- Exactly one pod reports `role=master`.
- `status.currentPrimary` and `-leader` Service point to `AUTHORITATIVE`.
- Former stale primary reports `role=slave` and `masterLinkStatus=up`.
- Cluster returns to `Healthy`.

## Escalation

- If both primaries diverged significantly and authoritative source is unclear, pause writes and escalate to incident commander/data owner.
- If stale pod repeatedly returns as master after restart, keep it fenced and escalate with full status/event/log capture.
