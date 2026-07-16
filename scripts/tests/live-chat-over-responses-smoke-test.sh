#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
SMOKE_SCRIPT="${REPO_ROOT}/scripts/live-chat-over-responses-smoke.sh"
TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/vekil-chat-responses-smoke-test.XXXXXX")"

server_pids=()
server_ports=()
MOCK_SERVER_PID=""
MOCK_SERVER_PORT=""
failures=0

log() {
  printf '==> %s\n' "$*" >&2
}

process_is_running() {
  local pid="$1"
  local state
  kill -0 "${pid}" 2>/dev/null || return 1
  state="$(ps -o stat= -p "${pid}" 2>/dev/null | awk 'NR == 1 { print $1 }')"
  [[ "${state}" != Z* ]]
}

port_accepts_tcp() {
  python3 - "$1" <<'PY_PORT'
import socket
import sys

try:
    with socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=0.2):
        pass
except OSError:
    raise SystemExit(1)
PY_PORT
}

stop_servers() {
  local pid attempt
  for pid in "${server_pids[@]:-}"; do
    [[ -n "${pid}" ]] || continue
    if process_is_running "${pid}"; then
      kill -TERM "${pid}" 2>/dev/null || true
      for ((attempt = 0; attempt < 50; attempt++)); do
        process_is_running "${pid}" || break
        sleep 0.05
      done
      if process_is_running "${pid}"; then
        kill -KILL "${pid}" 2>/dev/null || true
      fi
    fi
    wait "${pid}" 2>/dev/null || true
  done
}

