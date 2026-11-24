#!/usr/bin/env bash

set -euo pipefail

unset GITHUB_STEP_SUMMARY

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/vekil-smoke-reliability.XXXXXX")"
ORIGINAL_PATH="${PATH}"

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

stop_mock_servers() {
  local pid attempt
  for pid in "${server_pids[@]:-}"; do
    [[ -n "${pid}" ]] || continue
    if kill -0 "${pid}" 2>/dev/null; then
      kill "${pid}" 2>/dev/null || true
      for ((attempt = 0; attempt < 50; attempt++)); do
        kill -0 "${pid}" 2>/dev/null || break
        sleep 0.05
      done
      if kill -0 "${pid}" 2>/dev/null; then
        kill -KILL "${pid}" 2>/dev/null || true
      fi
    fi
    wait "${pid}" 2>/dev/null || true
  done
}

cleanup() {
  stop_mock_servers
  rm -rf "${TMP_ROOT}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

cat > "${TMP_ROOT}/mock_server.py" <<'PY'
import argparse
import json
import pathlib
import signal
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

parser = argparse.ArgumentParser()
parser.add_argument("--port-file", required=True)
parser.add_argument("--host", default="127.0.0.1")
parser.add_argument("--port", type=int, default=0)
parser.add_argument("--gemini-model", action="store_true")
parser.add_argument("--compact-status", type=int, default=200)
parser.add_argument("--compact-code", default="")
parser.add_argument("--replay-status", type=int, default=200)
parser.add_argument("--replay-code", default="")
args = parser.parse_args()

MODELS = [
    {"id": "gpt-5.4", "supported_endpoints": ["/responses"]},
    {"id": "gpt-5.6-luna-utility", "supported_endpoints": ["/responses"]},
    {"id": "claude-sonnet-4.6", "supported_endpoints": ["/chat/completions"]},
    {"id": "claude-sonnet-5", "supported_endpoints": ["/chat/completions"]},
    {"id": "claude-haiku-5.5", "supported_endpoints": ["/chat/completions"]},
]
if args.gemini_model:
    MODELS.append({"id": "gpt-4.1", "supported_endpoints": ["/chat/completions"]})

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, _format, *_args):
        return

    def send_json(self, status, payload):
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path == "/healthz":
            self.send_json(200, {"status": "ok"})
            return
        if self.path == "/readyz":
            self.send_json(200, {"status": "ready"})
            return
        if self.path == "/v1/models":
            self.send_json(200, {"data": MODELS})
            return
        self.send_json(404, {"error": {"message": "not found"}})

    def do_POST(self):
        length = int(self.headers.get("content-length", "0"))
        body = self.rfile.read(length) if length else b""
        if self.path == "/v1/responses/compact":
            if args.compact_status != 200:
                self.send_json(args.compact_status, {
                    "error": {
                        "message": f"mock HTTP {args.compact_status}",
                        "code": args.compact_code or "mock_error",
                    }
                })
                return
            self.send_json(200, {"output": [{"type": "compaction", "encrypted_content": "opaque"}]})
            return
        if self.path == "/v1/responses":
            if args.replay_status != 200:
                self.send_json(args.replay_status, {
                    "error": {
                        "message": f"mock HTTP {args.replay_status}",
                        "code": args.replay_code or "mock_error",
                    }
                })
                return
            self.send_json(200, {"output": [{"type": "message", "content": [{"type": "output_text", "text": "VEKIL_COMPACTION_REPLAY_OK"}]}]})
            return
        self.send_json(404, {"error": {"message": "not found"}})

server = ThreadingHTTPServer((args.host, args.port), Handler)
pathlib.Path(args.port_file).write_text(str(server.server_address[1]))

def stop(_signum, _frame):
    raise SystemExit(0)

signal.signal(signal.SIGTERM, stop)
try:
    server.serve_forever()
finally:
    server.server_close()
PY

cat > "${TMP_ROOT}/hang_server.py" <<'PY'
import socket
import sys
import time

host = sys.argv[1]
port = int(sys.argv[2])
with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.bind((host, port))
    sock.listen()
    while True:
        conn, _ = sock.accept()
        time.sleep(300)
        conn.close()
