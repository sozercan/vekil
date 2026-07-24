#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
SMOKE_SCRIPT="${REPO_ROOT}/scripts/live-policy-routing-smoke.sh"
TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/vekil-policy-routing-smoke-test.XXXXXX")"
umask 077

MOCK_SERVER_PIDS=()
MOCK_SERVER_PORTS=()
TEST_PROXY_BIN="${TMP_ROOT}/vekil"
TEST_PROXY_WRAPPER="${TMP_ROOT}/vekil-startup-wrapper.sh"
PROXY_WRAPPER_ATTEMPTS="${TMP_ROOT}/proxy-wrapper-attempts.txt"
SMOKE_DIR="${TMP_ROOT}/smoke"
HARNESS_STDOUT="${TMP_ROOT}/harness.stdout"
HARNESS_STDERR="${TMP_ROOT}/harness.stderr"

LIGHTWEIGHT_SECRET="local-lightweight-credential-62b1edc1"
PRIMARY_SECRET="local-primary-credential-f4188ca2"
SECONDARY_SECRET="local-secondary-credential-7037feda"
LIGHTWEIGHT_MODEL="local-lightweight-model"
PRIMARY_MODEL="local-powerful-primary-model"
SECONDARY_MODEL="local-powerful-secondary-model"
CLASSIFIER_MODEL="local-classifier-model"
PUBLIC_MODEL="vekil-local-policy-smoke"

log() {
  printf '==> %s\n' "$*" >&2
}

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

process_is_running() {
  local pid="$1"
  local state
  kill -0 "${pid}" 2>/dev/null || return 1
  state="$(ps -o stat= -p "${pid}" 2>/dev/null | awk 'NR == 1 { print $1 }')"
  [[ "${state}" != Z* ]]
}

port_accepts_tcp() {
  python3 - "$1" <<'PY_PORT' >/dev/null 2>&1
import socket
import sys

try:
    with socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=0.2):
        pass
except OSError:
    raise SystemExit(1)
PY_PORT
}

wait_for_file() {
  local path="$1"
  local attempt
  for ((attempt = 0; attempt < 200; attempt++)); do
    [[ -s "${path}" ]] && return 0
    sleep 0.05
  done
  return 1
}

wait_for_port_release() {
  local port="$1"
  local attempt
  for ((attempt = 0; attempt < 100; attempt++)); do
    if ! port_accepts_tcp "${port}"; then
      return 0
    fi
    sleep 0.05
  done
  return 1
}

file_mode() {
  local path="$1"
  if stat -f '%Lp' "${path}" >/dev/null 2>&1; then
    stat -f '%Lp' "${path}"
  else
    stat -c '%a' "${path}"
  fi
}

redact_file() {
  local source="$1"
  local destination="$2"
  python3 - "${source}" "${destination}" \
    "${LIGHTWEIGHT_SECRET}" "${PRIMARY_SECRET}" "${SECONDARY_SECRET}" <<'PY_REDACT'
import pathlib
import re
import sys

source = pathlib.Path(sys.argv[1])
destination = pathlib.Path(sys.argv[2])
secrets = [value for value in sys.argv[3:] if value]
text = source.read_text(encoding="utf-8", errors="replace")
for value in sorted(set(secrets), key=len, reverse=True):
    text = text.replace(value, "[REDACTED]")
text = re.sub(r"(?im)^(authorization|api-key|x-api-key)\s*:\s*.*$", r"\1: [REDACTED]", text)
destination.write_text(text, encoding="utf-8")
PY_REDACT
}

assert_no_test_credentials() {
  local path="$1"
  [[ -f "${path}" ]] || return 0
  python3 - "${path}" \
    "${LIGHTWEIGHT_SECRET}" "${PRIMARY_SECRET}" "${SECONDARY_SECRET}" <<'PY_NO_SECRETS'
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
data = path.read_text(encoding="utf-8", errors="replace")
for secret in sys.argv[2:]:
    if secret and secret in data:
        raise SystemExit(f"{path}: test credential appeared in diagnostics")
PY_NO_SECRETS
}