cleanup() {
  stop_servers
  rm -rf "${TMP_ROOT}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

cat > "${TMP_ROOT}/mock_chat_server.py" <<'PY_SERVER'
import argparse
import json
import os
import pathlib
import signal
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

parser = argparse.ArgumentParser()
parser.add_argument("--host", default="127.0.0.1")
parser.add_argument("--port", type=int, default=0)
parser.add_argument("--port-file", required=True)
parser.add_argument("--state-file", required=True)
parser.add_argument("--scenario", choices=("success", "no-model", "redaction"), default="success")
parser.add_argument("--secret", default="")
args = parser.parse_args()

MODEL = "gpt-5.6-sol"
TEXT_MARKER = "VEKIL_CHAT_OVER_RESPONSES_TEXT_OK"
STREAM_MARKER = "VEKIL_CHAT_OVER_RESPONSES_STREAM_OK"
SINGLE_MARKER = "VEKIL_CHAT_OVER_RESPONSES_SINGLE_OK"
PARALLEL_MARKER = "VEKIL_CHAT_OVER_RESPONSES_PARALLEL_OK"
SINGLE_TOOL = "vekil_live_single_lookup"
PARALLEL_TOOLS = ("vekil_live_parallel_left", "vekil_live_parallel_right")
PARTIAL_TOOLS = ("vekil_live_partial_left", "vekil_live_partial_right")
IDS = {
    SINGLE_TOOL: "call_vekil_" + "A" * 22,
    PARALLEL_TOOLS[0]: "call_vekil_" + "B" * 22,
    PARALLEL_TOOLS[1]: "call_vekil_" + "C" * 22,
    PARTIAL_TOOLS[0]: "call_vekil_" + "D" * 22,
    PARTIAL_TOOLS[1]: "call_vekil_" + "E" * 22,
    "partial_reissue": "call_vekil_" + "F" * 22,
}
state = {"scenario": args.scenario, "requests": [], "errors": []}


def persist():
    pathlib.Path(args.state_file).write_text(json.dumps(state, indent=2, sort_keys=True))


def fail(message):
    state["errors"].append(message)
    persist()
    return {"error": {"message": message, "type": "mock_validation_error"}}


def chat_text(text):
    return {
        "id": "chatcmpl_mock_text",
        "object": "chat.completion",
        "created": 1,
        "model": MODEL,
        "choices": [{"index": 0, "message": {"role": "assistant", "content": text}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10},
    }


def chat_tools(calls):
    return {
        "id": "chatcmpl_mock_tools",
        "object": "chat.completion",
        "created": 1,
        "model": MODEL,
        "choices": [{
            "index": 0,
            "message": {"role": "assistant", "content": None, "tool_calls": calls},
            "finish_reason": "tool_calls",
        }],
        "usage": {"prompt_tokens": 9, "completion_tokens": 4, "total_tokens": 13},
    }


def call(name, arguments="{}", call_id=None):
    return {
        "id": call_id or IDS[name],
        "type": "function",
        "function": {"name": name, "arguments": arguments},
    }


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, _format, *_args):
        return

    def send_bytes(self, status, content_type, data):
        self.send_response(status)
        self.send_header("content-type", content_type)
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def send_json(self, status, payload):
        self.send_bytes(status, "application/json", json.dumps(payload, separators=(",", ":")).encode())

    def do_GET(self):
        if self.path == "/readyz":
            self.send_json(200, {"status": "ready"})
            return
        if self.path == "/healthz":
            self.send_json(200, {"status": "ok"})
            return
        if self.path == "/v1/models":
            if args.scenario == "no-model":
                data = [
                    {"id": "chat-only", "supported_endpoints": ["/chat/completions"]},
                    {"id": "hybrid", "supported_endpoints": ["/responses", "/chat/completions"]},
                ]
            else:
                data = [
                    {"id": "fallback-responses-only", "supported_endpoints": ["/responses"]},
                    {"id": "hybrid", "supported_endpoints": ["/responses", "/chat/completions"]},
                    {"id": MODEL, "supported_endpoints": ["/responses"], "capabilities": {"supports": {"parallel_tool_calls": True}}},
                    {"id": "chat-only", "supported_endpoints": ["/chat/completions"]},
                ]
            self.send_json(200, {"object": "list", "data": data})
            return
        self.send_json(404, {"error": {"message": "not found"}})

    def do_POST(self):
        length = int(self.headers.get("content-length", "0"))
        raw = self.rfile.read(length)
        try:
            body = json.loads(raw)
        except Exception as exc:
            self.send_json(400, fail(f"invalid JSON: {exc}"))
            return
        state["requests"].append(body)
        persist()

        if self.path != "/v1/chat/completions":
            self.send_json(404, {"error": {"message": "not found"}})
            return
        if args.scenario == "redaction":
            payload = {
                "error": {"message": f"Authorization: Bearer {args.secret}"},
                "token": args.secret,
                "encrypted_content": "CIPHER_SHOULD_NOT_LEAK",
            }
            self.send_json(500, payload)
            return
        if body.get("model") != MODEL:
            self.send_json(400, fail(f"unexpected model: {body.get('model')!r}"))
            return

        messages = body.get("messages") or []
        tools = body.get("tools") or []
        tool_names = [tool.get("function", {}).get("name") for tool in tools]
        for index, tool in enumerate(tools):
            if "strict" in tool.get("function", {}):
                self.send_json(400, fail(f"tools[{index}].function.strict must be omitted by the public smoke payload"))
                return
        tool_results = [message for message in messages if message.get("role") == "tool"]
        assistant_messages = [message for message in messages if message.get("role") == "assistant" and message.get("tool_calls")]
        prompt = "\n".join(str(message.get("content", "")) for message in messages if message.get("role") == "user")

        if body.get("stream") is True:
            if STREAM_MARKER not in prompt or tools:
                self.send_json(400, fail("unexpected streaming request"))
                return
            events = [
                {"id": "chatcmpl_mock_stream", "object": "chat.completion.chunk", "created": 1, "model": MODEL, "choices": [{"index": 0, "delta": {"role": "assistant", "content": "VEKIL_CHAT_OVER_"}, "finish_reason": None}]},
                {"id": "chatcmpl_mock_stream", "object": "chat.completion.chunk", "created": 1, "model": MODEL, "choices": [{"index": 0, "delta": {"content": "RESPONSES_STREAM_OK"}, "finish_reason": None}]},
                {"id": "chatcmpl_mock_stream", "object": "chat.completion.chunk", "created": 1, "model": MODEL, "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]},
                {"id": "chatcmpl_mock_stream", "object": "chat.completion.chunk", "created": 1, "model": MODEL, "choices": [], "usage": {"prompt_tokens": 8, "completion_tokens": 4, "total_tokens": 12}},
            ]
            data = "".join(f"data: {json.dumps(event, separators=(',', ':'))}\n\n" for event in events)
            data += "data: [DONE]\n\n"
            self.send_bytes(200, "text/event-stream", data.encode())
            return

        if not tools:
            if TEXT_MARKER not in prompt:
                self.send_json(400, fail("unexpected non-stream text request"))
                return
            self.send_json(200, chat_text(TEXT_MARKER))
            return

        if tool_names == [SINGLE_TOOL]:
            if not tool_results:
                choice = body.get("tool_choice", {})
                if choice.get("function", {}).get("name") != SINGLE_TOOL or body.get("parallel_tool_calls") is not False:
                    self.send_json(400, fail("single-call request did not force the expected tool"))
                    return
                self.send_json(200, chat_tools([call(SINGLE_TOOL, '{"key":"alpha-synthetic"}')]))
                return
            if len(tool_results) != 1 or not assistant_messages:
                self.send_json(400, fail("single-call continuation shape mismatch"))
                return
            if tool_results[0].get("tool_call_id") != IDS[SINGLE_TOOL] or body.get("tool_choice") != "none":
                self.send_json(400, fail("single-call continuation used the wrong tool id or choice"))
                return
            self.send_json(200, chat_text(SINGLE_MARKER))
            return

        if tuple(tool_names) == PARALLEL_TOOLS:
            if not tool_results:
                if body.get("tool_choice") != "required" or body.get("parallel_tool_calls") is not True:
                    self.send_json(400, fail("parallel request did not require parallel tools"))
                    return
                self.send_json(200, chat_tools([call(PARALLEL_TOOLS[0]), call(PARALLEL_TOOLS[1])]))
                return
            if len(tool_results) != 2 or not assistant_messages:
                self.send_json(400, fail("parallel continuation shape mismatch"))
                return
            got_ids = [message.get("tool_call_id") for message in tool_results]
            want_ids = [IDS[PARALLEL_TOOLS[1]], IDS[PARALLEL_TOOLS[0]]]
            if got_ids != want_ids or body.get("tool_choice") != "none":
                self.send_json(400, fail(f"parallel results were not reversed: {got_ids!r}"))
                return
            self.send_json(200, chat_text(PARALLEL_MARKER))
            return

        if tuple(tool_names) == PARTIAL_TOOLS:
            if not tool_results:
                if body.get("tool_choice") != "required" or body.get("parallel_tool_calls") is not True:
                    self.send_json(400, fail("partial setup did not request both tools"))
                    return
                self.send_json(200, chat_tools([call(PARTIAL_TOOLS[0]), call(PARTIAL_TOOLS[1])]))
                return
            if len(tool_results) != 1 or not assistant_messages:
                self.send_json(400, fail("partial continuation shape mismatch"))
                return
            choice = body.get("tool_choice", {})
            if tool_results[0].get("tool_call_id") != IDS[PARTIAL_TOOLS[0]] or choice.get("function", {}).get("name") != PARTIAL_TOOLS[1]:
                self.send_json(400, fail("partial continuation did not complete one call and force the missing call"))
                return
            self.send_json(200, chat_tools([call(PARTIAL_TOOLS[1], call_id=IDS["partial_reissue"])]))
            return

        self.send_json(400, fail(f"unexpected tools: {tool_names!r}"))


persist()
server = ThreadingHTTPServer((args.host, args.port), Handler)
pathlib.Path(args.port_file).write_text(str(server.server_address[1]))


def stop(_signum, _frame):
    raise SystemExit(0)


signal.signal(signal.SIGTERM, stop)
try:
    server.serve_forever()
finally:
    server.server_close()
    persist()
PY_SERVER

wait_for_file() {
  local path="$1"
  local attempt
  for ((attempt = 0; attempt < 100; attempt++)); do
    [[ -s "${path}" ]] && return 0
    sleep 0.05
  done
  return 1
}

start_mock_server() {
  local case_dir="$1"
  local scenario="${2:-success}"
  local port="${3:-0}"
  local secret="${4:-}"
  local port_file="${case_dir}/port"
  local state_file="${case_dir}/state.json"
  mkdir -p "${case_dir}"
  python3 "${TMP_ROOT}/mock_chat_server.py" \
    --host 127.0.0.1 \
    --port "${port}" \
    --port-file "${port_file}" \
    --state-file "${state_file}" \
    --scenario "${scenario}" \
    --secret "${secret}" \
    >"${case_dir}/server.log" 2>&1 &
  MOCK_SERVER_PID=$!
  server_pids+=("${MOCK_SERVER_PID}")
  if ! wait_for_file "${port_file}"; then
    cat "${case_dir}/server.log" >&2 || true
    return 1
  fi
  MOCK_SERVER_PORT="$(cat "${port_file}")"
  server_ports+=("${MOCK_SERVER_PORT}")
}

write_bind_failure_proxy() {
  local path="$1"
  mkdir -p "$(dirname "${path}")"
  cat > "${path}" <<'EOF_PROXY'
#!/usr/bin/env bash
printf '{"level":"fatal","msg":"serve error","error":"server start error: listen: address already in use"}\n'
exit 1
EOF_PROXY
  chmod +x "${path}"
}

write_healthy_proxy() {
  local path="$1"
  local state_file="$2"
  local port_file="$3"
  local child_pid_file="$4"
  mkdir -p "$(dirname "${path}")"
  cat > "${path}" <<EOF_PROXY
#!/usr/bin/env bash
set -euo pipefail
host=127.0.0.1
port=
token_dir=
while [[ \$# -gt 0 ]]; do
  case "\$1" in
    --host) host="\$2"; shift 2 ;;
    --port) port="\$2"; shift 2 ;;
    --token-dir) token_dir="\$2"; shift 2 ;;
    *) shift ;;
  esac
done
python3 - "\${token_dir}" <<'PY_TOKEN'
import os
import pathlib
import stat
import sys

path = pathlib.Path(sys.argv[1]) / "access-token"
expected_dir = pathlib.Path(os.environ["LIVE_CHAT_OVER_RESPONSES_SMOKE_DIR"]) / "token"
if pathlib.Path(sys.argv[1]) != expected_dir:
    raise SystemExit(f"unexpected token dir: {sys.argv[1]}")
if path.read_text().strip() != os.environ["COPILOT_GITHUB_TOKEN"]:
    raise SystemExit("seeded token mismatch")
if stat.S_IMODE(path.stat().st_mode) & 0o077:
    raise SystemExit("seeded token permissions are not owner-only")
PY_TOKEN
sleep 300 &
printf '%s\n' "\$!" > "${child_pid_file}"
printf '{"addr":"%s:%s","level":"info","msg":"vekil listening","time":"2026-07-16T00:00:00Z"}\n' "\${host}" "\${port}"
exec python3 "${TMP_ROOT}/mock_chat_server.py" \
  --host "\${host}" \
  --port "\${port}" \
  --port-file "${port_file}" \
  --state-file "${state_file}" \
  --scenario success
EOF_PROXY
  chmod +x "${path}"
}

