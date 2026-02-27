#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME=${CLUSTER_NAME:-redis-operator-upgrade}
OPERATOR_NS=${OPERATOR_NS:-redis-operator-system}
TEST_NS=${TEST_NS:-default}
RELEASE_NAME=${RELEASE_NAME:-redis-operator}

BEFORE_IMAGE=${BEFORE_IMAGE:-ghcr.io/howl-cloud/redis-operator:v0.1.1}
AFTER_IMAGE=${AFTER_IMAGE:-redis-operator:upgrade-test}

REDIS_CLUSTER_NAME=${REDIS_CLUSTER_NAME:-upgrade-redis}
POST_UPGRADE_CLUSTER_NAME=${POST_UPGRADE_CLUSTER_NAME:-upgrade-redis-2}
REDIS_INSTANCES=${REDIS_INSTANCES:-3}
BASE_REDIS_IMAGE=${BASE_REDIS_IMAGE:-redis:7.2}
ROLLING_UPDATE_IMAGE=${ROLLING_UPDATE_IMAGE:-redis:7.2.1}
KEY_COUNT=${KEY_COUNT:-100}
KEEP_CLUSTER=${KEEP_CLUSTER:-false}

log() {
  printf "[%s] %s\n" "$(date -u +%H:%M:%S)" "$*"
}

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

require() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

create_webhook_tls_secret() {
  local cert_secret_name="${RELEASE_NAME}-webhook-cert"
  local service_name="${RELEASE_NAME}-webhook-service"
  local tmpdir
  tmpdir=$(mktemp -d)

  cat > "${tmpdir}/openssl.cnf" <<EOF
[req]
distinguished_name = req_distinguished_name
req_extensions = v3_req
prompt = no

[req_distinguished_name]
CN = ${service_name}.${OPERATOR_NS}.svc

[v3_req]
keyUsage = critical, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names

[alt_names]
DNS.1 = ${service_name}
DNS.2 = ${service_name}.${OPERATOR_NS}
DNS.3 = ${service_name}.${OPERATOR_NS}.svc
DNS.4 = ${service_name}.${OPERATOR_NS}.svc.cluster.local
EOF

  openssl req \
    -x509 \
    -nodes \
    -newkey rsa:2048 \
    -days 1 \
    -keyout "${tmpdir}/tls.key" \
    -out "${tmpdir}/tls.crt" \
    -config "${tmpdir}/openssl.cnf" \
    -extensions v3_req >/dev/null 2>&1

  kubectl create secret tls "${cert_secret_name}" \
    -n "${OPERATOR_NS}" \
    --cert="${tmpdir}/tls.crt" \
    --key="${tmpdir}/tls.key" \
    --dry-run=client \
    -o yaml | kubectl apply -f - >/dev/null

  rm -rf "${tmpdir}"
}

cleanup() {
  local exit_code=$?
  if [[ ${exit_code} -ne 0 ]] && command -v kubectl >/dev/null 2>&1; then
    echo "upgrade test failed; collecting diagnostics..." >&2
    kubectl get redisclusters -A || true
    kubectl get pods -A || true
    kubectl describe rediscluster "${REDIS_CLUSTER_NAME}" -n "${TEST_NS}" || true
    kubectl describe rediscluster "${POST_UPGRADE_CLUSTER_NAME}" -n "${TEST_NS}" || true
    kubectl describe pods -n "${OPERATOR_NS}" || true
    kubectl describe pods -n "${TEST_NS}" || true
    kubectl logs deployment/"${RELEASE_NAME}" -n "${OPERATOR_NS}" --all-containers=true || true
    kubectl get events -A --sort-by=.lastTimestamp | tail -n 120 || true
  fi

  if [[ "${KEEP_CLUSTER}" != "true" ]] && command -v kind >/dev/null 2>&1; then
    kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  fi

  exit "${exit_code}"
}
trap cleanup EXIT

retry_until() {
  local description=$1
  local timeout_seconds=$2
  local interval_seconds=$3
  shift 3

  local start=$SECONDS
  while true; do
    if "$@"; then
      return 0
    fi
    if (( SECONDS - start >= timeout_seconds )); then
      log "Timed out waiting for: ${description}"
      return 1
    fi
    sleep "${interval_seconds}"
  done
}

image_repository() {
  local image=$1
  printf "%s\n" "${image%:*}"
}

image_tag() {
  local image=$1
  printf "%s\n" "${image##*:}"
}

validate_tagged_image() {
  local image=$1
  local repo
  local tag
  repo=$(image_repository "${image}")
  tag=$(image_tag "${image}")
  if [[ "${repo}" == "${image}" || -z "${tag}" ]]; then
    fail "image must include a tag: ${image}"
  fi
}