stop_mock_servers() {
  local pid attempt
  for pid in "${MOCK_SERVER_PIDS[@]:-}"; do
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

dump_diagnostics() {
  local path redacted index=0
  for path in \
    "${HARNESS_STDOUT}" \
    "${HARNESS_STDERR}" \
    "${SMOKE_DIR}/summary.txt" \
    "${SMOKE_DIR}/preflight-offline.log" \
    "${SMOKE_DIR}/preflight-live.log" \
    "${SMOKE_DIR}/powerful-primary-shim.log" \
    "${TMP_ROOT}/lightweight/state.json" \
    "${TMP_ROOT}/primary/state.json" \
    "${TMP_ROOT}/secondary/state.json"; do
    if [[ -f "${path}" ]]; then
      redacted="${TMP_ROOT}/diagnostic-${index}.redacted"
      redact_file "${path}" "${redacted}"
      printf '%s\n' "--- ${path} (redacted) ---" >&2
      head -c 32768 "${redacted}" >&2 || true
      printf '\n' >&2
      index=$((index + 1))
    fi
  done
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  stop_mock_servers
  if [[ "${rc}" -ne 0 ]]; then
    dump_diagnostics
  fi
  if [[ "${KEEP_LIVE_POLICY_ROUTING_TEST_ARTIFACTS:-0}" == "1" ]]; then
    log "Preserving test artifacts: ${TMP_ROOT}"
  else
    rm -rf "${TMP_ROOT}"
  fi
  exit "${rc}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

write_mock_server() {
  cat > "${TMP_ROOT}/mock_chat_server.py" <<'PY_SERVER'
#!/usr/bin/env python3
import argparse
import json
import pathlib
import signal
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

parser = argparse.ArgumentParser()
parser.add_argument("--host", default="127.0.0.1")
parser.add_argument("--port", type=int, default=0)
parser.add_argument("--port-file", required=True)
parser.add_argument("--state-file", required=True)
parser.add_argument("--role", choices=("lightweight", "primary", "secondary"), required=True)
parser.add_argument("--terminal-model", required=True)
parser.add_argument("--classifier-model", default="")
parser.add_argument("--secret", required=True)
args = parser.parse_args()

state_path = pathlib.Path(args.state_file)
state_lock = threading.Lock()
state = {"role": args.role, "requests": [], "errors": []}
request_sequence = 0


def persist():
    temporary = state_path.with_suffix(state_path.suffix + ".tmp")
    temporary.write_text(json.dumps(state, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    temporary.replace(state_path)


def record(entry, errors):
    global request_sequence
    with state_lock:
        request_sequence += 1
        entry["sequence"] = request_sequence
        state["requests"].append(entry)
        state["errors"].extend(errors)
        persist()
        return request_sequence


def completion(model, content):
    return {
        "id": "chatcmpl_local_text",
        "object": "chat.completion",
        "created": 1,
        "model": model,
        "choices": [{
            "index": 0,
            "message": {"role": "assistant", "content": content},
            "finish_reason": "stop",
        }],
        "usage": {"prompt_tokens": 12, "completion_tokens": 4, "total_tokens": 16},
    }


def tool_completion(model, calls):
    return {
        "id": "chatcmpl_local_tools",
        "object": "chat.completion",
        "created": 1,
        "model": model,
        "choices": [{
            "index": 0,
            "message": {"role": "assistant", "content": None, "tool_calls": calls},
            "finish_reason": "tool_calls",
        }],
        "usage": {"prompt_tokens": 18, "completion_tokens": 8, "total_tokens": 26},
    }


def function_call(call_id, name, arguments):
    return {
        "id": call_id,
        "type": "function",
        "function": {"name": name, "arguments": json.dumps(arguments, separators=(",", ":"))},
    }


def classifier_signals(body):
    messages = body.get("messages") if isinstance(body, dict) else None
    facts = ""
    if isinstance(messages, list):
        for message in messages:
            if isinstance(message, dict) and message.get("role") == "user" and isinstance(message.get("content"), str):
                facts = message["content"]
    lowered = facts.lower()
    powerful_terms = (
        "cross-module", "cross_module", "multi-file", "multi_file", "multiple files",
        "debug", "review", "migration", "security and cancellation", "coordinated edits",
    )
    powerful = any(term in lowered for term in powerful_terms)
    if powerful:
        return {
            "abstain": False,
            "turn_type": "debug",
            "code_scope": "cross_module",
            "tool_call_count_estimate": 4,
            "modifying_tool_call_count_estimate": 2,
            "requires_codebase_context": True,
            "risk_level": "medium",
        }, True
    return {
        "abstain": False,
        "turn_type": "lookup",
        "code_scope": "function",
        "tool_call_count_estimate": 1,
        "modifying_tool_call_count_estimate": 0,
        "requires_codebase_context": False,
        "risk_level": "low",
    }, False


def sse_text(model, content):
    events = [
        {
            "id": "chatcmpl_local_stream",
            "object": "chat.completion.chunk",
            "created": 1,
            "model": model,
            "choices": [{"index": 0, "delta": {"role": "assistant", "content": content}, "finish_reason": None}],
        },
        {
            "id": "chatcmpl_local_stream",
            "object": "chat.completion.chunk",
            "created": 1,
            "model": model,
            "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 12, "completion_tokens": 4, "total_tokens": 16},
        },
    ]
    return "".join(f"data: {json.dumps(event, separators=(',', ':'))}\n\n" for event in events) + "data: [DONE]\n\n"


def sse_tools(model, calls):
    first_calls = []
    for index, call in enumerate(calls):
        item = dict(call)
        item["index"] = index
        first_calls.append(item)
    events = [
        {
            "id": "chatcmpl_local_tool_stream",
            "object": "chat.completion.chunk",
            "created": 1,
            "model": model,
            "choices": [{
                "index": 0,
                "delta": {"role": "assistant", "tool_calls": first_calls},
                "finish_reason": None,
            }],
        },
        {
            "id": "chatcmpl_local_tool_stream",
            "object": "chat.completion.chunk",
            "created": 1,
            "model": model,
            "choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}],
            "usage": {"prompt_tokens": 18, "completion_tokens": 8, "total_tokens": 26},
        },
    ]
    return "".join(f"data: {json.dumps(event, separators=(',', ':'))}\n\n" for event in events) + "data: [DONE]\n\n"


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, _format, *_args):
        return

    def send_bytes(self, status, content_type, data):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Connection", "close")
        self.send_header("X-Request-ID", f"local-{args.role}-request-id-must-not-leak")
        self.send_header("Request-ID", f"local-{args.role}-legacy-id-must-not-leak")
        self.end_headers()
        if data:
            try:
                self.wfile.write(data)
                self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError):
                pass

    def send_json(self, status, payload):
        self.send_bytes(status, "application/json", json.dumps(payload, separators=(",", ":")).encode())

    def do_GET(self):
        if self.path == "/v1/models":
            models = [args.terminal_model]
            if args.classifier_model:
                models.append(args.classifier_model)
            self.send_json(200, {
                "object": "list",
                "data": [{"id": model, "object": "model", "owned_by": "local-test"} for model in models],
            })
            return
        self.send_json(404, {"error": {"type": "not_found", "message": "not found"}})

    def do_POST(self):
        errors = []
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            length = 0
            errors.append("content_length_invalid")
        raw = self.rfile.read(length) if length > 0 else b""
        try:
            body = json.loads(raw)
        except (UnicodeDecodeError, json.JSONDecodeError):
            body = None
            errors.append("body_invalid_json")

        authorization = self.headers.get("Authorization", "")
        if authorization != f"Bearer {args.secret}":
            errors.append("authorization_invalid")
        if self.headers.get("api-key"):
            errors.append("unexpected_api_key")
        if self.path != "/v1/chat/completions":
            errors.append("path_invalid")
        if not isinstance(body, dict):
            errors.append("body_not_object")
            body = {}
        tools = body.get("tools") if isinstance(body.get("tools"), list) else []
        tool_names = [
            tool.get("function", {}).get("name")
            for tool in tools
            if isinstance(tool, dict) and isinstance(tool.get("function"), dict)
        ]
        is_classifier = tool_names == ["emit_policy_signals"]
        expected_model = args.classifier_model if is_classifier else args.terminal_model
        if body.get("model") != expected_model:
            errors.append("model_invalid")
        if not isinstance(body.get("messages"), list) or not body.get("messages"):
            errors.append("messages_invalid")
        if "temperature" in body or "top_p" in body:
            errors.append("sampling_fields_not_dropped")
        if "max_tokens" in body:
            errors.append("max_tokens_not_rewritten")
        if not isinstance(body.get("max_completion_tokens"), int):
            errors.append("max_completion_tokens_missing")

        selected_powerful = False
        if is_classifier:
            signals, selected_powerful = classifier_signals(body)
            if args.role != "primary":
                errors.append("classifier_on_non_primary")
            if body.get("store") is not False:
                errors.append("classifier_store_invalid")
            if body.get("stream") not in (None, False):
                errors.append("classifier_stream_invalid")
            if body.get("parallel_tool_calls") is not False:
                errors.append("classifier_parallel_invalid")
            choice = body.get("tool_choice")
            if not (
                isinstance(choice, dict)
                and choice.get("type") == "function"
                and isinstance(choice.get("function"), dict)
                and choice["function"].get("name") == "emit_policy_signals"
            ):
                errors.append("classifier_choice_invalid")
        else:
            signals = None

        entry = {
            "kind": "classifier" if is_classifier else "terminal",
            "model_valid": body.get("model") == expected_model,
            "stream": body.get("stream") is True,
            "tool_names": tool_names,
            "tool_choice_kind": "object" if isinstance(body.get("tool_choice"), dict) else body.get("tool_choice"),
            "selected_powerful": selected_powerful,
            "sampling_fields_absent": "temperature" not in body and "top_p" not in body,
            "max_completion_tokens": body.get("max_completion_tokens"),
            "valid": not errors,
            "errors": list(errors),
        }
        sequence = record(entry, errors)
        if errors:
            self.send_json(400, {"error": {"type": "mock_validation_error", "message": ";".join(errors)}})
            return

        model = body["model"]
        if is_classifier:
            arguments = json.dumps(signals, separators=(",", ":"))
            payload = tool_completion(model, [function_call(f"call_classifier_{sequence}", "emit_policy_signals", json.loads(arguments))])
            self.send_json(200, payload)
            return

        choice = body.get("tool_choice")
        calls = []
        if isinstance(choice, dict) and isinstance(choice.get("function"), dict) and choice["function"].get("name") == "lookup_symbol":
            calls = [function_call(f"call_lookup_{sequence}", "lookup_symbol", {"symbol": "main"})]
        elif choice == "required" and set(tool_names) == {"fetch_account", "fetch_permissions"}:
            calls = [
                function_call(f"call_account_{sequence}", "fetch_account", {}),
                function_call(f"call_permissions_{sequence}", "fetch_permissions", {}),
            ]

        if body.get("stream") is True:
            text = sse_tools(model, calls) if calls else sse_text(model, "LOCAL_STREAM_OK")
            self.send_bytes(200, "text/event-stream", text.encode())
        elif calls:
            self.send_json(200, tool_completion(model, calls))
        else:
            self.send_json(200, completion(model, "LOCAL_TEXT_OK"))


server = None
while True:
    candidate = ThreadingHTTPServer((args.host, args.port), Handler)
    if candidate.server_port != 1337:
        server = candidate
        break
    candidate.server_close()
    args.port = 0
pathlib.Path(args.port_file).write_text(str(server.server_port), encoding="utf-8")
persist()


def stop(_signum, _frame):
    raise SystemExit(0)


signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)
try:
    server.serve_forever(poll_interval=0.1)
finally:
    server.server_close()
    persist()
PY_SERVER
  chmod 700 "${TMP_ROOT}/mock_chat_server.py"
}

start_mock_server() {
  local role="$1"
  local terminal_model="$2"
  local classifier_model="$3"
  local secret="$4"
  local case_dir="${TMP_ROOT}/${role}"
  local port_file="${case_dir}/port"
  local state_file="${case_dir}/state.json"
  local pid port

  mkdir -p "${case_dir}"
  chmod 700 "${case_dir}"
  python3 "${TMP_ROOT}/mock_chat_server.py" \
    --port-file "${port_file}" \
    --state-file "${state_file}" \
    --role "${role}" \
    --terminal-model "${terminal_model}" \
    --classifier-model "${classifier_model}" \
    --secret "${secret}" \
    >"${case_dir}/server.log" 2>&1 &
  pid=$!
  MOCK_SERVER_PIDS+=("${pid}")
  wait_for_file "${port_file}" || fail "${role} mock did not publish its port"
  port="$(cat "${port_file}")"
  [[ "${port}" != "1337" ]] || fail "${role} mock selected default port 1337"
  MOCK_SERVER_PORTS+=("${port}")
  port_accepts_tcp "${port}" || fail "${role} mock is not listening on port ${port}"
  printf '%s\n' "${port}"
}

write_proxy_wrapper() {
  : > "${PROXY_WRAPPER_ATTEMPTS}"
  chmod 600 "${PROXY_WRAPPER_ATTEMPTS}"
  cat > "${TEST_PROXY_WRAPPER}" <<'EOF_WRAPPER'
#!/usr/bin/env bash
set -euo pipefail

real="${REAL_PROXY_BIN:?}"
if [[ "${1:-}" == "config" ]]; then
  exec "${real}" "$@"
fi

port=""
args=("$@")
for ((index = 0; index < ${#args[@]}; index++)); do
  if [[ "${args[index]}" == "--port" ]] && (( index + 1 < ${#args[@]} )); then
    port="${args[index + 1]}"
    break
  fi
done
[[ -n "${port}" ]] || { printf 'missing --port in proxy wrapper\n' >&2; exit 98; }
printf '%s\n' "${port}" >> "${PROXY_WRAPPER_ATTEMPTS:?}"
attempt_count="$(wc -l < "${PROXY_WRAPPER_ATTEMPTS}" | tr -d ' ')"
if [[ "${attempt_count}" == "1" ]]; then
  printf '{"time":"2026-07-19T00:00:00Z","level":"fatal","msg":"server failed","error":"listen tcp 127.0.0.1:%s: bind: address already in use"}\n' "${port}" >&2
  exit 1
fi

exec "${real}" "$@"
EOF_WRAPPER
  chmod 700 "${TEST_PROXY_WRAPPER}"
}

assert_summary_markers() {
  local summary="${SMOKE_DIR}/summary.txt"
  local marker ready_count pass_count unique_count
  [[ -f "${summary}" ]] || fail "harness did not create summary.txt"

  while IFS= read -r marker; do
    [[ -n "${marker}" ]] || continue
    grep -Fxq "${marker}" "${summary}" || fail "missing harness summary marker: ${marker}"
  done <<'EOF_MARKERS'
PASS private-schema-v2-config-generated
PASS powerful-primary-shim-validation-guard
PASS offline-config-validation
PASS live-classifier-preflight
PASS off-baseline-lightweight-no-classifier
PASS observe-lightweight-baseline-powerful-shadow
PASS enforce-lightweight-selection
PASS enforce-powerful-selection
PASS forced-tool-and-continuation
PASS parallel-distinct-tools
PASS powerful-stream-canonical-identity
PASS powerful-primary-429-real-secondary-failover
PASS policy-responses-text
PASS local-rejections-zero-upstream
PASS public-only-stats-privacy-and-request-id
EOF_MARKERS

  ready_count="$(grep -Ec '^PASS (off|observe|enforce)-mode-ready port=[0-9]+$' "${summary}" || true)"
  [[ "${ready_count}" -eq 3 ]] || fail "ready summary markers=${ready_count}, want 3"
  while IFS= read -r port; do
    [[ "${port}" != "1337" ]] || fail "harness used default port 1337"
  done < <(sed -nE 's/^PASS (off|observe|enforce)-mode-ready port=([0-9]+)$/\2/p' "${summary}")

  pass_count="$(grep -c '^PASS ' "${summary}" || true)"
  unique_count="$(grep '^PASS ' "${summary}" | sort -u | wc -l | tr -d ' ')"
  [[ "${pass_count}" -eq 18 ]] || fail "summary PASS lines=${pass_count}, want 18"
  [[ "${unique_count}" -eq "${pass_count}" ]] || fail "summary contains duplicate PASS markers"
}

assert_generated_config() {
  local config="${SMOKE_DIR}/providers.json"
  [[ -f "${config}" ]] || fail "harness did not preserve generated config"
  [[ "$(file_mode "${config}")" == "600" ]] || fail "generated config mode=$(file_mode "${config}"), want 600"
  jq -e --arg public_model "${PUBLIC_MODEL}" \
    --arg lightweight_model "${LIGHTWEIGHT_MODEL}" \
    --arg primary_model "${PRIMARY_MODEL}" \
    --arg secondary_model "${SECONDARY_MODEL}" \
    --arg classifier_model "${CLASSIFIER_MODEL}" '
      .schema_version == 2
      and (.providers | length) == 3
      and ([.providers[].type] | all(. == "openai-compatible"))
      and ([.providers[].api_key_env] | sort) == [
        "LIVE_POLICY_ROUTING_LIGHTWEIGHT_API_KEY",
        "LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_API_KEY",
        "LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_API_KEY"
      ]
      and (.providers[] | select(.id == "live-powerful-primary-provider").classifier_no_store_supported) == true
      and .policy_profiles[0].data_policy.allow_provider_retention == false
      and (.model_routes | length) == 3
      and (.model_routes[] | select(.id == "live-semantic-lightweight").targets[0].upstream_model) == $lightweight_model
      and (.model_routes[] | select(.id == "live-semantic-powerful").targets | map(.upstream_model)) == [$primary_model,$secondary_model]
      and (.model_routes[] | select(.id == "live-semantic-classifier").targets[0].upstream_model) == $classifier_model
      and .policy_profiles[0].public_id == $public_model
      and .policy_profiles[0].baseline_tier == "lightweight"
      and .policy_profiles[0].classifier_unavailable_tier == "lightweight"
      and .policy_profiles[0].classifier_uncertain_tier == "powerful"
    ' "${config}" >/dev/null || fail "generated schema-v2 config did not match the deterministic topology"

  for secret in "${LIGHTWEIGHT_SECRET}" "${PRIMARY_SECRET}" "${SECONDARY_SECRET}"; do
    if grep -Fq -- "${secret}" "${config}"; then
      fail "generated config contained a test credential"
    fi
  done
}

assert_mock_state() {
  local role="$1"
  local classifier_count="$2"
  local terminal_count="$3"
  local state="${TMP_ROOT}/${role}/state.json"
  [[ -f "${state}" ]] || fail "missing ${role} mock state"
  jq -e --argjson classifiers "${classifier_count}" --argjson terminals "${terminal_count}" '
    (.errors | length) == 0
    and ([.requests[] | select(.valid != true)] | length) == 0
    and ([.requests[] | select(.kind == "classifier")] | length) == $classifiers
    and ([.requests[] | select(.kind == "terminal")] | length) == $terminals
    and ([.requests[] | select(.sampling_fields_absent != true)] | length) == 0
    and ([.requests[] | select(.model_valid != true)] | length) == 0
  ' "${state}" >/dev/null || fail "${role} mock observed unexpected request shape or count"
}

assert_ports_released() {
  local summary="${SMOKE_DIR}/summary.txt"
  local port
  while IFS= read -r port; do
    [[ -n "${port}" ]] || continue
    wait_for_port_release "${port}" || fail "harness proxy port ${port} remained open"
  done < <(sed -nE 's/^PASS (off|observe|enforce)-mode-ready port=([0-9]+)$/\2/p' "${summary}")

  if [[ -f "${SMOKE_DIR}/powerful-primary-shim.log" ]]; then
    port="$(jq -R -s -r '[split("\n")[] | fromjson? | select(.event == "listening")][0].port // empty' "${SMOKE_DIR}/powerful-primary-shim.log")"
    [[ -z "${port}" || "${port}" != "1337" ]] || fail "control shim used default port 1337"
    [[ -z "${port}" ]] || wait_for_port_release "${port}" || fail "control shim port ${port} remained open"
  fi
}

assert_wrapper_ports() {
  local count port
  [[ -f "${PROXY_WRAPPER_ATTEMPTS}" ]] || fail "proxy wrapper did not record startup attempts"
  count="$(wc -l < "${PROXY_WRAPPER_ATTEMPTS}" | tr -d ' ')"
  [[ "${count}" -eq 4 ]] || fail "proxy startup attempts=${count}, want 4 (one injected collision plus off, observe, enforce)"
  local wrapper_ports=()
  while IFS= read -r port; do
    [[ "${port}" =~ ^[0-9]+$ ]] || fail "proxy wrapper recorded invalid port ${port}"
    [[ "${port}" != "1337" ]] || fail "proxy wrapper observed forbidden default port 1337"
    wrapper_ports+=("${port}")
  done < "${PROXY_WRAPPER_ATTEMPTS}"
  [[ "${wrapper_ports[0]}" != "${wrapper_ports[1]}" ]] || fail "auto-port retry reused collided port ${wrapper_ports[0]}"
}

main() {
  require_cmd bash
  require_cmd curl
  require_cmd go
  require_cmd jq
  require_cmd ps
  require_cmd python3
  [[ -f "${SMOKE_SCRIPT}" ]] || fail "missing live policy-routing harness: ${SMOKE_SCRIPT}"
  bash -n "${SMOKE_SCRIPT}"

  write_mock_server
  write_proxy_wrapper

  log "Building isolated Vekil binary"
  (cd "${REPO_ROOT}" && go build -trimpath -o "${TEST_PROXY_BIN}" .)

  local lightweight_port primary_port secondary_port
  lightweight_port="$(start_mock_server lightweight "${LIGHTWEIGHT_MODEL}" "" "${LIGHTWEIGHT_SECRET}")"
  primary_port="$(start_mock_server primary "${PRIMARY_MODEL}" "${CLASSIFIER_MODEL}" "${PRIMARY_SECRET}")"
  secondary_port="$(start_mock_server secondary "${SECONDARY_MODEL}" "" "${SECONDARY_SECRET}")"

  log "Running live policy-routing harness against deterministic local providers"
  env \
    PROXY_BIN="${TEST_PROXY_WRAPPER}" \
    REAL_PROXY_BIN="${TEST_PROXY_BIN}" \
    PROXY_WRAPPER_ATTEMPTS="${PROXY_WRAPPER_ATTEMPTS}" \
    LIVE_POLICY_ROUTING_ALLOW_INSECURE_HTTP=1 \
    LIVE_POLICY_ROUTING_KEEP_ARTIFACTS=1 \
    LIVE_POLICY_ROUTING_SMOKE_DIR="${SMOKE_DIR}" \
    LIVE_POLICY_ROUTING_PUBLIC_MODEL="${PUBLIC_MODEL}" \
    LIVE_POLICY_ROUTING_LIGHTWEIGHT_TYPE=openai-compatible \
    LIVE_POLICY_ROUTING_LIGHTWEIGHT_BASE_URL="http://127.0.0.1:${lightweight_port}/v1" \
    LIVE_POLICY_ROUTING_LIGHTWEIGHT_MODEL="${LIGHTWEIGHT_MODEL}" \
    LIVE_POLICY_ROUTING_LIGHTWEIGHT_API_KEY="${LIGHTWEIGHT_SECRET}" \
    LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_TYPE=openai-compatible \
    LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_BASE_URL="http://127.0.0.1:${primary_port}/v1" \
    LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_MODEL="${PRIMARY_MODEL}" \
    LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_API_KEY="${PRIMARY_SECRET}" \
    LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_TYPE=openai-compatible \
    LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_BASE_URL="http://127.0.0.1:${secondary_port}/v1" \
    LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_MODEL="${SECONDARY_MODEL}" \
    LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_API_KEY="${SECONDARY_SECRET}" \
    LIVE_POLICY_ROUTING_CLASSIFIER_MODEL="${CLASSIFIER_MODEL}" \
    LIVE_POLICY_ROUTING_CLASSIFIER_NO_STORE_SUPPORTED=true \
    SMOKE_STARTUP_TIMEOUT_SECONDS=20 \
    SMOKE_VALIDATE_TIMEOUT_SECONDS=20 \
    SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1 \
    SMOKE_CURL_MAX_TIME_SECONDS=15 \
    SMOKE_COMPLEX_CURL_MAX_TIME_SECONDS=20 \
    SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS=2 \
    SMOKE_OBSERVE_SETTLE_TIMEOUT_SECONDS=10 \
    SMOKE_PROCESS_TERM_GRACE_SECONDS=3 \
    SMOKE_PORT_RELEASE_TIMEOUT_SECONDS=5 \
    SMOKE_SHIM_UPSTREAM_TIMEOUT_SECONDS=15 \
    SMOKE_AUTO_PORT_MAX_ATTEMPTS=3 \
    "${SMOKE_SCRIPT}" >"${HARNESS_STDOUT}" 2>"${HARNESS_STDERR}"

  grep -Fq 'Live semantic policy-routing smoke passed.' "${HARNESS_STDERR}" || \
    fail "harness did not emit its success marker"
  assert_summary_markers
  assert_generated_config
  assert_wrapper_ports
  assert_ports_released

  assert_mock_state lightweight 0 7
  assert_mock_state primary 12 2
  assert_mock_state secondary 0 1

  assert_no_test_credentials "${HARNESS_STDOUT}"
  assert_no_test_credentials "${HARNESS_STDERR}"
  assert_no_test_credentials "${SMOKE_DIR}/summary.txt"
  assert_no_test_credentials "${SMOKE_DIR}/preflight-offline.log"
  assert_no_test_credentials "${SMOKE_DIR}/preflight-live.log"
  assert_no_test_credentials "${SMOKE_DIR}/powerful-primary-shim.log"
  while IFS= read -r artifact; do
    assert_no_test_credentials "${artifact}"
  done < <(find "${SMOKE_DIR}" -type f -print)
  assert_no_test_credentials "${TMP_ROOT}/lightweight/state.json"
  assert_no_test_credentials "${TMP_ROOT}/primary/state.json"
  assert_no_test_credentials "${TMP_ROOT}/secondary/state.json"

  local redaction_probe="${TMP_ROOT}/redaction-probe.txt"
  local redaction_result="${TMP_ROOT}/redaction-probe.redacted"
  printf 'Authorization: Bearer %s\napi-key: %s\n%s\n' \
    "${PRIMARY_SECRET}" "${SECONDARY_SECRET}" "${LIGHTWEIGHT_SECRET}" > "${redaction_probe}"
  redact_file "${redaction_probe}" "${redaction_result}"
  assert_no_test_credentials "${redaction_result}"
  grep -Fq '[REDACTED]' "${redaction_result}" || fail "diagnostic redactor did not replace test credentials"

  stop_mock_servers
  local index
  for index in "${!MOCK_SERVER_PORTS[@]}"; do
    wait_for_port_release "${MOCK_SERVER_PORTS[index]}" || \
      fail "mock provider port ${MOCK_SERVER_PORTS[index]} remained open"
  done

  log "Deterministic live policy-routing process smoke passed"
}

main "$@"