run_bounded() {
  local timeout_seconds="$1"
  local stdout_file="$2"
  local stderr_file="$3"
  shift 3

  set +e
  python3 - "${timeout_seconds}" "${stdout_file}" "${stderr_file}" "$@" <<'PY_RUN'
import os
import signal
import subprocess
import sys

seconds = float(sys.argv[1])
stdout_path = sys.argv[2]
stderr_path = sys.argv[3]
command = sys.argv[4:]
with open(stdout_path, "wb") as stdout, open(stderr_path, "wb") as stderr:
    process = subprocess.Popen(command, stdout=stdout, stderr=stderr, start_new_session=True)
    try:
        code = process.wait(timeout=seconds)
    except subprocess.TimeoutExpired:
        os.killpg(process.pid, signal.SIGTERM)
        try:
            process.wait(timeout=1)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()
        code = 124
raise SystemExit(code)
PY_RUN
  local rc=$?
  set -e
  return "${rc}"
}

record_success() {
  printf 'ok - %s\n' "$1" >&2
}

record_failure() {
  failures=$((failures + 1))
  printf 'not ok - %s: %s\n' "$1" "$2" >&2
}

expect_success() {
  local name="$1"
  local timeout_seconds="$2"
  shift 2
  local result_dir="${TMP_ROOT}/results/${name//[^a-zA-Z0-9_.-]/_}"
  mkdir -p "${result_dir}"
  local rc=0
  run_bounded "${timeout_seconds}" "${result_dir}/stdout" "${result_dir}/stderr" "$@" || rc=$?
  if [[ "${rc}" -ne 0 ]]; then
    record_failure "${name}" "command failed with ${rc}"
    cat "${result_dir}/stderr" >&2 || true
    return 1
  fi
  record_success "${name}"
}

