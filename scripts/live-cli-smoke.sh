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
# SMOKE_PROVIDER selects the upstream under test:
#   copilot (default) -> zero-config GitHub Copilot, credentialed, used by the gated
#                        Live Copilot Smoke workflow.
#   zen               -> OpenCode Zen free tier via --providers-config; no credentials,
#                        runs on fork PRs. See main_zen below.
SMOKE_PROVIDER="${SMOKE_PROVIDER:-copilot}"
PROVIDERS_CONFIG="${PROVIDERS_CONFIG:-${REPO_ROOT}/examples/opencode-zen-free.yaml}"
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
  if [[ "${SMOKE_PROVIDER}" == "zen" ]]; then
    PROXY_PORT=8899
  else
    PROXY_PORT=1337
  fi
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
    | index($endpoint)
  ' "${MODELS_JSON}" >/dev/null
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

pick_optional_gemini_model() {
  local candidate

  for candidate in "$@"; do
    if model_exists "${candidate}" && model_supports_endpoint "${candidate}" "/chat/completions"; then
      printf '%s\n' "${candidate}"
      return 0
    fi
  done

  # The Gemini CLI hits Gemini-native proxy routes, but Vekil translates
  # those requests to upstream OpenAI chat completions internally, so the
  # selected model must advertise /chat/completions support.
  candidate="$(jq -r '
    [
      .data[]?
      | select((.id | type) == "string")
      | select(.id | startswith("gemini-"))
      | select((.supported_endpoints // []) | index("/chat/completions"))
      | .id
    ][0] // ""
  ' "${MODELS_JSON}")"
  if [[ -n "${candidate}" ]]; then
    printf '%s\n' "${candidate}"
    return 0
  fi

  log "Skipping Gemini smoke: no Gemini model with /chat/completions support is listed by ${PROXY_BASE_URL}/v1/models."
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

  if [[ "${SMOKE_PROVIDER}" == "zen" ]]; then
    [[ -f "${PROVIDERS_CONFIG}" ]] || die "providers config not found: ${PROVIDERS_CONFIG}"
    log "Starting proxy at ${PROXY_BASE_URL} with ${PROVIDERS_CONFIG} (no credentials)"
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
    return
  fi

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
  HOME="${home_dir}" \
  ANTHROPIC_BASE_URL="${PROXY_BASE_URL}" \
  ANTHROPIC_API_KEY=dummy \
  CLAUDE_CODE_DISABLE_ADVISOR_TOOL="${CLAUDE_CODE_DISABLE_ADVISOR_TOOL:-1}" \
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

# ---------------------------------------------------------------------------
# Zen mode (SMOKE_PROVIDER=zen): credential-free OpenCode Zen free-tier smoke.
#
# The free tier rotates and is rate-limited per IP, so the contract is strict:
#   - an initial canary may skip only specifically recognized transient upstream
#     conditions evidenced by HTTP (listed model unavailable, promotion ended,
#     408/425/429, or 5xx);
#   - after a 200 canary, a CLI nonzero/timeout/invalid output gets one bounded
#     second canary; a still-reachable model is recorded as incompatible and the
#     client must pass another candidate;
#   - every installed client must pass independently on at least one candidate;
#   - neutral exit 0 is allowed only if no model was reachable before any client
#     was exercised.
# Zen's configured free models advertise text Chat support, not reliable coding
# tool use, so these client checks use a direct exact-text prompt. The
# credentialed Copilot-mode checks above retain their file-reading fixture.
# Codex is intentionally excluded: codex CLI is /responses-only and always sends
# a nameless web_search tool that Zen free upstreams reject. Copilot CLI covers
# the same role via COPILOT_PROVIDER_WIRE_API=completions.
# ---------------------------------------------------------------------------

# Preference order for free models; intersected with the live /v1/models catalog.
ZEN_MODEL_PREFS=(
  deepseek-v4-flash-free
  mimo-v2.5-free
  hy3-free
  ling-3.0-tiny-free
  nemotron-3.5-lightning-free
)

ATTEMPT_STATUS=""
ATTEMPT_DETAIL=""
ZEN_ANY_CLIENT_EXERCISED=0

zen_model_unavailable_is_transient() {
  local message="$1"
  printf '%s' "${message}" | grep -qiE \
    'model( [[:alnum:]_.:/-]+)? is unavailable[.]?$'
}

zen_error_is_transient() {
  local message="$1"
  printf '%s' "${message}" | grep -qiE \
    'promotion (has )?ended|free promotion[^[:alnum:]]+ended|^model [[:alnum:]_.:/-]+ is not supported$|rate[ -]?limit|too many requests|temporar(il)?y unavailable|service unavailable|overload(ed)?|over capacity|capacity (has been )?exceeded|upstream[^[:alnum:]]+(timeout|unavailable)|gateway timeout'
}

# zen_canary <model> [artifact-tag] -> echoes:
#   OK <detail> | TRANSIENT <recognized-reason> | FAIL <reason>
zen_canary() {
  local model="$1"
  local tag="${2:-initial}"
  local safe="${model//[^a-zA-Z0-9_.-]/_}-${tag//[^a-zA-Z0-9_.-]/_}"
  local body="${SMOKE_DIR}/canary-${safe}.json"
  local request="${SMOKE_DIR}/canary-req-${safe}.json"
  local curl_error="${SMOKE_DIR}/canary-${safe}.curl.err"
  local code errmsg curl_rc

  jq -n --arg model "${model}" \
    '{model: $model, max_tokens: 16, messages: [{role: "user", content: "ping"}]}' \
    > "${request}" || { printf 'FAIL request-build-error\n'; return; }

  if code="$(curl --silent --show-error \
    --output "${body}" \
    --write-out '%{http_code}' \
    --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${SMOKE_CURL_MAX_TIME_SECONDS}" \
    -X POST "${PROXY_BASE_URL}/v1/chat/completions" \
    -H 'content-type: application/json' \
    --data-binary "@${request}" \
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

  errmsg="$(jq -r '.error.message? // empty' "${body}" 2>/dev/null || true)"

  # Status and successful-response shape are authoritative. In particular, a
  # hard 404/405 or malformed 200 cannot be softened by transient-looking text.
  case "${code}" in
    200)
      if jq -e '.choices[0].message' "${body}" >/dev/null 2>&1; then
        printf 'OK http-200\n'
      else
        printf 'FAIL http-200-bad-shape\n'
      fi
      ;;
    400)
      if zen_model_unavailable_is_transient "${errmsg}"; then
        printf 'TRANSIENT message:%s\n' "${errmsg:0:80}"
      else
        printf 'FAIL http-%s:%s\n' "${code}" "${errmsg:0:80}"
      fi
      ;;
    404|405)
      printf 'FAIL http-%s:%s\n' "${code}" "${errmsg:0:80}"
      ;;
    408|425|429|5??)
      if printf '%s' "${errmsg}" | grep -qiE 'does not support /|unknown model|no upstream'; then
        printf 'FAIL proxy:%s\n' "${errmsg:0:80}"
      else
        printf 'TRANSIENT http-%s\n' "${code}"
      fi
      ;;
    401|403)
      if printf '%s' "${errmsg}" | grep -qiE 'does not support /|unknown model|no upstream'; then
        printf 'FAIL proxy:%s\n' "${errmsg:0:80}"
      elif zen_error_is_transient "${errmsg}"; then
        printf 'TRANSIENT message:%s\n' "${errmsg:0:80}"
      else
        printf 'FAIL http-%s:%s\n' "${code}" "${errmsg:0:80}"
      fi
      ;;
    *)
      printf 'FAIL http-%s:%s\n' "${code}" "${errmsg:0:80}"
      ;;
  esac
}

