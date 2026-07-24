#!/usr/bin/env bash

set -euo pipefail

log() { printf '==> %s\n' "$*" >&2; }
fail() { printf 'error: %s\n' "$*" >&2; exit 1; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
SMOKE_SCRIPT="${REPO_ROOT}/scripts/live-policy-routing-sol-effort-smoke.sh"
TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/vekil-sol-effort-smoke-test.XXXXXX")"
PROXY_BIN="${TMP_ROOT}/vekil"
BRIDGE_SCRIPT="${TMP_ROOT}/fake-bridge.py"
BRIDGE_STATE="${TMP_ROOT}/bridge-state.json"
BRIDGE_LOG="${TMP_ROOT}/bridge.log"
SMOKE_DIR="${TMP_ROOT}/smoke"
STDOUT_FILE="${TMP_ROOT}/smoke.stdout"
STDERR_FILE="${TMP_ROOT}/smoke.stderr"
bridge_pid=""
bridge_port=""

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  if [[ -n "${bridge_pid}" ]]; then
    kill -TERM "${bridge_pid}" 2>/dev/null || true
    wait "${bridge_pid}" 2>/dev/null || true
  fi
  rm -rf "${TMP_ROOT}"
  exit "${rc}"
}
trap cleanup EXIT INT TERM

