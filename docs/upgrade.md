# Operator Upgrade Guide

This document describes how operator upgrades work for existing `RedisCluster` resources, how CRD compatibility is maintained, and how to validate an upgrade with Helm.

## Current API Versioning State

- The CRD API currently serves `redis.io/v1`.
- There is no active multi-version CRD setup and no conversion webhook in the current release.
- Future API versions should follow additive schema evolution first, with conversion webhooks introduced when a new served/storage version is required.

## CRD Compatibility Policy

To avoid data loss and API breakage during upgrades:

1. Prefer additive changes (`+optional` fields, new enum values where safe).
2. Do not remove persisted fields without a deprecation window.
3. Mark fields as deprecated before removal (`// Deprecated:` in Go types).
4. Keep reconcile logic backward compatible for at least one full release after deprecation.
5. Only introduce conversion webhooks when adding a new CRD version (for example, `v2` alongside `v1`).

## What Happens During an Operator Upgrade

When the operator Deployment image is updated:

1. A new controller pod is rolled out by Kubernetes.
2. Leader election keeps one active controller at a time, enabling zero-downtime control plane handoff.
3. The reconciler computes a deterministic `redis.io/spec-hash` from tracked pod inputs:
   - Redis image
   - Redis container resources
   - Operator init container image (`OPERATOR_IMAGE_NAME`)
   - Projected secret references used by data pods
   - Redis config map entries from `.spec.redis`
4. The rolling update flow compares each data pod's hash to the desired hash.
5. Outdated replicas are recreated one at a time (highest ordinal first), then the primary is switched over and updated last.

This ensures controlled rolling behavior instead of bulk restarts.

## Helm Upgrade Procedure (Existing Clusters)

The following procedure validates `helm upgrade` against a namespace that already has running `RedisCluster` resources.

### 1) Pre-upgrade snapshot

```bash
kubectl get redisclusters.redis.io -A
kubectl get pods -n <namespace> -l redis.io/cluster=<cluster-name> -o wide
kubectl get svc -n <namespace> <cluster-name>-leader <cluster-name>-replica <cluster-name>-any
kubectl get events -n <namespace> --sort-by=.lastTimestamp | tail -n 40
```

Record:
- Current primary pod name (`.status.currentPrimary`)
- Redis pod restart counts
- Ready pod counts

### 2) Upgrade the Helm release

```bash
helm upgrade <release-name> charts/redis-operator \
  --namespace <operator-namespace> \
  --reuse-values \
  --set image.repository=<repo> \
  --set image.tag=<new-tag>
```

### 3) Watch operator rollout

```bash
kubectl rollout status deployment/<operator-deployment-name> -n <operator-namespace>
kubectl get lease -n <operator-namespace>
kubectl logs -n <operator-namespace> deploy/<operator-deployment-name> --tail=200
```

Expected result:
- Deployment completes successfully.
- Leader election lease is continuously held.
- Reconcile loop continues without prolonged gaps.

### 4) Validate Redis cluster continuity

```bash
kubectl get rediscluster <cluster-name> -n <namespace> -o yaml
kubectl get pods -n <namespace> -l redis.io/cluster=<cluster-name> -w
```

Expected result:
- Cluster stays available throughout the upgrade.
- Pods are only recreated when their spec hash is outdated.
- Recreation happens one pod at a time for replicas, then primary last.
- No simultaneous restart of all healthy data pods.

### 5) Post-upgrade checks

```bash
kubectl get rediscluster <cluster-name> -n <namespace> -o jsonpath='{.status.phase}{"\n"}'
kubectl get rediscluster <cluster-name> -n <namespace> -o jsonpath='{.status.currentPrimary}{"\n"}'
kubectl get pods -n <namespace> -l redis.io/cluster=<cluster-name>
```

Confirm:
- `status.phase` returns to `Healthy`.
- A valid primary is present in `status.currentPrimary`.
- Redis clients can still read/write through the leader service.

## Conversion Webhook Readiness

No conversion webhook is required for the current single-version `v1` API.

If a future release introduces a second CRD version:

1. Add a new API package for the new version.
2. Mark one version as storage and serve both versions.
3. Implement conversion interfaces (`Hub`/`Convertible` pattern).
4. Enable CRD conversion webhook strategy and test round-trip conversions.
5. Ship migration notes in `CHANGELOG.md` before making storage-version-only fields unavailable.
