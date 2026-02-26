---
id: 20
title: "Node maintenance window support"
priority: p3
type: feature
labels: [operations, availability]
created: 2026-02-23
updated: 2026-02-23
depends_on: []
completed: true
---

## Summary

There is no mechanism to temporarily suspend self-healing and relax PodDisruptionBudgets during planned node maintenance (e.g., Kubernetes upgrades, kernel patches). Node drains can conflict with the operator's self-healing — the operator tries to recreate deleted pods while the drain is trying to evict them.

## Why this is an issue

During planned node maintenance:
- Draining a node evicts pods, triggering the operator's self-healing (pod recreation)
- The operator may recreate pods on the very node being drained, or on other nodes about to be drained
- PDBs may block the drain entirely for single-instance or two-instance clusters
- Operators must manually scale down or annotate clusters to work around this, which is error-prone

A maintenance mode that temporarily disables self-healing and relaxes PDBs makes planned maintenance predictable and safe.

## CloudNativePG equivalent

CNPG provides `spec.nodeMaintenanceWindow` with two fields:
- **`inProgress`** (bool): When `true`, disables standard self-healing procedures, rolling updates, and PDBs
- **`reusePVC`** (bool, default: `true`):
  - `true`: Kubernetes waits for the node to come back and reuses the existing PVC. PDBs are temporarily removed.
  - `false`: Pod is recreated on a different node with a new PVC. The old PVC is destroyed after the new replica catches up via streaming replication. Not recommended for large datasets.

For single-instance clusters with `reusePVC: false`, draining is still prevented to avoid data loss. The user must either enable `reusePVC` (accepting downtime) or scale up first.

## How to implement in the Redis operator

1. **Add spec field**: `spec.nodeMaintenanceWindow` with `inProgress` (bool) and `reusePVC` (bool, default: true). Add to `api/v1/rediscluster_types.go`.

2. **Reconciler changes** in `internal/controller/cluster/reconciler.go`:
   - When `nodeMaintenanceWindow.inProgress == true`:
     - Skip pod reconciliation (don't recreate missing pods)
     - Skip rolling update logic
     - Delete PDBs (or set `maxUnavailable` to cluster size)
   - When `reusePVC == true`: Keep PVCs in place, expect pods to return on the same node
   - When `reusePVC == false`: Allow PVCs to be recreated on different nodes (requires replica resync)

3. **Status tracking**: Add a condition `MaintenanceInProgress` so the state is visible in `kubectl describe`.

4. **Event recording**: Record `MaintenanceWindowStarted` and `MaintenanceWindowEnded` events.

5. **Safety guard**: If `reusePVC == false` and `spec.instances == 1`, refuse to enter maintenance mode (or warn) since there's no replica to preserve data.

## Acceptance Criteria

- [x] Setting `spec.nodeMaintenanceWindow.inProgress: true` disables pod recreation and PDB enforcement
- [x] Node drain proceeds without operator interference
- [x] After maintenance (`inProgress: false`), pods are recreated and cluster returns to `Healthy`
- [x] `reusePVC: true` preserves PVCs across the drain
- [x] Condition `MaintenanceInProgress` visible in cluster status
- [x] E2E test: enable maintenance → drain node → disable maintenance → verify cluster recovery

## Notes

An alternative to the spec field is an annotation-based approach (`redis.io/maintenance: "on"`) similar to hibernation. This avoids spec changes and is more aligned with the "temporary operational override" pattern. The annotation approach is simpler but less discoverable. Consider supporting both — annotation for quick operations, spec field for GitOps.