PY

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
  local compact_status="${2:-200}"
  local compact_code="${3:-}"
  local replay_status="${4:-200}"
  local replay_code="${5:-}"
  local port_file="${case_dir}/port"
  local args=(
    --port-file "${port_file}"
    --compact-status "${compact_status}"
    --replay-status "${replay_status}"
  )
  if [[ "${MOCK_GEMINI_MODEL:-0}" == "1" ]]; then
    args+=(--gemini-model)
  fi
  if [[ -n "${compact_code}" ]]; then
    args+=(--compact-code "${compact_code}")
  fi
  if [[ -n "${replay_code}" ]]; then
    args+=(--replay-code "${replay_code}")
  fi
  mkdir -p "${case_dir}"
  python3 "${TMP_ROOT}/mock_server.py" "${args[@]}" \
    >"${case_dir}/server.log" 2>&1 &
  MOCK_SERVER_PID=$!
  server_pids+=("${MOCK_SERVER_PID}")
  if ! wait_for_file "${port_file}"; then
    cat "${case_dir}/server.log" >&2 || true
    kill "${MOCK_SERVER_PID}" 2>/dev/null || true
    wait "${MOCK_SERVER_PID}" 2>/dev/null || true
    return 1
  fi
  MOCK_SERVER_PORT="$(cat "${port_file}")"
  server_ports+=("${MOCK_SERVER_PORT}")
}

write_fake_client() {
  local path="$1"
  local mode="$2"
  local mode_q
  printf -v mode_q %q "${mode}"
  cat > "${path}" <<EOF_CLIENT
#!/usr/bin/env bash
set -euo pipefail
mode=${mode_q}
if [[ ! -f left.txt || ! -f right.txt ]]; then
  printf 'fake client found no left.txt/right.txt fixtures in %s\n' "\${PWD}" >&2
  exit 98
fi
expected="\$(cat left.txt)|\$(cat right.txt)"
left="\${expected%%|*}"
right="\${expected#*|}"
case "\${mode}" in
  pass)
    printf '%s\n' "\${expected}"
    ;;
  wrapped-output)
    FAKE_LEFT="\${left}" FAKE_RIGHT="\${right}" python3 - <<'PY_WRAPPED_OUTPUT'
import json
import os

left = os.environ["FAKE_LEFT"]
right = os.environ["FAKE_RIGHT"]
print(
    json.dumps({"output": left}, separators=(",", ":"))
    + "|"
    + json.dumps({"output": right}, separators=(",", ":"))
)
PY_WRAPPED_OUTPUT
    ;;
  json-wrapped)
    jq -cn --arg output "\${left}" '{output: \$output}'
    printf '|'
    jq -cn --arg output "\${right}" '{output: \$output}'
    ;;
  json-wrapped-whole)
    jq -cn --arg output "\${expected}" '{output: \$output}'
    ;;
  json-wrapped-trailing-separator)
    jq -cn --arg output "\${left}" '{output: \$output}'
    printf '|'
    jq -cn --arg output "\${right}" '{output: \$output}'
    printf '|'
    ;;
  json-wrapped-three)
    jq -cn --arg output "\${left}" '{output: \$output}'
    printf '|'
    jq -cn --arg output 'unexpected-third-wrapper' '{output: \$output}'
    printf '|'
    jq -cn --arg output "\${right}" '{output: \$output}'
    ;;
  *)
    printf 'unknown fake client mode: %s\n' "\${mode}" >&2
    exit 99
    ;;
esac
EOF_CLIENT
  chmod +x "${path}"
}

write_fake_default_smoke_clients() {
  local bin_dir="$1"
  local gemini_mode="${2:-}"
  mkdir -p "${bin_dir}"
  if [[ -n "${gemini_mode}" ]]; then
    write_fake_client "${bin_dir}/gemini" "${gemini_mode}"
  fi

  cat > "${bin_dir}/codex" <<'EOF_CODEX'
#!/usr/bin/env bash
set -euo pipefail
case_dir=""
output_file=""
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --cd)
      case_dir="$2"
      shift 2
      ;;
    -o)
      output_file="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
[[ -n "${case_dir}" && -n "${output_file}" ]]
printf '%s|%s\n' "$(cat "${case_dir}/left.txt")" "$(cat "${case_dir}/right.txt")" > "${output_file}"
EOF_CODEX

  cat > "${bin_dir}/claude" <<'EOF_CLAUDE'
#!/usr/bin/env bash
set -euo pipefail
capture_dir="${FAKE_CLAUDE_CAPTURE_DIR:?}"
mkdir -p "${capture_dir}"
printf '%s' "${CLAUDE_CODE_DISABLE_ADVISOR_TOOL-}" > "${capture_dir}/disable-advisor-tool"
printf '%s' "${MAX_THINKING_TOKENS-}" > "${capture_dir}/max-thinking-tokens"
model=""
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --model)
      model="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
