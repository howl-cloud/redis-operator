---
id: 17
title: "Rich Prometheus metrics for Redis instances"
priority: p1
type: feature
labels: [production-readiness, observability, monitoring]
created: 2026-02-23
updated: 2026-02-23
depends_on: []
completed: true
---

## Summary

The instance manager exposes a `/metrics` endpoint but it serves minimal data. The controller-manager exposes default controller-runtime metrics (reconcile duration, queue depth). There are no per-cluster Redis metrics (replication lag, memory usage, ops/sec, connected clients, keyspace stats) — the data that operators actually need to monitor Redis in production.

## Why this is an issue

Without rich metrics, operators cannot:
- Set up alerts for replication lag exceeding a threshold
- Monitor memory usage approaching `maxmemory` and predict eviction
- Track ops/sec for capacity planning
- Detect slow commands or connection storms
- Integrate Redis monitoring into existing Prometheus/Grafana stacks without deploying a separate `redis_exporter` sidecar

Every serious database operator ships with a built-in metrics exporter. Requiring users to deploy and manage a separate exporter defeats the purpose of an integrated operator.

## CloudNativePG equivalent

CNPG runs a built-in Prometheus exporter on port 9187 (`/metrics`) in every pod. Key metrics:
- `cnpg_collector_up` — PostgreSQL is responsive
- `cnpg_collector_pg_wal` — WAL size
- `cnpg_collector_pg_wal_archive_status` — WAL archiving health
- `cnpg_collector_sync_replicas` — synchronous replica count
- `cnpg_collector_fencing_on` — instance is fenced
- `cnpg_collector_collection_duration_seconds` — exporter latency
- Plus all standard `pg_stat_*` metrics

CNPG also supports **custom metrics** defined via ConfigMap/Secret, letting users add application-specific queries without modifying the operator.

## How to implement in the Redis operator

1. **Parse `INFO` output**: The instance manager already connects to Redis. Periodically (every 15s) run `INFO ALL` and parse the response into Prometheus metrics using the `prometheus/client_golang` library.

2. **Core gauge metrics** (from `INFO` sections):

   | Metric | Source | Type |
   |--------|--------|------|
   | `redis_up` | PING success | Gauge |
   | `redis_instance_info` | `redis_version`, `role`, `os` | Info (labels) |
   | `redis_connected_clients` | `connected_clients` | Gauge |
   | `redis_blocked_clients` | `blocked_clients` | Gauge |
   | `redis_used_memory_bytes` | `used_memory` | Gauge |
   | `redis_used_memory_rss_bytes` | `used_memory_rss` | Gauge |
   | `redis_maxmemory_bytes` | `maxmemory` | Gauge |
   | `redis_mem_fragmentation_ratio` | `mem_fragmentation_ratio` | Gauge |
   | `redis_total_commands_processed` | `total_commands_processed` | Counter |
   | `redis_instantaneous_ops_per_sec` | `instantaneous_ops_per_sec` | Gauge |
   | `redis_keyspace_hits_total` | `keyspace_hits` | Counter |
   | `redis_keyspace_misses_total` | `keyspace_misses` | Counter |
   | `redis_connected_replicas` | `connected_slaves` | Gauge |
   | `redis_replication_offset` | `master_repl_offset` | Gauge |
   | `redis_replica_repl_offset` | `slave_repl_offset` | Gauge |
   | `redis_replication_lag_bytes` | offset difference | Gauge |
   | `redis_master_link_up` | `master_link_status` | Gauge |
   | `redis_rdb_last_save_timestamp` | `rdb_last_save_time` | Gauge |
   | `redis_rdb_changes_since_last_save` | `rdb_changes_since_last_save` | Gauge |
   | `redis_loading` | `loading` | Gauge |
   | `redis_fenced` | fencing annotation present | Gauge |

3. **Labels**: All metrics should carry labels `namespace`, `cluster`, `pod`, `role`.

4. **Controller-level metrics**: In the reconciler, register:
   - `redis_cluster_instances_total` (desired vs actual)
   - `redis_cluster_phase` (label-based)
   - `redis_reconcile_duration_seconds` (histogram)
   - `redis_failover_total` (counter)

5. **Helm chart**: The `ServiceMonitor` template already exists — just ensure it targets the correct port and path.

## Acceptance Criteria

- [x] `/metrics` on instance manager pods returns Prometheus-format metrics from `INFO ALL`
- [x] At least 15 distinct Redis metrics are exported (see table above)
- [x] Metrics include `namespace`, `cluster`, `pod`, `role` labels
- [x] Controller-level metrics exported on the manager's metrics port
- [x] ServiceMonitor template works with Prometheus Operator out of the box
- [x] Grafana dashboard JSON included in `charts/redis-operator/dashboards/` (optional but recommended)

## Notes

The `oliver006/redis_exporter` project is the de facto standard for Redis Prometheus metrics. Use its metric naming conventions for compatibility — users may already have Grafana dashboards built for those metric names. Don't reinvent the naming scheme.
