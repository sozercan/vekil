#!/usr/bin/env bash

# Live smoke test for the OpenCode Zen free tier via Vekil.
#
# Starts the proxy with examples/opencode-zen-free.yaml on a NON-default port,
# waits for /readyz, lists /v1/models, then sends one tiny chat completion per
# configured free model.
#
# The OpenCode Zen free set rotates and individual promotions end without notice
# (ended models currently return "Free promotion has ended ...", "Model ... is
# not supported", or HTTP 400 with "Model is unavailable"). This script treats
# only HTTP-evidenced transient conditions (promotion ended, model removed or
# unavailable, rate limits, or 5xx capacity failures) as unavailable and
# passes as long as at least one configured free model returns a completion.
# Unknown statuses such as 404/405, invalid response shapes, proxy faults, and
# local transport errors and timeouts are hard failures.
#
# Usage:
#   make build
#   scripts/live-zen-smoke.sh
#
# Overrides (env): PROXY_BIN, PROXY_HOST, PROXY_PORT, PROVIDERS_CONFIG, START_PROXY.

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

PROXY_BIN="${PROXY_BIN:-${REPO_ROOT}/vekil}"
PROXY_HOST="${PROXY_HOST:-127.0.0.1}"
START_PROXY="${START_PROXY:-1}"
PROVIDERS_CONFIG="${PROVIDERS_CONFIG:-${REPO_ROOT}/examples/opencode-zen-free.yaml}"
SMOKE_STARTUP_TIMEOUT_SECONDS="${SMOKE_STARTUP_TIMEOUT_SECONDS:-30}"
SMOKE_CURL_CONNECT_TIMEOUT_SECONDS="${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS:-5}"
SMOKE_CURL_MAX_TIME_SECONDS="${SMOKE_CURL_MAX_TIME_SECONDS:-90}"
SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS="${SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS:-5}"
SMOKE_PROCESS_TERM_GRACE_SECONDS="${SMOKE_PROCESS_TERM_GRACE_SECONDS:-5}"
SMOKE_PORT_RELEASE_TIMEOUT_SECONDS="${SMOKE_PORT_RELEASE_TIMEOUT_SECONDS:-5}"

python_command() {
  if command -v python3 >/dev/null 2>&1; then
    command -v python3
    return
  fi
  if command -v python >/dev/null 2>&1; then
    command -v python
    return
  fi
  die "python3 (or python) is required to allocate and verify an isolated smoke port"
}

connect_host() {
  case "${PROXY_HOST}" in
    0.0.0.0) printf '127.0.0.1\n' ;;
    ::|\[::\]) printf '::1\n' ;;
    *) printf '%s\n' "${PROXY_HOST}" ;;
  esac
}

allocate_free_port() {
  local python_bin host
  python_bin="$(python_command)"
  host="$(connect_host)"
  "${python_bin}" - "${host}" <<'PY_PORT'
import socket
import sys

host = sys.argv[1]
family = socket.AF_INET6 if ":" in host else socket.AF_INET
for _ in range(20):
    with socket.socket(family, socket.SOCK_STREAM) as sock:
        sock.bind((host, 0))
        port = sock.getsockname()[1]
    if port != 1337:
        print(port)
        raise SystemExit(0)
raise SystemExit("unable to allocate a non-default port")
PY_PORT
}

proxy_port_was_set=0
if [[ ${PROXY_PORT+x} == x ]]; then
  proxy_port_was_set=1
fi
if [[ "${START_PROXY}" == "1" && "${proxy_port_was_set}" == "0" ]]; then
  PROXY_PORT="$(allocate_free_port)"
elif [[ "${proxy_port_was_set}" == "0" ]]; then
  PROXY_PORT=8899
fi
[[ "${PROXY_PORT}" =~ ^[0-9]+$ ]] || die "PROXY_PORT must be numeric: ${PROXY_PORT}"