printf '%s' "${model}" > "${capture_dir}/model"
printf '%s|%s\n' "$(cat left.txt)" "$(cat right.txt)"
EOF_CLAUDE

  chmod +x "${bin_dir}/codex" "${bin_dir}/claude"
}

write_bind_failure_proxy() {
  local path="$1"
  cat > "${path}" <<'EOF_PROXY'
#!/usr/bin/env bash
printf 'Please visit https://github.com/login/device and enter code: TEST-CODE\n' >&2
printf '{"level":"fatal","msg":"serve error","error":"server start error: listen: address already in use"}\n' >&2
printf '{"level":"debug","msg":"rewrote compaction items"}\n' >&2
exit 1
EOF_PROXY
  chmod +x "${path}"
}

write_healthy_proxy() {
  local path="$1"
  mkdir -p "$(dirname "${path}")"
  cat > "${path}" <<EOF_PROXY
#!/usr/bin/env bash
set -euo pipefail
host=127.0.0.1
port=
while [[ \$# -gt 0 ]]; do
  case "\$1" in
    --host) host="\$2"; shift 2 ;;
    --port) port="\$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf '{"addr":"%s:%s","level":"info","msg":"vekil listening","time":"2026-01-01T00:00:00Z"}\n' "\${host}" "\${port}"
printf 'Please visit https://github.com/login/device and enter code: TEST-CODE\n'
printf '{"level":"debug","msg":"rewrote compaction items"}\n'
exec python3 "${TMP_ROOT}/mock_server.py" --host "\${host}" --port "\${port}" --port-file "${TMP_ROOT}/healthy-proxy-port"
EOF_PROXY
  chmod +x "${path}"
}

write_hanging_proxy() {
  local path="$1"
  cat > "${path}" <<EOF_PROXY
#!/usr/bin/env bash
set -euo pipefail
host=127.0.0.1
port=
while [[ \$# -gt 0 ]]; do
  case "\$1" in
    --host) host="\$2"; shift 2 ;;
    --port) port="\$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf '{"addr":"%s:%s","level":"info","msg":"vekil listening","time":"2026-01-01T00:00:00Z"}\n' "\${host}" "\${port}"
exec python3 "${TMP_ROOT}/hang_server.py" "\${host}" "\${port}"
EOF_PROXY
  chmod +x "${path}"
}

run_bounded() {
  local timeout_seconds="$1"
  local stdout_file="$2"
  local stderr_file="$3"
  shift 3

  set +e
  python3 - "${timeout_seconds}" "${stdout_file}" "${stderr_file}" "$@" <<'PY'
import os
import signal
import subprocess
import sys

timeout = float(sys.argv[1])
stdout_path = sys.argv[2]
stderr_path = sys.argv[3]
command = sys.argv[4:]
with open(stdout_path, "wb") as stdout, open(stderr_path, "wb") as stderr:
    proc = subprocess.Popen(command, stdout=stdout, stderr=stderr, start_new_session=True)
    try:
        rc = proc.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        os.killpg(proc.pid, signal.SIGTERM)
        try:
            proc.wait(timeout=1)
        except subprocess.TimeoutExpired:
            os.killpg(proc.pid, signal.SIGKILL)
            proc.wait()
        rc = 124
sys.exit(rc)
PY
  local rc=$?
  set -e
  return "${rc}"
}

record_failure() {
  local name="$1"
  local detail="$2"
  failures=$((failures + 1))
  printf 'not ok - %s: %s\n' "${name}" "${detail}" >&2
}

record_success() {
  printf 'ok - %s\n' "$1" >&2
}

expect_hard_failure() {
  local name="$1"
  local timeout_seconds="$2"
  shift 2
  local case_dir="${TMP_ROOT}/cases/${name//[^a-zA-Z0-9_.-]/_}"
  mkdir -p "${case_dir}"
  local rc=0
  run_bounded "${timeout_seconds}" "${case_dir}/stdout" "${case_dir}/stderr" "$@" || rc=$?
  if [[ "${rc}" -eq 0 ]]; then
    record_failure "${name}" "command unexpectedly succeeded"
    cat "${case_dir}/stderr" >&2 || true
    return
  fi
  if [[ "${rc}" -eq 124 ]]; then
    record_failure "${name}" "command exceeded ${timeout_seconds}s outer test deadline"
    cat "${case_dir}/stderr" >&2 || true
    return
  fi
  record_success "${name}"
}

