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
SMOKE_CURL_MAX_TIME_SECONDS="${SMOKE_CURL_MAX_TIME_SECONDS:-90}"
SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS="${SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS:-5}"
SMOKE_CLI_TIMEOUT_SECONDS="${SMOKE_CLI_TIMEOUT_SECONDS:-240}"
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
  PROXY_PORT=1337
fi
[[ "${PROXY_PORT}" =~ ^[0-9]+$ ]] || die "PROXY_PORT must be numeric: ${PROXY_PORT}"
if [[ "${START_PROXY}" == "1" && "${PROXY_PORT}" == "1337" && "${proxy_port_was_set}" == "0" ]]; then
  die "auto-selected smoke port must not use the default port 1337"
fi

PROXY_BASE_URL="http://${PROXY_HOST}:${PROXY_PORT}"
TMP_PARENT="${LIVE_CLI_SMOKE_TMP_PARENT:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}}"
SMOKE_DIR="${LIVE_CLI_SMOKE_DIR:-$(mktemp -d "${TMP_PARENT%/}/live-cli-smoke.XXXXXX")}"
if [[ "${SMOKE_DIR}" != /* ]]; then
  SMOKE_DIR="${PWD}/${SMOKE_DIR}"
fi
PROXY_LOG="${SMOKE_DIR}/proxy.log"
MODELS_JSON="${SMOKE_DIR}/models.json"
PROMPT="Read left.txt and right.txt in the current directory and reply with exactly the two file contents joined by a vertical bar, with no spaces, no markdown, no commentary, and no extra text. Output only the final string."

if [[ -n "${COPILOT_GITHUB_TOKEN:-}" ]]; then
  PROXY_TOKEN_DIR="${PROXY_TOKEN_DIR:-${SMOKE_DIR}/proxy-token}"
else
  PROXY_TOKEN_DIR="${PROXY_TOKEN_DIR:-${ORIGINAL_HOME}/.config/vekil}"
fi

proxy_pid=""
proxy_pgid=""
active_pid=""
active_pgid=""
proxy_listen_confirmed=0
LAST_PROCESS_PID=""
LAST_PROCESS_PGID=""

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
  if [[ -n "${pid}" ]]; then
    wait "${pid}" 2>/dev/null || true
  fi
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
  # A CLI may exit while leaving descendants behind. Reap the entire dedicated
  # group before returning so one test cannot leak work into the next one.
  if process_group_is_alive "${pgid}"; then
    terminate_process_group "" "${pgid}"
  fi
  active_pid=""
  active_pgid=""
  return "${rc}"
}

port_is_open() {
  local python_bin host
  python_bin="$(python_command)"
  host="$(connect_host)"
  "${python_bin}" - "${host}" "${PROXY_PORT}" <<'PY_CONNECT' >/dev/null 2>&1
import socket
import sys

host = sys.argv[1]
port = int(sys.argv[2])
try:
    with socket.create_connection((host, port), timeout=0.2):
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

  if [[ -n "${active_pgid}" ]]; then
    terminate_process_group "${active_pid}" "${active_pgid}"
    active_pid=""
    active_pgid=""
  fi
  if [[ "${proxy_listen_confirmed}" == "1" && -s "${MODELS_JSON}" ]]; then
    local usage_url=""
    if [[ -n "${proxy_pid}" ]] && process_is_running "${proxy_pid}"; then
      usage_url="${PROXY_BASE_URL}"
    fi
    "$(python_command)" "${SCRIPT_DIR}/live-smoke-usage.py" \
      --url "${usage_url}" --label "CLI smoke" --output "${SMOKE_DIR}/usage.json" \
      || log "Unable to publish smoke usage"
  fi
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

model_supports_endpoint() {
  local model="$1"
  local endpoint="$2"

  jq -e --arg model "${model}" --arg endpoint "${endpoint}" '
    .data[]?
    | select(.id == $model)
    | (.supported_endpoints // [])
    # Legacy Copilot Chat models list no endpoints; Vekil treats them as
    # supporting every endpoint.
    | (length == 0 or index($endpoint) != null)
  ' "${MODELS_JSON}" >/dev/null
}

pick_model() {
  local family="$1"
  local endpoint="$2"
  shift 2

  local candidate
  for candidate in "$@"; do
    if model_supports_endpoint "${candidate}" "${endpoint}"; then
      printf '%s\n' "${candidate}"
      return 0
    fi
  done

  log "Available models from ${PROXY_BASE_URL}/v1/models:"
  jq -r '.data[].id' "${MODELS_JSON}" >&2
  die "no approved ${family} model supports ${endpoint}; allowed models: $*"
}

pick_optional_gemini_model() {
  local candidate

  for candidate in "$@"; do
    if model_exists "${candidate}" && model_supports_endpoint "${candidate}" "/chat/completions"; then
      printf '%s\n' "${candidate}"
      return 0
    fi
  done

  log "Skipping Gemini smoke: no approved model with /chat/completions support is listed by ${PROXY_BASE_URL}/v1/models."
  return 0
}

write_case_files() {
  local case_dir="$1"
  local client="$2"
  local fixture_name
  fixture_name="$(printf '%s' "${client}" | tr '[:lower:]' '[:upper:]')"
  local left_value="ZX_${fixture_name}_LEFT"
  local right_value="ZX_${fixture_name}_RIGHT"

  mkdir -p "${case_dir}"
  # Keep fixtures newline-free so exact-output assertions compare only the
  # requested payload, not editor-added file terminators.
  printf '%s' "${left_value}" > "${case_dir}/left.txt"
  printf '%s' "${right_value}" > "${case_dir}/right.txt"
  printf '%s|%s' "${left_value}" "${right_value}"
}

assert_exact_output() {
  local client="$1"
  local expected="$2"
  local actual="$3"

  if [[ "${actual}" != "${expected}" ]]; then
    printf 'expected %s output: %s\n' "${client}" "${expected}" >&2
    printf 'actual %s output:   %s\n' "${client}" "${actual}" >&2
    die "${client} smoke output mismatch"
  fi
}

read_normalized_output() {
  awk 'NF { gsub(/\r/, "", $0); printf "%s", $0 }' "$1"
}

# Recent Gemini CLI versions may render Read results as strict
# {"output":"..."} objects even when text output was requested. Accept either
# one wrapper around the complete result or the exact two-wrapper sequence for
# this smoke's two-part fixture. Leave every other shape unchanged so malformed JSON,
# extra fields, unwrapped text, and unexpected commentary still fail the exact
# assertion.
read_gemini_normalized_output() {
  local path="$1"
  local python_bin
  python_bin="$(python_command)"
  "${python_bin}" - "${path}" <<'PY_GEMINI_OUTPUT'
import json
import pathlib
import sys

raw = "".join(
    line.replace("\r", "")
    for line in pathlib.Path(sys.argv[1]).read_text().splitlines()
    if line.strip()
)
try:
    whole = json.loads(raw)
    if set(whole) != {"output"} or not isinstance(whole["output"], str):
        raise ValueError("not a strict whole-result wrapper")
except (TypeError, ValueError, json.JSONDecodeError):
    try:
        decoder = json.JSONDecoder()
        outputs = []
        offset = 0
        while offset < len(raw):
            wrapper, offset = decoder.raw_decode(raw, offset)
            if set(wrapper) != {"output"} or not isinstance(wrapper["output"], str):
                raise ValueError("not a strict output wrapper")
            outputs.append(wrapper["output"])
            if offset == len(raw):
                break
            if raw[offset:offset + 1] != "|":
                raise ValueError("missing separator")
            offset += 1
            if offset == len(raw):
                raise ValueError("dangling separator")
        if len(outputs) != 2:
            raise ValueError("expected exactly two wrappers")
    except (TypeError, ValueError, json.JSONDecodeError):
        print(raw, end="")
    else:
        print("|".join(outputs), end="")
else:
    print(whole["output"], end="")
PY_GEMINI_OUTPUT
}

start_proxy() {
  [[ -x "${PROXY_BIN}" ]] || die "proxy binary not found or not executable: ${PROXY_BIN}"

  mkdir -p "${SMOKE_DIR}" "${SMOKE_DIR}/cases" "${SMOKE_DIR}/homes" "${SMOKE_DIR}/outputs"

  mkdir -p "${PROXY_TOKEN_DIR}"
  seed_access_token

  log "Starting proxy at ${PROXY_BASE_URL}"
  set -m
  "${PROXY_BIN}" \
    --host "${PROXY_HOST}" \
    --port "${PROXY_PORT}" \
    --log-level info \
    --token-dir "${PROXY_TOKEN_DIR}" \
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
      # Check again after the HTTP response. This prevents a stale listener from
      # satisfying readiness while the process we launched exits concurrently.
      assert_spawned_proxy_alive
      proxy_log_has_expected_listener || {
        dump_proxy_log
        die "spawned proxy never logged the expected listener ${PROXY_HOST}:${PROXY_PORT}"
      }
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

run_codex_smoke() {
  local case_dir="${SMOKE_DIR}/cases/codex"
  local home_dir="${SMOKE_DIR}/homes/codex-home"
  local output_file="${SMOKE_DIR}/outputs/codex.txt"
  local expected
  local actual

  expected="$(write_case_files "${case_dir}" "codex")"
  mkdir -p "${home_dir}/.codex"
  printf 'model = "%s"\nopenai_base_url = "%s"\n' "${CODEX_MODEL}" "${PROXY_BASE_URL}/v1" > "${home_dir}/.codex/config.toml"

  log "Running Codex smoke with model ${CODEX_MODEL}"
  run_with_deadline "${SMOKE_CLI_TIMEOUT_SECONDS}" "Codex CLI" \
    env \
      HOME="${home_dir}" \
      OPENAI_API_KEY=dummy \
      OPENAI_BASE_URL="${PROXY_BASE_URL}/v1" \
      codex exec \
        --skip-git-repo-check \
        --cd "${case_dir}" \
        --dangerously-bypass-approvals-and-sandbox \
        -m "${CODEX_MODEL}" \
        --color never \
        -o "${output_file}" \
        "${PROMPT}" \
    || die "Codex CLI failed or timed out"

  actual="$(read_normalized_output "${output_file}")"
  assert_exact_output "codex" "${expected}" "${actual}"
  printf '%s' "${actual}" > "${output_file}"
}

run_claude_command() {
  local case_dir="$1"
  local home_dir="$2"
  local output_file="$3"
  local model="$4"
  cd "${case_dir}"
  # This baseline compatibility smoke does not exercise Claude's experimental
  # Advisor Tool, whose beta header is not accepted by the Copilot endpoint.
  # Use Anthropic's version spelling for Claude models so Claude recognizes
  # their capabilities; Vekil normalizes it back to the Copilot catalog ID.
  # Other models keep their catalog ID. File reads need no thinking.
  if [[ "${model}" == claude-* ]]; then
    model="${model//./-}"
  fi
  HOME="${home_dir}" \
  ANTHROPIC_BASE_URL="${PROXY_BASE_URL}" \
  ANTHROPIC_API_KEY=dummy \
  CLAUDE_CODE_DISABLE_ADVISOR_TOOL="${CLAUDE_CODE_DISABLE_ADVISOR_TOOL:-1}" \
  MAX_THINKING_TOKENS=0 \
  claude \
    --dangerously-skip-permissions \
    --print \
    --output-format text \
    --model "${model}" \
    "${PROMPT}" \
    > "${output_file}" < /dev/null
}

run_gemini_command() {
  local case_dir="$1"
  local home_dir="$2"
  local output_file="$3"
  local model="$4"
  cd "${case_dir}"
  HOME="${home_dir}" \
  GEMINI_API_KEY=dummy \
  GOOGLE_GEMINI_BASE_URL="${PROXY_BASE_URL}" \
  GOOGLE_GENAI_API_VERSION=v1beta \
  GEMINI_CLI_NO_RELAUNCH=true \
  GEMINI_CLI_TRUST_WORKSPACE=true \
  gemini \
    -m "${model}" \
    -p "${PROMPT}" \
    -o text \
    -y \
    > "${output_file}" < /dev/null
}

run_claude_smoke() {
  local case_dir="${SMOKE_DIR}/cases/claude"
  local home_dir="${SMOKE_DIR}/homes/claude-home"
  local output_file="${SMOKE_DIR}/outputs/claude.txt"
  local expected
  local actual

  expected="$(write_case_files "${case_dir}" "claude")"
  mkdir -p "${home_dir}/.claude"
  cat > "${home_dir}/.claude/settings.json" <<EOF
{
  "env": {
    "ANTHROPIC_BASE_URL": "${PROXY_BASE_URL}",
    "ANTHROPIC_API_KEY": "dummy"
  },
  "skipDangerousModePermissionPrompt": true
}
EOF

  log "Running Claude smoke with model ${CLAUDE_MODEL}"
  run_with_deadline "${SMOKE_CLI_TIMEOUT_SECONDS}" "Claude CLI" \
    run_claude_command "${case_dir}" "${home_dir}" "${output_file}" "${CLAUDE_MODEL}" \
    || die "Claude CLI failed or timed out"

  actual="$(read_normalized_output "${output_file}")"
  assert_exact_output "claude" "${expected}" "${actual}"
  printf '%s' "${actual}" > "${output_file}"
}

run_gemini_smoke() {
  local case_dir="${SMOKE_DIR}/cases/gemini"
  local home_dir="${SMOKE_DIR}/homes/gemini-home"
  local output_file="${SMOKE_DIR}/outputs/gemini.txt"
  local expected
  local actual

  expected="$(write_case_files "${case_dir}" "gemini")"
  mkdir -p "${home_dir}/.gemini/tmp"
  printf '{"projects":{}}\n' > "${home_dir}/.gemini/projects.json"
  cat > "${home_dir}/.gemini/settings.json" <<EOF
{
  "security": {
    "auth": {
      "selectedType": "gemini-api-key"
    }
  }
}
EOF

  log "Running Gemini smoke with model ${GEMINI_MODEL}"
  run_with_deadline "${SMOKE_CLI_TIMEOUT_SECONDS}" "Gemini CLI" \
    run_gemini_command "${case_dir}" "${home_dir}" "${output_file}" "${GEMINI_MODEL}" \
    || die "Gemini CLI failed or timed out"

  actual="$(read_gemini_normalized_output "${output_file}")"
  assert_exact_output "gemini" "${expected}" "${actual}"
  printf '%s' "${actual}" > "${output_file}"
}

main() {
  require_cmd curl
  require_cmd jq
  require_cmd codex
  require_cmd claude

  mkdir -p "${SMOKE_DIR}" "${SMOKE_DIR}/cases" "${SMOKE_DIR}/homes" "${SMOKE_DIR}/outputs"

  if [[ "${START_PROXY}" == "1" ]]; then
    start_proxy
    wait_for_ready
  else
    log "Using existing proxy at ${PROXY_BASE_URL}"
  fi

  fetch_models

  # Deliberate allowlists: catalog changes must not silently increase spend.
  CODEX_MODEL="$(pick_model "Codex/OpenAI" /responses gpt-5.6-luna-utility)"
  CLAUDE_MODEL="$(pick_model "Claude" /chat/completions claude-haiku-5.5)"
  GEMINI_MODEL="$(pick_optional_gemini_model gpt-4.1 gpt-4o)"

  if [[ -n "${GEMINI_MODEL}" ]]; then
    require_cmd gemini
  fi

  log "Selected models:"
  log "  codex:  ${CODEX_MODEL}"
  log "  claude: ${CLAUDE_MODEL}"
  if [[ -n "${GEMINI_MODEL}" ]]; then
    log "  gemini: ${GEMINI_MODEL}"
  else
    log "  gemini: skipped (no supported Gemini model listed)"
  fi

  run_codex_smoke
  run_claude_smoke
  if [[ -n "${GEMINI_MODEL}" ]]; then
    run_gemini_smoke
  fi

  log "All enabled live CLI smoke checks passed."
  log "Artifacts: ${SMOKE_DIR}"
}

main "$@"