PROXY_BASE_URL="http://${PROXY_HOST}:${PROXY_PORT}"
TMP_PARENT="${LIVE_ZEN_SMOKE_TMP_PARENT:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}}"
SMOKE_DIR="${LIVE_ZEN_SMOKE_DIR:-$(mktemp -d "${TMP_PARENT%/}/live-zen-smoke.XXXXXX")}"
if [[ "${SMOKE_DIR}" != /* ]]; then
  SMOKE_DIR="${PWD}/${SMOKE_DIR}"
fi
PROXY_LOG="${SMOKE_DIR}/proxy.log"
MODELS_JSON="${SMOKE_DIR}/models.json"
PROMPT="Reply with exactly the word: pong"

proxy_pid=""
proxy_pgid=""
proxy_listen_confirmed=0

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

terminate_process_group() {
  local pid="$1"
  local pgid="$2"
  local deadline
  if process_group_is_alive "${pgid}"; then
    kill -TERM -- "-${pgid}" 2>/dev/null || true
    deadline=$((SECONDS + SMOKE_PROCESS_TERM_GRACE_SECONDS))
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

port_is_open() {
  local python_bin host
  python_bin="$(python_command)"
  host="$(connect_host)"
  "${python_bin}" - "${host}" "${PROXY_PORT}" <<'PY_CONNECT' >/dev/null 2>&1
import socket
import sys

try:
    with socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=0.2):
        pass
except OSError:
    raise SystemExit(1)
PY_CONNECT
}

wait_for_port_release() {
  local deadline=$((SECONDS + SMOKE_PORT_RELEASE_TIMEOUT_SECONDS))
  while (( SECONDS < deadline )); do
    if ! port_is_open; then
      return 0
    fi
    sleep 0.1
  done
  ! port_is_open
}

dump_proxy_log() {
  if [[ -f "${PROXY_LOG}" ]]; then
    log "Proxy log:"
    cat "${PROXY_LOG}" >&2
  fi
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  if [[ -n "${proxy_pgid}" ]]; then
    terminate_process_group "${proxy_pid}" "${proxy_pgid}"
    proxy_pid=""
    proxy_pgid=""
  fi
  if [[ "${proxy_listen_confirmed}" == "1" ]] && ! wait_for_port_release; then
    printf 'error: proxy cleanup did not release %s:%s\n' "${PROXY_HOST}" "${PROXY_PORT}" >&2
    rc=1
  fi
  exit "${rc}"
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

start_proxy() {
  [[ -x "${PROXY_BIN}" ]] || die "proxy binary not found or not executable: ${PROXY_BIN} (run: make build)"
  [[ -f "${PROVIDERS_CONFIG}" ]] || die "providers config not found: ${PROVIDERS_CONFIG}"

  mkdir -p "${SMOKE_DIR}"

  log "Starting proxy at ${PROXY_BASE_URL} with ${PROVIDERS_CONFIG}"
  set -m
  "${PROXY_BIN}" \
    --host "${PROXY_HOST}" \
    --port "${PROXY_PORT}" \
    --log-level info \
    --providers-config "${PROVIDERS_CONFIG}" \
    >"${PROXY_LOG}" 2>&1 &
  proxy_pid="$!"
  proxy_pgid="${proxy_pid}"
  set +m
}

proxy_log_has_expected_listener() {
  [[ -f "${PROXY_LOG}" ]] || return 1
  jq -R -s -e --arg addr "${PROXY_HOST}:${PROXY_PORT}" '
    [
      split("\n")[]
      | fromjson?
      | select(.level == "info" and .msg == "vekil listening" and .addr == $addr)
    ]
    | length > 0
  ' "${PROXY_LOG}" >/dev/null 2>&1
}

proxy_log_has_fatal() {
  [[ -f "${PROXY_LOG}" ]] || return 1
  jq -R -s -e '
    [split("\n")[] | fromjson? | select(.level == "fatal")]
    | length > 0
  ' "${PROXY_LOG}" >/dev/null 2>&1
}

assert_spawned_proxy_alive() {
  if proxy_log_has_fatal; then
    dump_proxy_log
    die "spawned proxy logged a fatal startup error"
  fi
  if ! process_is_running "${proxy_pid}"; then
    dump_proxy_log
    die "spawned proxy PID ${proxy_pid} exited before readiness"
  fi
}

wait_for_ready() {
  local deadline=$((SECONDS + SMOKE_STARTUP_TIMEOUT_SECONDS))
  local listen_seen=0
  while (( SECONDS < deadline )); do
    assert_spawned_proxy_alive
    if [[ "${listen_seen}" == "0" ]]; then
      if proxy_log_has_expected_listener; then
        listen_seen=1
        proxy_listen_confirmed=1
      else
        sleep 0.1
        continue
      fi
    fi
    if curl --fail --silent --show-error \
      --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
      --max-time "${SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS}" \
      "${PROXY_BASE_URL}/readyz" >"${SMOKE_DIR}/readyz.json" 2>/dev/null; then
      assert_spawned_proxy_alive
      proxy_log_has_expected_listener || die "spawned proxy never logged expected listener ${PROXY_HOST}:${PROXY_PORT}"
      return 0
    fi
    sleep 0.2
  done
  dump_proxy_log
  die "proxy never became ready at ${PROXY_BASE_URL} within ${SMOKE_STARTUP_TIMEOUT_SECONDS}s"
}

fetch_models() {
  curl --fail --silent --show-error \
    --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${SMOKE_CURL_MAX_TIME_SECONDS}" \
    "${PROXY_BASE_URL}/v1/models" >"${MODELS_JSON}" \
    || die "GET /v1/models failed"
  jq -e '.data | length > 0' "${MODELS_JSON}" >/dev/null || die "no models returned by ${PROXY_BASE_URL}/v1/models"
}

zen_model_unavailable_is_transient() {
  local message="$1"
  printf '%s' "${message}" | grep -qiE \
    'model( [[:alnum:]_.:/-]+)? is unavailable[.]?$'
}

zen_error_is_transient() {
  local message="$1"
  if zen_model_unavailable_is_transient "${message}"; then
    return 0
  fi
  printf '%s' "${message}" | grep -qiE \
    'promotion (has )?ended|free promotion[^[:alnum:]]+ended|^model [[:alnum:]_.:/-]+ is not supported$|rate[ -]?limit|too many requests|temporar(il)?y unavailable|service unavailable|overload(ed)?|over capacity|capacity (has been )?exceeded|upstream[^[:alnum:]]+(timeout|unavailable)|gateway timeout'
}

# Probe one model. Echoes one of: OK | TRANSIENT | FAIL plus a reason.
probe_model() {
  local model="$1"
  local body_file="${SMOKE_DIR}/resp-${model//[^a-zA-Z0-9_.-]/_}.json"
  local request_file="${SMOKE_DIR}/req-${model//[^a-zA-Z0-9_.-]/_}.json"
  local curl_error="${body_file}.curl.err"
  local code curl_rc errmsg

  jq -n --arg model "${model}" --arg prompt "${PROMPT}" \
    '{model: $model, max_tokens: 64, messages: [{role: "user", content: $prompt}]}' \
    > "${request_file}" || { printf 'FAIL request-build-error\n'; return; }

  if code="$(curl --silent --show-error \
    --output "${body_file}" \
    --write-out '%{http_code}' \
    --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${SMOKE_CURL_MAX_TIME_SECONDS}" \
    -X POST "${PROXY_BASE_URL}/v1/chat/completions" \
    -H 'content-type: application/json' \
    --data-binary "@${request_file}" \
    2>"${curl_error}")"; then
    curl_rc=0
  else
    curl_rc=$?
  fi

  if [[ "${curl_rc}" -ne 0 ]]; then
    # This curl terminates at the local Vekil boundary. A timeout or transport
    # failure may be a stuck proxy handler, so only an HTTP response can prove
    # an upstream transient.
    printf 'FAIL curl-exit-%s\n' "${curl_rc}"
    return
  fi

  errmsg="$(jq -r '.error.message? // empty' "${body_file}" 2>/dev/null || true)"

  # Classify hard statuses and malformed success responses before inspecting
  # transient-looking error text.
  case "${code}" in
    200)
      if jq -e '.choices[0].message' "${body_file}" >/dev/null 2>&1; then
        local echoed finish
        echoed="$(jq -r '.model // "?"' "${body_file}")"
        finish="$(jq -r '.choices[0].finish_reason // "?"' "${body_file}")"
        printf 'OK echo=%s finish=%s\n' "${echoed}" "${finish}"
      else
        printf 'FAIL http-200-bad-shape\n'
      fi
      ;;
    400)
      if zen_model_unavailable_is_transient "${errmsg}"; then
        printf 'TRANSIENT message:%s\n' "${errmsg:0:70}"
      else
        printf 'FAIL http-%s %s\n' "${code}" "${errmsg:0:60}"
      fi
      ;;
    404|405)
      printf 'FAIL http-%s %s\n' "${code}" "${errmsg:0:60}"
      ;;
    408|425|429|5??)
      if printf '%s' "${errmsg}" | grep -qiE 'does not support /|unknown model|no upstream'; then
        printf 'FAIL proxy:%s\n' "${errmsg:0:70}"
      else
        printf 'TRANSIENT http-%s %s\n' "${code}" "${errmsg:0:60}"
      fi
      ;;
    401|403)
      if printf '%s' "${errmsg}" | grep -qiE 'does not support /|unknown model|no upstream'; then
        printf 'FAIL proxy:%s\n' "${errmsg:0:70}"
      elif zen_error_is_transient "${errmsg}"; then
        printf 'TRANSIENT message:%s\n' "${errmsg:0:70}"
      else
        printf 'FAIL http-%s %s\n' "${code}" "${errmsg:0:60}"
      fi
      ;;
    *)
      printf 'FAIL http-%s %s\n' "${code}" "${errmsg:0:60}"
      ;;
  esac
}

main() {
  require_cmd curl
  require_cmd jq

  if [[ "${START_PROXY}" == "1" ]]; then
    start_proxy
    wait_for_ready
  else
    log "Using existing proxy at ${PROXY_BASE_URL}"
  fi

  fetch_models

  local models
  models="$(jq -r '.data[].id' "${MODELS_JSON}")"
  log "Models listed by proxy:"
  printf '%s\n' "${models}" | sed 's/^/    /' >&2

  local total=0 ok=0 transient=0 failed=0
  local model result status
  while IFS= read -r model; do
    [[ -n "${model}" ]] || continue
    total=$((total + 1))
    result="$(probe_model "${model}")"
    status="${result%% *}"
    case "${status}" in
      OK)   ok=$((ok + 1)) ;;
      TRANSIENT) transient=$((transient + 1)) ;;
      *)    failed=$((failed + 1)) ;;
    esac
    printf '%-5s %-26s %s\n' "${status}" "${model}" "${result#* }" >&2
  done <<< "${models}"

  log "Summary: ${ok} ok, ${transient} recognized transient, ${failed} failed, of ${total} listed."

  if [[ "${failed}" -gt 0 ]]; then
    die "${failed} model(s) failed through the proxy (see ${SMOKE_DIR})."
  fi
  if [[ "${ok}" -lt 1 ]]; then
    die "no free model returned a completion; the free set may have fully rotated. Re-check: curl --connect-timeout 10 --max-time 30 -s https://opencode.ai/zen/v1/models -H 'authorization: Bearer public'"
  fi

  log "OpenCode Zen free-tier smoke passed."
  log "Artifacts: ${SMOKE_DIR}"
}

main "$@"