expect_hard_failure_with_stderr() {
  local name="$1"
  local timeout_seconds="$2"
  local pattern="$3"
  shift 3
  local case_dir="${TMP_ROOT}/cases/${name//[^a-zA-Z0-9_.-]/_}"
  mkdir -p "${case_dir}"
  local rc=0
  run_bounded "${timeout_seconds}" "${case_dir}/stdout" "${case_dir}/stderr" "$@" || rc=$?
  if [[ "${rc}" -eq 0 ]]; then
    record_failure "${name}" "command unexpectedly succeeded"
    cat "${case_dir}/stderr" >&2 || true
    return
  fi
  if [[ "${rc}" -eq 124 ]]; then
    record_failure "${name}" "command exceeded ${timeout_seconds}s outer test deadline"
    cat "${case_dir}/stderr" >&2 || true
    return
  fi
  if ! grep -Eq "${pattern}" "${case_dir}/stderr"; then
    record_failure "${name}" "stderr did not match ${pattern}"
    cat "${case_dir}/stderr" >&2 || true
    return
  fi
  record_success "${name}"
}

expect_success() {
  local name="$1"
  local timeout_seconds="$2"
  shift 2
  local case_dir="${TMP_ROOT}/cases/${name//[^a-zA-Z0-9_.-]/_}"
  mkdir -p "${case_dir}"
  local rc=0
  run_bounded "${timeout_seconds}" "${case_dir}/stdout" "${case_dir}/stderr" "$@" || rc=$?
  if [[ "${rc}" -ne 0 ]]; then
    record_failure "${name}" "command failed with ${rc}"
    cat "${case_dir}/stderr" >&2 || true
    return 1
  fi
  record_success "${name}"
}

expect_exit_code() {
  local name="$1"
  local timeout_seconds="$2"
  local expected="$3"
  shift 3
  local case_dir="${TMP_ROOT}/cases/${name//[^a-zA-Z0-9_.-]/_}"
  mkdir -p "${case_dir}"
  local rc=0
  run_bounded "${timeout_seconds}" "${case_dir}/stdout" "${case_dir}/stderr" "$@" || rc=$?
  if [[ "${rc}" -ne "${expected}" ]]; then
    record_failure "${name}" "command exited ${rc}, want ${expected}"
    cat "${case_dir}/stderr" >&2 || true
    return 1
  fi
  record_success "${name}"
}

port_accepts_tcp() {
  python3 - "$1" <<'PY_PORT_CHECK'
import socket
import sys
try:
    with socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=0.2):
        pass
except OSError:
    raise SystemExit(1)
PY_PORT_CHECK
}

run_gemini_case() {
  local expectation="$1"
  local name="$2"
  local gemini_mode="$3"
  local case_dir="${TMP_ROOT}/setup/${name//[^a-zA-Z0-9_.-]/_}"
  MOCK_GEMINI_MODEL=1 start_mock_server "${case_dir}/server"
  local port="${MOCK_SERVER_PORT}"
  write_fake_default_smoke_clients "${case_dir}/bin" "${gemini_mode}"
  local cmd=(
    env PATH="${case_dir}/bin:${ORIGINAL_PATH}" START_PROXY=0
    PROXY_HOST=127.0.0.1 PROXY_PORT="${port}" LIVE_CLI_SMOKE_DIR="${case_dir}/smoke"
    FAKE_CLAUDE_CAPTURE_DIR="${case_dir}/capture" SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1
    SMOKE_CURL_MAX_TIME_SECONDS=2 SMOKE_CLI_TIMEOUT_SECONDS=2
    "${REPO_ROOT}/scripts/live-cli-smoke.sh"
  )
  if [[ "${expectation}" == "reject" ]]; then
    expect_hard_failure_with_stderr "${name}" 8 'gemini smoke output mismatch' "${cmd[@]}"
    return
  fi
  if expect_success "${name}" 8 "${cmd[@]}"; then
    if grep -q '^==> Running Gemini smoke with model gpt-4\.1$' \
      "${TMP_ROOT}/cases/${name//[^a-zA-Z0-9_.-]/_}/stderr"; then
      record_success "${name} exercised Gemini"
    else
      record_failure "${name} exercised Gemini" "harness skipped the Gemini client"
    fi
  fi
}

log "Running deterministic smoke reliability regressions"

