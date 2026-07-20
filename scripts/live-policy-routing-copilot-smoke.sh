#!/usr/bin/env bash

# Copilot-backed entrypoint for the schema-v2 semantic-policy smoke.
#
# Policy profiles intentionally accept only static Azure/OpenAI-compatible
# destinations and classifiers. This wrapper preserves that product contract by
# starting a private zero-config Vekil bridge backed by GitHub Copilot, selecting
# live Chat-capable models from its catalog, and presenting the loopback bridge
# to scripts/live-policy-routing-smoke.sh as static openai-compatible targets.
#
# Required environment:
#   COPILOT_GITHUB_TOKEN
#
# Optional model overrides must name models advertised by the bridge with native
# /chat/completions support:
#   LIVE_POLICY_ROUTING_COPILOT_LIGHTWEIGHT_MODEL
#   LIVE_POLICY_ROUTING_COPILOT_CLASSIFIER_MODEL
#   LIVE_POLICY_ROUTING_COPILOT_POWERFUL_PRIMARY_MODEL
#   LIVE_POLICY_ROUTING_COPILOT_POWERFUL_SECONDARY_MODEL
#
# Additional optional environment:
#   PROXY_BIN                                  policy proxy binary; default ./vekil
#   LIVE_POLICY_ROUTING_COPILOT_BRIDGE_BIN     bridge binary; defaults to PROXY_BIN
#   LIVE_POLICY_ROUTING_HARNESS                delegated harness path
#   LIVE_POLICY_ROUTING_SMOKE_DIR              artifact directory
#   LIVE_POLICY_ROUTING_KEEP_ARTIFACTS=0       delete artifacts after success
#   SMOKE_*                                     bounded timeout overrides

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

