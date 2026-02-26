---
id: 22
title: "Backup and restore E2E test suite"
priority: p0
type: testing
labels: [production-readiness, testing, backup, data-safety]
created: 2026-02-23
updated: 2026-02-24
depends_on: [14]
completed: true
---

## Summary

Issue #14 wires the automated restore code path, but there is no end-to-end test that proves the full backup→destroy→restore cycle actually works. Without a verified automated test, the restore path is untrusted: it could silently fail, produce corrupted data, or only work under specific conditions. No production operator should ship without a validated data recovery story.

## Why this is an issue

Code that is never tested is broken by definition. The restore path is the most critical and least-exercised code in any database operator — it only runs during disasters, when stakes are highest. Manual testing is not sufficient because:
- It is not repeatable across code changes
- It does not catch regressions when backup or restore logic is refactored
- It does not test edge cases (empty backup, partial backup, backup from replica, AOF vs RDB)
- It gives operators no confidence they can actually recover when it matters

This is the gap between "we have backup/restore code" and "we have backup/restore that works."

## CloudNativePG equivalent

CNPG has a comprehensive backup/restore E2E test suite covering:
- On-demand backup → new cluster bootstrap from that backup → data verification
- Scheduled backup → PITR recovery to a specific timestamp → data verification
- Backup from standby → restore on new cluster → verify data matches primary
- Volume snapshot backup → restore → verify
- These tests run in CI on every PR against an S3-compatible object store spun up in the Kind cluster

CNPG's `hack/setup-cluster.sh` deploys MinIO as that dependency. For this Redis operator, standardize on **Garage** as the local S3-compatible dependency for backup/restore testing.

## How to implement in the Redis operator

1. **Garage in Kind setup**: Update `test/e2e/suite_test.go` (or the Kind-based backup test harness) to deploy a single-node Garage instance for object storage. Provision it during setup:
   - apply layout (`garage layout assign` + `garage layout apply`)
   - create bucket and API key (`garage bucket create`, `garage key create`)
   - grant bucket permissions (`garage bucket allow --read --write --owner ...`)
   - create `backupCredentialsSecret` with `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`
   - set backup destination endpoint to Garage (`spec.destination.s3.endpoint`)
   - ensure region matches Garage config (`spec.destination.s3.region` vs Garage `s3_region`)

2. **Test scenario: full cycle** (`test/e2e/backup_restore_test.go`):
   ```
   Given a RedisCluster "source" with 3 instances
   When I write 1000 keys via redis-cli
   And I create a RedisBackup targeting "source"
   And the backup reaches Completed phase
   And I delete the "source" cluster (PVCs too)
   When I create a RedisCluster "restored" with spec.bootstrap.backupName=<backup>
   Then the cluster reaches Healthy phase
   And all 1000 keys are present in "restored"
   And replicas are in sync with the restored primary
   ```

3. **Test scenario: backup from replica**:
   - Create backup with `spec.target: prefer-replica`
   - Verify backup completes from a replica pod
   - Restore and verify data matches

4. **Test scenario: scheduled backup**:
   - Create `RedisScheduledBackup` with a 1-minute cron
   - Wait for at least one backup to complete
   - Restore from the auto-created backup
   - Verify data integrity

5. **Test scenario: restore idempotency**:
   - Restore twice from the same backup into two separate clusters
   - Verify both clusters have identical data

6. **Test scenario: corrupted / missing backup**:
   - Create a cluster with `spec.bootstrap.backupName` referencing a non-existent backup
   - Verify webhook rejects it or cluster enters a clear error phase (not silent hang)

## Acceptance Criteria

- [x] Full backup→delete→restore→verify cycle passes in CI
- [x] Backup from replica produces a restorable backup
- [x] Scheduled backup auto-creates a backup that can be restored
- [x] Restore into two clusters from the same backup produces identical data
- [x] Invalid `backupName` produces a clear error, not a silent hang
- [x] Tests run against local Garage (no external cloud credentials required in CI)
- [x] Added to `make test-e2e` and/or a dedicated `make test-backup` target

## Notes

Garage is the standard local S3-compatible backend for this repository's backup/restore testing. It can run as a single-node deployment in Kind without persistent storage for CI. Garage requires explicit post-install provisioning (layout + bucket/key/permissions), but once bootstrapped it provides the same S3 API surface needed by this operator's backup/restore paths. The Garage setup in the test suite can be shared with future DR/replica-cluster tests (issue #24).
