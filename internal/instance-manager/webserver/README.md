# internal/instance-manager/webserver

HTTP server running inside each Redis pod, listening on `:8080`.

## Endpoints

| Method | Path | Consumer | Description |
|--------|------|----------|-------------|
| `GET` | `/healthz` | kubelet | Liveness probe: checks `redis-server` process is alive |
| `GET` | `/readyz` | kubelet | Readiness probe: checks Redis responds to `PING` and replication lag is acceptable |
| `GET` | `/metrics` | Prometheus | Exposes Redis metrics in Prometheus exposition format (see Metrics section below) |
| `GET` | `/v1/status` | Operator | Returns JSON status: role, replication offset, connected replicas, lag |
| `POST` | `/v1/promote` | Operator | Issues `REPLICAOF NO ONE` to promote this instance to primary |
| `POST` | `/v1/demote` | Operator | Issues `REPLICAOF <primary> 6379` to (re)join as replica |
| `POST` | `/v1/backup` | Operator | Triggers `BGSAVE` (or AOF rewrite) and optionally uploads to object storage |
| `PUT` | `/v1/update` | Operator | Accepts new instance manager binary, performs in-place upgrade via `syscall.Exec` |

## Metrics

`/metrics` is scraped by Prometheus. Metrics are collected from `INFO all` on each scrape and exposed in Prometheus exposition format. A `PodMonitor` or `ServiceMonitor` CRD (for Prometheus Operator users) is included in `config/monitoring/`.

Key metrics exposed:

| Metric | Type | Source |
|--------|------|--------|
| `redis_connected_clients` | Gauge | `INFO clients` |
| `redis_blocked_clients` | Gauge | `INFO clients` |
| `redis_used_memory_bytes` | Gauge | `INFO memory` |
| `redis_used_memory_peak_bytes` | Gauge | `INFO memory` |
| `redis_rdb_last_save_timestamp` | Gauge | `INFO persistence` |
| `redis_aof_enabled` | Gauge | `INFO persistence` |
| `redis_replication_role` | Gauge (0=replica, 1=primary) | `INFO replication` |
| `redis_connected_replicas` | Gauge | `INFO replication` |
| `redis_replication_offset` | Gauge | `INFO replication` |
| `redis_replica_lag_seconds` | Gauge | `INFO replication` (replicas only) |
| `redis_keyspace_hits_total` | Counter | `INFO stats` |
| `redis_keyspace_misses_total` | Counter | `INFO stats` |
| `redis_commands_processed_total` | Counter | `INFO stats` |
| `redis_uptime_seconds` | Gauge | `INFO server` |

## Notes

- The operator always calls pod IP directly, never through a Service, since most operations are instance-specific.
- `/v1/status` responses are the source of truth for `ClusterReconciler`'s status collection loop.
- All write endpoints (`/v1/promote`, `/v1/demote`, `/v1/backup`, `/v1/update`) require a bearer token matching the cluster's operator secret to prevent unauthorized calls.
- `/metrics` is unauthenticated (standard Prometheus scrape convention) but can be restricted by NetworkPolicy.