expect_failure_matching() {
  local name="$1"
  local timeout_seconds="$2"
  local pattern="$3"
  shift 3
  local result_dir="${TMP_ROOT}/results/${name//[^a-zA-Z0-9_.-]/_}"
  mkdir -p "${result_dir}"
  local rc=0
  run_bounded "${timeout_seconds}" "${result_dir}/stdout" "${result_dir}/stderr" "$@" || rc=$?
  if [[ "${rc}" -eq 0 ]]; then
    record_failure "${name}" "command unexpectedly succeeded"
    return 1
  fi
  if [[ "${rc}" -eq 124 ]]; then
    record_failure "${name}" "command exceeded ${timeout_seconds}s outer deadline"
    cat "${result_dir}/stderr" >&2 || true
    return 1
  fi
  if ! grep -Eq "${pattern}" "${result_dir}/stderr"; then
    record_failure "${name}" "stderr did not match ${pattern}"
    cat "${result_dir}/stderr" >&2 || true
    return 1
  fi
  record_success "${name}"
}

log "Running dedicated Chat-over-Responses smoke regressions"

if [[ ! -x "${SMOKE_SCRIPT}" ]]; then
  record_failure "smoke script exists" "missing executable ${SMOKE_SCRIPT}"
else
  record_success "smoke script exists"
