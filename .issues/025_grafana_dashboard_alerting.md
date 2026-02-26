---
id: 25
title: "Grafana dashboard and Prometheus alerting rules"
priority: p2
type: feature
labels: [production-readiness, observability, monitoring]
created: 2026-02-23
updated: 2026-02-24
depends_on: [17]
completed: false
---

## Summary

Issue #17 adds rich Prometheus metrics to the instance manager. This issue takes that further: shipping pre-built Grafana dashboard JSON and Prometheus alerting rules so operators have actionable observability out of the box. Without these, teams must build monitoring from scratch, which delays production adoption and means incidents go undetected until users complain.

## Why this is an issue

Metrics without dashboards and alerts are passive telemetry. A production Redis cluster needs active monitoring:
- **Replication lag** alerts before replicas fall too far behind (data loss risk on failover)
- **Primary unavailable** alerts when no primary is accepting writes
- **Memory usage** alerts before Redis starts evicting keys or OOMing
- **Backup missing** alerts when scheduled backups stop succeeding
- **Connection saturation** alerts when `maxclients` is approached

Without pre-built assets, every team deploying this operator must rediscover which metrics matter and build their own dashboards. This is a significant adoption barrier for an operator claiming production-readiness.

## CloudNativePG equivalent

CNPG ships:
- A full Grafana dashboard (JSON) in `docs/src/samples/monitoring/grafana-configmap.yaml` covering cluster health, replication, WAL activity, connection pooling, and backup status
- Prometheus alerting rules in `docs/src/samples/monitoring/prometheusrule.yaml` covering: `CNPGClusterHaNoStandby`, `CNPGClusterUnhealthy`, `CNPGBackupFailed`, `CNPGReplicationSlotFailing`, etc.
- A `PodMonitor`/`ServiceMonitor` to configure Prometheus scraping
- Documentation on how to configure custom queries via ConfigMap

These assets are versioned alongside the operator and tested as part of the release process.

## How to implement in the Redis operator

### 1. Prometheus alerting rules (`config/monitoring/prometheusrule.yaml`)

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: redis-operator-alerts
spec:
  groups:
  - name: redis-operator
    rules:
    - alert: RedisPrimaryUnavailable
      expr: sum by (cluster) (redis_instance_role{role="master"}) == 0
      for: 30s
      labels:
        severity: critical
      annotations:
        summary: "Redis cluster {{ $labels.cluster }} has no primary"

    - alert: RedisReplicationLagHigh
      expr: redis_instance_replication_lag_bytes > 10485760  # 10MB
      for: 2m
      labels:
        severity: warning
      annotations:
        summary: "Redis replica {{ $labels.pod }} is {{ $value }} bytes behind"

    - alert: RedisMemoryUsageHigh
      expr: redis_memory_used_bytes / redis_memory_max_bytes > 0.85
      for: 5m
      labels:
        severity: warning
      annotations:
        summary: "Redis pod {{ $labels.pod }} memory usage above 85%"

    - alert: RedisBackupMissing
      expr: time() - redis_last_successful_backup_timestamp > 86400  # 24h
      for: 1h
      labels:
        severity: warning
      annotations:
        summary: "Redis cluster {{ $labels.cluster }} has not had a successful backup in 24h"

    - alert: RedisInstanceDown
      expr: redis_instance_up == 0
      for: 1m
      labels:
        severity: critical
      annotations:
        summary: "Redis instance {{ $labels.pod }} is unreachable"
```

### 2. Grafana dashboard (`config/monitoring/grafana-dashboard.json`)

Panels to include:
- **Cluster overview**: phase, instance count, healthy/unhealthy instances
- **Replication**: lag bytes per replica, connected replicas, master link status
- **Memory**: used/max memory ratio, evictions/sec, fragmentation ratio
- **Connections**: connected clients, blocked clients, rejected connections
- **Operations**: ops/sec by command type (get, set, del), keyspace hits/misses ratio
- **Persistence**: last RDB save duration, last AOF rewrite, last backup time
- **Backup**: backup phase history, last successful backup age

### 3. Helm chart integration

Add to `charts/redis-operator/templates/`:
- `prometheusrule.yaml`: Conditional on `monitoring.alertingRules.enabled` (default: `true`)
- `grafana-configmap.yaml`: ConfigMap containing dashboard JSON, labeled for Grafana sidecar auto-discovery (`grafana_dashboard: "1"`)
- `servicemonitor.yaml`: Already partially done; ensure it scrapes both operator metrics and instance manager metrics

### 4. Documentation

Add `docs/monitoring.md` covering:
- Which metrics are exposed and what they mean
- How to import the Grafana dashboard
- How to configure alert thresholds per cluster (custom PrometheusRule)
- How to add custom metrics queries via ConfigMap (mirrors CNPG's `customQueriesConfigMap`)

## Acceptance Criteria

- [ ] `PrometheusRule` resource deployed by Helm includes all critical alerts
- [ ] Alerts fire correctly in a test environment (verify with `amtool check-config`)
- [ ] Grafana dashboard JSON importable and displays data from a running cluster
- [ ] Helm `monitoring.alertingRules.enabled: false` disables the PrometheusRule
- [ ] Helm `monitoring.grafanaDashboard.enabled: false` disables the dashboard ConfigMap
- [ ] Documentation in `docs/monitoring.md` covers setup and customisation

## Notes

The Grafana dashboard JSON should be generated from a real cluster if possible, not hand-written. Spin up a cluster locally with Prometheus + Grafana (via `kube-prometheus-stack`), populate it with test data, build the dashboard interactively, then export the JSON. This ensures all panel queries are validated against real metric names and labels.

The alert thresholds in this issue are reasonable defaults. Individual teams will need to tune them — consider making thresholds configurable via Helm values (e.g., `monitoring.alerts.replicationLagThresholdBytes`).
