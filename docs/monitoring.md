# Monitoring

`redis-operator` ships dashboard and alerting assets so clusters are observable by default with Prometheus Operator + Grafana.

## What is exposed

Metrics are exposed from two places:

- **Operator controller** (`:9090/metrics`)
  - `redis_cluster_phase`
  - `redis_cluster_instances_total`
  - `redis_failover_total`
  - `redis_reconcile_duration_seconds`
  - `redis_last_successful_backup_timestamp`
  - `redis_backup_phase_count`
- **Redis instance manager in each data pod** (`:8080/metrics`)
  - Availability/role: `redis_up`, `redis_instance_info`
  - Replication: `redis_replication_lag_bytes`, `redis_connected_replicas`, `redis_master_link_up`
  - Memory: `redis_used_memory_bytes`, `redis_maxmemory_bytes`, `redis_mem_fragmentation_ratio`, `redis_evicted_keys_total`
  - Connections: `redis_connected_clients`, `redis_blocked_clients`, `redis_rejected_connections_total`
  - Operations: `redis_instantaneous_ops_per_sec`, `redis_command_calls_total`, `redis_keyspace_hits_total`, `redis_keyspace_misses_total`
  - Persistence: `redis_rdb_last_save_timestamp`, `redis_rdb_last_bgsave_duration_seconds`, `redis_aof_last_rewrite_duration_seconds`

## Helm setup

Key chart values (`charts/redis-operator/values.yaml`):

```yaml
metrics:
  serviceMonitor:
    enabled: true

monitoring:
  podMonitor:
    enabled: true
  alertingRules:
    enabled: true
  grafanaDashboard:
    enabled: true
```

Templates installed by the chart:

- `templates/servicemonitor.yaml` (operator metrics scrape)
- `templates/podmonitor.yaml` (instance-manager metrics scrape)
- `templates/prometheusrule.yaml` (default Redis alerts)
- `templates/grafana-configmap.yaml` (dashboard ConfigMap with `grafana_dashboard: "1"`)

## Grafana dashboard

The bundled dashboard JSON lives at:

- `charts/redis-operator/dashboards/redis-overview.json`

It includes:

- Cluster overview (phase, desired/healthy/unhealthy instances)
- Replication health (lag, connected replicas, master link)
- Memory and connection pressure
- Command-level throughput and hit ratio
- Persistence and backup visibility

Import options:

- **Automatic**: keep `monitoring.grafanaDashboard.enabled=true` and use Grafana sidecar discovery.
- **Manual**: import the JSON file directly in Grafana UI.

## Alerting rules

The chart creates the following alerts:

- `RedisPrimaryUnavailable`
- `RedisReplicationLagHigh`
- `RedisMemoryUsageHigh`
- `RedisBackupMissing`
- `RedisInstanceDown`

### Tuning global thresholds

```yaml
monitoring:
  alertingRules:
    replicationLagThresholdBytes: 10485760
    memoryUsageThresholdRatio: 0.85
    backupMissingSeconds: 86400
```

### Per-cluster customization

Keep the default PrometheusRule enabled for baseline coverage, then add a second `PrometheusRule` with stricter/looser expressions filtered by `{namespace="<ns>", cluster="<cluster>"}`.

Example:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: redis-operator-alerts-team-a
  namespace: monitoring
spec:
  groups:
    - name: redis-team-a
      rules:
        - alert: RedisReplicationLagHigh
          expr: max by (namespace, cluster, pod) (redis_replication_lag_bytes{namespace="payments",cluster="orders",role="slave"}) > 5242880
          for: 2m
          labels:
            severity: warning
```

## Custom query packs via ConfigMap

A CNPG-style pattern is to version custom query logic in ConfigMaps. For Redis, use this for organization-specific recording/alert queries and custom Grafana views.

Example ConfigMap containing custom PromQL recording rules:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: redis-custom-queries
  namespace: monitoring
data:
  rules.yaml: |
    groups:
      - name: redis-custom
        rules:
          - record: redis:read_write_ratio
            expr: |
              sum(rate(redis_command_calls_total{command="get"}[5m]))
              /
              clamp_min(sum(rate(redis_command_calls_total{command=~"set|del"}[5m])), 1)
```

Deploy these queries via your Prometheus/stack workflow (for example, an additional `PrometheusRule` rendered from this ConfigMap in GitOps).

## Validation tips

- Render chart with monitoring CRDs enabled in Helm capabilities and confirm all monitoring objects are present.
- Check alert rule syntax with `promtool check rules`.
- Validate dashboard JSON with `jq empty`.
