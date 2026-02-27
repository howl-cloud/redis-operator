#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART_DIR="${ROOT_DIR}/charts/redis-operator"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

require_tool() {
  local tool_name="$1"
  if ! command -v "${tool_name}" >/dev/null 2>&1; then
    echo "missing required tool: ${tool_name}" >&2
    exit 1
  fi
}

docker_daemon_available() {
  command -v docker >/dev/null 2>&1 || return 1
  docker info >/dev/null 2>&1
}

require_tool helm
require_tool jq
require_tool rg

HELM_API_VERSIONS=(
  --api-versions monitoring.coreos.com/v1/PrometheusRule
  --api-versions monitoring.coreos.com/v1/ServiceMonitor
  --api-versions monitoring.coreos.com/v1/PodMonitor
)

echo "Rendering chart with monitoring CRDs enabled..."
helm template redis-operator "${CHART_DIR}" "${HELM_API_VERSIONS[@]}" > "${TMP_DIR}/rendered.yaml"

echo "Checking PrometheusRule exists and includes required alerts..."
rg -q "^kind: PrometheusRule$" "${TMP_DIR}/rendered.yaml"
required_alerts=(
  RedisPrimaryUnavailable
  RedisReplicationLagHigh
  RedisMemoryUsageHigh
  RedisBackupMissing
  RedisInstanceDown
)
for alert_name in "${required_alerts[@]}"; do
  rg -q "alert: ${alert_name}" "${TMP_DIR}/rendered.yaml"
done

if command -v promtool >/dev/null 2>&1; then
  echo "Validating PrometheusRule expressions with promtool..."
  awk '
    /^kind: PrometheusRule$/ { in_rule = 1 }
    in_rule && /^spec:$/ { in_spec = 1; next }
    in_spec && /^---$/ { exit }
    in_spec {
      sub(/^  /, "")
      print
    }
  ' "${TMP_DIR}/rendered.yaml" > "${TMP_DIR}/prometheus-rules.yaml"
  promtool check rules "${TMP_DIR}/prometheus-rules.yaml"
elif docker_daemon_available; then
  echo "promtool not found; validating with prom/prometheus container..."
  awk '
    /^kind: PrometheusRule$/ { in_rule = 1 }
    in_rule && /^spec:$/ { in_spec = 1; next }
    in_spec && /^---$/ { exit }
    in_spec {
      sub(/^  /, "")
      print
    }
  ' "${TMP_DIR}/rendered.yaml" > "${TMP_DIR}/prometheus-rules.yaml"
  docker run --rm \
    -v "${TMP_DIR}:/tmpdata" \
    --entrypoint promtool \
    prom/prometheus:v2.54.1 \
    check rules /tmpdata/prometheus-rules.yaml
else
  if command -v docker >/dev/null 2>&1; then
    echo "promtool not found and docker daemon unavailable; skipping promtool check rules"
  else
    echo "promtool not found and docker unavailable; skipping promtool check rules"
  fi
fi

if command -v amtool >/dev/null 2>&1; then
  echo "Validating Alertmanager config parser with amtool..."
  cat > "${TMP_DIR}/alertmanager-minimal.yaml" <<'EOF'
route:
  receiver: default
receivers:
  - name: default
EOF
  amtool check-config "${TMP_DIR}/alertmanager-minimal.yaml"
elif docker_daemon_available; then
  echo "amtool not found; validating with prom/alertmanager container..."
  cat > "${TMP_DIR}/alertmanager-minimal.yaml" <<'EOF'
route:
  receiver: default
receivers:
  - name: default
EOF
  docker run --rm \
    -v "${TMP_DIR}:/tmpdata" \
    --entrypoint amtool \
    prom/alertmanager:v0.27.0 \
    check-config /tmpdata/alertmanager-minimal.yaml
else
  if command -v docker >/dev/null 2>&1; then
    echo "amtool not found and docker daemon unavailable; skipping amtool check-config"
  else
    echo "amtool not found and docker unavailable; skipping amtool check-config"
  fi
fi

echo "Validating Grafana dashboard JSON..."
jq empty "${CHART_DIR}/dashboards/redis-overview.json"

echo "Checking Helm toggle: monitoring.alertingRules.enabled=false..."
helm template redis-operator "${CHART_DIR}" "${HELM_API_VERSIONS[@]}" \
  --set monitoring.alertingRules.enabled=false > "${TMP_DIR}/no-alerting-rules.yaml"
if rg -q "^kind: PrometheusRule$" "${TMP_DIR}/no-alerting-rules.yaml"; then
  echo "PrometheusRule rendered while monitoring.alertingRules.enabled=false" >&2
  exit 1
fi

echo "Checking Helm toggle: monitoring.grafanaDashboard.enabled=false..."
helm template redis-operator "${CHART_DIR}" "${HELM_API_VERSIONS[@]}" \
  --set monitoring.grafanaDashboard.enabled=false > "${TMP_DIR}/no-grafana-dashboard.yaml"
if rg -q "redis-operator-overview\\.json|grafana_dashboard: \"1\"" "${TMP_DIR}/no-grafana-dashboard.yaml"; then
  echo "Grafana dashboard ConfigMap rendered while monitoring.grafanaDashboard.enabled=false" >&2
  exit 1
fi

echo "Monitoring validation passed."
