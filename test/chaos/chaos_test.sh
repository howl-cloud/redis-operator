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

PASSWORD=""
NETWORK_BLOCKER_POD=""
NETWORK_BLOCK_TARGET_IP=""
NETWORK_BLOCK_SOURCE_IP=""

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

cleanup() {
  local exit_code=$?

  remove_network_blocker || true

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

cluster_phase() {
  kubectl get rediscluster "${REDIS_CLUSTER_NAME}" -n "${TEST_NS}" -o jsonpath='{.status.phase}' 2>/dev/null || true
}

cluster_current_primary() {
  kubectl get rediscluster "${REDIS_CLUSTER_NAME}" -n "${TEST_NS}" -o jsonpath='{.status.currentPrimary}' 2>/dev/null || true
}

fencing_annotation() {
  kubectl get rediscluster "${REDIS_CLUSTER_NAME}" -n "${TEST_NS}" -o jsonpath='{.metadata.annotations.redis\.io/fencedInstances}' 2>/dev/null || true
}

get_cluster_pods() {
  kubectl get pods -n "${TEST_NS}" -l "redis.io/cluster=${REDIS_CLUSTER_NAME}" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
}

get_any_cluster_pod() {
  kubectl get pods -n "${TEST_NS}" -l "redis.io/cluster=${REDIS_CLUSTER_NAME}" -o jsonpath='{.items[0].metadata.name}'
}

get_primary_pod() {
  kubectl get pods -n "${TEST_NS}" -l "redis.io/cluster=${REDIS_CLUSTER_NAME},redis.io/role=primary" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true
}

get_replica_pods() {
  kubectl get pods -n "${TEST_NS}" -l "redis.io/cluster=${REDIS_CLUSTER_NAME},redis.io/role=replica" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
}

pod_exists() {
  local pod=$1
  kubectl get pod "${pod}" -n "${TEST_NS}" >/dev/null 2>&1
}

wait_for_pod_ready() {
  local pod=$1
  local timeout_seconds=$2

  retry_until "pod ${pod} to exist" "${timeout_seconds}" 2 pod_exists "${pod}" || fail "pod ${pod} was not created"
  kubectl wait --for=condition=Ready "pod/${pod}" -n "${TEST_NS}" --timeout="${timeout_seconds}s" >/dev/null
}

phase_is() {
  local expected=$1
  [[ "$(cluster_phase)" == "${expected}" ]]
}

wait_for_phase() {
  local expected=$1
  local timeout_seconds=$2
  retry_until "cluster phase ${expected}" "${timeout_seconds}" 3 phase_is "${expected}" || fail "cluster did not reach phase ${expected}"
}

wait_for_cluster_healthy() {
  local timeout_seconds=${1:-240}

  wait_for_phase "Healthy" "${timeout_seconds}"
  local ordinal
  for ((ordinal = 0; ordinal < REDIS_INSTANCES; ordinal++)); do
    wait_for_pod_ready "${REDIS_CLUSTER_NAME}-${ordinal}" "${timeout_seconds}"
  done
}

wait_for_primary_change() {
  local old_primary=$1
  local timeout_seconds=$2
  local start=$SECONDS

  while true; do
    local current
    current=$(cluster_current_primary)
    if [[ -n "${current}" && "${current}" != "${old_primary}" ]]; then
      printf "%s\n" "${current}"
      return 0
    fi
    if (( SECONDS - start >= timeout_seconds )); then
      fail "primary did not change from ${old_primary} within ${timeout_seconds}s"
    fi
    sleep 3
  done
}

wait_for_fencing_annotation() {
  local timeout_seconds=$1
  local start=$SECONDS

  while true; do
    local fence
    fence=$(fencing_annotation)
    if [[ -n "${fence}" ]]; then
      return 0
    fi
    if (( SECONDS - start >= timeout_seconds )); then
      fail "fencing annotation did not appear within ${timeout_seconds}s"
    fi
    sleep 2
  done
}

refresh_password() {
  PASSWORD=$(kubectl get secret "${REDIS_CLUSTER_NAME}-auth" -n "${TEST_NS}" -o go-template='{{index .data "password" | base64decode}}')
  if [[ -z "${PASSWORD}" ]]; then
    fail "resolved empty redis password from secret ${REDIS_CLUSTER_NAME}-auth"
  fi
}

exec_redis_on_pod() {
  local pod=$1
  shift
  kubectl exec -n "${TEST_NS}" "${pod}" -- env REDISCLI_AUTH="${PASSWORD}" redis-cli --no-auth-warning "$@"
}

exec_redis_leader() {
  local client_pod
  client_pod=$(get_any_cluster_pod)
  if [[ -z "${client_pod}" ]]; then
    fail "unable to find a client pod for leader redis-cli commands"
  fi
  kubectl exec -n "${TEST_NS}" "${client_pod}" -- env REDISCLI_AUTH="${PASSWORD}" redis-cli --no-auth-warning -h "${REDIS_CLUSTER_NAME}-leader" "$@"
}

flush_cluster_data() {
  exec_redis_leader FLUSHALL >/dev/null
}

write_keys() {
  local prefix=$1
  local count=$2
  local client_pod
  client_pod=$(get_any_cluster_pod)

  log "Writing ${count} keys with prefix ${prefix}"
  kubectl exec -n "${TEST_NS}" "${client_pod}" -- env REDISCLI_AUTH="${PASSWORD}" sh -ceu "
i=1
while [ \$i -le ${count} ]; do
  redis-cli --no-auth-warning -h ${REDIS_CLUSTER_NAME}-leader SET ${prefix}:\$i value-\$i >/dev/null
  i=\$((i+1))
done
"
}

count_keys() {
  local prefix=$1
  local client_pod
  client_pod=$(get_any_cluster_pod)
  kubectl exec -n "${TEST_NS}" "${client_pod}" -- env REDISCLI_AUTH="${PASSWORD}" sh -ceu "
redis-cli --no-auth-warning -h ${REDIS_CLUSTER_NAME}-leader --scan --pattern '${prefix}:*' | wc -l
" | tr -d '[:space:]'
}

assert_keys() {
  local prefix=$1
  local expected_count=$2

  local actual_count
  actual_count=$(count_keys "${prefix}")
  if [[ "${actual_count}" != "${expected_count}" ]]; then
    fail "expected ${expected_count} keys with prefix ${prefix}, got ${actual_count}"
  fi

  if (( expected_count > 0 )); then
    local first_val
    local last_val
    first_val=$(exec_redis_leader GET "${prefix}:1" | tr -d '\r')
    last_val=$(exec_redis_leader GET "${prefix}:${expected_count}" | tr -d '\r')

    [[ "${first_val}" == "value-1" ]] || fail "unexpected value for ${prefix}:1: ${first_val}"
    [[ "${last_val}" == "value-${expected_count}" ]] || fail "unexpected value for ${prefix}:${expected_count}: ${last_val}"
  fi
}

wait_for_replication() {
  local primary_pod=$1
  local required_replicas=$2
  local timeout_ms=$3

  local acked
  acked=$(exec_redis_on_pod "${primary_pod}" WAIT "${required_replicas}" "${timeout_ms}" | tr -d '\r[:space:]')
  if [[ -z "${acked}" || "${acked}" -lt "${required_replicas}" ]]; then
    fail "WAIT ${required_replicas} ${timeout_ms} returned ${acked}"
  fi
}

redis_role_of_pod() {
  local pod=$1
  exec_redis_on_pod "${pod}" INFO replication | tr -d '\r' | awk -F: '/^role:/{print $2; exit}' | tr -d '[:space:]'
}

pod_has_role() {
  local pod=$1
  local expected_role=$2
  local current_role
  current_role=$(redis_role_of_pod "${pod}" 2>/dev/null || true)
  [[ "${current_role}" == "${expected_role}" ]]
}

wait_for_pod_role() {
  local pod=$1
  local expected_role=$2
  local timeout_seconds=$3
  retry_until "pod ${pod} role ${expected_role}" "${timeout_seconds}" 3 pod_has_role "${pod}" "${expected_role}" || fail "pod ${pod} did not become role ${expected_role}"
}

offset_of_pod() {
  local pod=$1
  local offset
  offset=$(exec_redis_on_pod "${pod}" INFO replication | tr -d '\r' | awk -F: '/^(master_repl_offset|slave_repl_offset):/ {print $2; exit}' | tr -d '[:space:]')
  if [[ -z "${offset}" ]]; then
    fail "could not read replication offset for pod ${pod}"
  fi
  printf "%s\n" "${offset}"
}

assert_offset_not_regressed() {
  local before=$1
  local after=$2
  if (( after < before )); then
    fail "replication offset regressed: before=${before}, after=${after}"
  fi
}

assert_no_split_brain() {
  local labeled_primary_count
  labeled_primary_count=$(kubectl get pods -n "${TEST_NS}" -l "redis.io/cluster=${REDIS_CLUSTER_NAME},redis.io/role=primary" --no-headers 2>/dev/null | wc -l | tr -d ' ')
  if [[ "${labeled_primary_count}" != "1" ]]; then
    fail "expected exactly one pod labeled primary, found ${labeled_primary_count}"
  fi

  local master_count=0
  local pod
  while IFS= read -r pod; do
    [[ -z "${pod}" ]] && continue
    local role
    role=$(redis_role_of_pod "${pod}")
    if [[ "${role}" == "master" ]]; then
      master_count=$((master_count + 1))
    fi
  done < <(get_cluster_pods)

  if [[ "${master_count}" != "1" ]]; then
    fail "expected exactly one Redis master role, found ${master_count}"
  fi

  local status_primary
  local status_primary_role
  status_primary=$(cluster_current_primary)
  status_primary_role=$(redis_role_of_pod "${status_primary}")
  [[ "${status_primary_role}" == "master" ]] || fail "status.currentPrimary (${status_primary}) is not Redis master (role=${status_primary_role})"
}

pod_restart_count() {
  local pod=$1
  kubectl get pod "${pod}" -n "${TEST_NS}" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || echo 0
}

wait_for_restart_count_increase() {
  local pod=$1
  local before=$2
  local timeout_seconds=$3
  local start=$SECONDS

  while true; do
    local now
    now=$(pod_restart_count "${pod}")
    if [[ -n "${now}" && "${now}" =~ ^[0-9]+$ ]] && (( now > before )); then
      return 0
    fi
    if (( SECONDS - start >= timeout_seconds )); then
      fail "restart count for ${pod} did not increase from ${before}"
    fi
    sleep 2
  done
}

remove_network_blocker() {
  if [[ -z "${NETWORK_BLOCKER_POD}" ]]; then
    return 0
  fi

  if kubectl get pod "${NETWORK_BLOCKER_POD}" -n "${TEST_NS}" >/dev/null 2>&1; then
    if [[ -n "${NETWORK_BLOCK_TARGET_IP}" && -n "${NETWORK_BLOCK_SOURCE_IP}" ]]; then
      kubectl exec -n "${TEST_NS}" "${NETWORK_BLOCKER_POD}" -- sh -ceu "
iptables -D FORWARD -s ${NETWORK_BLOCK_SOURCE_IP} -d ${NETWORK_BLOCK_TARGET_IP} -p tcp --dport 8080 -j DROP || true
iptables -D OUTPUT -s ${NETWORK_BLOCK_SOURCE_IP} -d ${NETWORK_BLOCK_TARGET_IP} -p tcp --dport 8080 -j DROP || true
" >/dev/null 2>&1 || true
    fi
    kubectl delete pod "${NETWORK_BLOCKER_POD}" -n "${TEST_NS}" --ignore-not-found=true --wait=true >/dev/null 2>&1 || true
  fi

  NETWORK_BLOCKER_POD=""
  NETWORK_BLOCK_TARGET_IP=""
  NETWORK_BLOCK_SOURCE_IP=""
}

apply_network_blocker() {
  local source_ip=$1
  local target_ip=$2
  local host_node=$3

  NETWORK_BLOCK_SOURCE_IP="${source_ip}"
  NETWORK_BLOCK_TARGET_IP="${target_ip}"
  NETWORK_BLOCKER_POD="${REDIS_CLUSTER_NAME}-netblock"

  cat <<YAML | kubectl apply -n "${TEST_NS}" -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${NETWORK_BLOCKER_POD}
  labels:
    app.kubernetes.io/name: redis-chaos-netblock
spec:
  restartPolicy: Never
  nodeName: ${host_node}
  hostNetwork: true
  hostPID: true
  tolerations:
    - operator: Exists
  containers:
    - name: blocker
      image: ${NETWORK_BLOCKER_IMAGE}
      securityContext:
        privileged: true
      command:
        - /bin/sh
        - -ceu
        - |
          # Block only operator->primary status traffic. This avoids affecting kubelet probes
          # or unrelated node-originated traffic to the primary.
          iptables -I FORWARD 1 -s ${source_ip} -d ${target_ip} -p tcp --dport 8080 -j DROP
          iptables -I OUTPUT 1 -s ${source_ip} -d ${target_ip} -p tcp --dport 8080 -j DROP
          trap 'iptables -D FORWARD -s ${source_ip} -d ${target_ip} -p tcp --dport 8080 -j DROP || true; iptables -D OUTPUT -s ${source_ip} -d ${target_ip} -p tcp --dport 8080 -j DROP || true' EXIT
          sleep 3600
YAML

  kubectl wait --for=condition=Ready "pod/${NETWORK_BLOCKER_POD}" -n "${TEST_NS}" --timeout=120s >/dev/null
}

apply_primary_isolation_blocker() {
  local source_ip=$1
  local host_node=$2
  local api_server_ip=$3
  shift 3
  local peer_ips=("$@")
  local peer_ips_flat=""
  local peer
  for peer in "${peer_ips[@]}"; do
    [[ -z "${peer}" ]] && continue
    peer_ips_flat+="${peer} "
  done
  peer_ips_flat="${peer_ips_flat% }"

  NETWORK_BLOCK_SOURCE_IP=""
  NETWORK_BLOCK_TARGET_IP=""
  NETWORK_BLOCKER_POD="${REDIS_CLUSTER_NAME}-primary-isolation-netblock"

  cat <<YAML | kubectl apply -n "${TEST_NS}" -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${NETWORK_BLOCKER_POD}
  labels:
    app.kubernetes.io/name: redis-chaos-netblock
spec:
  restartPolicy: Never
  nodeName: ${host_node}
  hostNetwork: true
  hostPID: true
  tolerations:
    - operator: Exists
  containers:
    - name: blocker
      image: ${NETWORK_BLOCKER_IMAGE}
      securityContext:
        privileged: true
      command:
        - /bin/sh
        - -ceu
        - |
          SOURCE_IP="${source_ip}"
          API_SERVER_IP="${api_server_ip}"
          PEER_IPS="${peer_ips_flat}"

          for peer in ${peer_ips_flat}; do
            iptables -I FORWARD 1 -s "${source_ip}" -d "${peer}" -p tcp --dport 8080 -j DROP
            iptables -I OUTPUT 1 -s "${source_ip}" -d "${peer}" -p tcp --dport 8080 -j DROP
          done

          iptables -I FORWARD 1 -s "${source_ip}" -d "${api_server_ip}" -p tcp --dport 443 -j DROP
          iptables -I OUTPUT 1 -s "${source_ip}" -d "${api_server_ip}" -p tcp --dport 443 -j DROP
          iptables -I FORWARD 1 -s "${source_ip}" -d "${api_server_ip}" -p tcp --dport 6443 -j DROP
          iptables -I OUTPUT 1 -s "${source_ip}" -d "${api_server_ip}" -p tcp --dport 6443 -j DROP

          cleanup() {
            for peer in ${PEER_IPS}; do
              iptables -D FORWARD -s "${SOURCE_IP}" -d "${peer}" -p tcp --dport 8080 -j DROP || true
              iptables -D OUTPUT -s "${SOURCE_IP}" -d "${peer}" -p tcp --dport 8080 -j DROP || true
            done
            iptables -D FORWARD -s "${SOURCE_IP}" -d "${API_SERVER_IP}" -p tcp --dport 443 -j DROP || true
            iptables -D OUTPUT -s "${SOURCE_IP}" -d "${API_SERVER_IP}" -p tcp --dport 443 -j DROP || true
            iptables -D FORWARD -s "${SOURCE_IP}" -d "${API_SERVER_IP}" -p tcp --dport 6443 -j DROP || true
            iptables -D OUTPUT -s "${SOURCE_IP}" -d "${API_SERVER_IP}" -p tcp --dport 6443 -j DROP || true
          }
          trap cleanup EXIT
          sleep 3600
YAML

  kubectl wait --for=condition=Ready "pod/${NETWORK_BLOCKER_POD}" -n "${TEST_NS}" --timeout=120s >/dev/null
}

get_operator_pod() {
  local pod
  pod=$(kubectl get pods -n "${OPERATOR_NS}" -l "app.kubernetes.io/name=redis-operator,app.kubernetes.io/instance=${RELEASE_NAME}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [[ -z "${pod}" ]]; then
    pod=$(kubectl get pods -n "${OPERATOR_NS}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  fi
  printf "%s\n" "${pod}"
}

assert_pod_not_writable() {
  local pod=$1
  local key=$2
  local output
  output=$(exec_redis_on_pod "${pod}" SET "${key}" blocked 2>&1 || true)
  output=$(printf "%s" "${output}" | tr -d '\r')
  if [[ "${output}" == "OK" ]]; then
    fail "isolated former primary ${pod} still accepted writes directly"
  fi
}

perform_manual_rolling_restart() {
  local workload_pid=${1:-}

  local replicas_sorted
  replicas_sorted=$(get_replica_pods | sed '/^$/d' | sort -Vr)

  if [[ -n "${workload_pid}" ]] && ! kill -0 "${workload_pid}" 2>/dev/null; then
    fail "workload finished before rolling restart began"
  fi

  local replica
  while IFS= read -r replica; do
    [[ -z "${replica}" ]] && continue
    if [[ -n "${workload_pid}" ]] && ! kill -0 "${workload_pid}" 2>/dev/null; then
      fail "workload ended before restarting replica ${replica}"
    fi
    log "Rolling restart replica ${replica}"
    kubectl delete pod "${replica}" -n "${TEST_NS}" --wait=false >/dev/null
    wait_for_pod_ready "${replica}" 240
    wait_for_phase "Healthy" 240
  done <<<"${replicas_sorted}"

  local primary
  primary=$(get_primary_pod)
  if [[ -n "${workload_pid}" ]] && ! kill -0 "${workload_pid}" 2>/dev/null; then
    fail "workload ended before restarting primary ${primary}"
  fi
  log "Rolling restart primary ${primary}"
  kubectl delete pod "${primary}" -n "${TEST_NS}" --wait=false >/dev/null
  wait_for_pod_ready "${primary}" 240
  wait_for_phase "Healthy" 240
}

test_primary_kill() {
  log "AC1: primary kill during active write workload"
  wait_for_cluster_healthy 240
  refresh_password
  flush_cluster_data

  local prefix="ac1"
  write_keys "${prefix}" 1000

  local writer_log
  writer_log=$(mktemp)
  local writer_pod
  writer_pod=$(get_any_cluster_pod)
  kubectl exec -n "${TEST_NS}" "${writer_pod}" -- env REDISCLI_AUTH="${PASSWORD}" sh -ceu "
i=1
while [ \$i -le 50000 ]; do
  redis-cli --no-auth-warning -h ${REDIS_CLUSTER_NAME}-leader SET ${prefix}:inflight:\$i inflight-\$i >/dev/null 2>&1 || true
  i=\$((i+1))
done
" >"${writer_log}" 2>&1 &
  local writer_pid=$!

  sleep 1
  if ! kill -0 "${writer_pid}" 2>/dev/null; then
    cat "${writer_log}" >&2
    fail "in-flight write workload finished before primary deletion"
  fi

  local old_primary
  old_primary=$(get_primary_pod)
  wait_for_replication "${old_primary}" 1 5000
  local offset_before
  offset_before=$(offset_of_pod "${old_primary}")

  kubectl delete pod "${old_primary}" -n "${TEST_NS}" --wait=false >/dev/null

  local writer_exit=0
  if ! wait "${writer_pid}"; then
    writer_exit=$?
  fi
  if [[ ${writer_exit} -ne 0 ]]; then
    cat "${writer_log}" >&2
    fail "in-flight write workload exited non-zero (${writer_exit})"
  fi
  rm -f "${writer_log}"

  local new_primary
  new_primary=$(wait_for_primary_change "${old_primary}" 180)
  wait_for_phase "Healthy" 240
  wait_for_pod_ready "${old_primary}" 240
  wait_for_pod_role "${old_primary}" "slave" 180

  assert_keys "${prefix}" 1000
  local offset_after
  offset_after=$(offset_of_pod "${new_primary}")
  assert_offset_not_regressed "${offset_before}" "${offset_after}"
  assert_no_split_brain
}

test_network_partition() {
  log "AC2: network partition of primary status endpoint"
  wait_for_cluster_healthy 240
  refresh_password
  flush_cluster_data

  local prefix="ac2"
  write_keys "${prefix}" 1000

  local old_primary
  old_primary=$(get_primary_pod)
  wait_for_replication "${old_primary}" 1 5000
  local offset_before
  offset_before=$(offset_of_pod "${old_primary}")

  local primary_ip
  local operator_pod
  local operator_ip
  local operator_node
  primary_ip=$(kubectl get pod "${old_primary}" -n "${TEST_NS}" -o jsonpath='{.status.podIP}')
  operator_pod=$(get_operator_pod)
  [[ -n "${operator_pod}" ]] || fail "could not identify operator pod for network partition scenario"
  operator_ip=$(kubectl get pod "${operator_pod}" -n "${OPERATOR_NS}" -o jsonpath='{.status.podIP}')
  operator_node=$(kubectl get pod "${operator_pod}" -n "${OPERATOR_NS}" -o jsonpath='{.spec.nodeName}')

  apply_network_blocker "${operator_ip}" "${primary_ip}" "${operator_node}"

  local new_primary
  new_primary=$(wait_for_primary_change "${old_primary}" 240)
  wait_for_phase "Healthy" 240

  local leader_targets
  leader_targets=$(kubectl get endpoints "${REDIS_CLUSTER_NAME}-leader" -n "${TEST_NS}" -o jsonpath='{.subsets[*].addresses[*].targetRef.name}')
  if [[ "${leader_targets}" == *"${old_primary}"* ]]; then
    fail "leader service still points to isolated former primary ${old_primary}"
  fi

  assert_pod_not_writable "${old_primary}" "${prefix}:isolated-write"

  assert_keys "${prefix}" 1000
  local offset_after
  offset_after=$(offset_of_pod "${new_primary}")
  assert_offset_not_regressed "${offset_before}" "${offset_after}"

  remove_network_blocker
  wait_for_pod_ready "${old_primary}" 240
  wait_for_pod_role "${old_primary}" "slave" 180
  assert_no_split_brain
}

test_primary_isolation() {
  log "AC-primary-isolation: primary self-kills on full isolation"
  wait_for_cluster_healthy 240
  refresh_password
  flush_cluster_data

  local prefix="ac-primary-isolation"
  write_keys "${prefix}" 1000

  local old_primary
  old_primary=$(get_primary_pod)
  wait_for_replication "${old_primary}" 1 5000
  local offset_before
  offset_before=$(offset_of_pod "${old_primary}")
  local restart_before
  restart_before=$(pod_restart_count "${old_primary}")

  local primary_ip
  local primary_node
  local api_server_ip
  primary_ip=$(kubectl get pod "${old_primary}" -n "${TEST_NS}" -o jsonpath='{.status.podIP}')
  primary_node=$(kubectl get pod "${old_primary}" -n "${TEST_NS}" -o jsonpath='{.spec.nodeName}')
  api_server_ip=$(kubectl get svc kubernetes -n default -o jsonpath='{.spec.clusterIP}')
  [[ -n "${api_server_ip}" ]] || fail "could not determine Kubernetes API service IP"

  local peers_raw
  peers_raw=$(get_cluster_pods | sed '/^$/d' | grep -v "^${old_primary}$" || true)
  [[ -n "${peers_raw}" ]] || fail "expected at least one peer pod for isolation scenario"

  local peer_ips=()
  local peer
  while IFS= read -r peer; do
    [[ -z "${peer}" ]] && continue
    peer_ips+=("$(kubectl get pod "${peer}" -n "${TEST_NS}" -o jsonpath='{.status.podIP}')")
  done <<<"${peers_raw}"

  apply_primary_isolation_blocker "${primary_ip}" "${primary_node}" "${api_server_ip}" "${peer_ips[@]}"

  wait_for_restart_count_increase "${old_primary}" "${restart_before}" 240

  local new_primary
  new_primary=$(wait_for_primary_change "${old_primary}" 240)
  wait_for_phase "Healthy" 240

  remove_network_blocker
  wait_for_pod_ready "${old_primary}" 240
  wait_for_pod_role "${old_primary}" "slave" 180

  assert_keys "${prefix}" 1000
  local offset_after
  offset_after=$(offset_of_pod "${new_primary}")
  assert_offset_not_regressed "${offset_before}" "${offset_after}"
  assert_no_split_brain
}

test_operator_restart_mid_failover() {
  log "AC3: operator restart during failover"
  wait_for_cluster_healthy 240
  refresh_password
  flush_cluster_data

  local prefix="ac3"
  write_keys "${prefix}" 500

  local old_primary
  old_primary=$(get_primary_pod)
  wait_for_replication "${old_primary}" 1 5000
  local offset_before
  offset_before=$(offset_of_pod "${old_primary}")

  kubectl delete pod "${old_primary}" -n "${TEST_NS}" --wait=false >/dev/null
  wait_for_fencing_annotation 180

  local operator_pod
  operator_pod=$(get_operator_pod)
  [[ -n "${operator_pod}" ]] || fail "could not identify operator pod"
  kubectl delete pod "${operator_pod}" -n "${OPERATOR_NS}" --wait=false >/dev/null

  kubectl rollout status deployment/"${RELEASE_NAME}" -n "${OPERATOR_NS}" --timeout=240s >/dev/null

  local new_primary
  new_primary=$(wait_for_primary_change "${old_primary}" 240)
  wait_for_phase "Healthy" 240
  wait_for_pod_ready "${old_primary}" 240
  wait_for_pod_role "${old_primary}" "slave" 180

  assert_keys "${prefix}" 500
  local offset_after
  offset_after=$(offset_of_pod "${new_primary}")
  assert_offset_not_regressed "${offset_before}" "${offset_after}"
  assert_no_split_brain
}

test_simultaneous_replica_failures() {
  log "AC4: simultaneous replica failures"
  wait_for_cluster_healthy 240
  refresh_password
  flush_cluster_data

  local prefix="ac4"
  write_keys "${prefix}" 200

  local primary
  primary=$(get_primary_pod)
  wait_for_replication "${primary}" 1 5000
  local offset_before
  offset_before=$(offset_of_pod "${primary}")

  local replicas_raw
  replicas_raw=$(get_replica_pods | sed '/^$/d')
  local replica_count
  replica_count=$(printf "%s\n" "${replicas_raw}" | sed '/^$/d' | wc -l | tr -d ' ')
  if (( replica_count < 2 )); then
    fail "expected at least 2 replicas, found ${replica_count}"
  fi
  local replica_args
  replica_args=$(printf "%s" "${replicas_raw}" | tr '\n' ' ')
  # shellcheck disable=SC2086
  kubectl delete pod ${replica_args} -n "${TEST_NS}" --wait=false >/dev/null

  local set_output
  set_output=$(exec_redis_on_pod "${primary}" SET "${prefix}:after-kill" yes | tr -d '\r')
  [[ "${set_output}" == "OK" ]] || fail "primary failed write after replica loss: ${set_output}"
  local get_output
  get_output=$(exec_redis_on_pod "${primary}" GET "${prefix}:after-kill" | tr -d '\r')
  [[ "${get_output}" == "yes" ]] || fail "primary read-after-write failed after replica loss: ${get_output}"

  wait_for_phase "Healthy" 300
  wait_for_cluster_healthy 300

  assert_keys "${prefix}" 200
  local offset_after
  offset_after=$(offset_of_pod "$(get_primary_pod)")
  assert_offset_not_regressed "${offset_before}" "${offset_after}"
  assert_no_split_brain
}

test_oom_injection() {
  log "AC5: simulate OOM/liveness restart by killing redis-server"
  wait_for_cluster_healthy 240
  refresh_password
  flush_cluster_data

  local prefix="ac5"
  write_keys "${prefix}" 300

  local primary
  primary=$(get_primary_pod)
  wait_for_replication "${primary}" 1 5000
  local offset_before
  offset_before=$(offset_of_pod "${primary}")
  local restart_before
  restart_before=$(pod_restart_count "${primary}")

  kubectl exec -n "${TEST_NS}" "${primary}" -- sh -ceu 'kill -9 "$(pgrep redis-server)"'

  wait_for_restart_count_increase "${primary}" "${restart_before}" 180
  wait_for_pod_ready "${primary}" 240
  wait_for_phase "Healthy" 240

  local current_primary
  current_primary=$(get_primary_pod)
  local offset_after
  offset_after=$(offset_of_pod "${current_primary}")
  assert_offset_not_regressed "${offset_before}" "${offset_after}"

  assert_keys "${prefix}" 300
  assert_no_split_brain
}

test_rolling_update_under_load() {
  log "AC6: rolling update under redis-benchmark load"
  wait_for_cluster_healthy 240
  refresh_password
  flush_cluster_data

  local prefix="ac6"
  write_keys "${prefix}" 250

  local primary
  primary=$(get_primary_pod)
  wait_for_replication "${primary}" 1 5000
  local offset_before
  offset_before=$(offset_of_pod "${primary}")

  local client_pod
  client_pod=$(get_any_cluster_pod)
  local bench_file
  bench_file=$(mktemp)
  kubectl exec -n "${TEST_NS}" "${client_pod}" -- env REDISCLI_AUTH="${PASSWORD}" redis-benchmark --no-auth-warning -h "${REDIS_CLUSTER_NAME}-leader" -t set -n 500000 -c 10 -e --csv >"${bench_file}" 2>&1 &
  local bench_pid=$!

  sleep 1
  if ! kill -0 "${bench_pid}" 2>/dev/null; then
    cat "${bench_file}" >&2
    fail "redis-benchmark finished before rolling restart started"
  fi

  # Attempt a spec-driven rolling update trigger, then enforce a pod-by-pod restart.
  kubectl patch rediscluster/"${REDIS_CLUSTER_NAME}" -n "${TEST_NS}" --type merge -p '{"spec":{"resources":{"requests":{"cpu":"150m","memory":"128Mi"},"limits":{"cpu":"600m","memory":"256Mi"}}}}' >/dev/null
  perform_manual_rolling_restart "${bench_pid}"

  local bench_exit=0
  if ! wait "${bench_pid}"; then
    bench_exit=$?
  fi
  if [[ ${bench_exit} -ne 0 ]]; then
    cat "${bench_file}" >&2
    fail "redis-benchmark exited with non-zero status (${bench_exit}) during rolling update"
  fi
  if grep -Ei "(^|[^a-z])(error|err|failed)([^a-z]|$)" "${bench_file}" >/dev/null; then
    cat "${bench_file}" >&2
    fail "redis-benchmark output contains write errors during rolling update"
  fi

  wait_for_phase "Healthy" 300
  wait_for_cluster_healthy 300

  local offset_after
  offset_after=$(offset_of_pod "$(get_primary_pod)")
  assert_offset_not_regressed "${offset_before}" "${offset_after}"
  assert_keys "${prefix}" 250
  assert_no_split_brain

  rm -f "${bench_file}"
}

run_scenario() {
  local title=$1
  local fn=$2
  log "---- ${title} ----"
  "${fn}"
  log "PASS: ${title}"
}

main() {
  require kind
  require kubectl
  require helm
  require docker
  require grep
  require awk
  require sed

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

  cat <<YAML | kubectl apply -n "${TEST_NS}" -f -
apiVersion: redis.io/v1
kind: RedisCluster
metadata:
  name: ${REDIS_CLUSTER_NAME}
spec:
  instances: ${REDIS_INSTANCES}
  storage:
    size: 1Gi
YAML

  local ordinal
  for ((ordinal = 0; ordinal < REDIS_INSTANCES; ordinal++)); do
    kubectl wait --for=create "pod/${REDIS_CLUSTER_NAME}-${ordinal}" -n "${TEST_NS}" --timeout=300s >/dev/null
    kubectl wait --for=condition=Ready "pod/${REDIS_CLUSTER_NAME}-${ordinal}" -n "${TEST_NS}" --timeout=300s >/dev/null
  done

  wait_for_cluster_healthy 300
  refresh_password
  assert_no_split_brain

  run_scenario "AC1 primary kill" test_primary_kill
  run_scenario "AC2 network partition" test_network_partition
  run_scenario "AC-primary-isolation runtime guard" test_primary_isolation
  run_scenario "AC3 operator restart mid-failover" test_operator_restart_mid_failover
  run_scenario "AC4 simultaneous replica failures" test_simultaneous_replica_failures
  run_scenario "AC5 OOM/liveness pressure" test_oom_injection
  run_scenario "AC6 rolling update under load" test_rolling_update_under_load

  log "All chaos and fault injection scenarios passed"
}

main "$@"
