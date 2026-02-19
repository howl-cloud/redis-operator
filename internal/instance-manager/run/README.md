# internal/instance-manager/run

Top-level `Run(ctx)` function for the instance manager.

## Startup Sequence

1. Parse environment (`POD_NAME`, `CLUSTER_NAME`, `POD_NAMESPACE`).
2. Fetch `RedisCluster` from the Kubernetes API to read current desired state.
3. **Split-brain guard** — compare `POD_NAME` against `cluster.status.currentPrimary`:
   - If they match → this pod is the designated primary; proceed to step 4.
   - If they do not match → this pod must start as a replica, regardless of any local data on disk. Write `replicaof <currentPrimary-ip> 6379` unconditionally. Redis will negotiate `PSYNC`/`SYNC` with the primary; any writes made by a former primary after the failover will be discarded. This mirrors CNPG's `pg_rewind` behaviour and prevents split-brain.
4. Determine data initialization mode:
   - **Fresh start**: no `/data/dump.rdb` or `/data/appendonly.aof` — start empty.
   - **Existing data + primary role**: start normally; data is intact.
   - **Existing data + replica role**: start with `replicaof` (step 3); Redis reconciles via replication.
   - **Restore**: bootstrap spec references a `RedisBackup` — download and restore before starting.
5. Write `redis.conf` to `/data/redis.conf`.
6. Start `redis-server /data/redis.conf` as a child process via `os/exec`.
7. Start the HTTP server (goroutine).
8. Start the `InstanceReconciler` watch loop (goroutine).
9. Block on `cmd.Wait()` — if `redis-server` exits unexpectedly, propagate the error.