# run_harness_iterated <client> <model>... ->
#   0 pass, 2 no initial reachability, 3 reachable candidates all incompatible.
run_harness_iterated() {
  local client="$1"
  shift
  local model verdict status second_verdict second_status
  local client_reachable=0

  for model in "$@"; do
    verdict="$(zen_canary "${model}" "${client}-before")"
    status="${verdict%% *}"
    case "${status}" in
      TRANSIENT)
        log "[${client}] skip ${model} before CLI (${verdict#* })"
        continue
        ;;
      FAIL)
        die "[${client}] canary failed for ${model}: ${verdict#* }"
        ;;
      OK)
        ;;
      *)
        die "[${client}] unrecognized canary verdict for ${model}: ${verdict}"
        ;;
    esac

    ZEN_ANY_CLIENT_EXERCISED=1
    client_reachable=1
    run_zen_harness_once "${client}" "${model}"
    if [[ "${ATTEMPT_STATUS}" == "PASS" ]]; then
      log "[${client}] PASS ${model}"
      return 0
    fi

    # A reachable model followed by a bad CLI result is not skippable on its own.
    # Give the upstream one bounded re-check; only an explicitly recognized
    # transient may excuse this attempt.
    second_verdict="$(zen_canary "${model}" "${client}-after")"
    second_status="${second_verdict%% *}"
    case "${second_status}" in
      TRANSIENT)
        log "[${client}] ${model} CLI ${ATTEMPT_DETAIL}; second canary proved transient (${second_verdict#* })"
        continue
        ;;
      OK)
        log "[${client}] ${model} remained reachable after CLI ${ATTEMPT_DETAIL}; trying next candidate"
        continue
        ;;
      FAIL)
        die "[${client}] ${model} CLI ${ATTEMPT_DETAIL}; second canary failed: ${second_verdict#* }"
        ;;
      *)
        die "[${client}] unrecognized second canary verdict for ${model}: ${second_verdict}"
        ;;
    esac
  done

  if [[ "${client_reachable}" == "1" ]]; then
    log "[${client}] no reachable candidate produced the exact expected output"
    return 3
  fi
  log "[${client}] no model was initially reachable"
  return 2
}