claude_contract_dir="${TMP_ROOT}/setup/claude-defaults-and-model-preference"
start_mock_server "${claude_contract_dir}/server"
claude_contract_port="${MOCK_SERVER_PORT}"
write_fake_default_smoke_clients "${claude_contract_dir}/bin"
claude_capture_dir="${claude_contract_dir}/capture"
if expect_success "Claude subprocess defaults and model preference" 8 \
  env -u CLAUDE_CODE_DISABLE_ADVISOR_TOOL \
    PATH="${claude_contract_dir}/bin:${ORIGINAL_PATH}" START_PROXY=0 \
    PROXY_HOST=127.0.0.1 PROXY_PORT="${claude_contract_port}" \
    LIVE_CLI_SMOKE_DIR="${claude_contract_dir}/smoke" \
    FAKE_CLAUDE_CAPTURE_DIR="${claude_capture_dir}" \
    SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1 SMOKE_CURL_MAX_TIME_SECONDS=2 SMOKE_CLI_TIMEOUT_SECONDS=2 \
    "${REPO_ROOT}/scripts/live-cli-smoke.sh"; then
  captured_advisor_default="$(cat "${claude_capture_dir}/disable-advisor-tool" 2>/dev/null || true)"
  if [[ "${captured_advisor_default}" == "1" ]]; then
    record_success "Claude subprocess disables the advisor tool by default"
  else
    record_failure "Claude subprocess disables the advisor tool by default" \
      "captured CLAUDE_CODE_DISABLE_ADVISOR_TOOL=${captured_advisor_default:-<missing>}"
  fi

  captured_claude_model="$(cat "${claude_capture_dir}/model" 2>/dev/null || true)"
  if [[ "${captured_claude_model}" == "claude-haiku-5-5" ]]; then
    record_success "Claude model selection uses its recognized Haiku alias despite catalogued Sonnet models"
  else
    record_failure "Claude model selection uses its recognized Haiku alias despite catalogued Sonnet models" \
      "captured model=${captured_claude_model:-<missing>}"
  fi

  captured_thinking_tokens="$(cat "${claude_capture_dir}/max-thinking-tokens" 2>/dev/null || true)"
  if [[ "${captured_thinking_tokens}" == "0" ]]; then
    record_success "Claude file-reading smoke disables thinking"
  else
    record_failure "Claude file-reading smoke disables thinking" \
      "captured MAX_THINKING_TOKENS=${captured_thinking_tokens:-<missing>}"
  fi
fi

escape_client="${TMP_ROOT}/json-wrapped-client"
write_fake_client "${escape_client}" json-wrapped
escape_case="${TMP_ROOT}/json-wrapped-fixtures"
mkdir -p "${escape_case}"
printf '%s' 'left "quote" \ slash' > "${escape_case}/left.txt"
printf 'right\ncontrol' > "${escape_case}/right.txt"
escape_output="$(cd "${escape_case}" && "${escape_client}")"
escape_left="${escape_output%%|*}"
escape_right="${escape_output#*|}"
if [[ "$(jq -r '.output' <<< "${escape_left}")" == 'left "quote" \ slash' ]] &&
   [[ "$(jq -r '.output' <<< "${escape_right}")" == $'right\ncontrol' ]]; then
  record_success "fake Gemini JSON wrapper escapes arbitrary fixture content"
else
  record_failure "fake Gemini JSON wrapper escapes arbitrary fixture content" "wrapper output was not valid escaped JSON: ${escape_output}"
fi

render_block="${TMP_ROOT}/k8s-render-kustomization.txt"
awk '
  /<<EOF_KUSTOMIZE$/ { capture=1; next }
  capture && /^EOF_KUSTOMIZE$/ { exit }
  capture { print }
' "${REPO_ROOT}/scripts/k8s-kind-smoke.sh" > "${render_block}"
pull_policy_path_count="$(grep -cF 'path: /spec/template/spec/containers/0/imagePullPolicy' "${render_block}" || true)"
pull_policy_value_count="$(grep -cF 'value: IfNotPresent' "${render_block}" || true)"
if [[ "${pull_policy_path_count}" == "1" && "${pull_policy_value_count}" == "1" ]]; then
  record_success "k8s render imagePullPolicy patch is unique"
else
  record_failure "k8s render imagePullPolicy patch is unique" \
    "found ${pull_policy_path_count} paths and ${pull_policy_value_count} IfNotPresent values"
fi

readiness_timeout="$(awk '
  $1 == "readinessProbe:" { in_readiness=1; next }
  in_readiness && $1 == "timeoutSeconds:" { print $2; exit }
