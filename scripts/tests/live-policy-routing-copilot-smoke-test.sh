#!/usr/bin/env bash

set -euo pipefail

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
  [[ -n "${pid}" ]] || return 1
  kill -0 "${pid}" 2>/dev/null || return 1
  [[ "$(ps -o stat= -p "${pid}" 2>/dev/null | tr -d '[:space:]')" != Z* ]]
}

port_accepts_tcp() {
  python3 - "$1" <<'PY'
import socket
import sys

port = int(sys.argv[1])
with socket.socket() as sock:
    sock.settimeout(0.2)
    raise SystemExit(0 if sock.connect_ex(("127.0.0.1", port)) == 0 else 1)
PY
}

wait_for_port_release() {
  local port="$1"
  local deadline=$((SECONDS + 5))
  while (( SECONDS < deadline )); do
    if ! port_accepts_tcp "${port}"; then
      return 0
    fi
    sleep 0.1
  done
  ! port_accepts_tcp "${port}"
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
WRAPPER="${REPO_ROOT}/scripts/live-policy-routing-copilot-smoke.sh"
TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/vekil-policy-copilot-wrapper-test.XXXXXX")"
SMOKE_DIR="${TMP_ROOT}/smoke"
BRIDGE_BIN="${TMP_ROOT}/fake-copilot-bridge.py"
HARNESS="${TMP_ROOT}/fake-policy-harness.sh"
RECORD="${TMP_ROOT}/harness-env.json"
CHILD_PID_FILE="${TMP_ROOT}/bridge-child.pid"
STDOUT_FILE="${TMP_ROOT}/wrapper.stdout"
STDERR_FILE="${TMP_ROOT}/wrapper.stderr"
TOKEN="synthetic-copilot-token-must-not-leak"

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  if [[ -f "${CHILD_PID_FILE}" ]]; then
    local child_pid
    child_pid="$(cat "${CHILD_PID_FILE}" 2>/dev/null || true)"
    if process_is_running "${child_pid}"; then
      kill -KILL "${child_pid}" 2>/dev/null || true
      rc=1
    fi
  fi
  rm -rf "${TMP_ROOT}"
  exit "${rc}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

write_fake_bridge() {
  cat > "${BRIDGE_BIN}" <<'PY'
#!/usr/bin/env python3
import argparse
import json
import os
import pathlib
import signal
import subprocess
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

parser = argparse.ArgumentParser(add_help=False)
parser.add_argument("--host", default="127.0.0.1")
parser.add_argument("--port", required=True, type=int)
parser.add_argument("--token-dir")
parser.add_argument("--log-level")
args, _ = parser.parse_known_args()

child = subprocess.Popen(["sleep", "300"])
pathlib.Path(os.environ["FAKE_BRIDGE_CHILD_PID_FILE"]).write_text(str(child.pid), encoding="utf-8")

models = [
    {"id": "gpt-5.4-mini", "supported_endpoints": ["/chat/completions"]},
    {"id": "gpt-5.4", "supported_endpoints": ["/chat/completions", "/responses"]},
    {"id": "claude-sonnet-4.6", "supported_endpoints": ["/chat/completions"]},
    {"id": "responses-only", "supported_endpoints": ["/responses"]},
]

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def send_json(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/readyz":
            self.send_json(200, {"status": "ready"})
            return
        if self.path == "/v1/models":
            self.send_json(200, {"object": "list", "data": models})
            return
        self.send_json(404, {"error": "not found"})

server = ThreadingHTTPServer((args.host, args.port), Handler)
print(json.dumps({"level": "info", "msg": "vekil listening", "addr": f"{args.host}:{args.port}"}), flush=True)

def stop(_signum, _frame):
    raise SystemExit(0)

signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)
try:
    server.serve_forever(poll_interval=0.1)
finally:
    server.server_close()
    if child.poll() is None:
        child.terminate()
        try:
            child.wait(timeout=2)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait(timeout=2)
PY
  chmod 700 "${BRIDGE_BIN}"
}

write_fake_harness() {
  cat > "${HARNESS}" <<'SH'
#!/usr/bin/env bash
set -euo pipefail

[[ -z "${COPILOT_GITHUB_TOKEN:-}" ]] || {
  echo "COPILOT_GITHUB_TOKEN leaked into policy harness" >&2
  exit 1
}
[[ "${LIVE_POLICY_ROUTING_LIGHTWEIGHT_TYPE}" == "openai-compatible" ]]
[[ "${LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_TYPE}" == "openai-compatible" ]]
[[ "${LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_TYPE}" == "openai-compatible" ]]
[[ "${LIVE_POLICY_ROUTING_LIGHTWEIGHT_BASE_URL}" == "${LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_BASE_URL}" ]]
[[ "${LIVE_POLICY_ROUTING_LIGHTWEIGHT_BASE_URL}" == "${LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_BASE_URL}" ]]
[[ "${LIVE_POLICY_ROUTING_LIGHTWEIGHT_BASE_URL}" == http://127.0.0.1:*/v1 ]]
[[ "${LIVE_POLICY_ROUTING_LIGHTWEIGHT_MODEL}" == "gpt-5.4-mini" ]]
[[ "${LIVE_POLICY_ROUTING_CLASSIFIER_MODEL}" == "gpt-5.4-mini" ]]
[[ "${LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_MODEL}" == "gpt-5.4" ]]
[[ "${LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_MODEL}" == "claude-sonnet-4.6" ]]
[[ "${LIVE_POLICY_ROUTING_CLASSIFIER_NO_STORE_SUPPORTED}" == "false" ]]
[[ "${LIVE_POLICY_ROUTING_ALLOW_PROVIDER_RETENTION}" == "true" ]]
[[ -n "${LIVE_POLICY_ROUTING_LIGHTWEIGHT_API_KEY}" ]]
[[ -n "${LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_API_KEY}" ]]
[[ -n "${LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_API_KEY}" ]]

jq -n \
  --arg base "${LIVE_POLICY_ROUTING_LIGHTWEIGHT_BASE_URL}" \
  --arg lightweight "${LIVE_POLICY_ROUTING_LIGHTWEIGHT_MODEL}" \
  --arg classifier "${LIVE_POLICY_ROUTING_CLASSIFIER_MODEL}" \
  --arg primary "${LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_MODEL}" \
  --arg secondary "${LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_MODEL}" \
  '{base:$base,lightweight:$lightweight,classifier:$classifier,primary:$primary,secondary:$secondary}' \
  > "${FAKE_HARNESS_RECORD}"
SH
  chmod 700 "${HARNESS}"
}

main() {
  require_cmd bash
  require_cmd curl
  require_cmd jq
  require_cmd ps
  require_cmd python3
  [[ -f "${WRAPPER}" ]] || fail "missing Copilot policy wrapper: ${WRAPPER}"
  bash -n "${WRAPPER}"

  write_fake_bridge
  write_fake_harness
  mkdir -p "${SMOKE_DIR}"

  log "Running Copilot semantic-policy wrapper against a deterministic bridge catalog"
  env \
    COPILOT_GITHUB_TOKEN="${TOKEN}" \
    PROXY_BIN=/usr/bin/true \
    LIVE_POLICY_ROUTING_COPILOT_BRIDGE_BIN="${BRIDGE_BIN}" \
    LIVE_POLICY_ROUTING_HARNESS="${HARNESS}" \
    LIVE_POLICY_ROUTING_SMOKE_DIR="${SMOKE_DIR}" \
    LIVE_POLICY_ROUTING_KEEP_ARTIFACTS=1 \
    FAKE_BRIDGE_CHILD_PID_FILE="${CHILD_PID_FILE}" \
    FAKE_HARNESS_RECORD="${RECORD}" \
    "${WRAPPER}" >"${STDOUT_FILE}" 2>"${STDERR_FILE}"

  [[ -s "${RECORD}" ]] || fail "fake harness did not record selected Copilot topology"
  jq -e '
    .lightweight == "gpt-5.4-mini"
    and .classifier == "gpt-5.4-mini"
    and .primary == "gpt-5.4"
    and .secondary == "claude-sonnet-4.6"
  ' "${RECORD}" >/dev/null || fail "wrapper selected unexpected Copilot models"

  local base port child_pid
  base="$(jq -r '.base' "${RECORD}")"
  port="$(python3 - "${base}" <<'PY'
import sys
import urllib.parse
print(urllib.parse.urlsplit(sys.argv[1]).port)
PY
)"
  [[ "${port}" != "1337" ]] || fail "Copilot bridge used forbidden default port 1337"
  wait_for_port_release "${port}" || fail "Copilot bridge port ${port} remained open"

  child_pid="$(cat "${CHILD_PID_FILE}")"
  if process_is_running "${child_pid}"; then
    fail "Copilot bridge descendant ${child_pid} remained alive"
  fi

  for path in "${STDOUT_FILE}" "${STDERR_FILE}" "${RECORD}" "${SMOKE_DIR}/copilot-bridge.log"; do
    [[ -f "${path}" ]] || continue
    if grep -Fq -- "${TOKEN}" "${path}"; then
      fail "Copilot token leaked into ${path}"
    fi
  done

  grep -Fq 'Copilot-backed semantic policy-routing smoke passed.' "${STDERR_FILE}" || \
    fail "wrapper did not emit success marker"
  log "Deterministic Copilot semantic-policy wrapper smoke passed"
}

main "$@"
