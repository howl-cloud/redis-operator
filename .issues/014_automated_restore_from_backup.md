---
id: 14
title: "Automated restore from backup on bootstrap"
priority: p0
type: feature
labels: [production-readiness, data-safety, backup]
created: 2026-02-23
updated: 2026-02-23
depends_on: []
completed: false
---

## Summary

The `spec.bootstrap.backupName` field exists in the `RedisCluster` CRD types but the actual restore-on-pod-startup flow is not wired. Creating a new cluster that references an existing `RedisBackup` does nothing — restore is currently manual. This is the single biggest gap blocking production-readiness for any workload where data durability matters.

## Why this is an issue

Without automated restore, disaster recovery requires manual intervention: an operator must download the RDB/AOF from object storage, copy it to the PVC, and restart the pod. This is error-prone, slow, and incompatible with GitOps workflows where clusters should be fully declarative. Every serious database operator provides automated bootstrap-from-backup.

## CloudNativePG equivalent

CNPG provides `bootstrap.recovery` which:
1. Injects an init container into the first pod that runs `barman-cloud-restore` and `barman-cloud-wal-restore`
2. Automatically selects the closest base backup before the target time
3. Supports Point-In-Time Recovery (PITR) via `recoveryTarget` (timestamp, label, or transaction ID)
4. Replays WAL segments to reach the desired recovery point
5. Once the primary is bootstrapped from backup, replicas clone from it via streaming replication

CNPG also supports `bootstrap.pg_basebackup` for cloning an existing cluster via streaming replication.

## How to implement in the Redis operator

1. **Init container injection**: When `spec.bootstrap.backupName` is set, inject an init container that:
   - Resolves the `RedisBackup` CR to find the object storage location and credentials
   - Downloads the RDB file from S3/S3-compatible storage using the backup credentials secret
   - Places it at the correct path in the PVC (`/data/dump.rdb`)
   - Exits successfully, allowing the main container to start

2. **Instance manager awareness**: The instance manager startup sequence in `internal/instance-manager/run/run.go` should detect a restored RDB and configure `redis-server` to load it (this is default Redis behavior if `dump.rdb` exists in the data directory).

3. **Status tracking**: Add a `BootstrapPhase` or condition (`Restoring`) so the reconciler waits for restore completion before proceeding with replica creation.

4. **Validation**: The webhook should validate that the referenced `RedisBackup` exists and has status `Completed`.

## Acceptance Criteria

- [ ] `spec.bootstrap.backupName` referencing a completed `RedisBackup` triggers an init container that downloads and restores the RDB
- [ ] New cluster bootstraps from the restored data and reaches `Healthy` phase
- [ ] Replicas created after bootstrap stream from the restored primary
- [ ] Webhook rejects `backupName` referencing a non-existent or incomplete backup
- [ ] E2E test: create backup → delete cluster → create new cluster from backup → verify data

## Notes

The RDB restore is simpler than PostgreSQL's WAL replay — Redis loads `dump.rdb` at startup automatically. The main work is the init container plumbing and object storage download logic, which can reuse the existing backup executor's S3 client code.