' "${REPO_ROOT}/k8s/vekil.yaml")"
if [[ "${readiness_timeout}" =~ ^[0-9]+$ ]] && (( readiness_timeout >= 10 )); then
  record_success "k8s readiness timeout covers the 10s provider probe"
else
  record_failure "k8s readiness timeout covers the 10s provider probe" \
    "timeoutSeconds=${readiness_timeout:-<missing>}"
fi

startup_values="$(awk '
  $1 == "startupProbe:" { in_startup=1; next }
  in_startup && $1 == "timeoutSeconds:" { timeout=$2 }
  in_startup && $1 == "periodSeconds:" { period=$2 }
  in_startup && $1 == "failureThreshold:" { print timeout, period, $2; exit }
' "${REPO_ROOT}/k8s/vekil.yaml")"
read -r startup_timeout startup_period startup_failures <<< "${startup_values}"
startup_budget=0
if [[ "${startup_period:-}" =~ ^[0-9]+$ && "${startup_failures:-}" =~ ^[0-9]+$ ]]; then
  startup_budget=$((startup_period * startup_failures))
fi
if [[ "${startup_timeout:-}" =~ ^[0-9]+$ \
  && "${startup_period:-}" =~ ^[0-9]+$ \
  && "${startup_failures:-}" =~ ^[0-9]+$ \
  && "${startup_period}" -ge "${startup_timeout}" \
  && "${startup_budget}" -ge 60 ]]; then
  record_success "k8s startup probe has a coherent >=60s failure budget"
else
  record_failure "k8s startup probe has a coherent >=60s failure budget" \
    "timeout=${startup_timeout:-<missing>} period=${startup_period:-<missing>} failures=${startup_failures:-<missing>} budget=${startup_budget}s"
fi

case_dir="${TMP_ROOT}/setup/stale-listener"
start_mock_server "${case_dir}/server"
port="${MOCK_SERVER_PORT}"
write_fake_default_smoke_clients "${case_dir}/bin"
write_bind_failure_proxy "${case_dir}/bind-fail-proxy"
expect_hard_failure "stale healthy listener while spawned proxy bind fails" 6 \
  env PATH="${case_dir}/bin:${ORIGINAL_PATH}" START_PROXY=1 \
    PROXY_HOST=127.0.0.1 PROXY_PORT="${port}" PROXY_BIN="${case_dir}/bind-fail-proxy" \
    PROXY_TOKEN_DIR="${case_dir}/token" LIVE_CLI_SMOKE_DIR="${case_dir}/smoke" \
    FAKE_CLAUDE_CAPTURE_DIR="${case_dir}/capture" SMOKE_STARTUP_TIMEOUT_SECONDS=2 \
    SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1 SMOKE_CURL_MAX_TIME_SECONDS=1 SMOKE_CLI_TIMEOUT_SECONDS=2 \
    "${REPO_ROOT}/scripts/live-cli-smoke.sh"

expect_hard_failure "compact smoke rejects stale healthy listener" 6 \
  env START_PROXY=1 PROXY_HOST=127.0.0.1 PROXY_PORT="${port}" \
    PROXY_BIN="${case_dir}/bind-fail-proxy" LIVE_COMPACT_SMOKE_DIR="${case_dir}/compact" \
    PROXY_TOKEN_DIR="${case_dir}/token" SMOKE_STARTUP_TIMEOUT_SECONDS=2 \
    SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1 SMOKE_CURL_MAX_TIME_SECONDS=1 \
    "${REPO_ROOT}/scripts/live-compact-smoke.sh"

auto_dir="${TMP_ROOT}/setup/auto-port"
write_fake_default_smoke_clients "${auto_dir}/bin"
write_healthy_proxy "${auto_dir}/healthy-proxy"
if expect_success "auto port is isolated and released" 10 \
  env PATH="${auto_dir}/bin:${ORIGINAL_PATH}" START_PROXY=1 \
    PROXY_HOST=127.0.0.1 PROXY_BIN="${auto_dir}/healthy-proxy" PROXY_TOKEN_DIR="${auto_dir}/token" \
    LIVE_CLI_SMOKE_DIR="${auto_dir}/smoke" FAKE_CLAUDE_CAPTURE_DIR="${auto_dir}/capture" \
    SMOKE_STARTUP_TIMEOUT_SECONDS=3 SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1 \
    SMOKE_CURL_MAX_TIME_SECONDS=2 SMOKE_CLI_TIMEOUT_SECONDS=2 \
    "${REPO_ROOT}/scripts/live-cli-smoke.sh"; then
  auto_stderr="${TMP_ROOT}/cases/auto_port_is_isolated_and_released/stderr"
  auto_port="$(sed -nE 's/.*Starting proxy at http:\/\/127\.0\.0\.1:([0-9]+).*/\1/p' "${auto_stderr}" | head -1)"
  if [[ -z "${auto_port}" || "${auto_port}" == "1337" ]]; then
    record_failure "auto port selection" "unexpected selected port ${auto_port:-<missing>}"
  elif port_accepts_tcp "${auto_port}"; then
    record_failure "auto port cleanup" "port ${auto_port} still accepts TCP after script exit"
  else
    record_success "auto port selection and cleanup verification"
  fi