fi

success_dir="${TMP_ROOT}/success"
start_mock_server "${success_dir}/server" success
success_port="${MOCK_SERVER_PORT}"
if expect_success "full mock behavior and preferred model selection" 15 \
  env START_PROXY=0 PROXY_HOST=127.0.0.1 PROXY_PORT="${success_port}" \
    LIVE_CHAT_OVER_RESPONSES_SMOKE_DIR="${success_dir}/smoke" \
    SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1 SMOKE_CURL_MAX_TIME_SECONDS=3 \
    "${SMOKE_SCRIPT}"; then
  if [[ "$(cat "${success_dir}/smoke/selected-model.txt" 2>/dev/null || true)" != "gpt-5.6-sol" ]]; then
    record_failure "preferred model selection" "gpt-5.6-sol was not selected"
  elif ! jq -e '.errors == [] and (.requests | length) == 8 and all(.requests[]; .model == "gpt-5.6-sol")' \
      "${success_dir}/server/state.json" >/dev/null; then
    record_failure "full mock behavior" "mock server did not observe the eight expected requests"
    cat "${success_dir}/server/state.json" >&2 || true
  elif ! grep -q '^PASS partial-missing-call-reissued$' "${success_dir}/smoke/summary.txt"; then
    record_failure "success summary" "partial replay assertion was not recorded"
  elif ! python3 - "${success_dir}/smoke" <<'PY_PERMS'
import pathlib
import stat
import sys

root = pathlib.Path(sys.argv[1])
paths = [root, *root.rglob("*")]
for path in paths:
    mode = stat.S_IMODE(path.stat().st_mode)
    if mode & 0o077:
        raise SystemExit(f"non-owner permission bits on {path}: {mode:o}")
PY_PERMS
  then
    record_failure "owner-only artifacts" "smoke artifacts exposed group/other permission bits"
  else
    record_success "preferred model, full behavior, summary, and owner-only artifacts"
  fi
fi

no_model_dir="${TMP_ROOT}/no-model"
start_mock_server "${no_model_dir}/server" no-model
expect_failure_matching "no Responses-only model is a hard failure" 8 \
  'no model advertises /responses while excluding /chat/completions' \
  env START_PROXY=0 PROXY_HOST=127.0.0.1 PROXY_PORT="${MOCK_SERVER_PORT}" \
    LIVE_CHAT_OVER_RESPONSES_SMOKE_DIR="${no_model_dir}/smoke" \
    SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1 SMOKE_CURL_MAX_TIME_SECONDS=2 \
    "${SMOKE_SCRIPT}"

redaction_dir="${TMP_ROOT}/redaction"
redaction_secret="SYNTHETIC_COPILOT_TOKEN_SHOULD_NOT_LEAK"
start_mock_server "${redaction_dir}/server" redaction 0 "${redaction_secret}"
if expect_failure_matching "failure output is bounded and redacted" 8 '\[REDACTED' \
  env START_PROXY=0 PROXY_HOST=127.0.0.1 PROXY_PORT="${MOCK_SERVER_PORT}" \
    COPILOT_GITHUB_TOKEN="${redaction_secret}" \
    LIVE_CHAT_OVER_RESPONSES_SMOKE_DIR="${redaction_dir}/smoke" \
    SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1 SMOKE_CURL_MAX_TIME_SECONDS=2 \
    "${SMOKE_SCRIPT}"; then
  redaction_stderr="${TMP_ROOT}/results/failure_output_is_bounded_and_redacted/stderr"
  if grep -Fq "${redaction_secret}" "${redaction_stderr}" || grep -Fq 'CIPHER_SHOULD_NOT_LEAK' "${redaction_stderr}"; then
    record_failure "failure redaction" "secret or encrypted content leaked to stderr"
  elif [[ "$(wc -c < "${redaction_stderr}")" -gt 65536 ]]; then
    record_failure "failure output bound" "stderr exceeded 64 KiB"
  else
    record_success "failure output redacts credentials/encrypted content and stays bounded"
  fi
