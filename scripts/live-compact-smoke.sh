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
ORIGINAL_HOME="${HOME}"

PROXY_BIN="${PROXY_BIN:-${REPO_ROOT}/vekil}"
PROXY_HOST="${PROXY_HOST:-127.0.0.1}"
START_PROXY="${START_PROXY:-1}"
SMOKE_STARTUP_TIMEOUT_SECONDS="${SMOKE_STARTUP_TIMEOUT_SECONDS:-120}"
SMOKE_CURL_CONNECT_TIMEOUT_SECONDS="${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS:-5}"
SMOKE_CURL_MAX_TIME_SECONDS="${SMOKE_CURL_MAX_TIME_SECONDS:-180}"
SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS="${SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS:-5}"
SMOKE_PROCESS_TERM_GRACE_SECONDS="${SMOKE_PROCESS_TERM_GRACE_SECONDS:-5}"
SMOKE_PORT_RELEASE_TIMEOUT_SECONDS="${SMOKE_PORT_RELEASE_TIMEOUT_SECONDS:-5}"
COPILOT_QUOTA_UNAVAILABLE_EXIT=75

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
  PROXY_PORT=1337
fi
[[ "${PROXY_PORT}" =~ ^[0-9]+$ ]] || die "PROXY_PORT must be numeric: ${PROXY_PORT}"