fi
mixed_compact_dir="${TMP_ROOT}/setup/mixed-log-compact"
write_healthy_proxy "${mixed_compact_dir}/healthy-proxy"
if expect_success "compact listener tolerates mixed JSON and plain-text logs" 10 \
  env START_PROXY=1 PROXY_HOST=127.0.0.1 PROXY_BIN="${mixed_compact_dir}/healthy-proxy" \
    LIVE_COMPACT_SMOKE_DIR="${mixed_compact_dir}/smoke" PROXY_TOKEN_DIR="${mixed_compact_dir}/token" \
    SMOKE_STARTUP_TIMEOUT_SECONDS=3 SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1 \
    SMOKE_CURL_MAX_TIME_SECONDS=2 "${REPO_ROOT}/scripts/live-compact-smoke.sh"; then
  :
fi

compact_quota_dir="${TMP_ROOT}/setup/compact-quota"
start_mock_server "${compact_quota_dir}/server" 402 quota_exceeded
expect_exit_code "compact smoke reports exact Copilot quota exhaustion" 8 75 \
  env START_PROXY=0 PROXY_HOST=127.0.0.1 PROXY_PORT="${MOCK_SERVER_PORT}" \
    LIVE_COMPACT_SMOKE_DIR="${compact_quota_dir}/smoke" SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1 \
    SMOKE_CURL_MAX_TIME_SECONDS=2 "${REPO_ROOT}/scripts/live-compact-smoke.sh"

compact_unknown_402_dir="${TMP_ROOT}/setup/compact-unknown-402"
start_mock_server "${compact_unknown_402_dir}/server" 402 payment_required
expect_exit_code "compact smoke keeps unknown HTTP 402 errors hard" 8 1 \
  env START_PROXY=0 PROXY_HOST=127.0.0.1 PROXY_PORT="${MOCK_SERVER_PORT}" \
    LIVE_COMPACT_SMOKE_DIR="${compact_unknown_402_dir}/smoke" SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1 \
    SMOKE_CURL_MAX_TIME_SECONDS=2 "${REPO_ROOT}/scripts/live-compact-smoke.sh"

replay_quota_dir="${TMP_ROOT}/setup/replay-quota"
start_mock_server "${replay_quota_dir}/server" 200 "" 402 quota_exceeded
expect_exit_code "compact replay keeps quota errors hard" 8 1 \
  env START_PROXY=0 PROXY_HOST=127.0.0.1 PROXY_PORT="${MOCK_SERVER_PORT}" \
    LIVE_COMPACT_SMOKE_DIR="${replay_quota_dir}/smoke" SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1 \
    SMOKE_CURL_MAX_TIME_SECONDS=2 "${REPO_ROOT}/scripts/live-compact-smoke.sh"

wrapped_escape_dir="${TMP_ROOT}/setup/gemini-wrapped-json-escaping"
mkdir -p "${wrapped_escape_dir}/bin" "${wrapped_escape_dir}/case"
write_fake_client "${wrapped_escape_dir}/bin/gemini" wrapped-output
printf '%s' 'left"\line' > "${wrapped_escape_dir}/case/left.txt"
printf 'right\nline' > "${wrapped_escape_dir}/case/right.txt"
if (
  cd "${wrapped_escape_dir}/case"
  "${wrapped_escape_dir}/bin/gemini" > output.txt
  python3 - <<'PY_WRAPPED_ASSERT'
import json
import pathlib

raw = pathlib.Path("output.txt").read_text().strip()
decoder = json.JSONDecoder()
left, offset = decoder.raw_decode(raw)
assert raw[offset:offset + 1] == "|"
right, end = decoder.raw_decode(raw, offset + 1)
assert end == len(raw)
assert left == {"output": 'left"\\line'}
assert right == {"output": "right\nline"}
PY_WRAPPED_ASSERT
); then
  record_success "Gemini wrapped Read outputs JSON-escape quotes, backslashes, and newlines"