allocate_free_port() {
  python3 - <<'PY'
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

port_is_open() {
  python3 - "$1" <<'PY'
import socket
import sys
with socket.socket() as sock:
    sock.settimeout(0.2)
    raise SystemExit(0 if sock.connect_ex(("127.0.0.1", int(sys.argv[1]))) == 0 else 1)
PY
}

write_fake_bridge() {
  cat > "${BRIDGE_SCRIPT}" <<'PY'
#!/usr/bin/env python3
import argparse
import json
import pathlib
import signal
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

parser = argparse.ArgumentParser()
parser.add_argument("--port", required=True, type=int)
parser.add_argument("--state", required=True)
args = parser.parse_args()
lock = threading.Lock()
state = {"errors": [], "requests": []}

def save():
    pathlib.Path(args.state).write_text(json.dumps(state, indent=2) + "\n", encoding="utf-8")

def classifier_tool(value):
    if isinstance(value, dict):
        if value.get("name") == "emit_policy_signals":
            return True
        return any(classifier_tool(item) for item in value.values())
    if isinstance(value, list):
        return any(classifier_tool(item) for item in value)
    return False

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def send_json(self, status, payload):
        data = json.dumps(payload, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path == "/readyz":
            self.send_json(200, {"status": "ready"})
            return
        if self.path == "/v1/models":
            self.send_json(200, {
                "object": "list",
                "data": [{
                    "id": "gpt-5.6-sol",
                    "supported_endpoints": ["/responses"],
                    "capabilities": {"supports": {"reasoning_effort": ["none", "low", "medium", "high", "xhigh", "max"]}},
                }],
            })
            return
        self.send_json(404, {"error": "not found"})

    def do_POST(self):
        length = int(self.headers.get("content-length", "0"))
        try:
            body = json.loads(self.rfile.read(length))
        except Exception as exc:
            self.send_json(400, {"error": type(exc).__name__})
            return
        is_classifier = classifier_tool(body.get("tools", []))
        reasoning = body.get("reasoning") if isinstance(body, dict) else None
        effort = reasoning.get("effort") if isinstance(reasoning, dict) else None
        errors = []
        if body.get("model") != "gpt-5.6-sol":
            errors.append("model_invalid")
        if is_classifier and effort is not None:
            errors.append("classifier_effort_present")
        if is_classifier and "store" in body:
            errors.append("classifier_store_present")
        if not is_classifier and effort not in {"low", "max"}:
            errors.append("terminal_effort_invalid")
        with lock:
            state["errors"].extend(errors)
            state["requests"].append({"kind": "classifier" if is_classifier else "terminal", "effort": effort, "model": body.get("model")})
            save()
        if errors:
            self.send_json(422, {"error": errors})
            return

        if is_classifier:
            text = json.dumps(body, separators=(",", ":"))
            complex_task = "SOL_MAX_TASK_SENTINEL" in text
            signals = {
                "abstain": False,
                "turn_type": "planning" if complex_task else "lookup",
                "code_scope": "cross_module" if complex_task else "none",
                "tool_call_count_estimate": 0,
                "modifying_tool_call_count_estimate": 0,
                "requires_codebase_context": complex_task,
                "risk_level": "high" if complex_task else "low",
            }
            self.send_json(200, {
                "id": "resp-classifier",
                "object": "response",
                "status": "completed",
                "model": "gpt-5.6-sol",
                "output": [{
                    "type": "function_call",
                    "id": "fc-classifier",
                    "call_id": "call-classifier",
                    "name": "emit_policy_signals",
                    "arguments": json.dumps(signals, separators=(",", ":")),
                    "status": "completed",
                }],
                "usage": {"input_tokens": 10, "output_tokens": 4, "total_tokens": 14},
            })
            return

        self.send_json(200, {
            "id": "resp-terminal",
            "object": "response",
            "status": "completed",
            "model": "gpt-5.6-sol",
            "output": [{
                "type": "message",
                "id": "msg-terminal",
                "status": "completed",
                "role": "assistant",
                "content": [{"type": "output_text", "text": "SOL_FAKE_OK"}],
            }],
            "usage": {"input_tokens": 12, "output_tokens": 3, "total_tokens": 15},
        })

server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
save()
def stop(_signum, _frame):
    raise SystemExit(0)
signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)
server.serve_forever(poll_interval=0.1)
server.server_close()
PY
  chmod 700 "${BRIDGE_SCRIPT}"
}

main() {
  require_cmd bash
  require_cmd curl
  require_cmd go
  require_cmd jq
  require_cmd python3
  [[ -x "${SMOKE_SCRIPT}" ]] || fail "missing Sol effort smoke: ${SMOKE_SCRIPT}"
  bash -n "${SMOKE_SCRIPT}"

  log "Building isolated Vekil binary"
  (cd "${REPO_ROOT}" && go build -o "${PROXY_BIN}" .)
  write_fake_bridge
  bridge_port="$(allocate_free_port)"
  python3 "${BRIDGE_SCRIPT}" --port "${bridge_port}" --state "${BRIDGE_STATE}" > "${BRIDGE_LOG}" 2>&1 &
  bridge_pid="$!"
  local deadline=$((SECONDS + 10))
  while (( SECONDS < deadline )); do
    port_is_open "${bridge_port}" && break
    sleep 0.1
  done
  port_is_open "${bridge_port}" || fail "fake bridge did not start"

  log "Running exact Sol low/max semantic effort smoke against deterministic bridge"
  PROXY_BIN="${PROXY_BIN}" \
  LIVE_POLICY_ROUTING_SOL_BRIDGE_BASE_URL="http://127.0.0.1:${bridge_port}" \
  LIVE_POLICY_ROUTING_SOL_SMOKE_DIR="${SMOKE_DIR}" \
  LIVE_POLICY_ROUTING_SOL_KEEP_ARTIFACTS=1 \
  SMOKE_STARTUP_TIMEOUT_SECONDS=20 \
  SMOKE_CURL_MAX_TIME_SECONDS=20 \
  "${SMOKE_SCRIPT}" > "${STDOUT_FILE}" 2> "${STDERR_FILE}"

  jq -e '
    (.errors | length) == 0
    and ([.requests[] | select(.kind == "classifier" and .effort != null)] | length) == 0
    and ([.requests[] | select(.kind == "terminal") | .effort] | sort) == ["low", "max"]
    and ([.requests[] | select(.model != "gpt-5.6-sol")] | length) == 0
  ' "${BRIDGE_STATE}" >/dev/null || fail "fake bridge observed invalid classifier or terminal requests"

  grep -Fq 'PASS sol-simple-conflicting-max-routed-low' "${SMOKE_DIR}/summary.txt" || fail "missing lightweight override marker"
  grep -Fq 'PASS sol-complex-conflicting-low-routed-max' "${SMOKE_DIR}/summary.txt" || fail "missing powerful override marker"
  grep -Fq 'Live GPT-5.6 Sol semantic low/max effort smoke passed.' "${STDERR_FILE}" || fail "missing success marker"
  log "Deterministic Sol semantic effort smoke passed"
}

main "$@"
