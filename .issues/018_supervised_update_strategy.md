---
id: 18
title: "Supervised primary update strategy for rolling updates"
priority: p2
type: feature
labels: [production-readiness, operations, rolling-update]
created: 2026-02-23
updated: 2026-02-23
depends_on: []
completed: false
---

## Summary

Rolling updates are currently fully automated: after all replicas are updated, the primary is automatically switched over and replaced. There is no option to pause after replicas are updated and wait for operator confirmation before touching the primary. Conservative production environments need this control.

## Why this is an issue

In high-stakes environments (financial services, healthcare, large-scale caches), operators want to:
- Verify replicas are healthy after update before risking the primary
- Run smoke tests or check metrics between phases
- Coordinate the primary switchover with a maintenance window
- Have a human in the loop for the most disruptive step of a rolling update

Fully automated updates are the right default, but lacking a supervised mode limits adoption in regulated environments.

## CloudNativePG equivalent

CNPG provides `spec.primaryUpdateStrategy` with two values:
- **`unsupervised`** (default): Fully automated — after replicas are updated, the primary is automatically switched over and replaced
- **`supervised`**: After all replicas are updated, the cluster enters `WaitingForUser` phase. The operator must manually trigger the primary update via `kubectl cnpg promote` or `kubectl cnpg restart`

Additionally, `spec.primaryUpdateMethod` controls *how* the primary is updated:
- **`restart`**: In-place restart if possible, otherwise switchover
- **`switchover`**: Always perform a switchover to a replica first

## How to implement in the Redis operator

1. **Add spec field**: `spec.primaryUpdateStrategy` with values `unsupervised` (default) and `supervised`. Add to `api/v1/rediscluster_types.go`.

2. **Modify rolling update logic** in `internal/controller/cluster/rolling_update.go`:
   - After all replicas are updated, check `spec.primaryUpdateStrategy`
   - If `supervised`: set phase to `WaitingForUser`, record an event (`PrimaryUpdatePaused`), and return `ctrl.Result{}` (stop reconciling the update)
   - If `unsupervised`: proceed with primary switchover as today

3. **Resume mechanism**: Add an annotation `redis.io/approve-primary-update: "true"` that the reconciler watches. When set:
   - Proceed with the primary switchover
   - Clear the annotation after completion
   - Alternatively, support a kubectl plugin or just document the annotation-based flow

4. **Status tracking**: Add a condition `PrimaryUpdateWaiting` so users can see the cluster is paused mid-update via `kubectl get rediscluster`.

5. **Webhook default**: Default to `unsupervised` in the defaulter webhook.

## Acceptance Criteria

- [ ] `spec.primaryUpdateStrategy: supervised` pauses rolling updates after replicas are done
- [ ] Cluster enters a visible waiting state (phase or condition)
- [ ] Event recorded: `PrimaryUpdatePaused` with message explaining how to resume
- [ ] Setting annotation `redis.io/approve-primary-update: "true"` resumes the update
- [ ] `spec.primaryUpdateStrategy: unsupervised` (default) behaves as today
- [ ] E2E test: supervised update pauses at the right point and resumes on annotation

## Notes

This is a small feature with outsized impact on enterprise adoption. The implementation is mostly a conditional branch in the rolling update path plus a new annotation watch. Consider also adding `spec.primaryUpdateMethod` (`restart` vs `switchover`) in the future, though for Redis the distinction is less meaningful than for PostgreSQL since Redis restarts are fast.
