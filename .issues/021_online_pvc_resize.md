---
id: 21
title: "Online PVC resize support"
priority: p3
type: feature
labels: [operations, storage]
created: 2026-02-23
updated: 2026-02-26
depends_on: []
completed: true
---

## Summary

The validating webhook explicitly blocks changes to `spec.storage.size` after cluster creation. Users cannot resize PVCs without creating a new cluster and migrating data. This is unnecessarily restrictive — Kubernetes has supported online PVC expansion (via `allowVolumeExpansion` on the StorageClass) since v1.24 GA.

## Why this is an issue

Redis memory datasets grow over time. When the underlying PVC fills up (especially with AOF persistence or RDB snapshots), the only options today are:
1. Create a new cluster with larger storage and migrate data (high effort, downtime risk)
2. Manually patch PVCs outside the operator, which the operator doesn't expect and may conflict with

Online PVC resize is a standard Kubernetes capability. Blocking it in the webhook forces users into painful workarounds for a routine operational task.

## CloudNativePG equivalent

CNPG supports PVC resize:
- Changing `spec.storage.size` to a larger value triggers the operator to patch the PVC's `spec.resources.requests.storage`
- On providers that support online expansion (most cloud providers), the resize happens without pod restart
- On AKS (which requires pod restart for resize), CNPG triggers a rolling update after the PVC resize
- The webhook validates that the new size is >= the current size (shrinking is not supported)

## How to implement in the Redis operator

1. **Relax webhook validation**: In `webhooks/rediscluster_validator.go`, change the storage size immutability check to only reject *decreases*. Allow increases.

2. **PVC reconciliation** in `internal/controller/cluster/pvcs.go`:
   - During PVC reconciliation, compare `spec.storage.size` with each PVC's current `spec.resources.requests.storage`
   - If the desired size is larger, patch the PVC with the new size
   - The StorageClass must have `allowVolumeExpansion: true` — if not, the patch will fail with a clear Kubernetes API error

3. **Rolling update trigger** (conditional):
   - Some storage providers require a pod restart for the resize to take effect
   - After patching PVCs, check if the PVC's `status.conditions` includes `FileSystemResizePending`
   - If so, trigger a rolling update (one pod at a time, replicas first) to allow the kubelet to expand the filesystem
   - If the resize completes online (no `FileSystemResizePending`), no restart needed

4. **Status tracking**: Add PVC resize status to per-instance status or add a condition `PVCResizeInProgress`.

5. **Event recording**: Record `PVCResizeStarted` and `PVCResizeCompleted` events.

## Acceptance Criteria

- [x] Increasing `spec.storage.size` patches PVCs to the new size
- [x] Decreasing `spec.storage.size` is rejected by the webhook
- [x] PVC resize works on StorageClasses with `allowVolumeExpansion: true`
- [x] Clear error event if StorageClass does not support expansion
- [x] Rolling update triggered if filesystem resize requires pod restart
- [x] E2E test: create cluster with 1Gi → resize to 2Gi → verify PVC capacity updated

## Notes

This is a low-risk feature since Kubernetes PVC expansion is a well-established API. The main complexity is detecting whether a rolling update is needed (varies by storage provider). Start with the simple case: patch PVCs and let Kubernetes handle it. Add the rolling-update-on-`FileSystemResizePending` logic as a follow-up if needed. Most modern CSI drivers (EBS, GCE-PD, Ceph-CSI) handle online expansion without pod restart.
