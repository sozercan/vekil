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
CLAUDE_BIN="${CLAUDE_BIN:-claude}"
PROXY_HOST="${PROXY_HOST:-127.0.0.1}"
SMOKE_STARTUP_TIMEOUT_SECONDS="${SMOKE_STARTUP_TIMEOUT_SECONDS:-120}"
SMOKE_FIRST_TURN_TIMEOUT_SECONDS="${SMOKE_FIRST_TURN_TIMEOUT_SECONDS:-120}"
SMOKE_CLAUDE_TIMEOUT_SECONDS="${SMOKE_CLAUDE_TIMEOUT_SECONDS:-300}"
SMOKE_CURL_CONNECT_TIMEOUT_SECONDS="${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS:-5}"
SMOKE_CURL_MAX_TIME_SECONDS="${SMOKE_CURL_MAX_TIME_SECONDS:-120}"
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
  die "python3 (or python) is required for isolated port allocation and carrier validation"
}

connect_host() {
  local host="${PROXY_HOST}"
  if [[ "${host}" == \[*\] ]]; then
    host="${host#\[}"
    host="${host%\]}"
  fi
  printf '%s\n' "${host}"
}

validate_proxy_host() {
  local host
  host="$(connect_host)"
  "${PYTHON_BIN}" - "${host}" <<'PY_HOST' || die "PROXY_HOST must be localhost or a loopback IP literal: ${PROXY_HOST}"
import ipaddress
import sys

host = sys.argv[1]
if host.lower() == "localhost":
    raise SystemExit(0)
try:
    address = ipaddress.ip_address(host)
except ValueError:
    raise SystemExit(1)
raise SystemExit(0 if address.is_loopback else 1)
PY_HOST
}

url_host() {
  local host
  host="$(connect_host)"
  if [[ "${host}" == *:* ]]; then
    printf '[%s]\n' "${host}"
  else
    printf '%s\n' "${host}"
  fi
}