fi

stale_dir="${TMP_ROOT}/stale-listener"
start_mock_server "${stale_dir}/server" success
stale_port="${MOCK_SERVER_PORT}"
write_bind_failure_proxy "${stale_dir}/bind-failure-proxy"
expect_failure_matching "stale listener cannot satisfy spawned proxy readiness" 8 \
  'spawned proxy (logged a fatal startup error|PID .* exited before readiness)' \
  env START_PROXY=1 PROXY_HOST=127.0.0.1 PROXY_PORT="${stale_port}" \
    PROXY_BIN="${stale_dir}/bind-failure-proxy" COPILOT_GITHUB_TOKEN=synthetic-test-token \
    LIVE_CHAT_OVER_RESPONSES_SMOKE_DIR="${stale_dir}/smoke" \
    SMOKE_STARTUP_TIMEOUT_SECONDS=2 SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1 \
    SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS=1 SMOKE_CURL_MAX_TIME_SECONDS=2 \
    "${SMOKE_SCRIPT}"

cleanup_dir="${TMP_ROOT}/cleanup"
mkdir -p "${cleanup_dir}"
write_healthy_proxy "${cleanup_dir}/healthy-proxy" "${cleanup_dir}/state.json" \
  "${cleanup_dir}/port-file" "${cleanup_dir}/child.pid"
if expect_success "spawned process group and port are released" 20 \
  env START_PROXY=1 PROXY_HOST=127.0.0.1 PROXY_BIN="${cleanup_dir}/healthy-proxy" \
    COPILOT_GITHUB_TOKEN=synthetic-test-token \
    LIVE_CHAT_OVER_RESPONSES_SMOKE_DIR="${cleanup_dir}/smoke" \
    SMOKE_STARTUP_TIMEOUT_SECONDS=3 SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1 \
    SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS=1 SMOKE_CURL_MAX_TIME_SECONDS=3 \
    SMOKE_PROCESS_TERM_GRACE_SECONDS=1 SMOKE_PORT_RELEASE_TIMEOUT_SECONDS=3 \
    "${SMOKE_SCRIPT}"; then
  cleanup_stderr="${TMP_ROOT}/results/spawned_process_group_and_port_are_released/stderr"
  cleanup_port="$(sed -nE 's/.*Starting staged proxy at http:\/\/127\.0\.0\.1:([0-9]+).*/\1/p' "${cleanup_stderr}" | head -1)"
  child_pid="$(cat "${cleanup_dir}/child.pid" 2>/dev/null || true)"
  if [[ -z "${cleanup_port}" || "${cleanup_port}" == "1337" ]]; then
    record_failure "auto port selection" "unexpected port ${cleanup_port:-<missing>}"
  elif port_accepts_tcp "${cleanup_port}"; then
    record_failure "port cleanup" "port ${cleanup_port} still accepts TCP"
  elif [[ -z "${child_pid}" ]]; then
    record_failure "process-group cleanup" "fake proxy did not record its child PID"
  elif process_is_running "${child_pid}"; then
    kill -KILL "${child_pid}" 2>/dev/null || true
    record_failure "process-group cleanup" "child PID ${child_pid} remained alive"
  else
    record_success "auto non-default port, isolated token dir, process-group cleanup, and port release"
  fi
fi

stop_servers
leaks=0
for pid in "${server_pids[@]:-}"; do
  if [[ -n "${pid}" ]] && process_is_running "${pid}"; then
    record_failure "mock server cleanup" "PID ${pid} remained alive"
    leaks=1
  fi
done
for port in "${server_ports[@]:-}"; do
  if [[ -n "${port}" ]] && port_accepts_tcp "${port}"; then
    record_failure "mock listener cleanup" "127.0.0.1:${port} remained open"
    leaks=1
  fi
done
if [[ "${leaks}" == "0" ]]; then
  record_success "all focused mock servers and listeners were cleaned up"
fi

if [[ "${failures}" -ne 0 ]]; then
  printf '%s dedicated Chat-over-Responses smoke regression(s) failed\n' "${failures}" >&2
  exit 1
fi

log "All dedicated Chat-over-Responses smoke regressions passed"