else
  record_failure "Gemini wrapped Read output escaping" "generated wrapper was not valid compact JSON"
fi

run_gemini_case accept "Gemini plain text output passes" pass
run_gemini_case accept "Gemini wrapped Read outputs normalize to exact text" wrapped-output
run_gemini_case accept "Gemini strict JSON wrapper around complete result normalizes to exact text" json-wrapped-whole
run_gemini_case accept "Gemini strict JSON wrapper sequence normalizes to exact text" json-wrapped
run_gemini_case reject "Gemini dangling JSON wrapper separator is rejected" json-wrapped-trailing-separator
run_gemini_case reject "Gemini three-wrapper sequence is rejected" json-wrapped-three
case_dir="${TMP_ROOT}/setup/never-responds"
write_fake_default_smoke_clients "${case_dir}/bin"
write_hanging_proxy "${case_dir}/hanging-proxy"
expect_hard_failure "accepts TCP but never responds exits within deadline" 7 \
  env PATH="${case_dir}/bin:${ORIGINAL_PATH}" START_PROXY=1 \
    PROXY_HOST=127.0.0.1 PROXY_BIN="${case_dir}/hanging-proxy" PROXY_TOKEN_DIR="${case_dir}/token" \
    LIVE_CLI_SMOKE_DIR="${case_dir}/smoke" FAKE_CLAUDE_CAPTURE_DIR="${case_dir}/capture" \
    SMOKE_STARTUP_TIMEOUT_SECONDS=2 SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1 \
    SMOKE_CURL_MAX_TIME_SECONDS=1 SMOKE_CLI_TIMEOUT_SECONDS=1 \
    "${REPO_ROOT}/scripts/live-cli-smoke.sh"

case_dir="${TMP_ROOT}/setup/fork-sleeper"
start_mock_server "${case_dir}/server"
port="${MOCK_SERVER_PORT}"
write_fake_default_smoke_clients "${case_dir}/bin"
cat > "${case_dir}/bin/codex" <<'EOF_FORK_CODEX'
#!/usr/bin/env bash
sleep 300 &
printf '%s\n' "$!" > "${FAKE_CLI_CHILD_PID_FILE:?}"
exit 42
EOF_FORK_CODEX
chmod +x "${case_dir}/bin/codex"
child_pid_file="${case_dir}/child.pid"
expect_hard_failure_with_stderr "fake CLI forks sleeper" 8 \
  'Codex CLI failed or timed out' \
  env PATH="${case_dir}/bin:${ORIGINAL_PATH}" START_PROXY=0 \
    PROXY_HOST=127.0.0.1 PROXY_PORT="${port}" LIVE_CLI_SMOKE_DIR="${case_dir}/smoke" \
    FAKE_CLI_CHILD_PID_FILE="${child_pid_file}" FAKE_CLAUDE_CAPTURE_DIR="${case_dir}/capture" \
    SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1 SMOKE_CURL_MAX_TIME_SECONDS=2 SMOKE_CLI_TIMEOUT_SECONDS=2 \
    "${REPO_ROOT}/scripts/live-cli-smoke.sh"
if [[ ! -s "${child_pid_file}" ]]; then
  record_failure "fake CLI forks sleeper cleanup" "fake CLI never recorded its child pid"
else
  child_pid="$(cat "${child_pid_file}")"
  sleep 0.2
  if process_is_running "${child_pid}"; then
    kill "${child_pid}" 2>/dev/null || true
    record_failure "fake CLI forks sleeper cleanup" "child ${child_pid} remained alive"
  else
    record_success "fake CLI forks sleeper cleanup"
  fi
fi
stop_mock_servers
mock_server_leaks=0
for pid in "${server_pids[@]:-}"; do
  if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null; then
    record_failure "mock server cleanup" "PID ${pid} remained alive"
    mock_server_leaks=1
  fi
done
for port in "${server_ports[@]:-}"; do
  if [[ -n "${port}" ]] && port_accepts_tcp "${port}"; then
    record_failure "mock server cleanup" "listener 127.0.0.1:${port} remained open"
    mock_server_leaks=1
  fi
done
if [[ "${mock_server_leaks}" == "0" ]]; then
  record_success "all tracked mock servers and listeners were cleaned up"
fi

if [[ "${failures}" -ne 0 ]]; then
  printf '%s deterministic smoke reliability test(s) failed\n' "${failures}" >&2
  exit 1
fi

log "All deterministic smoke reliability regressions passed"
