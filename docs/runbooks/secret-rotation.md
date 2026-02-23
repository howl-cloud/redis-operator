# Runbook: Secret rotation (`authSecret`)

**Severity**: P1  
**Estimated time**: 10-20 minutes

Use this to rotate Redis authentication credentials with no planned pod restarts.

## Symptoms

- Planned credential rotation window.
- Potential credential exposure requiring immediate password rollover.

## Prerequisites

- Permission to update Secrets and annotate `RedisCluster`.
- Client/application teams ready to switch credentials quickly after rotation.
- Shell variables:

```bash
export NS=<rediscluster-namespace>
export CLUSTER=<rediscluster-name>
```

## Diagnosis

1. Identify auth secret name:

```bash
export AUTH_SECRET="$(kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.spec.authSecret.name}')"
if [ -z "$AUTH_SECRET" ]; then
  AUTH_SECRET="${CLUSTER}-auth"
fi
echo "$AUTH_SECRET"
```

2. Capture current secret resourceVersion:

```bash
export OLD_SECRET_RV="$(kubectl get secret "$AUTH_SECRET" -n "$NS" -o jsonpath='{.metadata.resourceVersion}')"
echo "$OLD_SECRET_RV"
```

## Recovery steps

1. Generate a new password:

```bash
export NEW_PASSWORD="$(openssl rand -hex 24)"
export NEW_PASSWORD_B64="$(printf '%s' "$NEW_PASSWORD" | base64 | tr -d '\n')"
```

2. Update only the `password` key in the auth secret:

```bash
kubectl patch secret "$AUTH_SECRET" -n "$NS" --type=merge \
  -p "{\"data\":{\"password\":\"$NEW_PASSWORD_B64\"}}"
```

3. Trigger immediate reconciliation:

```bash
kubectl annotate rediscluster "$CLUSTER" -n "$NS" \
  runbooks.redis.io/secret-rotation-ts="$(date +%s)" --overwrite
```

4. Confirm the secret version changed:

```bash
kubectl get secret "$AUTH_SECRET" -n "$NS" -o jsonpath='{.metadata.resourceVersion}{"\n"}'
kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.status.secretsResourceVersion}{"\n"}'
```

5. Verify new password works on the current primary:

```bash
export CURRENT_PRIMARY="$(kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.status.currentPrimary}')"
kubectl exec -n "$NS" "$CURRENT_PRIMARY" -- redis-cli -a "$NEW_PASSWORD" ping
```

6. Confirm no pod restart was required:

```bash
kubectl get pods -n "$NS" -l redis.io/cluster="$CLUSTER",redis.io/workload=data \
  -o custom-columns=NAME:.metadata.name,RESTARTS:.status.containerStatuses[0].restartCount
```

7. Update application/client credentials to `NEW_PASSWORD`.

### Optional: ACL secret rotation

If `spec.aclConfigSecret` is set, rotate ACL rules by updating that secret's `acl` key.  
The instance manager applies this live with `ACL LOAD` (no pod restart expected).

## Verification

```bash
kubectl get rediscluster "$CLUSTER" -n "$NS" -o jsonpath='{.status.phase}{"\n"}'
kubectl get events -n "$NS" \
  --field-selector involvedObject.kind=RedisCluster,involvedObject.name="$CLUSTER" \
  --sort-by=.lastTimestamp
```

Expected:
- Secret `resourceVersion` changes.
- `status.secretsResourceVersion` includes updated version for `AUTH_SECRET`.
- `redis-cli -a "$NEW_PASSWORD" ping` succeeds.
- Data pods do not restart solely for auth rotation.
- Cluster returns/stays `Healthy`.

## Escalation

- If auth update succeeds but Redis rejects new password, check projected secret mounts under `/projected/<secretName>/password` and instance-manager logs.
- If rotation causes broad client auth failures, roll clients first/fast and coordinate incident communication.