# run_zen_harness_once <client> <model> -> ATTEMPT_STATUS=PASS|INVALID.
run_zen_harness_once() {
  local client="$1"
  local model="$2"
  local case_dir="${SMOKE_DIR}/cases/${client}"
  local output_file="${SMOKE_DIR}/outputs/${client}.txt"
  local fixture_name expected prompt actual rc

  rm -rf "${case_dir}"
  rm -f "${output_file}" "${SMOKE_DIR}/outputs/${client}.err"
  mkdir -p "${case_dir}"
  fixture_name="$(printf '%s' "${client}" | tr '[:lower:]' '[:upper:]')"
  expected="ZX_${fixture_name}_LEFT|ZX_${fixture_name}_RIGHT"
  prompt="Answer directly without tools. Return exactly ${expected} with no quotes, markdown, or extra text."

  if run_with_deadline "${SMOKE_CLI_TIMEOUT_SECONDS}" "${client} CLI (${model})" \
    "run_${client}_zen" "${case_dir}" "${model}" "${output_file}" "${prompt}"; then
    rc=0
  else
    rc=$?
  fi
  if [[ "${rc}" -ne 0 ]]; then
    ATTEMPT_STATUS="INVALID"
    ATTEMPT_DETAIL="exited ${rc}"
    return 0
  fi
  if [[ ! -f "${output_file}" ]]; then
    ATTEMPT_STATUS="INVALID"
    ATTEMPT_DETAIL="produced no output file"
    return 0
  fi

  if [[ "${client}" == "gemini" ]]; then
    actual="$(read_gemini_normalized_output "${output_file}")"
  else
    actual="$(read_normalized_output "${output_file}")"
  fi
  if [[ -z "${actual}" ]]; then
    ATTEMPT_STATUS="INVALID"
    ATTEMPT_DETAIL="produced empty output"
    return 0
  fi
  if [[ "${actual}" == "${expected}" ]]; then
    ATTEMPT_STATUS="PASS"
    ATTEMPT_DETAIL="passed"
    printf '%s' "${actual}" > "${output_file}"
    return 0
  fi

  printf 'expected %s output: %s\n' "${client}" "${expected}" >&2
  printf 'actual %s output:   %s\n' "${client}" "${actual}" >&2
  ATTEMPT_STATUS="INVALID"
  ATTEMPT_DETAIL="returned mismatched output"
  return 0
}

# GitHub Copilot CLI in offline BYOK mode -> vekil -> Zen, OpenAI completions wire.
run_copilot_zen() {
  local case_dir="$1"
  local model="$2"
  local output_file="$3"
  local prompt="$4"
  local home_dir="${SMOKE_DIR}/homes/copilot-home"

  rm -rf "${home_dir}"; mkdir -p "${home_dir}"
  (
    cd "${case_dir}"
    HOME="${home_dir}" \
    COPILOT_PROVIDER_BASE_URL="${PROXY_BASE_URL}/v1" \
    COPILOT_PROVIDER_TYPE=openai \
    COPILOT_PROVIDER_WIRE_API=completions \
    COPILOT_MODEL="${model}" \
    COPILOT_OFFLINE=true \
    copilot --allow-all-tools -p "${prompt}" -s \
      > "${output_file}" 2>"${SMOKE_DIR}/outputs/copilot.err" < /dev/null
  )
}

run_claude_zen() {
  local case_dir="$1"
  local model="$2"
  local output_file="$3"
  local prompt="$4"
  local home_dir="${SMOKE_DIR}/homes/claude-home"

  rm -rf "${home_dir}"; mkdir -p "${home_dir}/.claude"
  cat > "${home_dir}/.claude/settings.json" <<EOF
{
  "env": {
    "ANTHROPIC_BASE_URL": "${PROXY_BASE_URL}",
    "ANTHROPIC_API_KEY": "dummy"
  },
  "skipDangerousModePermissionPrompt": true
}
EOF
  (
    cd "${case_dir}"
    HOME="${home_dir}" \
    ANTHROPIC_BASE_URL="${PROXY_BASE_URL}" \
    ANTHROPIC_API_KEY=dummy \
    claude \
      --dangerously-skip-permissions \
      --print \
      --output-format text \
      --model "${model}" \
      "${prompt}" \
      > "${output_file}" 2>"${SMOKE_DIR}/outputs/claude.err" < /dev/null
  )
}

