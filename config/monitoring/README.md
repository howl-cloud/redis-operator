# Monitoring Assets

Pre-built monitoring assets are shipped with the Helm chart:

| Asset | Path |
|------|------|
| Grafana dashboard JSON | `charts/redis-operator/dashboards/redis-overview.json` |
| PrometheusRule template | `charts/redis-operator/templates/prometheusrule.yaml` |
| Grafana ConfigMap template | `charts/redis-operator/templates/grafana-configmap.yaml` |
| Instance-manager PodMonitor | `charts/redis-operator/templates/podmonitor.yaml` |
| Operator ServiceMonitor | `charts/redis-operator/templates/servicemonitor.yaml` |

## Metrics scrape endpoints

- **Operator controller metrics**: `:9090/metrics` (via Service + ServiceMonitor)
- **Redis instance-manager metrics**: `:8080/metrics` on each Redis data pod (via PodMonitor)

The PodMonitor targets pods with:

- `redis.io/workload=data`
- `redis.io/cluster` label present

## Helm toggles

Key values in `charts/redis-operator/values.yaml`:

- `monitoring.alertingRules.enabled` (default `true`)
- `monitoring.grafanaDashboard.enabled` (default `true`)
- `monitoring.podMonitor.enabled` (default `true`)
- `metrics.serviceMonitor.enabled` (default `false`)

## Documentation

See `docs/monitoring.md` for setup, dashboard import, alert threshold customization, and custom query patterns.
