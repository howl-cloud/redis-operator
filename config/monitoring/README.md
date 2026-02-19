# config/monitoring

Observability manifests for redis-operator managed clusters.

## Files

| File | Description |
|------|-------------|
| `pod-monitor.yaml` | `PodMonitor` (Prometheus Operator) — scrapes `/metrics` on port `9090` from all Redis pods |
| `grafana-dashboard.json` | Pre-built Grafana dashboard covering replication lag, memory, keyspace hit rate, connected clients |

## Metrics Scrape

Each Redis pod exposes `/metrics` on `:9090` (served by the instance manager's HTTP server). The `PodMonitor` targets pods with the label `redis.io/cluster: <name>`.

If Prometheus Operator is not available, scrape config can be added manually:

```yaml
scrape_configs:
  - job_name: redis-operator
    kubernetes_sd_configs:
      - role: pod
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_label_redis_io_cluster]
        action: keep
        regex: .+
      - source_labels: [__meta_kubernetes_pod_ip]
        target_label: __address__
        replacement: $1:9090
```

## Key Dashboard Panels

- Replication lag per replica (seconds behind primary)
- Memory used vs. peak vs. limit
- Keyspace hit rate (`hits / (hits + misses)`)
- Connected clients
- Commands processed per second
- Primary availability (up/down)
- Last successful RDB save age
