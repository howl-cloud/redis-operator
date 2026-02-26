#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME=${CLUSTER_NAME:-redis-operator-chaos}
OPERATOR_NS=${OPERATOR_NS:-redis-operator-system}
TEST_NS=${TEST_NS:-default}
RELEASE_NAME=${RELEASE_NAME:-redis-operator}
IMAGE=${IMAGE:-redis-operator:chaos}
REDIS_CLUSTER_NAME=${REDIS_CLUSTER_NAME:-chaos-redis}
REDIS_INSTANCES=${REDIS_INSTANCES:-3}
NETWORK_BLOCKER_IMAGE=${NETWORK_BLOCKER_IMAGE:-nicolaka/netshoot:v0.13}
KEEP_CLUSTER=${KEEP_CLUSTER:-false}
CHAOS_LABEL_FILTER=${CHAOS_LABEL_FILTER:-}

cleanup() {
  local exit_code=$?

  if [[ ${exit_code} -ne 0 ]] && command -v kubectl >/dev/null 2>&1; then
    echo "chaos test failed; collecting diagnostics..." >&2
    kubectl get redisclusters -A || true
    kubectl get pods -A || true
    kubectl describe rediscluster "${REDIS_CLUSTER_NAME}" -n "${TEST_NS}" || true
    kubectl describe pods -n "${OPERATOR_NS}" || true
    kubectl describe pods -n "${TEST_NS}" || true
    kubectl logs deployment/"${RELEASE_NAME}" -n "${OPERATOR_NS}" --all-containers=true || true
  fi

  if [[ "${KEEP_CLUSTER}" != "true" ]] && command -v kind >/dev/null 2>&1; then
    kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  fi

  exit "${exit_code}"
}
trap cleanup EXIT

require() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required command: $1" >&2
    exit 1
  }
}

require kind
require kubectl
require helm
require docker
require go

kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
kind create cluster --name "${CLUSTER_NAME}" --wait 120s

docker build -t "${IMAGE}" .
kind load docker-image "${IMAGE}" --name "${CLUSTER_NAME}"

kubectl apply -f config/crd/bases/ >/dev/null
kubectl create namespace "${OPERATOR_NS}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl create namespace "${TEST_NS}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

helm upgrade --install "${RELEASE_NAME}" charts/redis-operator \
  --namespace "${OPERATOR_NS}" \
  --create-namespace \
  --set image.repository="${IMAGE%:*}" \
  --set image.tag="${IMAGE##*:}" \
  --set image.pullPolicy=IfNotPresent \
  --set webhook.enabled=false >/dev/null

kubectl rollout status deployment/"${RELEASE_NAME}" -n "${OPERATOR_NS}" --timeout=240s >/dev/null

test_args=(-v -timeout 120m -count=1 ./test/chaos/...)
if [[ -n "${CHAOS_LABEL_FILTER}" ]]; then
  test_args+=("--ginkgo.label-filter=${CHAOS_LABEL_FILTER}")
fi

RUN_CHAOS_TESTS=1 \
TEST_NS="${TEST_NS}" \
OPERATOR_NS="${OPERATOR_NS}" \
RELEASE_NAME="${RELEASE_NAME}" \
REDIS_CLUSTER_NAME="${REDIS_CLUSTER_NAME}" \
REDIS_INSTANCES="${REDIS_INSTANCES}" \
NETWORK_BLOCKER_IMAGE="${NETWORK_BLOCKER_IMAGE}" \
go test "${test_args[@]}"
