# Runbook: PVC corruption (single replica rebuild)

**Severity**: P1  
**Estimated time**: 15-25 minutes

Use this when one replica's PVC is corrupted and that replica must be rebuilt from the current primary.

## Symptoms

- A single replica pod repeatedly crashes or fails readiness.
- Pod events/logs show storage or filesystem errors.
- `status.instancesStatus` shows one replica as disconnected/stale.

## Prerequisites

- At least one healthy primary is available.
- The corrupted instance is a replica (not the current primary).
- Shell variables:

```bash
export NS=<rediscluster-namespace>
export CLUSTER=<rediscluster-name>
```

## Diagnosis

1. Identify current primary:

```bash
kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.status.currentPrimary}{"\n"}'
```

2. Inspect per-instance status:

```bash
kubectl get rediscluster "$CLUSTER" -n "$NS" -o go-template='{{range $name,$s := .status.instancesStatus}}{{printf "%s role=%s connected=%t replOffset=%d masterLink=%s\n" $name $s.role $s.connected $s.replicationOffset $s.masterLinkStatus}}{{end}}'
```

3. Inspect suspected broken replica pod:

```bash
export BROKEN_REPLICA=<replica-pod-name>
kubectl describe pod "$BROKEN_REPLICA" -n "$NS"
kubectl logs "$BROKEN_REPLICA" -n "$NS" --tail=200
```

If `BROKEN_REPLICA` equals `status.currentPrimary`, stop and first run [manual-failover.md](manual-failover.md).

## Recovery steps

1. Derive PVC name for the broken replica:

```bash
export INDEX="${BROKEN_REPLICA##*-}"
export BROKEN_PVC="${CLUSTER}-data-${INDEX}"
```

2. Delete the broken replica pod first:

```bash
kubectl delete pod "$BROKEN_REPLICA" -n "$NS" --wait=true
```

3. Delete only the corrupted PVC:

```bash
kubectl delete pvc "$BROKEN_PVC" -n "$NS" --wait=true
```

4. Trigger reconciliation so the operator recreates PVC and pod immediately:

```bash
kubectl annotate rediscluster "$CLUSTER" -n "$NS" \
  runbooks.redis.io/rebuild-replica-ts="$(date +%s)" --overwrite
```

5. Watch for PVC and pod recreation:

```bash
kubectl get pvc -n "$NS" -l redis.io/cluster="$CLUSTER" -w
kubectl get pods -n "$NS" -l redis.io/cluster="$CLUSTER",redis.io/workload=data -w
```

## Verification

```bash
kubectl get pod "$BROKEN_REPLICA" -n "$NS"
kubectl get rediscluster "$CLUSTER" -n "$NS" -o go-template='{{range $name,$s := .status.instancesStatus}}{{printf "%s role=%s connected=%t masterLink=%s\n" $name $s.role $s.connected $s.masterLinkStatus}}{{end}}'
kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.status.phase}{"\n"}'
```

Expected:
- New PVC exists for the rebuilt replica.
- Recreated pod becomes ready.
- Rebuilt instance reports `role=slave` and `masterLinkStatus=up`.
- Cluster returns to `Healthy`.

## Escalation

- If rebuilt replica does not catch up, inspect primary health and network path between pods.
- If multiple PVCs are corrupted, treat as broader storage incident and escalate to platform/storage team.
