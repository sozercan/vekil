#!/usr/bin/env bash

# Focused live semantic-policy smoke for Responses-native GPT-5.6 Sol.
#
# The caller supplies an already-authenticated loopback Vekil bridge. This
# harness inserts a metadata-only capture shim, runs one policy proxy whose two
# terminal tiers both target gpt-5.6-sol, and proves prompt-selected low/max
# effort overrides conflicting public Responses values. Request content is never
# logged.
#
# Required environment:
#   LIVE_POLICY_ROUTING_SOL_BRIDGE_BASE_URL   loopback bridge root, e.g. http://127.0.0.1:12345
#
# Optional environment:
#   PROXY_BIN                                 default: ./vekil
#   LIVE_POLICY_ROUTING_SOL_MODEL             default: gpt-5.6-sol
#   LIVE_POLICY_ROUTING_SOL_PUBLIC_MODEL      default: gpt-5.6-semantic
#   LIVE_POLICY_ROUTING_SOL_SMOKE_DIR         explicit artifact directory
#   LIVE_POLICY_ROUTING_SOL_KEEP_ARTIFACTS=0  delete artifacts after success
#   SMOKE_*                                    bounded timeout overrides

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

require_env() {
  local name="$1"
  [[ -n "${!name:-}" ]] || die "required environment variable is empty: ${name}"
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
PROXY_BIN="${PROXY_BIN:-${REPO_ROOT}/vekil}"
BRIDGE_BASE_URL="${LIVE_POLICY_ROUTING_SOL_BRIDGE_BASE_URL:-}"
SOL_MODEL="${LIVE_POLICY_ROUTING_SOL_MODEL:-gpt-5.6-sol}"
PUBLIC_MODEL="${LIVE_POLICY_ROUTING_SOL_PUBLIC_MODEL:-gpt-5.6-semantic}"
SMOKE_STARTUP_TIMEOUT_SECONDS="${SMOKE_STARTUP_TIMEOUT_SECONDS:-120}"
SMOKE_CURL_CONNECT_TIMEOUT_SECONDS="${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS:-5}"
SMOKE_CURL_MAX_TIME_SECONDS="${SMOKE_CURL_MAX_TIME_SECONDS:-300}"
SMOKE_PROCESS_TERM_GRACE_SECONDS="${SMOKE_PROCESS_TERM_GRACE_SECONDS:-8}"
SMOKE_PORT_RELEASE_TIMEOUT_SECONDS="${SMOKE_PORT_RELEASE_TIMEOUT_SECONDS:-8}"
SMOKE_DIAGNOSTIC_MAX_BYTES="${SMOKE_DIAGNOSTIC_MAX_BYTES:-32768}"

TMP_PARENT="${LIVE_POLICY_ROUTING_TMP_PARENT:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}}"
SMOKE_DIR="${LIVE_POLICY_ROUTING_SOL_SMOKE_DIR:-$(mktemp -d "${TMP_PARENT%/}/live-policy-routing-sol-effort.XXXXXX")}"
if [[ "${SMOKE_DIR}" != /* ]]; then
  SMOKE_DIR="${PWD}/${SMOKE_DIR}"
fi
umask 077
mkdir -p "${SMOKE_DIR}"
chmod 700 "${SMOKE_DIR}"

MODELS_JSON="${SMOKE_DIR}/bridge-models.json"
CONFIG_JSON="${SMOKE_DIR}/providers.json"
CAPTURE_SCRIPT="${SMOKE_DIR}/capture-shim.py"
CAPTURE_LOG="${SMOKE_DIR}/capture-events.jsonl"
CAPTURE_STDERR="${SMOKE_DIR}/capture-shim.log"
LABEL_FILE="${SMOKE_DIR}/capture-label"
PROXY_LOG="${SMOKE_DIR}/proxy.log"
SUMMARY_FILE="${SMOKE_DIR}/summary.txt"

capture_pid=""
capture_pgid=""
capture_port=""
proxy_pid=""
proxy_pgid=""
proxy_port=""

python_command() {
  if command -v python3 >/dev/null 2>&1; then
    command -v python3
    return
  fi
  command -v python >/dev/null 2>&1 || die "python3 (or python) is required"
  command -v python
}

validate_positive_integer() {
  local name="$1"
  local value="$2"
  [[ "${value}" =~ ^[1-9][0-9]*$ ]] || die "${name} must be a positive integer: ${value}"
}

allocate_free_port() {
  "$(python_command)" - <<'PY'
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

process_group_is_alive() {
  local pgid="$1"
  [[ -n "${pgid}" ]] || return 1
  kill -0 -- "-${pgid}" 2>/dev/null
}

terminate_process_group() {
  local pid="$1"
  local pgid="$2"
  local deadline
  [[ -n "${pgid}" ]] || return 0
  kill -TERM -- "-${pgid}" 2>/dev/null || true
  deadline=$((SECONDS + SMOKE_PROCESS_TERM_GRACE_SECONDS))
  while (( SECONDS < deadline )); do
    process_group_is_alive "${pgid}" || break
    sleep 0.1
  done
  if process_group_is_alive "${pgid}"; then
    kill -KILL -- "-${pgid}" 2>/dev/null || true
  fi
  [[ -z "${pid}" ]] || wait "${pid}" 2>/dev/null || true
}

port_is_open() {
  local port="$1"
  "$(python_command)" - "${port}" <<'PY'
import socket
import sys
with socket.socket() as sock:
    sock.settimeout(0.2)
    raise SystemExit(0 if sock.connect_ex(("127.0.0.1", int(sys.argv[1]))) == 0 else 1)
PY
}

wait_for_port_release() {
  local port="$1"
  local deadline=$((SECONDS + SMOKE_PORT_RELEASE_TIMEOUT_SECONDS))
  while (( SECONDS < deadline )); do
    port_is_open "${port}" || return 0
    sleep 0.1
  done
  ! port_is_open "${port}"
}

print_diagnostic_file() {
  local label="$1"
  local path="$2"
  [[ -f "${path}" ]] || return 0
  printf '\n--- %s ---\n' "${label}" >&2
  head -c "${SMOKE_DIAGNOSTIC_MAX_BYTES}" "${path}" >&2 || true
  printf '\n' >&2
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  terminate_process_group "${proxy_pid}" "${proxy_pgid}"
  terminate_process_group "${capture_pid}" "${capture_pgid}"
  if [[ -n "${proxy_port}" ]] && ! wait_for_port_release "${proxy_port}"; then
    printf 'error: policy proxy port %s remained open\n' "${proxy_port}" >&2
    rc=1
  fi
  if [[ -n "${capture_port}" ]] && ! wait_for_port_release "${capture_port}"; then
    printf 'error: capture shim port %s remained open\n' "${capture_port}" >&2
    rc=1
  fi
  if [[ "${rc}" -ne 0 ]]; then
    print_diagnostic_file "summary.txt" "${SUMMARY_FILE}"
    print_diagnostic_file "capture-events.jsonl" "${CAPTURE_LOG}"
    print_diagnostic_file "capture-shim.log" "${CAPTURE_STDERR}"
    print_diagnostic_file "proxy.log" "${PROXY_LOG}"
  elif [[ "${LIVE_POLICY_ROUTING_SOL_KEEP_ARTIFACTS:-0}" == "0" ]]; then
    rm -rf "${SMOKE_DIR}"
  fi
  exit "${rc}"
}
trap cleanup EXIT INT TERM

validate_inputs() {
  require_cmd curl
  require_cmd jq
  require_cmd ps
  require_env LIVE_POLICY_ROUTING_SOL_BRIDGE_BASE_URL
  python_command >/dev/null
  validate_positive_integer SMOKE_STARTUP_TIMEOUT_SECONDS "${SMOKE_STARTUP_TIMEOUT_SECONDS}"
  validate_positive_integer SMOKE_CURL_MAX_TIME_SECONDS "${SMOKE_CURL_MAX_TIME_SECONDS}"
  [[ -x "${PROXY_BIN}" ]] || die "proxy binary not found or not executable: ${PROXY_BIN} (run: make build)"
  [[ "${SOL_MODEL}" != "${PUBLIC_MODEL}" ]] || die "Sol upstream model must differ from public semantic model"

  "$(python_command)" - "${BRIDGE_BASE_URL}" "${PUBLIC_MODEL}" "${SOL_MODEL}" <<'PY'
import sys
import urllib.parse
bridge, public_model, sol_model = sys.argv[1:]
parsed = urllib.parse.urlsplit(bridge.rstrip("/"))
if parsed.scheme != "http" or parsed.hostname not in {"127.0.0.1", "localhost"} or not parsed.port:
    raise SystemExit("LIVE_POLICY_ROUTING_SOL_BRIDGE_BASE_URL must be an absolute loopback HTTP URL with an explicit port")
if parsed.username or parsed.password or parsed.query or parsed.fragment:
    raise SystemExit("LIVE_POLICY_ROUTING_SOL_BRIDGE_BASE_URL must not contain credentials, query, or fragment")
for name, value in (("public model", public_model), ("Sol model", sol_model)):
    if not value.strip() or len(value.encode()) > 128 or any(ord(ch) < 32 or ord(ch) == 127 for ch in value):
        raise SystemExit(f"{name} must be non-empty, control-free, and at most 128 bytes")
PY
}

fetch_and_validate_bridge_model() {
  curl --fail --silent --show-error \
    --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${SMOKE_CURL_MAX_TIME_SECONDS}" \
    "${BRIDGE_BASE_URL%/}/v1/models" > "${MODELS_JSON}" \
    || die "GET ${BRIDGE_BASE_URL%/}/v1/models failed"
  chmod 600 "${MODELS_JSON}"
  jq -e --arg model "${SOL_MODEL}" '
    [.data[]?
      | select(.id == $model)
      | select(((.supported_endpoints // []) | index("/responses")) != null)
      | select(((.capabilities.supports.reasoning_effort // []) | index("low")) != null)
      | select(((.capabilities.supports.reasoning_effort // []) | index("max")) != null)
    ] | length == 1
  ' "${MODELS_JSON}" >/dev/null || die "bridge must advertise ${SOL_MODEL} with /responses plus low and max reasoning effort"
}

write_capture_shim() {
  cat > "${CAPTURE_SCRIPT}" <<'PY'
#!/usr/bin/env python3
import argparse
import http.client
import http.server
import json
import pathlib
import threading
import urllib.parse

parser = argparse.ArgumentParser()
parser.add_argument("--port", required=True, type=int)
parser.add_argument("--bridge", required=True)
parser.add_argument("--events", required=True)
parser.add_argument("--label", required=True)
args = parser.parse_args()
bridge = urllib.parse.urlsplit(args.bridge.rstrip("/"))
lock = threading.Lock()

def has_classifier_tool(value):
    if isinstance(value, dict):
        if value.get("name") == "emit_policy_signals":
            return True
        return any(has_classifier_tool(item) for item in value.values())
    if isinstance(value, list):
        return any(has_classifier_tool(item) for item in value)
    return False

def write_event(event):
    with lock:
        with open(args.events, "a", encoding="utf-8") as handle:
            handle.write(json.dumps(event, separators=(",", ":")) + "\n")

class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def log_message(self, *_args):
        pass

    def do_POST(self):
        length = int(self.headers.get("content-length", "0"))
        raw = self.rfile.read(length)
        try:
            body = json.loads(raw)
        except Exception:
            body = {}
        reasoning = body.get("reasoning") if isinstance(body, dict) else None
        effort = reasoning.get("effort") if isinstance(reasoning, dict) else None
        label_path = pathlib.Path(args.label)
        label = label_path.read_text(encoding="utf-8").strip() if label_path.exists() else ""
        event = {
            "label": label,
            "kind": "classifier" if has_classifier_tool(body.get("tools", [])) else "terminal",
            "path": self.path,
            "model": body.get("model"),
            "effort": effort,
            "stream": body.get("stream"),
        }
        try:
            connection = http.client.HTTPConnection(bridge.hostname, bridge.port, timeout=300)
            connection.request(
                "POST",
                (bridge.path.rstrip("/") + "/v1/responses") or "/v1/responses",
                body=raw,
                headers={"content-type": self.headers.get("content-type", "application/json")},
            )
            response = connection.getresponse()
            data = response.read()
            event["status"] = response.status
            write_event(event)
            self.send_response(response.status)
            for name, value in response.getheaders():
                if name.lower() in {"content-type", "openai-model", "x-openai-model", "ratelimit-remaining", "retry-after"}:
                    self.send_header(name, value)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            connection.close()
        except Exception as exc:
            event["status"] = 502
            event["error"] = type(exc).__name__
            write_event(event)
            self.send_error(502, "loopback bridge request failed")

server = http.server.ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
server.serve_forever(poll_interval=0.1)
PY
  chmod 700 "${CAPTURE_SCRIPT}"
}

start_capture_shim() {
  capture_port="$(allocate_free_port)"
  : > "${CAPTURE_LOG}"
  : > "${CAPTURE_STDERR}"
  printf 'preflight\n' > "${LABEL_FILE}"
  chmod 600 "${CAPTURE_LOG}" "${CAPTURE_STDERR}" "${LABEL_FILE}"
  set -m
  "$(python_command)" "${CAPTURE_SCRIPT}" \
    --port "${capture_port}" \
    --bridge "${BRIDGE_BASE_URL}" \
    --events "${CAPTURE_LOG}" \
    --label "${LABEL_FILE}" \
    > /dev/null 2>"${CAPTURE_STDERR}" &
  capture_pid="$!"
  capture_pgid="${capture_pid}"
  set +m
  local deadline=$((SECONDS + 10))
  while (( SECONDS < deadline )); do
    port_is_open "${capture_port}" && return 0
    sleep 0.1
  done
  die "capture shim did not listen on 127.0.0.1:${capture_port}"
}

write_policy_config() {
  "$(python_command)" - "${CONFIG_JSON}" "${capture_port}" "${PUBLIC_MODEL}" "${SOL_MODEL}" <<'PY'
import json
import os
import pathlib
import sys
path, capture_port, public_model, sol_model = sys.argv[1:]

def route(route_id, effort=None, classifier=False):
    value = {
        "id": route_id,
        "exposure": "internal",
        "endpoints": ["/responses"],
        "drop_sampling_params": True,
        "targets": [{"id": route_id + "-target", "provider": "bridge", "upstream_model": sol_model}],
        "routing": {"mode": "primary_only", "max_target_attempts": 1, "max_upstream_sends": 1},
    }
    if classifier:
        value["internal_purpose"] = "policy_classifier"
    else:
        value["parallel_tool_calls"] = True
        value["reasoning_effort"] = [effort]
    return value

config = {
    "schema_version": 2,
    "providers": [{
        "id": "bridge",
        "type": "openai-compatible",
        "base_url": f"http://127.0.0.1:{capture_port}/v1",
        "auth_type": "none",
        "model_discovery": "static",
        "trust_domain": "github-copilot-loopback",
        "classifier_no_store_supported": False,
    }],
    "model_routes": [
        route("semantic-sol-low", "low"),
        route("semantic-sol-max", "max"),
        route("semantic-sol-classifier", classifier=True),
    ],
    "policy_profiles": [{
        "id": "gpt-5-6-semantic-sol-policy",
        "public_id": public_model,
        "name": "GPT-5.6 Semantic Sol Effort Smoke",
        "mode": "enforce",
        "lightweight": {"route": "semantic-sol-low", "reasoning_effort": "low"},
        "powerful": {"route": "semantic-sol-max", "reasoning_effort": "max"},
        "baseline_tier": "lightweight",
        "classifier_unavailable_tier": "lightweight",
        "classifier_uncertain_tier": "powerful",
        "classifier": {"route": "semantic-sol-classifier", "timeout_ms": 10000},
        "data_policy": {"content_forwarding_acknowledged": True, "allow_provider_retention": True},
    }],
}
pathlib.Path(path).write_text(json.dumps(config, indent=2) + "\n", encoding="utf-8")
os.chmod(path, 0o600)
PY
}

wait_for_proxy_ready() {
  local deadline=$((SECONDS + SMOKE_STARTUP_TIMEOUT_SECONDS))
  while (( SECONDS < deadline )); do
    if curl --fail --silent --show-error \
      --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
      --max-time 3 \
      "http://127.0.0.1:${proxy_port}/readyz" >/dev/null 2>&1; then
      return 0
    fi
    process_group_is_alive "${proxy_pgid}" || return 1
    sleep 0.25
  done
  return 1
}

start_policy_proxy() {
  proxy_port="$(allocate_free_port)"
  : > "${PROXY_LOG}"
  chmod 600 "${PROXY_LOG}"
  set -m
  "${PROXY_BIN}" \
    --host 127.0.0.1 \
    --port "${proxy_port}" \
    --providers-config "${CONFIG_JSON}" \
    --policy-routing enforce \
    --log-level info \
    > "${PROXY_LOG}" 2>&1 &
  proxy_pid="$!"
  proxy_pgid="${proxy_pid}"
  set +m
  wait_for_proxy_ready || die "policy proxy did not become ready on 127.0.0.1:${proxy_port}"
}

fetch_stats() {
  local output="$1"
  curl --fail --silent --show-error \
    --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${SMOKE_CURL_MAX_TIME_SECONDS}" \
    "http://127.0.0.1:${proxy_port}/stats.json" > "${output}"
  chmod 600 "${output}"
}

profile_metric() {
  local path="$1"
  local expression="$2"
  jq -er --arg profile "${PUBLIC_MODEL}" '
    ([.policy_routing.profiles[]? | select(.profile == $profile)][0] // error("policy profile missing"))
    | '"${expression}" "${path}"
}

assert_delta() {
  local label="$1"
  local before="$2"
  local after="$3"
  local want="$4"
  [[ $((after - before)) -eq "${want}" ]] || die "${label} delta=$((after - before)), want ${want}"
}

write_request() {
  local output="$1"
  local prompt="$2"
  local client_effort="$3"
  local max_tokens="$4"
  jq -n \
    --arg model "${PUBLIC_MODEL}" \
    --arg prompt "${prompt}" \
    --arg effort "${client_effort}" \
    --argjson max_tokens "${max_tokens}" '
      {
        model: $model,
        input: $prompt,
        reasoning: {effort:$effort},
        max_output_tokens: $max_tokens,
        store: false,
        stream: false
      }
    ' > "${output}"
  chmod 600 "${output}"
}

post_request() {
  local label="$1"
  local request="$2"
  local response="$3"
  local status_file="$4"
  printf '%s\n' "${label}" > "${LABEL_FILE}"
  local status
  status="$(curl --silent --show-error \
    --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${SMOKE_CURL_MAX_TIME_SECONDS}" \
    --header 'Content-Type: application/json' \
    --data-binary "@${request}" \
    --output "${response}" \
    --write-out '%{http_code}' \
    "http://127.0.0.1:${proxy_port}/v1/responses")" || die "${label} request failed"
  printf '%s\n' "${status}" > "${status_file}"
  chmod 600 "${response}" "${status_file}"
  [[ "${status}" == "200" ]] || die "${label} status=${status}, want 200"
  jq -e --arg model "${PUBLIC_MODEL}" '
    .object == "response"
    and .model == $model
    and (.status == "completed" or .status == "incomplete")
  ' "${response}" >/dev/null || die "${label} response did not preserve the semantic public Responses identity"
}

validate_capture_events() {
  "$(python_command)" - "${CAPTURE_LOG}" "${SOL_MODEL}" <<'PY'
import json
import sys
events_path, sol_model = sys.argv[1:]
events = [json.loads(line) for line in open(events_path, encoding="utf-8") if line.strip()]
classifiers = [event for event in events if event.get("kind") == "classifier"]
if not classifiers:
    raise SystemExit("no classifier request was captured")
if any(event.get("effort") is not None for event in classifiers):
    raise SystemExit(f"classifier request received terminal effort: {classifiers}")
if any(event.get("path") != "/v1/responses" for event in events):
    raise SystemExit(f"non-Responses upstream path captured: {events}")
for label, expected in (("simple", "low"), ("complex", "max")):
    selected = [event for event in events if event.get("label") == label]
    label_classifiers = [event for event in selected if event.get("kind") == "classifier"]
    terminals = [event for event in selected if event.get("kind") == "terminal"]
    if len(label_classifiers) != 1:
        raise SystemExit(f"{label}: classifier requests={len(label_classifiers)}, want 1")
    if label_classifiers[0].get("status") != 200:
        raise SystemExit(f"{label}: classifier upstream status={label_classifiers[0].get('status')!r}, want 200")
    if len(terminals) != 1:
        raise SystemExit(f"{label}: terminal requests={len(terminals)}, want 1")
    terminal = terminals[0]
    if terminal.get("model") != sol_model:
        raise SystemExit(f"{label}: terminal model={terminal.get('model')!r}, want {sol_model!r}")
    if terminal.get("effort") != expected:
        raise SystemExit(f"{label}: terminal effort={terminal.get('effort')!r}, want {expected!r}")
    if terminal.get("status") != 200:
        raise SystemExit(f"{label}: terminal upstream status={terminal.get('status')!r}, want 200")
    if terminal.get("stream") is not False:
        raise SystemExit(f"{label}: terminal stream={terminal.get('stream')!r}, want false")
PY
}

run_requests() {
  local before="${SMOKE_DIR}/before.stats.json"
  local after_simple="${SMOKE_DIR}/after-simple.stats.json"
  local after_complex="${SMOKE_DIR}/after-complex.stats.json"
  local simple_request="${SMOKE_DIR}/simple.request.json"
  local simple_response="${SMOKE_DIR}/simple.response.json"
  local simple_status="${SMOKE_DIR}/simple.status"
  local complex_request="${SMOKE_DIR}/complex.request.json"
  local complex_response="${SMOKE_DIR}/complex.response.json"
  local complex_status="${SMOKE_DIR}/complex.status"

  fetch_stats "${before}"
  write_request "${simple_request}" \
    "SOL_LOW_TASK_SENTINEL: In one sentence, explain what path/filepath.Join does. This is a bounded read-only single-function lookup; do not inspect files or plan changes." \
    max 256
  post_request simple "${simple_request}" "${simple_response}" "${simple_status}"
  fetch_stats "${after_simple}"
  assert_delta "simple classifier sends" \
    "$(profile_metric "${before}" '.totals.physical_classifier_sends')" \
    "$(profile_metric "${after_simple}" '.totals.physical_classifier_sends')" 1
  assert_delta "simple classifier completions" \
    "$(profile_metric "${before}" '.totals.classifier.completion')" \
    "$(profile_metric "${after_simple}" '.totals.classifier.completion')" 1
  assert_delta "simple lightweight tier" \
    "$(profile_metric "${before}" '.totals.actual_tiers.lightweight')" \
    "$(profile_metric "${after_simple}" '.totals.actual_tiers.lightweight')" 1
  printf 'PASS sol-simple-conflicting-max-routed-low\n' >> "${SUMMARY_FILE}"

  write_request "${complex_request}" \
    "SOL_MAX_TASK_SENTINEL: produce a coordinated cross-module remediation plan covering authentication, durable storage, streaming cancellation, schema migration, rollback safety, and multi-file race tests." \
    low 1024
  post_request complex "${complex_request}" "${complex_response}" "${complex_status}"
  fetch_stats "${after_complex}"
  assert_delta "complex classifier sends" \
    "$(profile_metric "${after_simple}" '.totals.physical_classifier_sends')" \
    "$(profile_metric "${after_complex}" '.totals.physical_classifier_sends')" 1
  assert_delta "complex classifier completions" \
    "$(profile_metric "${after_simple}" '.totals.classifier.completion')" \
    "$(profile_metric "${after_complex}" '.totals.classifier.completion')" 1
  assert_delta "complex powerful tier" \
    "$(profile_metric "${after_simple}" '.totals.actual_tiers.powerful')" \
    "$(profile_metric "${after_complex}" '.totals.actual_tiers.powerful')" 1
  printf 'PASS sol-complex-conflicting-low-routed-max\n' >> "${SUMMARY_FILE}"

  validate_capture_events
  printf 'PASS sol-classifier-has-no-terminal-effort\n' >> "${SUMMARY_FILE}"
}

main() {
  validate_inputs
  : > "${SUMMARY_FILE}"
  chmod 600 "${SUMMARY_FILE}"
  fetch_and_validate_bridge_model
  printf 'PASS sol-responses-low-max-capability\n' >> "${SUMMARY_FILE}"
  write_capture_shim
  start_capture_shim
  write_policy_config
  start_policy_proxy
  run_requests
  log "Live GPT-5.6 Sol semantic low/max effort smoke passed."
}

main "$@"