require_env() {
  local name="$1"
  [[ -n "${!name:-}" ]] || die "required environment variable is empty: ${name}"
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
POLICY_PROXY_BIN="${PROXY_BIN:-${REPO_ROOT}/vekil}"
COPILOT_BRIDGE_BIN="${LIVE_POLICY_ROUTING_COPILOT_BRIDGE_BIN:-${POLICY_PROXY_BIN}}"
POLICY_HARNESS="${LIVE_POLICY_ROUTING_HARNESS:-${SCRIPT_DIR}/live-policy-routing-smoke.sh}"
SMOKE_STARTUP_TIMEOUT_SECONDS="${SMOKE_STARTUP_TIMEOUT_SECONDS:-90}"
SMOKE_CURL_CONNECT_TIMEOUT_SECONDS="${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS:-5}"
SMOKE_CURL_MAX_TIME_SECONDS="${SMOKE_CURL_MAX_TIME_SECONDS:-180}"
SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS="${SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS:-5}"
SMOKE_PROCESS_TERM_GRACE_SECONDS="${SMOKE_PROCESS_TERM_GRACE_SECONDS:-8}"
SMOKE_PORT_RELEASE_TIMEOUT_SECONDS="${SMOKE_PORT_RELEASE_TIMEOUT_SECONDS:-8}"
SMOKE_AUTO_PORT_MAX_ATTEMPTS="${SMOKE_AUTO_PORT_MAX_ATTEMPTS:-3}"
SMOKE_DIAGNOSTIC_MAX_BYTES="${SMOKE_DIAGNOSTIC_MAX_BYTES:-32768}"

TMP_PARENT="${LIVE_POLICY_ROUTING_TMP_PARENT:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}}"
SMOKE_DIR="${LIVE_POLICY_ROUTING_SMOKE_DIR:-$(mktemp -d "${TMP_PARENT%/}/live-policy-routing-copilot-smoke.XXXXXX")}"
if [[ "${SMOKE_DIR}" != /* ]]; then
  SMOKE_DIR="${PWD}/${SMOKE_DIR}"
fi
umask 077
mkdir -p "${SMOKE_DIR}"
chmod 700 "${SMOKE_DIR}"

BRIDGE_TOKEN_DIR="${SMOKE_DIR}/copilot-bridge-token"
BRIDGE_LOG="${SMOKE_DIR}/copilot-bridge.log"
BRIDGE_MODELS="${SMOKE_DIR}/copilot-models.json"
SELECTED_MODELS="${SMOKE_DIR}/copilot-selected-models.json"
bridge_pid=""
bridge_pgid=""
bridge_port=""
bridge_listen_confirmed=0
bridge_base_url=""
selected_lightweight=""
selected_classifier=""
selected_primary=""
selected_secondary=""

python_command() {
  if command -v python3 >/dev/null 2>&1; then
    command -v python3
    return
  fi
  if command -v python >/dev/null 2>&1; then
    command -v python
    return
  fi
  die "python3 (or python) is required"
}

validate_positive_integer() {
  local name="$1"
  local value="$2"
  [[ "${value}" =~ ^[1-9][0-9]*$ ]] || die "${name} must be a positive integer: ${value}"
}

allocate_free_port() {
  "$(python_command)" - <<'PY'
import socket

while True:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    if port != 1337:
        print(port)
        break
PY
}

process_is_running() {
  local pid="$1"
  [[ -n "${pid}" ]] || return 1
  kill -0 "${pid}" 2>/dev/null || return 1
  [[ "$(ps -o stat= -p "${pid}" 2>/dev/null | tr -d '[:space:]')" != Z* ]]
}

process_group_is_alive() {
  local pgid="$1"
  [[ -n "${pgid}" ]] || return 1
  kill -0 -- "-${pgid}" 2>/dev/null
}

terminate_process_group() {
  local pid="$1"
  local pgid="$2"
  local deadline
  [[ -n "${pgid}" ]] || return 0
  kill -TERM -- "-${pgid}" 2>/dev/null || true
  deadline=$((SECONDS + SMOKE_PROCESS_TERM_GRACE_SECONDS))
  while (( SECONDS < deadline )); do
    process_group_is_alive "${pgid}" || break
    sleep 0.1
  done
  if process_group_is_alive "${pgid}"; then
    kill -KILL -- "-${pgid}" 2>/dev/null || true
  fi
  if [[ -n "${pid}" ]]; then
    wait "${pid}" 2>/dev/null || true
  fi
}

port_is_open() {
  local port="$1"
  "$(python_command)" - "${port}" <<'PY'
import socket
import sys

with socket.socket() as sock:
    sock.settimeout(0.2)
    raise SystemExit(0 if sock.connect_ex(("127.0.0.1", int(sys.argv[1]))) == 0 else 1)
PY
}

wait_for_port_release() {
  local port="$1"
  local deadline=$((SECONDS + SMOKE_PORT_RELEASE_TIMEOUT_SECONDS))
  while (( SECONDS < deadline )); do
    if ! port_is_open "${port}"; then
      return 0
    fi
    sleep 0.1
  done
  ! port_is_open "${port}"
}

bridge_log_has_expected_listener() {
  [[ -f "${BRIDGE_LOG}" ]] || return 1
  jq -R -s -e --arg addr "127.0.0.1:${bridge_port}" '
    [split("\n")[] | fromjson? | select(.level == "info" and .msg == "vekil listening" and .addr == $addr)]
    | length > 0
  ' "${BRIDGE_LOG}" >/dev/null 2>&1
}

redact_and_print_bridge_log() {
  [[ -f "${BRIDGE_LOG}" ]] || return 0
  "$(python_command)" -     "${BRIDGE_LOG}"     "${SMOKE_DIAGNOSTIC_MAX_BYTES}"     "${COPILOT_GITHUB_TOKEN}"     "${selected_lightweight}"     "${selected_classifier}"     "${selected_primary}"     "${selected_secondary}" <<'PY'
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
limit = int(sys.argv[2])
text = path.read_text(encoding="utf-8", errors="replace")[:limit]
values = set(sys.argv[3:])
for value in sorted(values - {""}, key=len, reverse=True):
    text = text.replace(value, "[REDACTED]")
print("--- copilot-bridge.log (redacted) ---", file=sys.stderr)
print(text, file=sys.stderr)
PY
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM

  if [[ -n "${bridge_pgid}" ]]; then
    terminate_process_group "${bridge_pid}" "${bridge_pgid}"
    if [[ "${bridge_listen_confirmed}" == "1" && -n "${bridge_port}" ]] && ! wait_for_port_release "${bridge_port}"; then
      printf 'error: Copilot bridge cleanup did not release 127.0.0.1:%s\n' "${bridge_port}" >&2
      rc=1
    fi
  fi
  bridge_pid=""
  bridge_pgid=""
  bridge_listen_confirmed=0

  if [[ "${rc}" -ne 0 ]]; then
    redact_and_print_bridge_log
  elif [[ "${LIVE_POLICY_ROUTING_KEEP_ARTIFACTS:-0}" == "0" ]]; then
    rm -rf "${SMOKE_DIR}"
  fi
  exit "${rc}"
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

seed_bridge_token() {
  mkdir -p "${BRIDGE_TOKEN_DIR}"
  chmod 700 "${BRIDGE_TOKEN_DIR}"
  printf '%s\n' "${COPILOT_GITHUB_TOKEN}" > "${BRIDGE_TOKEN_DIR}/access-token"
  chmod 600 "${BRIDGE_TOKEN_DIR}/access-token"
}

wait_for_bridge_ready() {
  local deadline=$((SECONDS + SMOKE_STARTUP_TIMEOUT_SECONDS))
  while (( SECONDS < deadline )); do
    process_is_running "${bridge_pid}" || return 1
    if bridge_log_has_expected_listener; then
      bridge_listen_confirmed=1
      if curl --fail --silent --show-error \
        --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
        --max-time "${SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS}" \
        "${bridge_base_url}/readyz" >/dev/null 2>&1; then
        return 0
      fi
    fi
    sleep 0.2
  done
  return 1
}

start_copilot_bridge() {
  local attempt
  seed_bridge_token
  for ((attempt = 1; attempt <= SMOKE_AUTO_PORT_MAX_ATTEMPTS; attempt++)); do
    bridge_port="$(allocate_free_port)"
    bridge_base_url="http://127.0.0.1:${bridge_port}"
    : > "${BRIDGE_LOG}"
    chmod 600 "${BRIDGE_LOG}"
    log "Starting Copilot bridge at ${bridge_base_url} (attempt ${attempt}/${SMOKE_AUTO_PORT_MAX_ATTEMPTS})"
    set -m
    env -u COPILOT_GITHUB_TOKEN \
      "${COPILOT_BRIDGE_BIN}" \
      --host 127.0.0.1 \
      --port "${bridge_port}" \
      --token-dir "${BRIDGE_TOKEN_DIR}" \
      --log-level info \
      >"${BRIDGE_LOG}" 2>&1 &
    bridge_pid="$!"
    bridge_pgid="${bridge_pid}"
    set +m

    if wait_for_bridge_ready; then
      return 0
    fi
    terminate_process_group "${bridge_pid}" "${bridge_pgid}"
    bridge_pid=""
    bridge_pgid=""
    bridge_listen_confirmed=0
    if (( attempt < SMOKE_AUTO_PORT_MAX_ATTEMPTS )); then
      log "Copilot bridge port ${bridge_port} was unavailable or failed readiness; retrying"
    fi
  done
  die "Copilot bridge did not become ready"
}

fetch_copilot_models() {
  curl --fail --silent --show-error \
    --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${SMOKE_CURL_MAX_TIME_SECONDS}" \
    "${bridge_base_url}/v1/models" > "${BRIDGE_MODELS}" \
    || die "GET ${bridge_base_url}/v1/models failed"
  chmod 600 "${BRIDGE_MODELS}"
  jq -e '[.data[]? | select(((.supported_endpoints // []) | index("/chat/completions")) != null)] | length >= 2' \
    "${BRIDGE_MODELS}" >/dev/null || die "Copilot bridge must advertise at least two native-Chat models"
}

model_supports_chat() {
  local model="$1"
  jq -e --arg model "${model}" '
    .data[]?
    | select(.id == $model)
    | ((.supported_endpoints // []) | index("/chat/completions")) != null
  ' "${BRIDGE_MODELS}" >/dev/null
}

pick_copilot_model() {
  local label="$1"
  local override="$2"
  local excluded="$3"
  shift 3
  local candidate

  if [[ -n "${override}" ]]; then
    [[ "${override}" != "${excluded}" ]] || die "${label} override must differ from ${excluded}"
    model_supports_chat "${override}" || die "${label} override ${override} is not a Copilot native-Chat model"
    printf '%s\n' "${override}"
    return 0
  fi

  for candidate in "$@"; do
    [[ "${candidate}" != "${excluded}" ]] || continue
    if model_supports_chat "${candidate}"; then
      printf '%s\n' "${candidate}"
      return 0
    fi
  done

  candidate="$(jq -r --arg excluded "${excluded}" '
    [.data[]?
      | select((.id | type) == "string")
      | select(.id != $excluded)
      | select(((.supported_endpoints // []) | index("/chat/completions")) != null)
      | .id][0] // ""
  ' "${BRIDGE_MODELS}")"
  [[ -n "${candidate}" ]] || die "unable to select ${label} from Copilot native-Chat models"
  printf '%s\n' "${candidate}"
}

select_copilot_models() {
  selected_lightweight="$(pick_copilot_model \
    lightweight \
    "${LIVE_POLICY_ROUTING_COPILOT_LIGHTWEIGHT_MODEL:-}" \
    "" \
    gpt-5.4-mini gpt-5-mini gpt-4.1 gpt-4o claude-haiku-4.5)"
  selected_classifier="$(pick_copilot_model \
    classifier \
    "${LIVE_POLICY_ROUTING_COPILOT_CLASSIFIER_MODEL:-}" \
    "" \
    gpt-5.4-mini gpt-5-mini gpt-5.4 claude-sonnet-4.6 gpt-4.1)"
  selected_primary="$(pick_copilot_model \
    powerful-primary \
    "${LIVE_POLICY_ROUTING_COPILOT_POWERFUL_PRIMARY_MODEL:-}" \
    "" \
    gpt-5.4 claude-sonnet-4.6 gpt-5.3-codex claude-sonnet-4.5 gpt-5.2-codex gpt-4.1)"
  selected_secondary="$(pick_copilot_model \
    powerful-secondary \
    "${LIVE_POLICY_ROUTING_COPILOT_POWERFUL_SECONDARY_MODEL:-}" \
    "${selected_primary}" \
    claude-sonnet-4.6 gpt-5.4 gpt-5.3-codex claude-sonnet-4.5 gpt-5.2-codex gpt-4.1)"

  jq -n \
    --arg lightweight "${selected_lightweight}" \
    --arg classifier "${selected_classifier}" \
    --arg primary "${selected_primary}" \
    --arg secondary "${selected_secondary}" \
    '{lightweight:$lightweight,classifier:$classifier,powerful_primary:$primary,powerful_secondary:$secondary}' \
    > "${SELECTED_MODELS}"
  chmod 600 "${SELECTED_MODELS}"

  log "Selected Copilot policy models:"
  log "  lightweight: ${selected_lightweight}"
  log "  classifier: ${selected_classifier}"
  log "  powerful primary: ${selected_primary}"
  log "  powerful secondary: ${selected_secondary}"
}

random_bridge_key() {
  "$(python_command)" - <<'PY'
import secrets
print(secrets.token_urlsafe(24))
PY
}

run_policy_harness() {
  local bridge_api="${bridge_base_url}/v1"
  local lightweight_key primary_key secondary_key
  lightweight_key="$(random_bridge_key)"
  primary_key="$(random_bridge_key)"
  secondary_key="$(random_bridge_key)"

  env -u COPILOT_GITHUB_TOKEN \
    PROXY_BIN="${POLICY_PROXY_BIN}" \
    LIVE_POLICY_ROUTING_SMOKE_DIR="${SMOKE_DIR}" \
    LIVE_POLICY_ROUTING_ALLOW_INSECURE_HTTP=1 \
    LIVE_POLICY_ROUTING_LIGHTWEIGHT_TYPE=openai-compatible \
    LIVE_POLICY_ROUTING_LIGHTWEIGHT_BASE_URL="${bridge_api}" \
    LIVE_POLICY_ROUTING_LIGHTWEIGHT_MODEL="${selected_lightweight}" \
    LIVE_POLICY_ROUTING_LIGHTWEIGHT_API_KEY="${lightweight_key}" \
    LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_TYPE=openai-compatible \
    LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_BASE_URL="${bridge_api}" \
    LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_MODEL="${selected_primary}" \
    LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_API_KEY="${primary_key}" \
    LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_TYPE=openai-compatible \
    LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_BASE_URL="${bridge_api}" \
    LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_MODEL="${selected_secondary}" \
    LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_API_KEY="${secondary_key}" \
    LIVE_POLICY_ROUTING_CLASSIFIER_MODEL="${selected_classifier}" \
    LIVE_POLICY_ROUTING_CLASSIFIER_NO_STORE_SUPPORTED=false \
    LIVE_POLICY_ROUTING_ALLOW_PROVIDER_RETENTION=true \
    "${POLICY_HARNESS}"
}

main() {
  require_cmd curl
  require_cmd jq
  require_cmd ps
  require_env COPILOT_GITHUB_TOKEN
  python_command >/dev/null
  validate_positive_integer SMOKE_STARTUP_TIMEOUT_SECONDS "${SMOKE_STARTUP_TIMEOUT_SECONDS}"
  validate_positive_integer SMOKE_CURL_MAX_TIME_SECONDS "${SMOKE_CURL_MAX_TIME_SECONDS}"
  validate_positive_integer SMOKE_AUTO_PORT_MAX_ATTEMPTS "${SMOKE_AUTO_PORT_MAX_ATTEMPTS}"
  [[ -x "${POLICY_PROXY_BIN}" ]] || die "policy proxy binary not found or not executable: ${POLICY_PROXY_BIN} (run: make build)"
  [[ -x "${COPILOT_BRIDGE_BIN}" ]] || die "Copilot bridge binary not found or not executable: ${COPILOT_BRIDGE_BIN}"
  [[ -x "${POLICY_HARNESS}" ]] || die "policy harness not found or not executable: ${POLICY_HARNESS}"

  start_copilot_bridge
  fetch_copilot_models
  select_copilot_models
  run_policy_harness

  log "Copilot-backed semantic policy-routing smoke passed."
  if [[ "${LIVE_POLICY_ROUTING_KEEP_ARTIFACTS:-0}" == "1" ]]; then
    log "Artifacts: ${SMOKE_DIR}"
  fi
}

main "$@"