PROXY_BASE_URL="http://${PROXY_HOST}:${PROXY_PORT}"
TMP_PARENT="${LIVE_COMPACT_SMOKE_TMP_PARENT:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}}"
SMOKE_DIR="${LIVE_COMPACT_SMOKE_DIR:-$(mktemp -d "${TMP_PARENT%/}/live-compact-smoke.XXXXXX")}"
if [[ "${SMOKE_DIR}" != /* ]]; then
  SMOKE_DIR="${PWD}/${SMOKE_DIR}"
fi
PROXY_LOG="${SMOKE_DIR}/proxy.log"
MODELS_JSON="${SMOKE_DIR}/models.json"
COMPACT_REQUEST_JSON="${SMOKE_DIR}/compact-request.json"
COMPACT_RESPONSE_JSON="${SMOKE_DIR}/compact-response.json"
COMPACTION_ITEM_JSON="${SMOKE_DIR}/compaction-item.json"
REPLAY_REQUEST_JSON="${SMOKE_DIR}/replay-request.json"
REPLAY_RESPONSE_JSON="${SMOKE_DIR}/replay-response.json"
REPLAY_MARKER="VEKIL_COMPACTION_REPLAY_OK"

if [[ -n "${COPILOT_GITHUB_TOKEN:-}" ]]; then
  PROXY_TOKEN_DIR="${PROXY_TOKEN_DIR:-${SMOKE_DIR}/proxy-token}"
else
  PROXY_TOKEN_DIR="${PROXY_TOKEN_DIR:-${ORIGINAL_HOME}/.config/vekil}"
fi

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

seed_access_token() {
  if [[ -z "${COPILOT_GITHUB_TOKEN:-}" ]]; then
    return 0
  fi

  printf '%s\n' "${COPILOT_GITHUB_TOKEN}" > "${PROXY_TOKEN_DIR}/access-token"
  chmod 600 "${PROXY_TOKEN_DIR}/access-token"
}

model_exists() {
  jq -e --arg model "$1" '.data[]? | select(.id == $model)' "${MODELS_JSON}" >/dev/null
}

pick_model() {
  local family="$1"
  shift

  local candidate
  for candidate in "$@"; do
    if model_exists "${candidate}"; then
      printf '%s\n' "${candidate}"
      return 0
    fi
  done

  log "Available models from ${PROXY_BASE_URL}/v1/models:"
  jq -r '.data[].id' "${MODELS_JSON}" >&2
  die "unable to find a ${family} model from preferred list: $*"
}

start_proxy() {
  [[ -x "${PROXY_BIN}" ]] || die "proxy binary not found or not executable: ${PROXY_BIN}"

  mkdir -p "${SMOKE_DIR}" "${PROXY_TOKEN_DIR}"
  seed_access_token

  log "Starting proxy at ${PROXY_BASE_URL}"
  set -m
  "${PROXY_BIN}" \
    --host "${PROXY_HOST}" \
    --port "${PROXY_PORT}" \
    --token-dir "${PROXY_TOKEN_DIR}" \
    --log-level debug \
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
      "${PROXY_BASE_URL}/readyz" > "${SMOKE_DIR}/readyz.json" 2>/dev/null; then
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
    "${PROXY_BASE_URL}/v1/models" > "${MODELS_JSON}" \
    || die "GET ${PROXY_BASE_URL}/v1/models failed"
  jq -e '.data | length > 0' "${MODELS_JSON}" >/dev/null || die "no models returned by ${PROXY_BASE_URL}/v1/models"
}

write_compact_request() {
  local model="$1"

  jq -n \
    --arg model "${model}" \
    '{
      model: $model,
      input: [
        {
          type: "message",
          role: "user",
          content: [
            {
              type: "input_text",
              text: "We are testing live /v1/responses/compact through the local proxy. The task is in progress: create a compact checkpoint, then replay the returned compaction item through /v1/responses."
            }
          ]
        },
        {
          type: "message",
          role: "assistant",
          content: [
            {
              type: "output_text",
              text: "Acknowledged. The live compact smoke test should produce a concise checkpoint summary and an opaque compaction token."
            }
          ]
        }
      ]
    }' > "${COMPACT_REQUEST_JSON}"
}

post_json() {
  local endpoint="$1"
  local request_file="$2"
  local response_file="$3"
  local status

  status="$(curl --silent --show-error \
    --output "${response_file}" \
    --write-out '%{http_code}' \
    --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${SMOKE_CURL_MAX_TIME_SECONDS}" \
    -X POST "${PROXY_BASE_URL}${endpoint}" \
    -H 'Content-Type: application/json' \
    --data-binary "@${request_file}")" \
    || die "${endpoint} request failed before an HTTP response"

  if [[ "${status}" != "200" ]]; then
    if [[ "${status}" == "402" ]] && jq -e '.error.code == "quota_exceeded"' "${response_file}" >/dev/null 2>&1; then
      log "Copilot monthly quota is exhausted; live coverage is temporarily unavailable."
      exit "${COPILOT_QUOTA_UNAVAILABLE_EXIT}"
    fi
    if [[ -s "${response_file}" ]]; then
      log "${endpoint} response body:"
      cat "${response_file}" >&2
    fi
    die "${endpoint} returned HTTP ${status}"
  fi
}

assert_compact_response() {
  jq -e '
    ([.output[]? | select(.type == "compaction" and ((.encrypted_content // "") | length > 0))] | length > 0)
  ' "${COMPACT_RESPONSE_JSON}" >/dev/null || die "compact response did not contain a non-empty compaction item"

  jq -e '[.output[] | select(.type == "compaction" and ((.encrypted_content // "") | length > 0))] | .[0]' \
    "${COMPACT_RESPONSE_JSON}" > "${COMPACTION_ITEM_JSON}" || die "failed to extract compaction item"
}

write_replay_request() {
  local model="$1"

  jq -n \
    --arg model "${model}" \
    --arg marker "${REPLAY_MARKER}" \
    --slurpfile compaction "${COMPACTION_ITEM_JSON}" \
    '{
      model: $model,
      input: [
        $compaction[0],
        {
          type: "message",
          role: "user",
          content: [
            {
              type: "input_text",
              text: ("Reply with exactly " + $marker + " and no other text.")
            }
          ]
        }
      ]
    }' > "${REPLAY_REQUEST_JSON}"
}

responses_output_text() {
  jq -r '
    ([.output[]? | select(.type == "message") | .content[]? | select(.type == "output_text" or .type == "text") | .text] | join("\n"))
    | gsub("\r"; "")
    | sub("^[[:space:]]+"; "")
    | sub("[[:space:]]+$"; "")
  ' "$1"
}

assert_replay_response() {
  local replay_text
  replay_text="$(responses_output_text "${REPLAY_RESPONSE_JSON}")"

  if [[ "${replay_text}" != "${REPLAY_MARKER}" ]]; then
    printf 'expected replay response to equal %s after trimming whitespace\n' "${REPLAY_MARKER}" >&2
    printf 'actual normalized replay response:\n%s\n' "${replay_text}" >&2
    die "compaction replay output mismatch"
  fi

  if [[ "${START_PROXY}" == "1" ]] && [[ -f "${PROXY_LOG}" ]]; then
    grep -q 'rewrote compaction items' "${PROXY_LOG}" || die "proxy log did not show compaction item replay rewrite"
  fi
}

main() {
  require_cmd curl
  require_cmd jq

  mkdir -p "${SMOKE_DIR}"

  if [[ "${START_PROXY}" == "1" ]]; then
    start_proxy
    wait_for_ready
  else
    log "Using existing proxy at ${PROXY_BASE_URL}"
  fi

  fetch_models

  COMPACT_MODEL="$(pick_model "Codex/OpenAI" gpt-5.4 gpt-5.3-codex gpt-5.2-codex gpt-5.1-codex gpt-5.1 gpt-5-mini gpt-4.1 gpt-4o)"
  log "Selected compact model: ${COMPACT_MODEL}"

  log "Posting live compact request"
  write_compact_request "${COMPACT_MODEL}"
  post_json "/v1/responses/compact" "${COMPACT_REQUEST_JSON}" "${COMPACT_RESPONSE_JSON}"
  assert_compact_response

  log "Replaying returned compaction item through /v1/responses"
  write_replay_request "${COMPACT_MODEL}"
  post_json "/v1/responses" "${REPLAY_REQUEST_JSON}" "${REPLAY_RESPONSE_JSON}"
  assert_replay_response

  log "Live compaction smoke check passed."
  log "Artifacts: ${SMOKE_DIR}"
}

main "$@"
