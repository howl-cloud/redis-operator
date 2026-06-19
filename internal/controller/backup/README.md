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

### Schedule syntax

`spec.schedule` is parsed by [`robfig/cron`](https://pkg.go.dev/github.com/robfig/cron/v3) using the standard Kubernetes CronJob dialect:

- **Five fields** — `minute hour day-of-month month day-of-week` (e.g. `0 2 * * *` = 02:00 every day). Ranges (`1-5`), lists (`1,15`), steps (`*/10`), and names (`MON`, `JAN`) are supported.
- **Descriptors** — `@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly`/`@annually`, and `@every <duration>` (e.g. `@every 6h`).
- **Seconds are not supported.** A six-field expression is rejected. The smallest standard granularity is one minute.

Invalid expressions are rejected at apply time by the validating webhook. If the webhook is disabled, the controller still guards against them: it sets a `ScheduleValid=False` status condition, emits an `InvalidSchedule` warning event, and stops requeuing until the spec is corrected.

### Timezone behavior

`spec.timeZone` is an IANA zone name (e.g. `America/Chicago`) and **defaults to `UTC`**. The schedule is evaluated in this zone, independent of where the controller pod runs, so `0 2 * * *` with `timeZone: America/Chicago` fires at 02:00 Chicago wall-clock time (handling DST shifts via the zone database). The zone database must be present in the operator image (it ships in the base image). An unknown zone name is rejected the same way an invalid schedule is.

### Next run and missed runs

After each evaluation the controller writes `status.nextScheduleTime` (the next wall-clock fire time) and requeues for that instant. The next run is computed from `status.lastScheduleTime`, or from the resource's creation time if it has never run — so a freshly created schedule waits for its next occurrence rather than firing immediately.

Missed runs are handled with **at-most-one catch-up**: if the controller was down across one or more scheduled windows, exactly one backup fires on recovery and the schedule then advances past the current time. Missed windows never stack into a burst of backups.

### Retention

`spec.successfulBackupsHistoryLimit` and `spec.failedBackupsHistoryLimit` (both default `3`) cap how many `RedisBackup` **resources** are kept per schedule. When the count exceeds a limit, the oldest resources — by `metadata.creationTimestamp` — are deleted.

**These limits prune Kubernetes objects only. They do NOT delete the backup artifacts in object storage.** This mirrors Kubernetes CronJob's `successfulJobsHistoryLimit`/`failedJobsHistoryLimit`, which prune Job objects without touching whatever the jobs produced. Deleting the remote artifact — the actual disaster-recovery data — is intentionally out of the operator's scope:

- The default limit is small (`3`); coupling artifact deletion to it would make a low resource count silently destroy real backups.
- The controller holds no object-storage credentials. Artifacts are written by the in-pod instance manager (which projects `spec.backupCredentialsSecret`); the operator process has no client or delete permission for the bucket, and granting it one would expand its blast radius and conflict with Object Lock / immutability policies.

**Manage remote-artifact lifecycle outside the operator**, using the object store's native tooling, scoped to the prefix you configured in `spec.destination.s3.path` (or `azure.path`):

- **S3 / S3-compatible** — an [S3 Lifecycle](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lifecycle-mgmt.html) expiration rule on the prefix.
- **Azure Blob** — an [Azure Blob lifecycle management](https://learn.microsoft.com/azure/storage/blobs/lifecycle-management-overview) policy on the prefix.

## Key Files

| File | Description |
|------|-------------|
| `reconciler.go` | `BackupReconciler.Reconcile()` |
| `scheduled_reconciler.go` | `ScheduledBackupReconciler.Reconcile()`, cron scheduling logic |
| `executor.go` | HTTP call to instance manager to trigger backup and parse artifact metadata |

## Backup destinations

`spec.destination` selects the object-storage backend. Exactly one of `s3` or
`azure` must be set; the webhook-free validation in `reconciler.go` rejects
empty or ambiguous destinations and marks the backup `Failed`.

Credentials for both backends are read from the `RedisCluster`'s
`spec.backupCredentialsSecret`. The keys present in that Secret determine the
auth mode — no separate mode field is required.

### S3 (and S3-compatible stores)

```yaml
destination:
  s3:
    bucket: my-redis-backups
    path: prod/redis        # optional key prefix
    endpoint: ""            # set for MinIO/Garage/other S3-compatible stores
    region: us-east-1
```

Secret keys: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and optionally
`AWS_SESSION_TOKEN`.

### Azure Blob Storage

```yaml
destination:
  azure:
    container: my-redis-backups
    path: prod/redis        # optional blob-name prefix
    accountName: mystorageacct   # required for shared-key / SAS auth
    endpoint: ""            # override for Azurite or sovereign clouds
```

Credentials are auto-detected from the Secret in priority order:

| Auth mode | Secret key(s) |
|-----------|---------------|
| Connection string | `AZURE_STORAGE_CONNECTION_STRING` |
| Shared key | `AZURE_STORAGE_ACCOUNT` + `AZURE_STORAGE_KEY` |
| SAS token | `AZURE_STORAGE_SAS_TOKEN` (with `AZURE_STORAGE_ACCOUNT` or `endpoint`) |

The account name is resolved from `spec.destination.azure.accountName` if set,
otherwise from `AZURE_STORAGE_ACCOUNT` in the credentials secret — so shared-key
and SAS users only need it in one place. When a connection string is supplied it
takes precedence and carries its own endpoint/account, so `accountName`/`endpoint`
are ignored. Backup artifacts are recorded in `status.backupPath` as
`azblob://{container}/{blob}`.

### Example: on-demand backup to Azure

```yaml
apiVersion: redis.io/v1
kind: RedisBackup
metadata:
  name: cache-backup-now
spec:
  clusterName: cache
  target: prefer-replica
  method: rdb
  destination:
    azure:
      container: redis-backups
      path: cache
      accountName: mystorageacct
```

### Example: scheduled backup to Azure

```yaml
apiVersion: redis.io/v1
kind: RedisScheduledBackup
metadata:
  name: cache-nightly
spec:
  schedule: "0 2 * * *"
  timeZone: America/Chicago   # optional; defaults to UTC
  clusterName: cache
  method: rdb
  destination:
    azure:
      container: redis-backups
      path: cache/nightly
      accountName: mystorageacct
```

The referenced `RedisCluster` must set `spec.backupCredentialsSecret` to a Secret
containing the Azure credential keys above. Restore/bootstrap
(`spec.bootstrap.backupName`) downloads from whichever backend produced the
backup, using the same Secret.