run_gemini_zen() {
  local case_dir="$1"
  local model="$2"
  local output_file="$3"
  local prompt="$4"
  local home_dir="${SMOKE_DIR}/homes/gemini-home"

  rm -rf "${home_dir}"; mkdir -p "${home_dir}/.gemini/tmp"
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
  (
    cd "${case_dir}"
    HOME="${home_dir}" \
    GEMINI_API_KEY=dummy \
    GOOGLE_GEMINI_BASE_URL="${PROXY_BASE_URL}" \
    GOOGLE_GENAI_API_VERSION=v1beta \
    GEMINI_CLI_NO_RELAUNCH=true \
    GEMINI_CLI_TRUST_WORKSPACE=true \
    gemini \
      -m "${model}" \
      -p "${prompt}" \
      -o text \
      -y \
      > "${output_file}" 2>"${SMOKE_DIR}/outputs/gemini.err" < /dev/null
  )
}

main_zen() {
  require_cmd curl
  require_cmd jq
  require_cmd copilot

  mkdir -p "${SMOKE_DIR}" "${SMOKE_DIR}/cases" "${SMOKE_DIR}/homes" "${SMOKE_DIR}/outputs"

  if [[ "${START_PROXY}" == "1" ]]; then
    start_proxy
    wait_for_ready
  else
    log "Using existing proxy at ${PROXY_BASE_URL}"
  fi

  fetch_models

  local candidates=() model
  for model in "${ZEN_MODEL_PREFS[@]}"; do
    if model_exists "${model}"; then
      candidates+=("${model}")
    fi
  done
  [[ "${#candidates[@]}" -gt 0 ]] || die "zen config lists no usable models (checked: ${ZEN_MODEL_PREFS[*]})"
  log "Zen candidate models: ${candidates[*]}"

  local rc

  # Copilot CLI is required; installed Claude and Gemini clients are also gates.
  local clients=(copilot)
  if command -v claude >/dev/null 2>&1; then
    clients+=(claude)
  else
    log "claude not installed; skipping Claude harness"
  fi
  if command -v gemini >/dev/null 2>&1; then
    clients+=(gemini)
  else
    log "gemini not installed; skipping Gemini harness"
  fi

  local client
  for client in "${clients[@]}"; do
    if run_harness_iterated "${client}" "${candidates[@]}"; then
      rc=0
    else
      rc=$?
    fi
    case "${rc}" in
      0)
        ;;
      2)
        if [[ "${ZEN_ANY_CLIENT_EXERCISED}" == "0" ]]; then
          log "Zen smoke NEUTRAL SKIP: no free model was reachable before any client was exercised."
          log "This is not a proxy failure; re-check the live free set with:"
          log "  curl --connect-timeout 10 --max-time 30 -s https://opencode.ai/zen/v1/models -H 'authorization: Bearer public'"
          return 0
        fi
        die "[${client}] did not pass after the smoke had already exercised a reachable model"
        ;;
      3)
        die "[${client}] no reachable Zen model produced the exact expected output"
        ;;
      *)
        die "[${client}] harness returned unexpected status ${rc}"
        ;;
    esac
  done

  log "Zen smoke passed: every installed client produced exact output independently."
  log "Artifacts: ${SMOKE_DIR}"
}

main() {
  if [[ "${SMOKE_PROVIDER}" == "zen" ]]; then
    main_zen "$@"
    return
  fi

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

  CODEX_MODEL="$(pick_model "Codex/OpenAI" gpt-5.4 gpt-5.3-codex gpt-5.2-codex gpt-5.1-codex gpt-5.1 gpt-5-mini gpt-4.1 gpt-4o)"
  CLAUDE_MODEL="$(pick_model "Claude" claude-sonnet-5 claude-opus-4.8 claude-opus-4.7 claude-opus-4.6 claude-sonnet-4.6 claude-sonnet-4.5 claude-haiku-4.5 claude-sonnet-4)"
  GEMINI_MODEL="$(pick_optional_gemini_model gemini-3.1-pro-preview gemini-3-pro-preview gemini-2.5-pro gemini-3-flash-preview)"

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
