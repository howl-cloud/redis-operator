# Runbook: Migrate standalone to sentinel (HA)

**Severity**: Planned change
**Estimated time**: 5-15 minutes (depends on dataset size / replica sync)

Use this to move a running `standalone` `RedisCluster` to `sentinel` mode for
automatic, quorum-based failover. The migration is performed **in place** as a
normal spec edit — the existing primary pod and its PVC are preserved, so there
is no backup/restore step and no data-movement window.

## What this migration does

- Scales data pods up to the sentinel minimum (`spec.instances >= 3`). New pods
  join as replicas and sync from the existing primary.
- Provisions the fixed set of sentinel pods (`<cluster>-sentinel-N`), which begin
  monitoring the current primary.
- Hands failover authority from the operator (operator-driven) to Redis Sentinel
  (quorum-driven). `status.currentPrimary` is afterward synced from the elected
  sentinel master.

The data plane is identical between the two modes, so the existing primary is
**not** recreated.

## Supported direction and constraints

- **Only `standalone -> sentinel` is supported in place.** `sentinel -> standalone`,
  and any transition to/from `cluster`, are rejected by the validating webhook.
- The target spec must satisfy the sentinel invariants, enforced at admission:
  - `spec.instances >= 3`
  - The **in-place migration** does not support TLS — `spec.tlsSecret` /
    `spec.caSecret` must be unset on the source. Sentinel mode itself supports TLS
    for clusters created as sentinel (see [docs/tls.md](../tls.md)), but a
    TLS-enabled standalone cluster cannot migrate in place; use the
    [recreate fallback](#fallback-recreate-when-in-place-is-blocked).
- The cluster must already have an established primary (`status.currentPrimary`
  set). Migrate only when the cluster is `Healthy`; the webhook rejects the flip
  if no primary is recorded.

## Prerequisites

- Permission to patch the `RedisCluster`.
- Shell variables:

```bash
export NS=<rediscluster-namespace>
export CLUSTER=<rediscluster-name>
```

## Procedure

1. Confirm the cluster is healthy and has a primary:

```bash
kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.status.phase}{"\t"}{.status.currentPrimary}{"\n"}'
```

   Proceed only when the phase is `Healthy` and `currentPrimary` is non-empty.

2. Flip mode and (if needed) scale to at least 3 instances in a single edit:

```bash
kubectl patch rediscluster "$CLUSTER" -n "$NS" --type merge -p \
  '{"spec":{"mode":"sentinel","instances":3}}'
```

   If admission rejects the change, the error names the exact reason (too few
   instances, TLS set, no primary yet, or an unsupported direction). Fix the spec
   and retry.

3. Watch the topology converge:

```bash
# Data pods scale up; new pods come up as replicas and sync.
kubectl get pods -n "$NS" -l redis.io/cluster="$CLUSTER",redis.io/workload=data -o wide

# Sentinel pods are provisioned.
kubectl get pods -n "$NS" -l redis.io/cluster="$CLUSTER",redis.io/role=sentinel -o wide
```

   The phase moves through `Scaling` while data pods are added; `SentinelPodCreated`
   events are emitted as sentinels come up (`kubectl describe rediscluster`).

4. Verify HA is active — sentinel agrees on the master and it matches status:

```bash
kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.status.currentPrimary}{"\n"}'
```

   Confirm the cluster has returned to `status.phase=Healthy` before closing out.

## Auth and client endpoints

These are preserved automatically — no action required:

- **Auth secret** is unchanged. The same `spec.authSecret` (or the operator-managed
  `<cluster>-auth`) and its projected volumes are reused. Do not rotate the secret
  during the migration; if you need to, do it separately (see
  [secret-rotation.md](secret-rotation.md)).
- **Client endpoints** are unchanged. The `-leader`, `-replica`, and `-any`
  Services keep their names and selectors; the `-leader` selector continues to
  track the primary. Applications need no reconfiguration.

## Fallback: recreate (when in-place is blocked)

Use this only when in-place migration cannot be used — primarily a TLS-enabled
standalone cluster, since the in-place migration path does not support TLS.
Unlike the in-place path, the recreated sentinel cluster **can** keep TLS
enabled (sentinel mode supports TLS; see [docs/tls.md](../tls.md)).

1. Take a backup (see the `RedisBackup` docs) and confirm it reaches
   `phase: Completed`.
2. Delete the standalone `RedisCluster`.
3. Recreate it with `mode: sentinel`, `instances: 3`, the **same**
   `spec.authSecret`, and `spec.bootstrap.backupName` pointing at the backup so
   data is restored. You may retain `tlsSecret`/`caSecret` to keep TLS enabled on
   the sentinel cluster.
4. Re-point clients if Service names changed (they will not if the cluster name is
   reused).

## Rollback

In-place migration is one-way: `sentinel -> standalone` is not supported. If you
must return to standalone, use the recreate flow above (backup → delete → recreate
as `standalone` → restore via `bootstrap`).
