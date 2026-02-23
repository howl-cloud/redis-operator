# Runbook: Operator upgrade

**Severity**: P1  
**Estimated time**: 20-40 minutes

This runbook operationalizes the upgrade story from issue #7 and points to the full design/procedure in [../upgrade.md](../upgrade.md).

## Symptoms

- Planned operator version upgrade.
- Need to patch operator image without disrupting existing `RedisCluster` workloads.

## Prerequisites

- Helm access to the operator release.
- Ability to inspect operator and `RedisCluster` resources.
- Shell variables:

```bash
export OP_NS=<operator-namespace>
export RELEASE=<helm-release-name>
export CLUSTER_NS=<rediscluster-namespace>
export CLUSTER=<rediscluster-name>
```

## Diagnosis

1. Capture pre-upgrade state:

```bash
kubectl get redisclusters.redis.io -A
kubectl get pods -n "$CLUSTER_NS" -l redis.io/cluster="$CLUSTER" -o wide
kubectl get svc -n "$CLUSTER_NS" "$CLUSTER-leader" "$CLUSTER-replica" "$CLUSTER-any"
kubectl get events -n "$CLUSTER_NS" --sort-by=.lastTimestamp | tail -n 40
kubectl get rediscluster "$CLUSTER" -n "$CLUSTER_NS" -o jsonpath='{.status.currentPrimary}{"\n"}'
```

2. Confirm operator deployment identity:

```bash
kubectl get deploy -n "$OP_NS" -l app.kubernetes.io/name=redis-operator
export OP_DEPLOY="$(kubectl get deploy -n "$OP_NS" -l app.kubernetes.io/name=redis-operator -o jsonpath='{.items[0].metadata.name}')"
echo "$OP_DEPLOY"
```

## Recovery steps

1. Follow [../upgrade.md](../upgrade.md) as the source-of-truth procedure.

2. Run the Helm upgrade:

```bash
helm upgrade "$RELEASE" charts/redis-operator \
  --namespace "$OP_NS" \
  --reuse-values \
  --set image.repository=<repo> \
  --set image.tag=<new-tag>
```

3. Watch operator rollout and leader lease:

```bash
kubectl rollout status deployment/"$OP_DEPLOY" -n "$OP_NS"
kubectl get lease redis-operator-leader -n "$OP_NS"
kubectl logs -n "$OP_NS" deploy/"$OP_DEPLOY" --tail=200
```

4. Watch Redis cluster continuity during rollout:

```bash
kubectl get rediscluster "$CLUSTER" -n "$CLUSTER_NS" -o yaml
kubectl get pods -n "$CLUSTER_NS" -l redis.io/cluster="$CLUSTER" -w
```

## Verification

```bash
kubectl get rediscluster "$CLUSTER" -n "$CLUSTER_NS" -o jsonpath='{.status.phase}{"\n"}'
kubectl get rediscluster "$CLUSTER" -n "$CLUSTER_NS" -o jsonpath='{.status.currentPrimary}{"\n"}'
kubectl get pods -n "$CLUSTER_NS" -l redis.io/cluster="$CLUSTER"
```

Expected:
- Operator rollout completes successfully with leader election continuity.
- Existing Redis clusters remain available.
- Data pods roll in controlled order only when spec hash is outdated (replicas first, primary last).
- No bulk restart of all healthy data pods.
- Cluster returns to `Healthy`.

## Escalation

- If availability degrades during upgrade, pause and assess rollback:

```bash
helm rollback "$RELEASE" <previous-revision> --namespace "$OP_NS"
```

- Capture logs/events and compare against [../upgrade.md](../upgrade.md) expectations and issue #7 (`.issues/007_operator_upgrade_story.md`).