cluster_manifest() {
  local name=$1
  local instances=$2
  local image_name=$3
  cat <<YAML
apiVersion: redis.io/v1
kind: RedisCluster
metadata:
  name: ${name}
spec:
  instances: ${instances}
  imageName: ${image_name}
  storage:
    size: 1Gi
YAML
}

cluster_phase() {
  local cluster_name=$1
  kubectl get rediscluster "${cluster_name}" -n "${TEST_NS}" -o jsonpath='{.status.phase}' 2>/dev/null || true
}

cluster_ready_instances() {
  local cluster_name=$1
  kubectl get rediscluster "${cluster_name}" -n "${TEST_NS}" -o jsonpath='{.status.readyInstances}' 2>/dev/null || true
}

cluster_current_primary() {
  local cluster_name=$1
  kubectl get rediscluster "${cluster_name}" -n "${TEST_NS}" -o jsonpath='{.status.currentPrimary}' 2>/dev/null || true
}

cluster_is_healthy() {
  local cluster_name=$1
  local expected_ready=$2
  local phase
  local ready

  phase=$(cluster_phase "${cluster_name}")
  ready=$(cluster_ready_instances "${cluster_name}")
  [[ "${phase}" == "Healthy" && "${ready}" == "${expected_ready}" ]]
}

wait_for_cluster_healthy() {
  local cluster_name=$1
  local expected_ready=$2
  local timeout_seconds=$3
  retry_until "cluster ${cluster_name} to be Healthy (${expected_ready} ready)" "${timeout_seconds}" 3 \
    cluster_is_healthy "${cluster_name}" "${expected_ready}" || \
    fail "cluster ${cluster_name} did not become Healthy"
}

wait_for_pod_ready() {
  local pod_name=$1
  local timeout_seconds=$2
  kubectl wait --for=create "pod/${pod_name}" -n "${TEST_NS}" --timeout="${timeout_seconds}s" >/dev/null
  kubectl wait --for=condition=Ready "pod/${pod_name}" -n "${TEST_NS}" --timeout="${timeout_seconds}s" >/dev/null
}

wait_for_cluster_pods() {
  local cluster_name=$1
  local instances=$2
  local timeout_seconds=$3
  local ordinal

  for ((ordinal = 0; ordinal < instances; ordinal++)); do
    wait_for_pod_ready "${cluster_name}-${ordinal}" "${timeout_seconds}"
  done
}

snapshot_pod_creation_timestamps() {
  local cluster_name=$1
  kubectl get pods -n "${TEST_NS}" -l "redis.io/cluster=${cluster_name}" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.metadata.creationTimestamp}{"\n"}{end}' | \
    sed '/^$/d' | sort
}

operator_pod() {
  kubectl get pods -n "${OPERATOR_NS}" \
    -l "app.kubernetes.io/name=redis-operator,app.kubernetes.io/instance=${RELEASE_NAME}" \
    --sort-by=.metadata.creationTimestamp \
    -o custom-columns=NAME:.metadata.name,DELETING:.metadata.deletionTimestamp \
    --no-headers 2>/dev/null | \
    awk '$2 == "<none>" || $2 == "" {name = $1} END {print name}'
}

get_any_cluster_pod() {
  local cluster_name=$1
  kubectl get pods -n "${TEST_NS}" -l "redis.io/cluster=${cluster_name}" -o jsonpath='{.items[0].metadata.name}'
}

redis_password() {
  local cluster_name=$1
  kubectl get secret "${cluster_name}-auth" -n "${TEST_NS}" -o jsonpath='{.data.password}' | base64 -d
}

write_keys() {
  local cluster_name=$1
  local password=$2
  local client_pod
  client_pod=$(get_any_cluster_pod "${cluster_name}")

  kubectl exec -n "${TEST_NS}" "${client_pod}" -- env REDISCLI_AUTH="${password}" sh -ceu "
i=1
while [ \$i -le ${KEY_COUNT} ]; do
  redis-cli --no-auth-warning -h ${cluster_name}-leader SET upgrade-key-\$i value-\$i >/dev/null
  i=\$((i+1))
done
"
}