allocate_free_port() {
  local host
  host="$(connect_host)"
  "${PYTHON_BIN}" - "${host}" <<'PY_PORT'
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

PYTHON_BIN="$(python_command)"
validate_proxy_host
if [[ ${PROXY_PORT+x} != x ]]; then
  PROXY_PORT="$(allocate_free_port)"
fi
[[ "${PROXY_PORT}" =~ ^[0-9]+$ ]] || die "PROXY_PORT must be numeric: ${PROXY_PORT}"
[[ "${PROXY_PORT}" != "1337" ]] || die "Claude carrier smoke must not use the default port 1337"

# Vekil joins host and port itself, so its bind host must carry IPv6 brackets too.
PROXY_HOST="$(url_host)"
PROXY_BASE_URL="http://${PROXY_HOST}:${PROXY_PORT}"
TMP_PARENT="${LIVE_CLAUDE_REASONING_CARRIER_SMOKE_TMP_PARENT:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}}"
SMOKE_DIR="${LIVE_CLAUDE_REASONING_CARRIER_SMOKE_DIR:-$(mktemp -d "${TMP_PARENT%/}/live-claude-reasoning-carrier-smoke.XXXXXX")}"
if [[ "${SMOKE_DIR}" != /* ]]; then
  SMOKE_DIR="${PWD}/${SMOKE_DIR}"
fi

umask 077
mkdir -p "${SMOKE_DIR}"
chmod 700 "${SMOKE_DIR}"

INITIAL_PROXY_LOG="${SMOKE_DIR}/proxy-initial.log"
RESTARTED_PROXY_LOG="${SMOKE_DIR}/proxy-restarted.log"
CURRENT_PROXY_LOG=""
MODELS_JSON="${SMOKE_DIR}/models.json"
SELECTED_MODEL_FILE="${SMOKE_DIR}/selected-model.txt"
SUMMARY_FILE="${SMOKE_DIR}/summary.txt"
SANITIZED_TRACE="${SMOKE_DIR}/claude-sanitized.json"
CLAUDE_RAW="${SMOKE_DIR}/claude.stream.jsonl"
CLAUDE_ERR="${SMOKE_DIR}/claude.err"
CLAUDE_HOME="${SMOKE_DIR}/claude-home"
CLAUDE_SETTINGS="${CLAUDE_HOME}/.claude/settings.json"
CASE_DIR="${SMOKE_DIR}/case"
TOOL_STARTED_MARKER="${CASE_DIR}/.vekil-carrier-tool-started"
PROXY_READY_MARKER="${CASE_DIR}/.vekil-carrier-proxy-ready"
TOOL_RESULT_MARKER="VEKIL_CARRIER_TOOL_RESULT"
FINAL_MARKER="VEKIL_CARRIER_RESTART_OK"
# Expanded by the Bash tool inside Claude, not by this harness.
# shellcheck disable=SC2016
EXPECTED_TOOL_COMMAND='test -z "${COPILOT_GITHUB_TOKEN+x}" && printf started > .vekil-carrier-tool-started; while [ ! -f .vekil-carrier-proxy-ready ]; do sleep 0.1; done; printf VEKIL_CARRIER_TOOL_RESULT'
CLAUDE_BASH_ALLOW_RULE="Bash(${EXPECTED_TOOL_COMMAND})"

PROXY_TOKEN_DIR_OWNED=0
if [[ -n "${COPILOT_GITHUB_TOKEN:-}" ]]; then
  PROXY_TOKEN_DIR="${SMOKE_DIR}/proxy-token"
  PROXY_TOKEN_DIR_OWNED=1
else
  PROXY_TOKEN_DIR="${PROXY_TOKEN_DIR:-${ORIGINAL_HOME}/.config/vekil}"
fi

proxy_pid=""
proxy_pgid=""
claude_pid=""
claude_pgid=""
stopped_proxy_pid=""
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
  local host
  host="$(connect_host)"
  "${PYTHON_BIN}" - "${host}" "${PROXY_PORT}" <<'PY_CONNECT' >/dev/null 2>&1
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

redact_stream() {
  sed -E \
    -e 's/(Authorization: (Bearer|token) )[[:graph:]]+/\1[REDACTED]/g' \
    -e 's/gh[opusr]_[A-Za-z0-9_]+/[REDACTED_GITHUB_TOKEN]/g' \
    -e 's/github_pat_[A-Za-z0-9_]+/[REDACTED_GITHUB_TOKEN]/g' \
    -e 's/("access_token"[[:space:]]*:[[:space:]]*")[^"]*(")/\1[REDACTED]\2/g' \
    -e 's/("token"[[:space:]]*:[[:space:]]*")[^"]*(")/\1[REDACTED]\2/g' \
    -e 's/("encrypted_content"[[:space:]]*:[[:space:]]*")[^"]*(")/\1[REDACTED]\2/g' \
    -e 's/vekil1\.[A-Za-z0-9_-]+/[REDACTED_VEKIL_CARRIER]/g' \
    -e 's/(Please visit https:\/\/github\.com\/login\/device and enter code: )[A-Z0-9-]+/\1[REDACTED]/g'
}

dump_redacted_file() {
  local label="$1"
  local path="$2"
  [[ -f "${path}" ]] || return 0
  printf '%s\n' "--- ${label} ---" >&2
  redact_stream < "${path}" | { head -c 32768; cat >/dev/null; } >&2
  printf '\n' >&2
}

write_sanitized_trace() {
  [[ -f "${CLAUDE_RAW}" ]] || return 0
  "${PYTHON_BIN}" - "${CLAUDE_RAW}" > "${SANITIZED_TRACE}" <<'PY_TRACE' || true
import json
import pathlib
import sys

events = []
for line in pathlib.Path(sys.argv[1]).read_text(errors="replace").splitlines():
    try:
        events.append(json.loads(line))
    except json.JSONDecodeError:
        continue

summary = {
    "assistant_carriers": 0,
    "assistant_text_blocks": 0,
    "assistant_tool_uses": 0,
    "carrier_signature_deltas": 0,
    "init_api_key_source": "",
    "init_model": "",
    "result_is_error": None,
    "result_subtype": "",
    "tool_results": 0,
}
for event in events:
    if event.get("type") == "system" and event.get("subtype") == "init":
        summary["init_model"] = event.get("model", "")
        summary["init_api_key_source"] = event.get("apiKeySource", "")
    if event.get("type") == "stream_event":
        delta = (event.get("event") or {}).get("delta") or {}
        if delta.get("type") == "signature_delta" and str(delta.get("signature", "")).startswith("vekil1."):
            summary["carrier_signature_deltas"] += 1
    if event.get("type") == "assistant":
        for block in (event.get("message") or {}).get("content") or []:
            if block.get("type") == "tool_use":
                summary["assistant_tool_uses"] += 1
            elif block.get("type") == "thinking" and str(block.get("signature", "")).startswith("vekil1."):
                summary["assistant_carriers"] += 1
            elif block.get("type") == "text" and block.get("text"):
                summary["assistant_text_blocks"] += 1
    if event.get("type") == "user":
        for block in (event.get("message") or {}).get("content") or []:
            if block.get("type") == "tool_result":
                summary["tool_results"] += 1
    if event.get("type") == "result":
        summary["result_subtype"] = event.get("subtype", "")
        summary["result_is_error"] = event.get("is_error")

print(json.dumps(summary, indent=2, sort_keys=True))
PY_TRACE
  chmod 600 "${SANITIZED_TRACE}" 2>/dev/null || true
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM

  if [[ -n "${claude_pgid}" ]]; then
    terminate_process_group "${claude_pid}" "${claude_pgid}"
    claude_pid=""
    claude_pgid=""
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
  if [[ "${PROXY_TOKEN_DIR_OWNED}" == "1" ]]; then
    if [[ "${PROXY_TOKEN_DIR}" != "${SMOKE_DIR}/proxy-token" ]]; then
      printf 'error: refusing to remove unexpected proxy token directory: %s\n' "${PROXY_TOKEN_DIR}" >&2
      rc=1
    elif ! rm -rf -- "${PROXY_TOKEN_DIR}"; then
      printf 'error: failed to remove staged proxy credentials: %s\n' "${PROXY_TOKEN_DIR}" >&2
      rc=1
    fi
  fi
  if [[ "${rc}" -ne 0 ]]; then
    write_sanitized_trace
    dump_redacted_file "proxy-initial.log" "${INITIAL_PROXY_LOG}"
    dump_redacted_file "proxy-restarted.log" "${RESTARTED_PROXY_LOG}"
    dump_redacted_file "claude.err" "${CLAUDE_ERR}"
    dump_redacted_file "claude-sanitized.json" "${SANITIZED_TRACE}"
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

start_proxy() {
  local label="$1"
  local log_path="$2"
  [[ -x "${PROXY_BIN}" ]] || die "proxy binary not found or not executable: ${PROXY_BIN} (run: make build)"
  mkdir -p "${PROXY_TOKEN_DIR}"
  chmod 700 "${PROXY_TOKEN_DIR}"
  seed_access_token
  CURRENT_PROXY_LOG="${log_path}"
  : > "${CURRENT_PROXY_LOG}"
  chmod 600 "${CURRENT_PROXY_LOG}"
  log "Starting ${label} proxy at ${PROXY_BASE_URL}"
  set -m
  "${PROXY_BIN}" \
    --host "${PROXY_HOST}" \
    --port "${PROXY_PORT}" \
    --log-level info \
    --token-dir "${PROXY_TOKEN_DIR}" \
    >"${CURRENT_PROXY_LOG}" 2>&1 &
  proxy_pid="$!"
  proxy_pgid="${proxy_pid}"
  set +m
}

proxy_log_has_expected_listener() {
  [[ -f "${CURRENT_PROXY_LOG}" ]] || return 1
  jq -R -s -e --arg addr "${PROXY_HOST}:${PROXY_PORT}" '
    [split("\n")[] | fromjson? | select(.level == "info" and .msg == "vekil listening" and .addr == $addr)]
    | length > 0
  ' "${CURRENT_PROXY_LOG}" >/dev/null 2>&1
}

proxy_log_has_fatal() {
  [[ -f "${CURRENT_PROXY_LOG}" ]] || return 1
  jq -R -s -e '[split("\n")[] | fromjson? | select(.level == "fatal")] | length > 0' \
    "${CURRENT_PROXY_LOG}" >/dev/null 2>&1
}

assert_spawned_proxy_alive() {
  if proxy_log_has_fatal; then
    dump_redacted_file "current proxy log" "${CURRENT_PROXY_LOG}"
    die "spawned proxy logged a fatal startup error"
  fi
  if ! process_is_running "${proxy_pid}"; then
    dump_redacted_file "current proxy log" "${CURRENT_PROXY_LOG}"
    die "spawned proxy PID ${proxy_pid} exited before readiness"
  fi
}

wait_for_ready() {
  local phase="$1"
  local ready_file="${SMOKE_DIR}/readyz-${phase}.json"
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
      "${PROXY_BASE_URL}/readyz" > "${ready_file}" 2>/dev/null; then
      assert_spawned_proxy_alive
      proxy_log_has_expected_listener || die "spawned proxy never logged expected listener ${PROXY_HOST}:${PROXY_PORT}"
      return 0
    fi
    sleep 0.2
  done
  dump_redacted_file "current proxy log" "${CURRENT_PROXY_LOG}"
  die "${phase} proxy never became ready at ${PROXY_BASE_URL} within ${SMOKE_STARTUP_TIMEOUT_SECONDS}s"
}

stop_proxy_for_restart() {
  stopped_proxy_pid="${proxy_pid}"
  terminate_process_group "${proxy_pid}" "${proxy_pgid}"
  proxy_pid=""
  proxy_pgid=""
  wait_for_port_release || die "initial proxy did not release ${PROXY_HOST}:${PROXY_PORT} for restart"
}

fetch_models() {
  curl --fail --silent --show-error \
    --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${SMOKE_CURL_MAX_TIME_SECONDS}" \
    "${PROXY_BASE_URL}/v1/models" > "${MODELS_JSON}" \
    || die "GET ${PROXY_BASE_URL}/v1/models failed"
  jq -e '.data | length > 0' "${MODELS_JSON}" >/dev/null || die "no models returned by ${PROXY_BASE_URL}/v1/models"
}

pick_responses_only_gpt_model() {
  local selected
  selected="$(jq -r '
    def responses_only_gpt:
      (.id | type) == "string"
      and (.id | startswith("gpt-"))
      and ((.supported_endpoints // []) | index("/responses")) != null
      and ((.supported_endpoints // []) | index("/chat/completions")) == null
      and (.capabilities.supports.tool_calls // false) == true;
    ([.data[]? | select(responses_only_gpt) | .id] as $models
      | if ($models | index("gpt-5.6-sol")) != null then "gpt-5.6-sol"
        elif ($models | length) > 0 then $models[0]
        else ""
        end)
  ' "${MODELS_JSON}")"
  [[ -n "${selected}" ]] || die "no tool-capable GPT model advertises /responses while excluding /chat/completions"
  printf '%s\n' "${selected}"
}

validate_claude_stream() {
  local mode="$1"
  "${PYTHON_BIN}" - \
    "${mode}" "${CLAUDE_RAW}" "${CHAT_MODEL}" "${EXPECTED_TOOL_COMMAND}" "${TOOL_RESULT_MARKER}" "${FINAL_MARKER}" <<'PY_VALIDATE'
import base64
import json
import pathlib
import sys
import zlib

mode, path, model, expected_command, tool_result_marker, final_marker = sys.argv[1:]


def fail(message):
    print(f"carrier validation failed: {message}", file=sys.stderr)
    raise SystemExit(1)


events = []
for number, line in enumerate(pathlib.Path(path).read_text(errors="replace").splitlines(), 1):
    if not line.strip():
        continue
    try:
        events.append(json.loads(line))
    except json.JSONDecodeError:
        fail(f"Claude stdout line {number} was not JSON")

init = next((event for event in events if event.get("type") == "system" and event.get("subtype") == "init"), None)
if init is None:
    fail("Claude emitted no init event")
if init.get("model") != model:
    fail("Claude init selected the wrong model")
if init.get("apiKeySource") != "ANTHROPIC_API_KEY":
    fail("Claude did not use isolated API-key authentication")

tool_uses = []
assistant_carriers = set()
assistant_text = []
for event in events:
    if event.get("type") != "assistant":
        continue
    for block in (event.get("message") or {}).get("content") or []:
        if block.get("type") == "tool_use":
            tool_uses.append(block)
        elif block.get("type") == "thinking" and str(block.get("signature", "")).startswith("vekil1."):
            assistant_carriers.add(block["signature"])
        elif block.get("type") == "text" and block.get("text"):
            assistant_text.append(block["text"])

if len(tool_uses) != 1:
    fail("Claude did not return exactly one tool_use block")
tool_use = tool_uses[0]
tool_id = tool_use.get("id", "")
if not tool_id.startswith("call_vekil_") or tool_use.get("name") != "Bash":
    fail("Claude returned an unexpected tool id or name")
if (tool_use.get("input") or {}).get("command") != expected_command:
    fail("Claude changed the controlled delayed tool command")

delta_carriers = set()
for event in events:
    if event.get("type") != "stream_event":
        continue
    delta = (event.get("event") or {}).get("delta") or {}
    signature = str(delta.get("signature", ""))
    if delta.get("type") == "signature_delta" and signature.startswith("vekil1."):
        delta_carriers.add(signature)

shared_carriers = delta_carriers & assistant_carriers
if not shared_carriers:
    fail("Claude did not reconstruct the vekil1 signature_delta in its assistant message")


def decode_carrier(signature):
    encoded = signature[len("vekil1."):]
    encoded += "=" * (-len(encoded) % 4)
    compressed = base64.urlsafe_b64decode(encoded)
    return json.loads(zlib.decompress(compressed, -zlib.MAX_WBITS))


carrier_matches_tool = False
for signature in shared_carriers:
    try:
        payload = decode_carrier(signature)
    except (ValueError, json.JSONDecodeError, zlib.error):
        continue
    calls = payload.get("calls") or []
    for call in calls:
        if call.get("proxy_id") != tool_id or call.get("name") != "Bash":
            continue
        upstream_id = call.get("upstream_id", "")
        item_index = call.get("item_index")
        items = payload.get("items") or []
        if not upstream_id or not isinstance(item_index, int) or not (0 <= item_index < len(items)):
            continue
        item = items[item_index]
        if item.get("type") == "function_call" and item.get("call_id") == upstream_id and item.get("name") == "Bash":
            carrier_matches_tool = True
            break
    if carrier_matches_tool:
        break
if not carrier_matches_tool:
    fail("the carrier did not bind the returned proxy and upstream tool ids")

if mode == "first":
    raise SystemExit(0)

tool_results = []
for event in events:
    if event.get("type") != "user":
        continue
    for block in (event.get("message") or {}).get("content") or []:
        if block.get("type") == "tool_result":
            tool_results.append(block)
if len(tool_results) != 1:
    fail("Claude did not return exactly one tool_result block")
if tool_results[0].get("tool_use_id") != tool_id or tool_results[0].get("content") != tool_result_marker:
    fail("Claude did not echo the matching tool id and controlled result")

results = [event for event in events if event.get("type") == "result"]
if len(results) != 1:
    fail("Claude did not emit exactly one terminal result")
result = results[0]
if result.get("is_error") is not False or result.get("subtype") != "success" or result.get("result") != final_marker:
    fail("Claude did not finish with the exact success sentinel")
if assistant_text != [final_marker]:
    fail("Claude emitted assistant text other than the exact success sentinel")

print("PASS isolated-api-key-settings")
print("PASS carrier-signature-preserved")
print("PASS carrier-tool-ids-match")
print("PASS exact-final-sentinel")
PY_VALIDATE
}

start_claude() {
  local prompt
  prompt="Use Bash exactly once. Run this exact command without changes: ${EXPECTED_TOOL_COMMAND}. After the command returns ${TOOL_RESULT_MARKER}, reply with exactly ${FINAL_MARKER} and no other text."
  : > "${CLAUDE_RAW}"
  : > "${CLAUDE_ERR}"
  chmod 600 "${CLAUDE_RAW}" "${CLAUDE_ERR}"

  log "Starting isolated Claude Code smoke with model ${CHAT_MODEL}"
  set -m
  (
    cd "${CASE_DIR}"
    env -u COPILOT_GITHUB_TOKEN \
      -u ANTHROPIC_API_KEY \
      -u ANTHROPIC_AUTH_TOKEN \
      -u ANTHROPIC_BASE_URL \
      -u CLAUDE_CODE_OAUTH_TOKEN \
      -u CLAUDE_CODE_USE_BEDROCK \
      -u CLAUDE_CODE_USE_VERTEX \
      -u CLAUDE_CODE_USE_FOUNDRY \
      HOME="${CLAUDE_HOME}" \
      "${CLAUDE_BIN}" \
      --bare \
      --settings "${CLAUDE_SETTINGS}" \
      --permission-mode dontAsk \
      --no-session-persistence \
      --print \
      --output-format stream-json \
      --verbose \
      --include-partial-messages \
      --model "${CHAT_MODEL}" \
      --tools=Bash \
      --allowedTools="${CLAUDE_BASH_ALLOW_RULE}" \
      "${prompt}" \
      > "${CLAUDE_RAW}" 2> "${CLAUDE_ERR}" < /dev/null
  ) &
  claude_pid="$!"
  claude_pgid="${claude_pid}"
  set +m
}

wait_for_first_turn() {
  local deadline=$((SECONDS + SMOKE_FIRST_TURN_TIMEOUT_SECONDS))
  while (( SECONDS < deadline )); do
    if [[ -f "${TOOL_STARTED_MARKER}" ]] && validate_claude_stream first >/dev/null 2>&1; then
      return 0
    fi
    if ! process_is_running "${claude_pid}"; then
      die "Claude exited before returning a carrier-backed delayed tool call"
    fi
    sleep 0.2
  done
  die "Claude did not return a carrier-backed delayed tool call within ${SMOKE_FIRST_TURN_TIMEOUT_SECONDS}s"
}

wait_for_claude() {
  local deadline=$((SECONDS + SMOKE_CLAUDE_TIMEOUT_SECONDS))
  local rc
  while process_is_running "${claude_pid}"; do
    if (( SECONDS >= deadline )); then
      die "Claude exceeded ${SMOKE_CLAUDE_TIMEOUT_SECONDS}s deadline"
    fi
    sleep 0.2
  done
  if wait "${claude_pid}"; then
    rc=0
  else
    rc=$?
  fi
  if process_group_is_alive "${claude_pgid}"; then
    terminate_process_group "" "${claude_pgid}"
  fi
  claude_pid=""
  claude_pgid=""
  [[ "${rc}" -eq 0 ]] || die "Claude failed after the proxy restart (exit ${rc})"
}

assert_restarted_proxy_used_carrier() {
  jq -R -s -e --arg model "${CHAT_MODEL}" '
    [
      split("\n")[]
      | fromjson?
      | select(
          .method == "POST"
          and .path == "/v1/messages"
          and .model == $model
          and .status == 200
        )
    ]
    | length > 0
  ' "${RESTARTED_PROXY_LOG}" >/dev/null || die "restarted proxy logged no successful Claude continuation"

  if jq -R -s -e '
      [
        split("\n")[]
        | fromjson?
        | select(
            .msg == "responses replay projection mismatch; continuing without reasoning continuity"
            or .msg == "responses replay unavailable and the carrier could not answer"
          )
      ]
      | length > 0
    ' "${RESTARTED_PROXY_LOG}" >/dev/null; then
    die "restarted proxy bypassed or rejected the reasoning carrier"
  fi
}

main() {
  require_cmd curl
  require_cmd jq
  require_cmd "${CLAUDE_BIN}"

  mkdir -p "${CASE_DIR}" "${CLAUDE_HOME}/.claude"
  chmod 700 "${CASE_DIR}" "${CLAUDE_HOME}" "${CLAUDE_HOME}/.claude"
  rm -f "${TOOL_STARTED_MARKER}" "${PROXY_READY_MARKER}"

	  cat > "${CLAUDE_SETTINGS}" <<EOF
{
	  "env": {
	    "ANTHROPIC_BASE_URL": "${PROXY_BASE_URL}",
	    "ANTHROPIC_API_KEY": "dummy",
	    "CLAUDE_CODE_DISABLE_ADVISOR_TOOL": "1"
	  }
}
EOF
  chmod 600 "${CLAUDE_SETTINGS}"
  jq -e --arg base "${PROXY_BASE_URL}" '
	    .env.ANTHROPIC_BASE_URL == $base
	    and .env.ANTHROPIC_API_KEY == "dummy"
	    and .env.CLAUDE_CODE_DISABLE_ADVISOR_TOOL == "1"
	    and (has("skipDangerousModePermissionPrompt") | not)
  ' "${CLAUDE_SETTINGS}" >/dev/null || die "isolated Claude settings are invalid"

  : > "${SUMMARY_FILE}"
  chmod 600 "${SUMMARY_FILE}"

  start_proxy initial "${INITIAL_PROXY_LOG}"
  wait_for_ready initial
  fetch_models
  CHAT_MODEL="$(pick_responses_only_gpt_model)"
  export CHAT_MODEL
  printf '%s\n' "${CHAT_MODEL}" > "${SELECTED_MODEL_FILE}"
  chmod 600 "${SELECTED_MODEL_FILE}"
  log "Selected Responses-only GPT model: ${CHAT_MODEL}"

  start_claude
  wait_for_first_turn
  log "Claude is executing the delayed tool; restarting Vekil to discard replay state"
  stop_proxy_for_restart
  start_proxy restarted "${RESTARTED_PROXY_LOG}"
  [[ "${proxy_pid}" != "${stopped_proxy_pid}" ]] || die "proxy restart reused the initial PID unexpectedly"
  wait_for_ready restarted
  printf 'ready\n' > "${PROXY_READY_MARKER}"
  chmod 600 "${PROXY_READY_MARKER}"

  wait_for_claude
  assert_restarted_proxy_used_carrier
  {
    printf 'PASS proxy-restarted-before-tool-result\n'
    validate_claude_stream final
    printf 'PASS claude-copilot-secret-isolated\n'
    printf 'PASS carrier-restored-after-restart\n'
    printf 'PASS responses-only-gpt-selected\n'
  } >> "${SUMMARY_FILE}"
  write_sanitized_trace

  log "Live Claude reasoning-carrier restart smoke passed."
  log "Artifacts: ${SMOKE_DIR}"
}

main "$@"
