#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME=${CLUSTER_NAME:-redis-operator-smoke}
OPERATOR_NS=${OPERATOR_NS:-redis-operator-system}
TEST_NS=${TEST_NS:-default}
RELEASE_NAME=${RELEASE_NAME:-redis-operator}
IMAGE=${IMAGE:-redis-operator:smoke}
REDIS_CLUSTER_NAME=${REDIS_CLUSTER_NAME:-smoke-redis}

cleanup() {
  local exit_code=$?
  if [[ ${exit_code} -ne 0 ]] && command -v kubectl >/dev/null 2>&1; then
    echo "smoke test failed; collecting diagnostics..." >&2
    kubectl get pods -A || true
    kubectl describe pods -n "${OPERATOR_NS}" || true
    kubectl describe pods -n "${TEST_NS}" || true
    kubectl logs deployment/"${RELEASE_NAME}" -n "${OPERATOR_NS}" --all-containers=true || true
  fi

  if command -v kind >/dev/null 2>&1; then
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

kind create cluster --name "${CLUSTER_NAME}" --wait 120s

docker build -t "${IMAGE}" .
kind load docker-image "${IMAGE}" --name "${CLUSTER_NAME}"

kubectl apply -f config/crd/bases/

helm upgrade --install "${RELEASE_NAME}" charts/redis-operator \
  --namespace "${OPERATOR_NS}" \
  --create-namespace \
  --set image.repository="${IMAGE%:*}" \
  --set image.tag="${IMAGE##*:}" \
  --set image.pullPolicy=IfNotPresent \
  --set webhook.enabled=false

kubectl rollout status deployment/"${RELEASE_NAME}" -n "${OPERATOR_NS}" --timeout=180s

cat <<YAML | kubectl apply -n "${TEST_NS}" -f -
apiVersion: redis.io/v1
kind: RedisCluster
metadata:
  name: ${REDIS_CLUSTER_NAME}
spec:
  instances: 1
  storage:
    size: 1Gi
YAML

kubectl wait --for=create pod/${REDIS_CLUSTER_NAME}-0 -n "${TEST_NS}" --timeout=300s
kubectl wait --for=condition=Ready pod/${REDIS_CLUSTER_NAME}-0 -n "${TEST_NS}" --timeout=300s
kubectl get pvc ${REDIS_CLUSTER_NAME}-data-0 -n "${TEST_NS}" >/dev/null

kubectl patch rediscluster/${REDIS_CLUSTER_NAME} -n "${TEST_NS}" --type merge -p '{"spec":{"instances":3}}'
for ordinal in 0 1 2; do
  kubectl wait --for=create pod/${REDIS_CLUSTER_NAME}-${ordinal} -n "${TEST_NS}" --timeout=300s
  kubectl wait --for=condition=Ready pod/${REDIS_CLUSTER_NAME}-${ordinal} -n "${TEST_NS}" --timeout=300s
  kubectl get pvc ${REDIS_CLUSTER_NAME}-data-${ordinal} -n "${TEST_NS}" >/dev/null
done

primary_pod=$(kubectl get pods -n "${TEST_NS}" -l redis.io/cluster="${REDIS_CLUSTER_NAME}",redis.io/role=primary -o jsonpath='{.items[0].metadata.name}')
replica_count=$(kubectl get pods -n "${TEST_NS}" -l redis.io/cluster="${REDIS_CLUSTER_NAME}",redis.io/role=replica --no-headers | wc -l | tr -d ' ')

if [[ "${primary_pod}" != "${REDIS_CLUSTER_NAME}-0" ]]; then
  echo "expected primary pod ${REDIS_CLUSTER_NAME}-0, got ${primary_pod}" >&2
  exit 1
fi

if [[ "${replica_count}" != "2" ]]; then
  echo "expected 2 replicas, got ${replica_count}" >&2
  exit 1
fi

leader_targets=$(kubectl get endpoints ${REDIS_CLUSTER_NAME}-leader -n "${TEST_NS}" -o jsonpath='{.subsets[*].addresses[*].targetRef.name}')
replica_targets=$(kubectl get endpoints ${REDIS_CLUSTER_NAME}-replica -n "${TEST_NS}" -o jsonpath='{.subsets[*].addresses[*].targetRef.name}')

if [[ "${leader_targets}" != *"${REDIS_CLUSTER_NAME}-0"* ]]; then
  echo "leader service is not routing to primary pod" >&2
  exit 1
fi
if [[ "${replica_targets}" != *"${REDIS_CLUSTER_NAME}-1"* ]] || [[ "${replica_targets}" != *"${REDIS_CLUSTER_NAME}-2"* ]]; then
  echo "replica service is not routing to replica pods" >&2
  exit 1
fi

password=$(kubectl get secret ${REDIS_CLUSTER_NAME}-auth -n "${TEST_NS}" -o jsonpath='{.data.password}' | base64 -d)
unauth_ping_result=""
for ((attempt=1; attempt<=30; attempt++)); do
  unauth_ping_result=$(kubectl exec -n "${TEST_NS}" -c redis "${REDIS_CLUSTER_NAME}-0" -- redis-cli ping 2>&1 || true)
  if [[ "${unauth_ping_result}" == *"NOAUTH"* ]] || [[ "${unauth_ping_result}" == *"Authentication required"* ]]; then
    break
  fi
  sleep 2
done
if [[ "${unauth_ping_result}" == *"PONG"* ]]; then
  echo "expected unauthenticated ping to fail, got ${unauth_ping_result}" >&2
  exit 1
fi
if [[ "${unauth_ping_result}" != *"NOAUTH"* ]] && [[ "${unauth_ping_result}" != *"Authentication required"* ]]; then
  echo "expected unauthenticated ping to return NOAUTH/auth-required, got ${unauth_ping_result}" >&2
  exit 1
fi

ping_result=$(kubectl exec -n "${TEST_NS}" -c redis "${REDIS_CLUSTER_NAME}-0" -- redis-cli -a "${password}" ping 2>/dev/null | tr -d '\r')
if [[ "${ping_result}" != "PONG" ]]; then
  echo "expected PONG, got ${ping_result}" >&2
  exit 1
fi

kubectl delete pod ${REDIS_CLUSTER_NAME}-1 -n "${TEST_NS}" --wait=true
kubectl wait --for=create pod/${REDIS_CLUSTER_NAME}-1 -n "${TEST_NS}" --timeout=300s
kubectl wait --for=condition=Ready pod/${REDIS_CLUSTER_NAME}-1 -n "${TEST_NS}" --timeout=300s

kubectl patch rediscluster/${REDIS_CLUSTER_NAME} -n "${TEST_NS}" --type merge -p '{"spec":{"instances":1}}'
kubectl wait --for=delete pod/${REDIS_CLUSTER_NAME}-1 -n "${TEST_NS}" --timeout=300s
kubectl wait --for=delete pod/${REDIS_CLUSTER_NAME}-2 -n "${TEST_NS}" --timeout=300s
kubectl wait --for=condition=Ready pod/${REDIS_CLUSTER_NAME}-0 -n "${TEST_NS}" --timeout=180s

echo "real cluster smoke test passed"
