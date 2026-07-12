#!/usr/bin/env bash

set -euo pipefail

log() {
  printf '==> %s\n' "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
TMP_PARENT="${K8S_SMOKE_TMP_PARENT:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}}"
SMOKE_DIR="${K8S_SMOKE_DIR:-$(mktemp -d "${TMP_PARENT%/}/vekil-kind-smoke.XXXXXX")}"
if [[ "${SMOKE_DIR}" != /* ]]; then
  SMOKE_DIR="${PWD}/${SMOKE_DIR}"
fi

CLUSTER_NAME="${K8S_SMOKE_CLUSTER_NAME:-vekil-smoke-${GITHUB_RUN_ID:-local}-$$}"
NAMESPACE="${K8S_SMOKE_NAMESPACE:-vekil-smoke}"
IMAGE="${K8S_SMOKE_IMAGE:-vekil:k8s-smoke}"
LIVENESS_WINDOW_SECONDS="${K8S_SMOKE_LIVENESS_WINDOW_SECONDS:-15}"
DELETE_CLUSTER="${K8S_SMOKE_DELETE_CLUSTER:-1}"
KUBECTL_REQUEST_TIMEOUT_SECONDS="${K8S_SMOKE_KUBECTL_REQUEST_TIMEOUT_SECONDS:-10}"
K8S_COMMAND_TIMEOUT_SECONDS="${K8S_SMOKE_COMMAND_TIMEOUT_SECONDS:-60}"
K8S_BUILD_TIMEOUT_SECONDS="${K8S_SMOKE_BUILD_TIMEOUT_SECONDS:-600}"
K8S_CLUSTER_TIMEOUT_SECONDS="${K8S_SMOKE_CLUSTER_TIMEOUT_SECONDS:-180}"
K8S_ROLLOUT_TIMEOUT_SECONDS="${K8S_SMOKE_ROLLOUT_TIMEOUT_SECONDS:-120}"
PORT_FORWARD_STARTUP_TIMEOUT_SECONDS="${K8S_SMOKE_PORT_FORWARD_STARTUP_TIMEOUT_SECONDS:-20}"
CURL_CONNECT_TIMEOUT_SECONDS="${K8S_SMOKE_CURL_CONNECT_TIMEOUT_SECONDS:-2}"
CURL_MAX_TIME_SECONDS="${K8S_SMOKE_CURL_MAX_TIME_SECONDS:-5}"
PROCESS_TERM_GRACE_SECONDS="${K8S_SMOKE_PROCESS_TERM_GRACE_SECONDS:-5}"
PORT_RELEASE_TIMEOUT_SECONDS="${K8S_SMOKE_PORT_RELEASE_TIMEOUT_SECONDS:-5}"

[[ "${NAMESPACE}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || die "invalid namespace: ${NAMESPACE}"
[[ "${IMAGE}" =~ ^[A-Za-z0-9._/@:-]+$ ]] || die "unsupported image reference: ${IMAGE}"

python_command() {
  if command -v python3 >/dev/null 2>&1; then
    command -v python3
    return
  fi
  if command -v python >/dev/null 2>&1; then
    command -v python
    return
  fi
  die "python3 (or python) is required to allocate and verify the port-forward port"
}

allocate_free_port() {
  local python_bin
  python_bin="$(python_command)"
  "${python_bin}" - <<'PY_PORT'
import socket
for _ in range(20):
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    if port != 1337:
        print(port)
        raise SystemExit(0)
raise SystemExit("unable to allocate a non-default port")
PY_PORT
}

if [[ ${K8S_SMOKE_LOCAL_PORT+x} == x ]]; then
  LOCAL_PORT="${K8S_SMOKE_LOCAL_PORT}"
else
  LOCAL_PORT="$(allocate_free_port)"
fi
[[ "${LOCAL_PORT}" =~ ^[0-9]+$ ]] || die "K8S_SMOKE_LOCAL_PORT must be numeric: ${LOCAL_PORT}"

KUBECONFIG="${SMOKE_DIR}/kubeconfig"
export KUBECONFIG
PORT_FORWARD_LOG="${SMOKE_DIR}/port-forward.log"
RENDER_DIR="${SMOKE_DIR}/manifest"
RENDERED_MANIFEST="${SMOKE_DIR}/vekil-rendered.yaml"
PROVIDERS_CONFIG_FILE="${SMOKE_DIR}/providers.yaml"
DEPLOYMENT_PATCH_FILE="${SMOKE_DIR}/deployment-provider-patch.yaml"

active_pid=""
active_pgid=""
port_forward_pid=""
port_forward_pgid=""
port_forward_listen_confirmed=0
cluster_created=0
cluster_delete_needed=0
LAST_PROCESS_PID=""
LAST_PROCESS_PGID=""
CURRENT_POD=""

redact_device_codes() {
  sed -E 's/(enter code: )[A-Z0-9-]+/\1[REDACTED_DEVICE_CODE]/g'
}

process_is_running() {
  local pid="$1"
  local state
  kill -0 "${pid}" 2>/dev/null || return 1
  state="$(ps -o stat= -p "${pid}" 2>/dev/null | awk 'NR == 1 { print $1 }')"
  [[ "${state}" != Z* ]]
}

process_group_is_alive() {
  local pgid="$1"
  [[ -n "${pgid}" ]] || return 1
  kill -0 -- "-${pgid}" 2>/dev/null
}

start_process_group() {
  set -m
  "$@" &
  LAST_PROCESS_PID="$!"
  LAST_PROCESS_PGID="${LAST_PROCESS_PID}"
  set +m
}

terminate_process_group() {
  local pid="$1"
  local pgid="$2"
  local deadline
  if process_group_is_alive "${pgid}"; then
    kill -TERM -- "-${pgid}" 2>/dev/null || true
    deadline=$((SECONDS + PROCESS_TERM_GRACE_SECONDS))
    while process_group_is_alive "${pgid}" && (( SECONDS < deadline )); do
      sleep 0.1
    done
    if process_group_is_alive "${pgid}"; then
      kill -KILL -- "-${pgid}" 2>/dev/null || true
    fi
  elif [[ -n "${pid}" ]] && process_is_running "${pid}"; then
    kill -TERM "${pid}" 2>/dev/null || true
  fi
  [[ -z "${pid}" ]] || wait "${pid}" 2>/dev/null || true
}

run_with_deadline() {
  local timeout_seconds="$1"
  local label="$2"
  shift 2
  local deadline rc pid pgid

  start_process_group "$@"
  pid="${LAST_PROCESS_PID}"
  pgid="${LAST_PROCESS_PGID}"
  active_pid="${pid}"
  active_pgid="${pgid}"
  deadline=$((SECONDS + timeout_seconds))

  while process_is_running "${pid}"; do
    if (( SECONDS >= deadline )); then
      log "${label} exceeded ${timeout_seconds}s deadline; terminating process group ${pgid}"
      terminate_process_group "${pid}" "${pgid}"
      active_pid=""
      active_pgid=""
      return 124
    fi
    sleep 0.1
  done

  if wait "${pid}"; then
    rc=0
  else
    rc=$?
  fi
  if process_group_is_alive "${pgid}"; then
    terminate_process_group "" "${pgid}"
  fi
  active_pid=""
  active_pgid=""
  return "${rc}"
}

kube() {
  kubectl --request-timeout="${KUBECTL_REQUEST_TIMEOUT_SECONDS}s" "$@"
}

port_is_open() {
  local python_bin
  python_bin="$(python_command)"
  "${python_bin}" - "${LOCAL_PORT}" <<'PY_CONNECT' >/dev/null 2>&1
import socket
import sys
try:
    with socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=0.2):
        pass
except OSError:
    raise SystemExit(1)
PY_CONNECT
}

wait_for_port_release() {
  local deadline=$((SECONDS + PORT_RELEASE_TIMEOUT_SECONDS))
  while (( SECONDS < deadline )); do
    if ! port_is_open; then
      return 0
    fi
    sleep 0.1
  done
  ! port_is_open
}

port_forward_log_has_expected_listener() {
  [[ -f "${PORT_FORWARD_LOG}" ]] || return 1
  grep -Fqx "Forwarding from 127.0.0.1:${LOCAL_PORT} -> 1337" "${PORT_FORWARD_LOG}"
}

port_forward_log_has_error() {
  [[ -f "${PORT_FORWARD_LOG}" ]] || return 1
  grep -Eqi '(^|[[:space:]])error:|unable to listen|address already in use|lost connection' "${PORT_FORWARD_LOG}"
}

assert_port_forward_alive() {
  if port_forward_log_has_error; then
    cat "${PORT_FORWARD_LOG}" >&2 || true
    die "kubectl port-forward logged an error"
  fi
  if ! process_is_running "${port_forward_pid}"; then
    cat "${PORT_FORWARD_LOG}" >&2 || true
    die "kubectl port-forward PID ${port_forward_pid} exited"
  fi
}

stop_port_forward() {
  local release_failed=0
  if [[ -n "${port_forward_pgid}" ]]; then
    terminate_process_group "${port_forward_pid}" "${port_forward_pgid}"
  fi
  if [[ "${port_forward_listen_confirmed}" == "1" ]] && ! wait_for_port_release; then
    release_failed=1
  fi
  port_forward_pid=""
  port_forward_pgid=""
  port_forward_listen_confirmed=0
  [[ "${release_failed}" == "0" ]]
}

dump_debug() {
  [[ "${cluster_created}" == "1" ]] || return 0
  log "Kubernetes smoke debug output"
  run_with_deadline 15 "kubectl get all diagnostics" kube -n "${NAMESPACE}" get all -o wide >&2 || true
  run_with_deadline 15 "kubectl describe diagnostics" kube -n "${NAMESPACE}" describe pod -l app=vekil >&2 || true
  if run_with_deadline 15 "kubectl logs diagnostics" kube -n "${NAMESPACE}" logs -l app=vekil --all-containers --prefix >"${SMOKE_DIR}/pod.log" 2>&1; then
    redact_device_codes <"${SMOKE_DIR}/pod.log" >&2
  elif [[ -f "${SMOKE_DIR}/pod.log" ]]; then
    redact_device_codes <"${SMOKE_DIR}/pod.log" >&2
  fi
  if [[ -f "${PORT_FORWARD_LOG}" ]]; then
    log "kubectl port-forward log"
    cat "${PORT_FORWARD_LOG}" >&2 || true
  fi
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM

  if [[ -n "${active_pgid}" ]]; then
    terminate_process_group "${active_pid}" "${active_pgid}"
    active_pid=""
    active_pgid=""
  fi
  if ! stop_port_forward; then
    printf 'error: kubectl port-forward cleanup did not release 127.0.0.1:%s\n' "${LOCAL_PORT}" >&2
    rc=1
  fi
  if [[ "${rc}" -ne 0 ]]; then
    dump_debug
  fi
  if [[ "${DELETE_CLUSTER}" != "0" && "${cluster_delete_needed}" == "1" ]]; then
    run_with_deadline "${K8S_CLUSTER_TIMEOUT_SECONDS}" "kind cluster deletion" \
      kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || rc=1
  elif [[ "${cluster_delete_needed}" == "1" ]]; then
    log "Preserving cluster ${CLUSTER_NAME}; scoped kubeconfig: ${KUBECONFIG}"
  fi
  exit "${rc}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

render_manifest() {
  mkdir -p "${RENDER_DIR}"
  cp "${REPO_ROOT}/k8s/vekil.yaml" "${RENDER_DIR}/vekil.yaml"
  cat > "${RENDER_DIR}/kustomization.yaml" <<EOF_KUSTOMIZE
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - vekil.yaml
namespace: ${NAMESPACE}
patches:
  - target:
      group: apps
      version: v1
      kind: Deployment
      name: vekil
    patch: |-
      - op: replace
        path: /spec/template/spec/containers/0/image
        value: ${IMAGE}
      - op: replace
        path: /spec/template/spec/containers/0/imagePullPolicy
        value: IfNotPresent
EOF_KUSTOMIZE

  local pull_policy_path_count pull_policy_value_count rendered_pull_policy_count
  pull_policy_path_count="$(grep -cF 'path: /spec/template/spec/containers/0/imagePullPolicy' "${RENDER_DIR}/kustomization.yaml" || true)"
  pull_policy_value_count="$(grep -cF 'value: IfNotPresent' "${RENDER_DIR}/kustomization.yaml" || true)"
  if [[ "${pull_policy_path_count}" != "1" || "${pull_policy_value_count}" != "1" ]]; then
    die "render overlay must contain exactly one imagePullPolicy=IfNotPresent patch"
  fi

  run_with_deadline "${K8S_COMMAND_TIMEOUT_SECONDS}" "render checked-in Kubernetes manifest" \
    kubectl kustomize "${RENDER_DIR}" >"${RENDERED_MANIFEST}" \
    || die "failed to render k8s/vekil.yaml"

  rendered_pull_policy_count="$(grep -cE '^[[:space:]]+imagePullPolicy: IfNotPresent$' "${RENDERED_MANIFEST}" || true)"
  [[ "${rendered_pull_policy_count}" == "1" ]] \
    || die "rendered manifest must contain exactly one imagePullPolicy: IfNotPresent"
}

wait_for_running_pod() {
  local deadline=$((SECONDS + K8S_ROLLOUT_TIMEOUT_SECONDS))
  local pods_json="${SMOKE_DIR}/pods.json"
  local phase running_since
  CURRENT_POD=""

  while (( SECONDS < deadline )); do
    if run_with_deadline 15 "get Vekil pod" \
      kube -n "${NAMESPACE}" get pods -l app=vekil -o json >"${pods_json}" 2>/dev/null; then
      CURRENT_POD="$(jq -r '.items | sort_by(.metadata.creationTimestamp) | last | .metadata.name // ""' "${pods_json}")"
      phase="$(jq -r '.items | sort_by(.metadata.creationTimestamp) | last | .status.phase // ""' "${pods_json}")"
      running_since="$(jq -r '.items | sort_by(.metadata.creationTimestamp) | last | [.status.containerStatuses[]? | select(.name == "vekil") | .state.running.startedAt] | first // ""' "${pods_json}")"
      if [[ -n "${CURRENT_POD}" && "${phase}" == "Running" && -n "${running_since}" ]]; then
        return 0
      fi
    fi
    sleep 1
  done
  return 1
}

start_port_forward() {
  local pod="$1"
  rm -f "${PORT_FORWARD_LOG}"
  log "Port-forwarding ${pod} to 127.0.0.1:${LOCAL_PORT}"
  set -m
  kubectl -n "${NAMESPACE}" port-forward --address 127.0.0.1 "pod/${pod}" "${LOCAL_PORT}:1337" \
    >"${PORT_FORWARD_LOG}" 2>&1 &
  port_forward_pid="$!"
  port_forward_pgid="${port_forward_pid}"
  port_forward_listen_confirmed=0
  set +m
}

wait_for_port_forward_health() {
  local deadline=$((SECONDS + PORT_FORWARD_STARTUP_TIMEOUT_SECONDS))
  local health_file="${SMOKE_DIR}/healthz.json"
  while (( SECONDS < deadline )); do
    assert_port_forward_alive
    if [[ "${port_forward_listen_confirmed}" == "0" ]]; then
      if port_forward_log_has_expected_listener; then
        port_forward_listen_confirmed=1
      else
        sleep 0.1
        continue
      fi
    fi
    if curl --fail --silent --show-error \
      --connect-timeout "${CURL_CONNECT_TIMEOUT_SECONDS}" \
      --max-time "${CURL_MAX_TIME_SECONDS}" \
      "http://127.0.0.1:${LOCAL_PORT}/healthz" >"${health_file}" 2>/dev/null; then
      assert_port_forward_alive
      port_forward_log_has_expected_listener || die "port-forward never logged the expected local listener"
      jq -e '.status == "ok"' "${health_file}" >/dev/null \
        || die "unexpected /healthz response: $(cat "${health_file}" 2>/dev/null || true)"
      return 0
    fi
    sleep 0.2
  done
  cat "${PORT_FORWARD_LOG}" >&2 || true
  die "kubectl port-forward never served /healthz within ${PORT_FORWARD_STARTUP_TIMEOUT_SECONDS}s"
}

request_status() {
  local path="$1"
  local body_file="$2"
  curl --silent --show-error \
    --output "${body_file}" \
    --write-out '%{http_code}' \
    --connect-timeout "${CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${CURL_MAX_TIME_SECONDS}" \
    "http://127.0.0.1:${LOCAL_PORT}${path}"
}

assert_auth_pending() {
  local ready_body="${SMOKE_DIR}/readyz-auth-pending.json"
  local ready_code
  ready_code="$(request_status /readyz "${ready_body}")" || die "auth-pending /readyz request failed"
  [[ "${ready_code}" == "503" ]] \
    || die "expected /readyz HTTP 503 while startup auth is pending, got ${ready_code}: $(cat "${ready_body}" 2>/dev/null || true)"
  jq -e '.status == "not_ready"' "${ready_body}" >/dev/null \
    || die "expected /readyz not_ready body: $(cat "${ready_body}" 2>/dev/null || true)"

  local pod_json="${SMOKE_DIR}/auth-pending-pod.json"
  run_with_deadline 15 "read auth-pending pod status" \
    kube -n "${NAMESPACE}" get pod "${CURRENT_POD}" -o json >"${pod_json}" \
    || die "failed to read auth-pending pod"
  if jq -e '.status.conditions[]? | select(.type == "Ready" and .status == "True")' "${pod_json}" >/dev/null; then
    die "expected auth-pending pod to remain not Ready"
  fi

  local endpoints_json="${SMOKE_DIR}/auth-pending-endpoints.json"
  run_with_deadline 15 "read auth-pending Service endpoints" \
    kube -n "${NAMESPACE}" get endpoints vekil -o json >"${endpoints_json}" \
    || die "failed to read Vekil Service endpoints"
  jq -e '[.subsets[]?.addresses[]?] | length == 0' "${endpoints_json}" >/dev/null \
    || die "expected Service to expose no ready endpoint while auth is pending"
}

verify_liveness_window() {
  local deadline=$((SECONDS + LIVENESS_WINDOW_SECONDS))
  local health_file="${SMOKE_DIR}/healthz-window.json"
  log "Checking /healthz throughout a ${LIVENESS_WINDOW_SECONDS}s liveness window"
  while (( SECONDS < deadline )); do
    assert_port_forward_alive
    curl --fail --silent --show-error \
      --connect-timeout "${CURL_CONNECT_TIMEOUT_SECONDS}" \
      --max-time "${CURL_MAX_TIME_SECONDS}" \
      "http://127.0.0.1:${LOCAL_PORT}/healthz" >"${health_file}" \
      || die "/healthz failed during the liveness window"
    jq -e '.status == "ok"' "${health_file}" >/dev/null \
      || die "invalid /healthz body during liveness window"
    sleep 2
  done

  local pod_json="${SMOKE_DIR}/auth-pending-pod-after-window.json"
  run_with_deadline 15 "read liveness restart count" \
    kube -n "${NAMESPACE}" get pod "${CURRENT_POD}" -o json >"${pod_json}" \
    || die "failed to read restart count"
  local restarts
  restarts="$(jq -r '[.status.containerStatuses[]? | select(.name == "vekil") | .restartCount] | first // -1' "${pod_json}")"
  [[ "${restarts}" == "0" ]] || die "expected zero liveness restarts while auth is pending, got ${restarts}"
}

configure_static_provider() {
  cat > "${PROVIDERS_CONFIG_FILE}" <<'EOF_PROVIDERS'
providers:
  - id: kind-smoke
    type: openai-compatible
    base_url: http://127.0.0.1:1/v1
    auth_type: none
    model_discovery: static
    models:
      - public_id: kind-smoke-model
        endpoints:
          - /chat/completions
EOF_PROVIDERS

  run_with_deadline 20 "render provider ConfigMap" \
    kube -n "${NAMESPACE}" create configmap vekil-providers \
      --from-file="providers.yaml=${PROVIDERS_CONFIG_FILE}" \
      --dry-run=client -o yaml >"${SMOKE_DIR}/providers-configmap.yaml" \
    || die "failed to render provider ConfigMap"
  run_with_deadline 30 "apply provider ConfigMap" \
    kube apply -f "${SMOKE_DIR}/providers-configmap.yaml" \
    || die "failed to apply provider ConfigMap"

  cat > "${DEPLOYMENT_PATCH_FILE}" <<'EOF_PATCH'
spec:
  template:
    spec:
      containers:
        - name: vekil
          args:
            - --providers-config
            - /etc/vekil/providers.yaml
          volumeMounts:
            - name: providers-config
              mountPath: /etc/vekil
              readOnly: true
      volumes:
        - name: providers-config
          configMap:
            name: vekil-providers
EOF_PATCH

  run_with_deadline 30 "patch Vekil with deterministic provider config" \
    kube -n "${NAMESPACE}" patch deployment vekil --type strategic --patch-file "${DEPLOYMENT_PATCH_FILE}" \
    || die "failed to patch Vekil provider config"
  run_with_deadline "${K8S_ROLLOUT_TIMEOUT_SECONDS}" "wait for configured-provider rollout" \
    kubectl --request-timeout=0 -n "${NAMESPACE}" rollout status deployment/vekil \
      --timeout="${K8S_ROLLOUT_TIMEOUT_SECONDS}s" \
    || die "configured-provider deployment did not become Ready"
}

assert_configured_ready() {
  local ready_body="${SMOKE_DIR}/readyz-configured.json"
  local models_body="${SMOKE_DIR}/models-configured.json"
  local ready_code
  ready_code="$(request_status /readyz "${ready_body}")" || die "configured /readyz request failed"
  [[ "${ready_code}" == "200" ]] || die "configured /readyz returned ${ready_code}: $(cat "${ready_body}" 2>/dev/null || true)"
  jq -e '.status == "ready"' "${ready_body}" >/dev/null || die "configured /readyz did not report ready"

  curl --fail --silent --show-error \
    --connect-timeout "${CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${CURL_MAX_TIME_SECONDS}" \
    "http://127.0.0.1:${LOCAL_PORT}/v1/models" >"${models_body}" \
    || die "configured /v1/models request failed"
  jq -e '.data[]? | select(.id == "kind-smoke-model")' "${models_body}" >/dev/null \
    || die "configured provider model was not advertised"

  local pod_json="${SMOKE_DIR}/configured-pod.json"
  run_with_deadline 15 "read configured pod status" \
    kube -n "${NAMESPACE}" get pod "${CURRENT_POD}" -o json >"${pod_json}" \
    || die "failed to read configured pod"
  jq -e '.status.conditions[]? | select(.type == "Ready" and .status == "True")' "${pod_json}" >/dev/null \
    || die "configured-provider pod was not Ready"

  local endpoints_json="${SMOKE_DIR}/configured-endpoints.json"
  local endpoint_deadline=$((SECONDS + 30))
  while (( SECONDS < endpoint_deadline )); do
    if run_with_deadline 15 "read configured Service endpoints" \
      kube -n "${NAMESPACE}" get endpoints vekil -o json >"${endpoints_json}" 2>/dev/null \
      && jq -e '[.subsets[]?.addresses[]?] | length > 0' "${endpoints_json}" >/dev/null; then
      return 0
    fi
    sleep 1
  done
  die "expected Service to expose a ready endpoint after configured rollout"
}

require_cmd docker
require_cmd kind
require_cmd kubectl
require_cmd curl
require_cmd jq
require_cmd "$(python_command)"

cd "${REPO_ROOT}"
mkdir -p "${SMOKE_DIR}"
render_manifest

log "Building ${IMAGE}"
run_with_deadline "${K8S_BUILD_TIMEOUT_SECONDS}" "Docker image build" \
  docker build -t "${IMAGE}" . || die "Docker image build failed"

log "Creating isolated kind cluster ${CLUSTER_NAME}"
run_with_deadline "${K8S_CLUSTER_TIMEOUT_SECONDS}" "delete pre-existing kind cluster" \
  kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
cluster_delete_needed=1
run_with_deadline "${K8S_CLUSTER_TIMEOUT_SECONDS}" "kind cluster creation" \
  kind create cluster --name "${CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" --wait 120s \
  || die "kind cluster creation failed"
cluster_created=1

log "Loading ${IMAGE} into ${CLUSTER_NAME}"
run_with_deadline "${K8S_CLUSTER_TIMEOUT_SECONDS}" "load image into kind" \
  kind load docker-image "${IMAGE}" --name "${CLUSTER_NAME}" \
  || die "failed to load image into kind"

run_with_deadline "${K8S_COMMAND_TIMEOUT_SECONDS}" "client dry-run of rendered manifest" \
  kubectl apply --dry-run=client --validate=false -f "${RENDERED_MANIFEST}" >/dev/null \
  || die "rendered k8s/vekil.yaml failed client dry-run"
run_with_deadline "${K8S_COMMAND_TIMEOUT_SECONDS}" "create smoke namespace" \
  kube create namespace "${NAMESPACE}" || die "failed to create namespace ${NAMESPACE}"
log "Applying rendered k8s/vekil.yaml without credentials"
run_with_deadline "${K8S_COMMAND_TIMEOUT_SECONDS}" "apply rendered Vekil manifest" \
  kube apply -f "${RENDERED_MANIFEST}" || die "failed to apply rendered Vekil manifest"

run_with_deadline 15 "verify deployed probe configuration" \
  kube -n "${NAMESPACE}" get deployment vekil -o json >"${SMOKE_DIR}/deployment.json" \
  || die "failed to read deployed Vekil manifest"
jq -e '
  .spec.template.spec.containers[]
  | select(.name == "vekil")
  | .startupProbe.httpGet.path == "/healthz"
    and .startupProbe.httpGet.port == "http"
    and (.startupProbe.timeoutSeconds <= .startupProbe.periodSeconds)
    and ((.startupProbe.periodSeconds * .startupProbe.failureThreshold) >= 60)
    and ((.startupProbe.periodSeconds * .startupProbe.failureThreshold) <= 90)
    and .livenessProbe.httpGet.path == "/healthz"
    and .livenessProbe.httpGet.port == "http"
    and .readinessProbe.httpGet.path == "/readyz"
    and .readinessProbe.httpGet.port == "http"
    and (.readinessProbe.timeoutSeconds >= 10)
    and (.readinessProbe.periodSeconds >= .readinessProbe.timeoutSeconds)
    and .readinessProbe.failureThreshold == 1
' "${SMOKE_DIR}/deployment.json" >/dev/null || die "deployed startup/liveness/readiness probes do not match k8s/vekil.yaml"

wait_for_running_pod || die "Vekil container did not enter Running state"
log "Auth-pending pod: ${CURRENT_POD}"
start_port_forward "${CURRENT_POD}"
wait_for_port_forward_health
assert_auth_pending
verify_liveness_window
assert_auth_pending

log "Auth-pending phase passed: health stayed live, Pod stayed not Ready, Service had no ready endpoint"
stop_port_forward || die "port-forward cleanup did not release 127.0.0.1:${LOCAL_PORT}"

log "Applying deterministic static-provider phase"
configure_static_provider
wait_for_running_pod || die "configured Vekil pod did not enter Running state"
log "Configured-provider pod: ${CURRENT_POD}"
start_port_forward "${CURRENT_POD}"
wait_for_port_forward_health
assert_configured_ready

log "Kubernetes smoke passed: checked-in probes gate auth-pending traffic and admit configured-provider traffic"
log "Rendered manifest: ${RENDERED_MANIFEST}"
