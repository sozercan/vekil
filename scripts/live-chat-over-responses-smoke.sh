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
PROXY_BIN="${PROXY_BIN:-${REPO_ROOT}/vekil}"
PROXY_HOST="${PROXY_HOST:-127.0.0.1}"
START_PROXY="${START_PROXY:-1}"
SMOKE_STARTUP_TIMEOUT_SECONDS="${SMOKE_STARTUP_TIMEOUT_SECONDS:-120}"
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
  die "python3 (or python) is required for isolated port allocation and SSE validation"
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
TMP_PARENT="${LIVE_CHAT_OVER_RESPONSES_SMOKE_TMP_PARENT:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}}"
SMOKE_DIR="${LIVE_CHAT_OVER_RESPONSES_SMOKE_DIR:-$(mktemp -d "${TMP_PARENT%/}/live-chat-over-responses-smoke.XXXXXX")}"
if [[ "${SMOKE_DIR}" != /* ]]; then
  SMOKE_DIR="${PWD}/${SMOKE_DIR}"
fi
umask 077
mkdir -p "${SMOKE_DIR}"
chmod 700 "${SMOKE_DIR}"

PROXY_LOG="${SMOKE_DIR}/proxy.log"
MODELS_JSON="${SMOKE_DIR}/models.json"
SELECTED_MODEL_FILE="${SMOKE_DIR}/selected-model.txt"
SUMMARY_FILE="${SMOKE_DIR}/summary.txt"
PROXY_TOKEN_DIR="${PROXY_TOKEN_DIR:-${SMOKE_DIR}/token}"
# Two minted shapes: the legacy random suffix, and the self-describing one that embeds
# Copilot's own call id. 48 is what Anthropic's 64-char cap leaves after "call_vekil_call_".
CALL_ID_PATTERN='^call_vekil_([A-Za-z0-9_-]{22}|call_[A-Za-z0-9_-]{1,48})$'

TEXT_MARKER="VEKIL_CHAT_OVER_RESPONSES_TEXT_OK"
STREAM_MARKER="VEKIL_CHAT_OVER_RESPONSES_STREAM_OK"
SINGLE_MARKER="VEKIL_CHAT_OVER_RESPONSES_SINGLE_OK"
PARALLEL_MARKER="VEKIL_CHAT_OVER_RESPONSES_PARALLEL_OK"
SINGLE_TOOL="vekil_live_single_lookup"
PARALLEL_LEFT_TOOL="vekil_live_parallel_left"
PARALLEL_RIGHT_TOOL="vekil_live_parallel_right"
PARTIAL_LEFT_TOOL="vekil_live_partial_left"
PARTIAL_RIGHT_TOOL="vekil_live_partial_right"

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

redact_stream() {
  sed -E \
    -e 's/(Authorization: (Bearer|token) )[[:graph:]]+/\1[REDACTED]/g' \
    -e 's/gh[opusr]_[A-Za-z0-9_]+/[REDACTED_GITHUB_TOKEN]/g' \
    -e 's/github_pat_[A-Za-z0-9_]+/[REDACTED_GITHUB_TOKEN]/g' \
    -e 's/("access_token"[[:space:]]*:[[:space:]]*")[^"]*(")/\1[REDACTED]\2/g' \
    -e 's/("token"[[:space:]]*:[[:space:]]*")[^"]*(")/\1[REDACTED]\2/g' \
    -e 's/("encrypted_content"[[:space:]]*:[[:space:]]*")[^"]*(")/\1[REDACTED]\2/g' \
    -e 's/(Please visit https:\/\/github\.com\/login\/device and enter code: )[A-Z0-9-]+/\1[REDACTED]/g'
}

dump_redacted_file() {
  local label="$1"
  local path="$2"
  [[ -f "${path}" ]] || return 0
  printf '%s\n' "--- ${label} ---" >&2
  head -c 32768 "${path}" | redact_stream >&2
  printf '\n' >&2
}

dump_proxy_log() {
  dump_redacted_file "proxy.log" "${PROXY_LOG}"
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

start_proxy() {
  [[ -x "${PROXY_BIN}" ]] || die "proxy binary not found or not executable: ${PROXY_BIN} (run: make build)"
  mkdir -p "${PROXY_TOKEN_DIR}"
  chmod 700 "${PROXY_TOKEN_DIR}"
  seed_access_token
  log "Starting staged proxy at ${PROXY_BASE_URL}"
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
    [split("\n")[] | fromjson? | select(.level == "info" and .msg == "vekil listening" and .addr == $addr)]
    | length > 0
  ' "${PROXY_LOG}" >/dev/null 2>&1
}

proxy_log_has_fatal() {
  [[ -f "${PROXY_LOG}" ]] || return 1
  jq -R -s -e '[split("\n")[] | fromjson? | select(.level == "fatal")] | length > 0' \
    "${PROXY_LOG}" >/dev/null 2>&1
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

pick_responses_only_model() {
  local selected
  selected="$(jq -r '
    def responses_only:
      ((.supported_endpoints // []) | index("/responses")) != null
      and ((.supported_endpoints // []) | index("/chat/completions")) == null;
    ([.data[]? | select((.id | type) == "string") | select(responses_only) | .id] as $models
      | if ($models | index("gpt-5.6-sol")) != null then "gpt-5.6-sol"
        elif ($models | length) > 0 then $models[0]
        else ""
        end)
  ' "${MODELS_JSON}")"
  if [[ -z "${selected}" ]]; then
    die "no model advertises /responses while excluding /chat/completions"
  fi
  printf '%s\n' "${selected}"
}

post_chat() {
  local label="$1"
  local request_file="$2"
  local response_file="${SMOKE_DIR}/${label}.response.json"
  local curl_error="${SMOKE_DIR}/${label}.curl.err"
  local status rc=0

  status="$(curl --silent --show-error \
    --output "${response_file}" \
    --write-out '%{http_code}' \
    --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${SMOKE_CURL_MAX_TIME_SECONDS}" \
    -X POST "${PROXY_BASE_URL}/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    --data-binary "@${request_file}" \
    2>"${curl_error}")" || rc=$?
  if [[ "${rc}" -ne 0 ]]; then
    dump_redacted_file "${label}.curl.err" "${curl_error}"
    die "${label} failed before an HTTP response (curl exit ${rc})"
  fi
  if [[ "${status}" != "200" ]]; then
    dump_redacted_file "${label}.response.json" "${response_file}"
    die "${label} returned HTTP ${status}"
  fi
  if [[ "${START_PROXY}" == "1" ]]; then
    assert_spawned_proxy_alive
  fi
  printf '%s\n' "${response_file}"
}

write_function_tools() {
  local output="$1"
  shift
  local name
  printf '[' > "${output}"
  local first=1
  for name in "$@"; do
    if [[ "${first}" == "0" ]]; then printf ',' >> "${output}"; fi
    first=0
    jq -cn --arg name "${name}" '{type:"function",function:{name:$name,description:("Return a synthetic result for " + $name + "."),parameters:{type:"object",properties:{},additionalProperties:false}}}' >> "${output}"
  done
  printf ']\n' >> "${output}"
}

assert_chat_text() {
  local response="$1"
  local marker="$2"
  local label="$3"
  if ! jq -e --arg model "${CHAT_MODEL}" --arg marker "${marker}" '
      .object == "chat.completion"
      and .model == $model
      and (.choices | length) == 1
      and .choices[0].message.role == "assistant"
      and ((.choices[0].message.content // "") | gsub("^[[:space:]]+|[[:space:]]+$"; "")) == $marker
      and .choices[0].finish_reason == "stop"
      and (.usage.total_tokens // 0) > 0
    ' "${response}" >/dev/null; then
    dump_redacted_file "${label}.response.json" "${response}"
    die "${label} returned an unexpected Chat response"
  fi
}

extract_tool_calls() {
  local response="$1"
  local output="$2"
  local expected_count="$3"
  local label="$4"
  jq -e --arg model "${CHAT_MODEL}" --argjson expected "${expected_count}" '
    .object == "chat.completion"
    and .model == $model
    and .choices[0].finish_reason == "tool_calls"
    and (.choices[0].message.tool_calls | length) == $expected
  ' "${response}" >/dev/null || {
    dump_redacted_file "${label}.response.json" "${response}"
    die "${label} returned an unexpected tool-call response"
  }
  jq '.choices[0].message.tool_calls' "${response}" > "${output}"
  if ! jq -e --arg pattern "${CALL_ID_PATTERN}" '
      all(.[]; (.id | test($pattern)) and .type == "function" and (.function.name | length) > 0 and (.function.arguments | type) == "string")
    ' "${output}" >/dev/null; then
    dump_redacted_file "${label}.tool-calls.json" "${output}"
    die "${label} returned an invalid proxy tool-call ID or shape"
  fi
}

run_text_case() {
  local request="${SMOKE_DIR}/text.request.json"
  jq -n --arg model "${CHAT_MODEL}" --arg marker "${TEXT_MARKER}" '{model:$model,max_tokens:64,messages:[{role:"user",content:("Reply with exactly " + $marker)}]}' > "${request}"
  local response
  response="$(post_chat text "${request}")"
  assert_chat_text "${response}" "${TEXT_MARKER}" text
  printf 'PASS nonstream-text\n' >> "${SUMMARY_FILE}"
}

run_stream_case() {
  local request="${SMOKE_DIR}/stream.request.json"
  local response="${SMOKE_DIR}/stream.response.sse"
  local curl_error="${SMOKE_DIR}/stream.curl.err"
  local status rc=0
  jq -n --arg model "${CHAT_MODEL}" --arg marker "${STREAM_MARKER}" '{model:$model,max_tokens:64,stream:true,stream_options:{include_usage:true},messages:[{role:"user",content:("Reply with exactly " + $marker)}]}' > "${request}"
  status="$(curl --silent --show-error --no-buffer \
    --output "${response}" --write-out '%{http_code}' \
    --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" --max-time "${SMOKE_CURL_MAX_TIME_SECONDS}" \
    -X POST "${PROXY_BASE_URL}/v1/chat/completions" -H 'Content-Type: application/json' \
    --data-binary "@${request}" 2>"${curl_error}")" || rc=$?
  if [[ "${rc}" -ne 0 || "${status}" != "200" ]]; then
    dump_redacted_file "stream.curl.err" "${curl_error}"
    dump_redacted_file "stream.response.sse" "${response}"
    die "stream request failed (curl=${rc}, HTTP=${status:-none})"
  fi
  if [[ "${START_PROXY}" == "1" ]]; then
    assert_spawned_proxy_alive
  fi
  "$(python_command)" - "${response}" "${STREAM_MARKER}" <<'PY_SSE' || {
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
marker = sys.argv[2]
raw = path.read_text()
content = ""
finish = None
usage = None
done = False
for block in raw.replace("\r\n", "\n").split("\n\n"):
    for line in block.splitlines():
        if not line.startswith("data:"):
            continue
        payload = line[5:].strip()
        if payload == "[DONE]":
            done = True
            continue
        if not payload:
            continue
        event = json.loads(payload)
        if event.get("usage"):
            usage = event["usage"]
        choices = event.get("choices") or []
        if choices:
            delta = choices[0].get("delta") or {}
            if isinstance(delta.get("content"), str):
                content += delta["content"]
            if choices[0].get("finish_reason"):
                finish = choices[0]["finish_reason"]
if content.strip() != marker or finish != "stop" or not done or not usage or usage.get("total_tokens", 0) <= 0:
    raise SystemExit(f"unexpected stream: content={content!r} finish={finish!r} done={done} usage={usage!r}")
PY_SSE
    dump_redacted_file "stream.response.sse" "${response}"
    die "stream response did not contain the expected text, finish, usage, and [DONE]"
  }
  printf 'PASS stream-text\n' >> "${SUMMARY_FILE}"
}

run_single_tool_case() {
  local tools="${SMOKE_DIR}/single.tools.json"
  local initial_request="${SMOKE_DIR}/single-initial.request.json"
  local initial_calls="${SMOKE_DIR}/single-initial.tool-calls.json"
  jq -n --arg tool "${SINGLE_TOOL}" '[{type:"function",function:{name:$tool,description:"Return one synthetic token for the requested key.",parameters:{type:"object",properties:{key:{type:"string",enum:["alpha-synthetic"]}},required:["key"],additionalProperties:false}}}]' > "${tools}"
  jq -n --arg model "${CHAT_MODEL}" --arg tool "${SINGLE_TOOL}" --arg marker "${SINGLE_MARKER}" --slurpfile tools "${tools}" '{model:$model,max_completion_tokens:512,stream:false,parallel_tool_calls:false,messages:[{role:"system",content:"Follow the tool protocol exactly. Never invent tool results."},{role:"user",content:("Call " + $tool + " exactly once with key alpha-synthetic. After its result arrives, reply with exactly " + $marker)}],tools:$tools[0],tool_choice:{type:"function",function:{name:$tool}}}' > "${initial_request}"
  local initial_response
  initial_response="$(post_chat single-initial "${initial_request}")"
  extract_tool_calls "${initial_response}" "${initial_calls}" 1 single-initial
  jq -e --arg tool "${SINGLE_TOOL}" '. == [{id:.[0].id,type:"function",function:{name:$tool,arguments:"{\"key\":\"alpha-synthetic\"}"}}]' "${initial_calls}" >/dev/null || {
    dump_redacted_file "single-initial.tool-calls.json" "${initial_calls}"
    die "single initial tool call did not match the requested function and arguments"
  }

  local continuation_request="${SMOKE_DIR}/single-continuation.request.json"
  jq -n --arg model "${CHAT_MODEL}" --arg tool "${SINGLE_TOOL}" --arg marker "${SINGLE_MARKER}" --slurpfile tools "${tools}" --slurpfile calls "${initial_calls}" '{model:$model,max_completion_tokens:512,stream:false,parallel_tool_calls:false,messages:[{role:"system",content:"Follow the tool protocol exactly. Never invent tool results."},{role:"user",content:("Call " + $tool + " exactly once with key alpha-synthetic. After its result arrives, reply with exactly " + $marker)},{role:"assistant",content:null,tool_calls:$calls[0]},{role:"tool",tool_call_id:$calls[0][0].id,content:{token:$marker}}],tools:$tools[0],tool_choice:"none"}' > "${continuation_request}"
  local continuation_response
  continuation_response="$(post_chat single-continuation "${continuation_request}")"
  assert_chat_text "${continuation_response}" "${SINGLE_MARKER}" single-continuation
  printf 'PASS single-tool-replay\n' >> "${SUMMARY_FILE}"
}

run_parallel_tool_case() {
  local tools="${SMOKE_DIR}/parallel.tools.json"
  local initial_request="${SMOKE_DIR}/parallel-initial.request.json"
  local calls="${SMOKE_DIR}/parallel-initial.tool-calls.json"
  write_function_tools "${tools}" "${PARALLEL_LEFT_TOOL}" "${PARALLEL_RIGHT_TOOL}"
  jq -n --arg model "${CHAT_MODEL}" --arg marker "${PARALLEL_MARKER}" --slurpfile tools "${tools}" '{model:$model,max_completion_tokens:768,stream:false,parallel_tool_calls:true,messages:[{role:"system",content:"Call both available tools once and wait for both results. Never invent tool results."},{role:"user",content:("Call both tools. After both results arrive, reply with exactly " + $marker)}],tools:$tools[0],tool_choice:"required"}' > "${initial_request}"
  local initial_response
  initial_response="$(post_chat parallel-initial "${initial_request}")"
  extract_tool_calls "${initial_response}" "${calls}" 2 parallel-initial
  jq -e --arg left "${PARALLEL_LEFT_TOOL}" --arg right "${PARALLEL_RIGHT_TOOL}" '
    length == 2
    and ([.[].function.name] | sort) == ([$left, $right] | sort)
    and ([.[].function.name] | unique | length) == 2
  ' "${calls}" >/dev/null || {
    dump_redacted_file "parallel-initial.tool-calls.json" "${calls}"
    die "parallel initial response did not return each declared tool exactly once"
  }

  local continuation_request="${SMOKE_DIR}/parallel-continuation.request.json"
  jq -n --arg model "${CHAT_MODEL}" --arg marker "${PARALLEL_MARKER}" --arg left "${PARALLEL_LEFT_TOOL}" --arg right "${PARALLEL_RIGHT_TOOL}" --slurpfile tools "${tools}" --slurpfile calls "${calls}" '
    ($calls[0] | map({key:.function.name,value:.id}) | from_entries) as $ids
    | {model:$model,max_completion_tokens:768,stream:false,parallel_tool_calls:true,messages:[
        {role:"system",content:"Call both available tools once and wait for both results. Never invent tool results."},
        {role:"user",content:("Call both tools. After both results arrive, reply with exactly " + $marker)},
        {role:"assistant",content:null,tool_calls:$calls[0]},
        {role:"tool",tool_call_id:$ids[$right],content:($marker + "_RIGHT")},
        {role:"tool",tool_call_id:$ids[$left],content:($marker + "_LEFT")}
      ],tools:$tools[0],tool_choice:"none"}
  ' > "${continuation_request}"
  local continuation_response
  continuation_response="$(post_chat parallel-continuation "${continuation_request}")"
  assert_chat_text "${continuation_response}" "${PARALLEL_MARKER}" parallel-continuation
  printf 'PASS parallel-results-reversed\n' >> "${SUMMARY_FILE}"
}

run_partial_tool_case() {
  local tools="${SMOKE_DIR}/partial.tools.json"
  local initial_request="${SMOKE_DIR}/partial-initial.request.json"
  local calls="${SMOKE_DIR}/partial-initial.tool-calls.json"
  write_function_tools "${tools}" "${PARTIAL_LEFT_TOOL}" "${PARTIAL_RIGHT_TOOL}"
  jq -n --arg model "${CHAT_MODEL}" --slurpfile tools "${tools}" '{model:$model,max_completion_tokens:768,stream:false,parallel_tool_calls:true,messages:[{role:"system",content:"Call both available tools once."},{role:"user",content:"Call both tools, then reissue only a missing tool when one result is provided."}],tools:$tools[0],tool_choice:"required"}' > "${initial_request}"
  local initial_response
  initial_response="$(post_chat partial-initial "${initial_request}")"
  extract_tool_calls "${initial_response}" "${calls}" 2 partial-initial
  jq -e --arg left "${PARTIAL_LEFT_TOOL}" --arg right "${PARTIAL_RIGHT_TOOL}" '
    length == 2
    and ([.[].function.name] | sort) == ([$left, $right] | sort)
    and ([.[].function.name] | unique | length) == 2
  ' "${calls}" >/dev/null || {
    dump_redacted_file "partial-initial.tool-calls.json" "${calls}"
    die "partial initial response did not return each declared tool exactly once"
  }

  local continuation_request="${SMOKE_DIR}/partial-continuation.request.json"
  local reissued_calls="${SMOKE_DIR}/partial-continuation.tool-calls.json"
  jq -n --arg model "${CHAT_MODEL}" --arg completed "${PARTIAL_LEFT_TOOL}" --arg missing "${PARTIAL_RIGHT_TOOL}" --slurpfile tools "${tools}" --slurpfile calls "${calls}" '
    ($calls[0] | map({key:.function.name,value:.id}) | from_entries) as $ids
    | {model:$model,max_completion_tokens:768,stream:false,parallel_tool_calls:true,messages:[
        {role:"system",content:"Call both available tools once."},
        {role:"user",content:"Call both tools, then reissue only a missing tool when one result is provided."},
        {role:"assistant",content:null,tool_calls:$calls[0]},
        {role:"tool",tool_call_id:$ids[$completed],content:"LEFT_RESULT_AVAILABLE"}
      ],tools:$tools[0],tool_choice:{type:"function",function:{name:$missing}}}
  ' > "${continuation_request}"
  local continuation_response
  continuation_response="$(post_chat partial-continuation "${continuation_request}")"
  extract_tool_calls "${continuation_response}" "${reissued_calls}" 1 partial-continuation
  jq -e --arg missing "${PARTIAL_RIGHT_TOOL}" --slurpfile original "${calls}" '
    ($original[0] | map({key:.function.name,value:.id}) | from_entries) as $ids
    | . == [{id:.[0].id,type:"function",function:{name:$missing,arguments:"{}"}}]
      and .[0].id != $ids[$missing]
  ' "${reissued_calls}" >/dev/null || {
    dump_redacted_file "partial-continuation.tool-calls.json" "${reissued_calls}"
    die "partial continuation did not reissue only the missing call with a fresh proxy ID"
  }
  printf 'PASS partial-missing-call-reissued\n' >> "${SUMMARY_FILE}"
}

main() {
  require_cmd curl
  require_cmd jq
  python_command >/dev/null

  : > "${SUMMARY_FILE}"
  chmod 600 "${SUMMARY_FILE}"

  if [[ "${START_PROXY}" == "1" ]]; then
    start_proxy
    wait_for_ready
  else
    log "Using existing proxy at ${PROXY_BASE_URL}"
  fi

  fetch_models
  CHAT_MODEL="$(pick_responses_only_model)"
  export CHAT_MODEL
  printf '%s\n' "${CHAT_MODEL}" > "${SELECTED_MODEL_FILE}"
  chmod 600 "${SELECTED_MODEL_FILE}"
  log "Selected Responses-only Chat model: ${CHAT_MODEL}"

  run_text_case
  run_stream_case
  run_single_tool_case
  run_parallel_tool_case
  run_partial_tool_case

  printf 'PASS responses-only-model-selected\n' >> "${SUMMARY_FILE}"
  log "Live Chat-over-Responses smoke check passed."
  log "Artifacts: ${SMOKE_DIR}"
}

main "$@"