verify_keys() {
  local cluster_name=$1
  local password=$2
  local client_pod
  client_pod=$(get_any_cluster_pod "${cluster_name}")

  kubectl exec -n "${TEST_NS}" "${client_pod}" -- env REDISCLI_AUTH="${password}" sh -ceu "
i=1
while [ \$i -le ${KEY_COUNT} ]; do
  got=\$(redis-cli --no-auth-warning -h ${cluster_name}-leader GET upgrade-key-\$i | tr -d '\r')
  expected=value-\$i
  if [ \"\$got\" != \"\$expected\" ]; then
    echo \"key mismatch for upgrade-key-\$i: got=\$got expected=\$expected\" >&2
    exit 1
  fi
  i=\$((i+1))
done
"
}

webhook_accepts_cluster() {
  local probe_name=$1
  cluster_manifest "${probe_name}" 1 "${BASE_REDIS_IMAGE}" | \
    kubectl apply --dry-run=server -n "${TEST_NS}" -f - >/dev/null 2>&1
}

all_cluster_pods_on_image() {
  local cluster_name=$1
  local instances=$2
  local target_image=$3
  local ordinal
  local image

  for ((ordinal = 0; ordinal < instances; ordinal++)); do
    image=$(kubectl get pod "${cluster_name}-${ordinal}" -n "${TEST_NS}" \
      -o jsonpath='{.spec.containers[?(@.name=="redis")].image}' 2>/dev/null || true)
    if [[ "${image}" != "${target_image}" ]]; then
      return 1
    fi
  done
  return 0
}

wait_for_operator_rollout_with_healthy_cluster() {
  local cluster_name=$1
  local timeout_seconds=$2
  local start=$SECONDS

  while true; do
    local phase
    phase=$(cluster_phase "${cluster_name}")
    if [[ "${phase}" != "Healthy" ]]; then
      fail "cluster ${cluster_name} left Healthy during operator rollout (phase=${phase})"
    fi

    if kubectl rollout status deployment/"${RELEASE_NAME}" -n "${OPERATOR_NS}" --timeout=5s >/dev/null 2>&1; then
      return 0
    fi

    if (( SECONDS - start >= timeout_seconds )); then
      fail "operator deployment rollout did not complete within ${timeout_seconds}s"
    fi

    sleep 2
  done
}

main() {
  require kind
  require kubectl
  require helm
  require docker
  require base64
  require sed
  require openssl

  validate_tagged_image "${BEFORE_IMAGE}"
  validate_tagged_image "${AFTER_IMAGE}"

  local before_repo
  local before_tag
  before_repo=$(image_repository "${BEFORE_IMAGE}")
  before_tag=$(image_tag "${BEFORE_IMAGE}")

  log "Creating kind cluster ${CLUSTER_NAME}"
  kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  kind create cluster --name "${CLUSTER_NAME}" --wait 120s

  log "Preparing BEFORE_IMAGE ${BEFORE_IMAGE}"
  if ! docker image inspect "${BEFORE_IMAGE}" >/dev/null 2>&1; then
    docker pull "${BEFORE_IMAGE}" >/dev/null
  else
    log "Using local BEFORE_IMAGE ${BEFORE_IMAGE}"
  fi
  kind load docker-image "${BEFORE_IMAGE}" --name "${CLUSTER_NAME}" >/dev/null

  kubectl apply -f config/crd/bases/ >/dev/null
  kubectl create namespace "${OPERATOR_NS}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl create namespace "${TEST_NS}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  create_webhook_tls_secret

  log "Installing operator release ${RELEASE_NAME} with ${BEFORE_IMAGE}"
  helm upgrade --install "${RELEASE_NAME}" charts/redis-operator \
    --namespace "${OPERATOR_NS}" \
    --create-namespace \
    --set image.repository="${before_repo}" \
    --set image.tag="${before_tag}" \
    --set image.pullPolicy=IfNotPresent \
    --set webhook.enabled=true >/dev/null

  kubectl rollout status deployment/"${RELEASE_NAME}" -n "${OPERATOR_NS}" --timeout=240s >/dev/null

  log "Waiting for webhook to accept valid requests"
  retry_until "webhook to become functional" 120 2 webhook_accepts_cluster "pre-upgrade-webhook-check" || \
    fail "webhook did not become functional before pre-upgrade setup"

  log "Creating baseline cluster ${REDIS_CLUSTER_NAME}"
  cluster_manifest "${REDIS_CLUSTER_NAME}" "${REDIS_INSTANCES}" "${BASE_REDIS_IMAGE}" | \
    kubectl apply -n "${TEST_NS}" -f - >/dev/null
  wait_for_cluster_pods "${REDIS_CLUSTER_NAME}" "${REDIS_INSTANCES}" 300
  wait_for_cluster_healthy "${REDIS_CLUSTER_NAME}" "${REDIS_INSTANCES}" 300

  local password
  password=$(redis_password "${REDIS_CLUSTER_NAME}")
  [[ -n "${password}" ]] || fail "empty redis password for ${REDIS_CLUSTER_NAME}"

  log "Writing ${KEY_COUNT} keys before operator upgrade"
  write_keys "${REDIS_CLUSTER_NAME}" "${password}"
  verify_keys "${REDIS_CLUSTER_NAME}" "${password}"

  local old_primary
  old_primary=$(cluster_current_primary "${REDIS_CLUSTER_NAME}")
  [[ -n "${old_primary}" ]] || fail "unable to determine current primary before upgrade"

  local before_snapshot
  before_snapshot=$(snapshot_pod_creation_timestamps "${REDIS_CLUSTER_NAME}")
  [[ -n "${before_snapshot}" ]] || fail "unable to capture pre-upgrade pod creation timestamps"

  local old_operator_pod
  old_operator_pod=$(operator_pod)
  [[ -n "${old_operator_pod}" ]] || fail "unable to determine operator pod before upgrade"

  log "Building AFTER_IMAGE ${AFTER_IMAGE} from source"
  docker build -t "${AFTER_IMAGE}" . >/dev/null
  kind load docker-image "${AFTER_IMAGE}" --name "${CLUSTER_NAME}" >/dev/null

  log "Upgrading operator release ${RELEASE_NAME} to ${AFTER_IMAGE}"
  kubectl set image deployment/"${RELEASE_NAME}" \
    -n "${OPERATOR_NS}" \
    manager="${AFTER_IMAGE}" >/dev/null

  wait_for_operator_rollout_with_healthy_cluster "${REDIS_CLUSTER_NAME}" 300

  local new_operator_pod
  new_operator_pod=$(operator_pod)
  [[ -n "${new_operator_pod}" ]] || fail "unable to determine operator pod after upgrade"
  kubectl wait --for=condition=Ready "pod/${new_operator_pod}" -n "${OPERATOR_NS}" --timeout=120s >/dev/null
  if [[ "${old_operator_pod}" == "${new_operator_pod}" ]]; then
    fail "expected operator pod restart during upgrade, but pod name did not change (${old_operator_pod})"
  fi

  wait_for_cluster_healthy "${REDIS_CLUSTER_NAME}" "${REDIS_INSTANCES}" 300

  local new_primary
  new_primary=$(cluster_current_primary "${REDIS_CLUSTER_NAME}")
  if [[ "${new_primary}" != "${old_primary}" ]]; then
    fail "primary changed during operator upgrade (before=${old_primary}, after=${new_primary})"
  fi

  local after_snapshot
  after_snapshot=$(snapshot_pod_creation_timestamps "${REDIS_CLUSTER_NAME}")
  if [[ "${before_snapshot}" != "${after_snapshot}" ]]; then
    echo "before snapshot:" >&2
    echo "${before_snapshot}" >&2
    echo "after snapshot:" >&2
    echo "${after_snapshot}" >&2
    fail "redis pod creation timestamps changed during operator upgrade"
  fi

  log "Verifying pre-upgrade data after operator upgrade"
  verify_keys "${REDIS_CLUSTER_NAME}" "${password}"

  log "Ensuring webhook recovers within 30s of new operator readiness"
  retry_until "post-upgrade webhook recovery" 30 2 webhook_accepts_cluster "post-upgrade-webhook-check" || \
    fail "webhook did not recover within 30s after operator readiness"

  log "Creating post-upgrade cluster ${POST_UPGRADE_CLUSTER_NAME}"
  cluster_manifest "${POST_UPGRADE_CLUSTER_NAME}" 1 "${BASE_REDIS_IMAGE}" | \
    kubectl apply -n "${TEST_NS}" -f - >/dev/null
  wait_for_cluster_pods "${POST_UPGRADE_CLUSTER_NAME}" 1 300
  wait_for_cluster_healthy "${POST_UPGRADE_CLUSTER_NAME}" 1 300

  log "Triggering post-upgrade rolling update to ${ROLLING_UPDATE_IMAGE}"
  local rolling_before_snapshot
  rolling_before_snapshot=$(snapshot_pod_creation_timestamps "${REDIS_CLUSTER_NAME}")

  kubectl patch rediscluster/"${REDIS_CLUSTER_NAME}" -n "${TEST_NS}" --type merge \
    -p "{\"spec\":{\"imageName\":\"${ROLLING_UPDATE_IMAGE}\"}}" >/dev/null

  retry_until "all pods to use ${ROLLING_UPDATE_IMAGE}" 600 5 \
    all_cluster_pods_on_image "${REDIS_CLUSTER_NAME}" "${REDIS_INSTANCES}" "${ROLLING_UPDATE_IMAGE}" || \
    fail "rolling update did not converge to image ${ROLLING_UPDATE_IMAGE}"

  wait_for_cluster_healthy "${REDIS_CLUSTER_NAME}" "${REDIS_INSTANCES}" 600
  wait_for_cluster_pods "${REDIS_CLUSTER_NAME}" "${REDIS_INSTANCES}" 300

  local rolling_after_snapshot
  rolling_after_snapshot=$(snapshot_pod_creation_timestamps "${REDIS_CLUSTER_NAME}")
  if [[ "${rolling_before_snapshot}" == "${rolling_after_snapshot}" ]]; then
    fail "post-upgrade rolling update did not restart any pods"
  fi

  verify_keys "${REDIS_CLUSTER_NAME}" "${password}"
  log "operator upgrade test passed"
}

main "$@"
