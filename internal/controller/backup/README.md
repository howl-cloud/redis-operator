# internal/controller/backup

`BackupReconciler` and `ScheduledBackupReconciler` — manage `RedisBackup` and `RedisScheduledBackup` resources.

## BackupReconciler

Handles on-demand backup requests. When a `RedisBackup` object is created:

1. Validates the referenced `RedisCluster` exists and is healthy.
2. Selects a target pod (primary or prefer-replica, per `spec.target`).
3. Issues `POST /v1/backup` to the target pod's instance manager HTTP endpoint with backup method and destination.
4. The instance manager runs the backup locally:
   - `method: rdb` -> `BGSAVE` + upload `dump.rdb`
   - `method: aof` -> `BGREWRITEAOF` + archive `appendonlydir/` as `tar.gz` + upload
5. The instance manager returns artifact metadata (`artifactType`, `backupPath`, `backupSize`, checksum).
6. Reconciler writes `RedisBackup.status` (`phase`, `backupPath`, `artifactType`, `backupSize`, timestamps).

## ScheduledBackupReconciler

Watches `RedisScheduledBackup` objects and creates `RedisBackup` objects on the configured cron schedule. Tracks `status.lastScheduleTime` and `status.nextScheduleTime`.

## Key Files

| File | Description |
|------|-------------|
| `reconciler.go` | `BackupReconciler.Reconcile()` |
| `scheduled_reconciler.go` | `ScheduledBackupReconciler.Reconcile()`, cron scheduling logic |
| `executor.go` | HTTP call to instance manager to trigger backup and parse artifact metadata |
