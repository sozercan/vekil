#!/usr/bin/env bash

# Credentialed schema-v2 semantic-policy end-to-end smoke.
#
# The harness creates a private, temporary provider configuration containing
# only api_key_env references, starts a loopback control shim in front of the
# powerful primary, and exercises policy off/observe/enforce behavior. The shim
# forwards classifier and healthy terminal traffic to the real primary, but can
# inject one validated HTTP 429 for a powerful terminal request so Vekil must
# fail over to the real powerful secondary without crossing semantic tiers.
#
# Required environment:
#   LIVE_POLICY_ROUTING_LIGHTWEIGHT_TYPE
#   LIVE_POLICY_ROUTING_LIGHTWEIGHT_BASE_URL
#   LIVE_POLICY_ROUTING_LIGHTWEIGHT_MODEL
#   LIVE_POLICY_ROUTING_LIGHTWEIGHT_API_KEY
#   LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_TYPE
#   LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_BASE_URL
#   LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_MODEL
#   LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_API_KEY
#   LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_TYPE
#   LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_BASE_URL
#   LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_MODEL
#   LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_API_KEY
#   LIVE_POLICY_ROUTING_CLASSIFIER_MODEL
#   LIVE_POLICY_ROUTING_CLASSIFIER_NO_STORE_SUPPORTED=true|false
#
# When classifier no-store support is false, the synthetic smoke must explicitly
# acknowledge retention with LIVE_POLICY_ROUTING_ALLOW_PROVIDER_RETENTION=true.
#
# Provider types are azure-openai or openai-compatible. Azure bases must use
# their OpenAI-v1 form ending in /openai/v1. Generic compatible bases use bearer
# auth. All live listeners bind only to loopback and auto-select ports other
# than the product default 1337.
#
# Optional environment:
#   PROXY_BIN                                  default: ./vekil
#   LIVE_POLICY_ROUTING_PUBLIC_MODEL           default: vekil-live-semantic
#   LIVE_POLICY_ROUTING_SMOKE_DIR               explicit artifact directory
#   LIVE_POLICY_ROUTING_TMP_PARENT              artifact parent
#   LIVE_POLICY_ROUTING_ALLOW_INSECURE_HTTP=1   local development only
#   LIVE_POLICY_ROUTING_KEEP_ARTIFACTS=0        delete artifacts after success
#   LIVE_POLICY_ROUTING_ALLOW_PROVIDER_RETENTION=false
#   SMOKE_*                                     bounded timeout overrides

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
PUBLIC_MODEL="${LIVE_POLICY_ROUTING_PUBLIC_MODEL:-vekil-live-semantic}"
LIGHTWEIGHT_ROUTE_ID="live-semantic-lightweight"
POWERFUL_ROUTE_ID="live-semantic-powerful"
CLASSIFIER_ROUTE_ID="live-semantic-classifier"
PRIVACY_SENTINEL="LIVE_POLICY_PROMPT_SENTINEL_7d0e08d1"
TOOL_SCHEMA_SENTINEL="LIVE_POLICY_TOOL_SCHEMA_SENTINEL_46548e4e"
TOOL_RESULT_SENTINEL="LIVE_POLICY_TOOL_RESULT_SENTINEL_6698b62d"
INJECTED_REQUEST_ID="live-policy-upstream-request-id-must-not-leak"

SMOKE_STARTUP_TIMEOUT_SECONDS="${SMOKE_STARTUP_TIMEOUT_SECONDS:-90}"
SMOKE_VALIDATE_TIMEOUT_SECONDS="${SMOKE_VALIDATE_TIMEOUT_SECONDS:-180}"
SMOKE_CURL_CONNECT_TIMEOUT_SECONDS="${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS:-5}"
SMOKE_CURL_MAX_TIME_SECONDS="${SMOKE_CURL_MAX_TIME_SECONDS:-180}"
SMOKE_COMPLEX_CURL_MAX_TIME_SECONDS="${SMOKE_COMPLEX_CURL_MAX_TIME_SECONDS:-300}"
SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS="${SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS:-5}"
SMOKE_OBSERVE_SETTLE_TIMEOUT_SECONDS="${SMOKE_OBSERVE_SETTLE_TIMEOUT_SECONDS:-30}"
SMOKE_PROCESS_TERM_GRACE_SECONDS="${SMOKE_PROCESS_TERM_GRACE_SECONDS:-8}"
SMOKE_PORT_RELEASE_TIMEOUT_SECONDS="${SMOKE_PORT_RELEASE_TIMEOUT_SECONDS:-8}"
SMOKE_SHIM_UPSTREAM_TIMEOUT_SECONDS="${SMOKE_SHIM_UPSTREAM_TIMEOUT_SECONDS:-300}"
SMOKE_AUTO_PORT_MAX_ATTEMPTS="${SMOKE_AUTO_PORT_MAX_ATTEMPTS:-3}"
SMOKE_DIAGNOSTIC_MAX_BYTES="${SMOKE_DIAGNOSTIC_MAX_BYTES:-32768}"

TMP_PARENT="${LIVE_POLICY_ROUTING_TMP_PARENT:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}}"
SMOKE_DIR="${LIVE_POLICY_ROUTING_SMOKE_DIR:-$(mktemp -d "${TMP_PARENT%/}/live-policy-routing-smoke.XXXXXX")}"
if [[ "${SMOKE_DIR}" != /* ]]; then
  SMOKE_DIR="${PWD}/${SMOKE_DIR}"
fi
umask 077
mkdir -p "${SMOKE_DIR}"
chmod 700 "${SMOKE_DIR}"

CONFIG_JSON="${SMOKE_DIR}/providers.json"
SUMMARY_FILE="${SMOKE_DIR}/summary.txt"
PREFLIGHT_OFFLINE_LOG="${SMOKE_DIR}/preflight-offline.log"
PREFLIGHT_LIVE_LOG="${SMOKE_DIR}/preflight-live.log"
SHIM_SCRIPT="${SMOKE_DIR}/powerful-primary-shim.py"
SHIM_LOG="${SMOKE_DIR}/powerful-primary-shim.log"
SHIM_MODE_FILE="${SMOKE_DIR}/powerful-primary-shim.mode"

proxy_pid=""
proxy_pgid=""
proxy_port=""
proxy_base_url=""
proxy_listen_confirmed=0
proxy_mode=""
mode_dir=""
proxy_log=""
shim_pid=""
shim_pgid=""
shim_port=""
shim_listen_confirmed=0

python_command() {
  if command -v python3 >/dev/null 2>&1; then
    command -v python3
    return
  fi
  if command -v python >/dev/null 2>&1; then
    command -v python
    return
  fi
  die "python3 (or python) is required"
}

validate_positive_integer() {
  local name="$1"
  local value="$2"
  [[ "${value}" =~ ^[1-9][0-9]*$ ]] || die "${name} must be a positive integer: ${value}"
}

allocate_free_port() {
  "$(python_command)" - <<'PY_PORT'
import socket

for _ in range(50):
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    if port != 1337:
        print(port)
        raise SystemExit(0)
raise SystemExit("unable to allocate a non-default loopback port")
PY_PORT
}

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
  local port="$1"
  "$(python_command)" - "${port}" <<'PY_CONNECT' >/dev/null 2>&1
import socket
import sys

try:
    with socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=0.25):
        pass
except OSError:
    raise SystemExit(1)
PY_CONNECT
}

wait_for_port_release() {
  local port="$1"
  local deadline=$((SECONDS + SMOKE_PORT_RELEASE_TIMEOUT_SECONDS))
  while (( SECONDS < deadline )); do
    if ! port_is_open "${port}"; then
      return 0
    fi
    sleep 0.1
  done
  ! port_is_open "${port}"
}

redact_and_print_file() {
  local path="$1"
  local label="$2"
  [[ -f "${path}" ]] || return 0
  printf '\n--- %s ---\n' "${label}" >&2
  "$(python_command)" - "${path}" "${SMOKE_DIAGNOSTIC_MAX_BYTES}" <<'PY_REDACT' >&2
import os
import pathlib
import re
import sys

path = pathlib.Path(sys.argv[1])
limit = int(sys.argv[2])
try:
    text = path.read_text(encoding="utf-8", errors="replace")
except OSError as exc:
    print(f"<unable to read diagnostic: {type(exc).__name__}>")
    raise SystemExit(0)

secret_names = [
    "LIVE_POLICY_ROUTING_LIGHTWEIGHT_API_KEY",
    "LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_API_KEY",
    "LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_API_KEY",
]
values = [os.environ.get(name, "") for name in secret_names]
values += [
    os.environ.get("LIVE_POLICY_ROUTING_LIGHTWEIGHT_BASE_URL", ""),
    os.environ.get("LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_BASE_URL", ""),
    os.environ.get("LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_BASE_URL", ""),
    os.environ.get("LIVE_POLICY_ROUTING_LIGHTWEIGHT_MODEL", ""),
    os.environ.get("LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_MODEL", ""),
    os.environ.get("LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_MODEL", ""),
    os.environ.get("LIVE_POLICY_ROUTING_CLASSIFIER_MODEL", ""),
    "live-lightweight-provider",
    "live-powerful-primary-provider",
    "live-powerful-secondary-provider",
    "live-semantic-lightweight",
    "live-semantic-powerful",
    "live-semantic-classifier",
    "live-lightweight-primary",
    "live-powerful-primary",
    "live-powerful-secondary",
    "live-classifier-primary",
    "LIVE_POLICY_PROMPT_SENTINEL_7d0e08d1",
    "LIVE_POLICY_TOOL_SCHEMA_SENTINEL_46548e4e",
    "LIVE_POLICY_TOOL_RESULT_SENTINEL_6698b62d",
]
for value in sorted({value for value in values if value}, key=len, reverse=True):
    text = text.replace(value, "[REDACTED]")
text = re.sub(r"(?im)^(authorization|api-key|x-api-key|proxy-authorization)\s*:\s*.*$", r"\1: [REDACTED]", text)
text = re.sub(r'(?i)("arguments"\s*:\s*)"(?:[^"\\]|\\.)*"', r'\1"[REDACTED]"', text)
encoded = text.encode("utf-8", errors="replace")
if len(encoded) > limit:
    encoded = encoded[:limit]
    text = encoded.decode("utf-8", errors="ignore") + "\n<truncated>\n"
print(text, end="" if text.endswith("\n") else "\n")
PY_REDACT
}

emit_diagnostics() {
  local path rel
  log "Smoke failed; printing bounded redacted diagnostics. Generated config and request bodies are never printed."
  redact_and_print_file "${SUMMARY_FILE}" "summary.txt"
  redact_and_print_file "${PREFLIGHT_OFFLINE_LOG}" "preflight-offline.log"
  redact_and_print_file "${PREFLIGHT_LIVE_LOG}" "preflight-live.log"
  redact_and_print_file "${SHIM_LOG}" "powerful-primary-shim.log"
  while IFS= read -r path; do
    rel="${path#"${SMOKE_DIR}"/}"
    case "${rel}" in
      providers.json|*.request.json|powerful-primary-shim.py|powerful-primary-shim.mode) continue ;;
    esac
    redact_and_print_file "${path}" "${rel}"
  done < <(find "${SMOKE_DIR}" -type f \( \
    -name 'proxy.log' -o -name 'models.json' -o -name '*.stats.json' -o \
    -name '*.response.json' -o -name '*.headers.txt' -o -name '*.status' -o \
    -name '*.sse' \) | sort)
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM

  if [[ -n "${proxy_pgid}" ]]; then
    terminate_process_group "${proxy_pid}" "${proxy_pgid}"
    if [[ "${proxy_listen_confirmed}" == "1" && -n "${proxy_port}" ]] && ! wait_for_port_release "${proxy_port}"; then
      printf 'error: proxy cleanup did not release 127.0.0.1:%s\n' "${proxy_port}" >&2
      rc=1
    fi
  fi
  proxy_pid=""
  proxy_pgid=""
  proxy_listen_confirmed=0

  if [[ -n "${shim_pgid}" ]]; then
    terminate_process_group "${shim_pid}" "${shim_pgid}"
    if [[ "${shim_listen_confirmed}" == "1" && -n "${shim_port}" ]] && ! wait_for_port_release "${shim_port}"; then
      printf 'error: shim cleanup did not release 127.0.0.1:%s\n' "${shim_port}" >&2
      rc=1
    fi
  fi
  shim_pid=""
  shim_pgid=""
  shim_listen_confirmed=0

  if [[ "${rc}" -ne 0 ]]; then
    emit_diagnostics
  elif [[ "${LIVE_POLICY_ROUTING_KEEP_ARTIFACTS:-0}" == "0" ]]; then
    rm -rf "${SMOKE_DIR}"
  fi
  exit "${rc}"
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

validate_inputs() {
  local role type_var base_var model_var type base model
  local required=(
    LIVE_POLICY_ROUTING_LIGHTWEIGHT_TYPE
    LIVE_POLICY_ROUTING_LIGHTWEIGHT_BASE_URL
    LIVE_POLICY_ROUTING_LIGHTWEIGHT_MODEL
    LIVE_POLICY_ROUTING_LIGHTWEIGHT_API_KEY
    LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_TYPE
    LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_BASE_URL
    LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_MODEL
    LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_API_KEY
    LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_TYPE
    LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_BASE_URL
    LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_MODEL
    LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_API_KEY
    LIVE_POLICY_ROUTING_CLASSIFIER_MODEL
    LIVE_POLICY_ROUTING_CLASSIFIER_NO_STORE_SUPPORTED
  )

  for role in "${required[@]}"; do
    require_env "${role}"
  done
  local classifier_no_store_supported="${LIVE_POLICY_ROUTING_CLASSIFIER_NO_STORE_SUPPORTED}"
  local allow_provider_retention="${LIVE_POLICY_ROUTING_ALLOW_PROVIDER_RETENTION:-false}"
  case "${classifier_no_store_supported}" in
    true|false) ;;
    *) die "LIVE_POLICY_ROUTING_CLASSIFIER_NO_STORE_SUPPORTED must be true or false" ;;
  esac
  case "${allow_provider_retention}" in
    true|false) ;;
    *) die "LIVE_POLICY_ROUTING_ALLOW_PROVIDER_RETENTION must be true or false" ;;
  esac
  if [[ "${classifier_no_store_supported}" != "true" && "${allow_provider_retention}" != "true" ]]; then
    die "LIVE_POLICY_ROUTING_ALLOW_PROVIDER_RETENTION must be true when classifier no-store support is false"
  fi

  for role in LIGHTWEIGHT POWERFUL_PRIMARY POWERFUL_SECONDARY; do
    type_var="LIVE_POLICY_ROUTING_${role}_TYPE"
    base_var="LIVE_POLICY_ROUTING_${role}_BASE_URL"
    model_var="LIVE_POLICY_ROUTING_${role}_MODEL"
    type="${!type_var}"
    base="${!base_var}"
    model="${!model_var}"
    case "${type}" in
      azure-openai|openai-compatible) ;;
      *) die "${type_var} must be azure-openai or openai-compatible: ${type}" ;;
    esac
    [[ "${model}" != "${PUBLIC_MODEL}" ]] || die "${model_var} must differ from public model ${PUBLIC_MODEL}"

    "$(python_command)" - "${role}" "${type}" "${base}" "${LIVE_POLICY_ROUTING_ALLOW_INSECURE_HTTP:-0}" <<'PY_VALIDATE_URL'
import sys
import urllib.parse

role, provider_type, raw, allow_http = sys.argv[1:]
parsed = urllib.parse.urlsplit(raw.rstrip("/"))
allowed = {"https"} if allow_http != "1" else {"https", "http"}
if parsed.scheme not in allowed:
    raise SystemExit(f"LIVE_POLICY_ROUTING_{role}_BASE_URL must use HTTPS")
if not parsed.netloc or parsed.username or parsed.password or parsed.query or parsed.fragment:
    raise SystemExit(f"LIVE_POLICY_ROUTING_{role}_BASE_URL must be an absolute API base with no credentials, query, or fragment")
if provider_type == "azure-openai" and not parsed.path.rstrip("/").endswith("/openai/v1"):
    raise SystemExit(f"LIVE_POLICY_ROUTING_{role}_BASE_URL must end in /openai/v1 for azure-openai")
PY_VALIDATE_URL
  done

  "$(python_command)" - "${PUBLIC_MODEL}" <<'PY_PUBLIC_ID'
import sys
value = sys.argv[1]
if not value.strip() or len(value.encode()) > 128 or any(ord(ch) < 32 or ord(ch) == 127 for ch in value):
    raise SystemExit("LIVE_POLICY_ROUTING_PUBLIC_MODEL must be non-empty, control-free, and at most 128 bytes")
PY_PUBLIC_ID

  if [[ "${LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_TYPE}" == "${LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_TYPE}" && \
        "${LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_BASE_URL%/}" == "${LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_BASE_URL%/}" && \
        "${LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_MODEL}" == "${LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_MODEL}" ]]; then
    die "powerful primary and secondary must identify distinct targets"
  fi

  validate_positive_integer SMOKE_STARTUP_TIMEOUT_SECONDS "${SMOKE_STARTUP_TIMEOUT_SECONDS}"
  validate_positive_integer SMOKE_VALIDATE_TIMEOUT_SECONDS "${SMOKE_VALIDATE_TIMEOUT_SECONDS}"
  validate_positive_integer SMOKE_CURL_MAX_TIME_SECONDS "${SMOKE_CURL_MAX_TIME_SECONDS}"
  validate_positive_integer SMOKE_COMPLEX_CURL_MAX_TIME_SECONDS "${SMOKE_COMPLEX_CURL_MAX_TIME_SECONDS}"
  validate_positive_integer SMOKE_AUTO_PORT_MAX_ATTEMPTS "${SMOKE_AUTO_PORT_MAX_ATTEMPTS}"
  [[ -x "${PROXY_BIN}" ]] || die "proxy binary not found or not executable: ${PROXY_BIN} (run: make build)"
}

run_with_deadline() {
  local label="$1"
  local timeout_seconds="$2"
  local output_file="$3"
  shift 3

  if ! "$(python_command)" - "${timeout_seconds}" "${output_file}" "$@" <<'PY_DEADLINE'
import os
import signal
import subprocess
import sys
import time

timeout = float(sys.argv[1])
output_path = sys.argv[2]
command = sys.argv[3:]
with open(output_path, "wb") as output:
    process = subprocess.Popen(command, stdout=output, stderr=subprocess.STDOUT, start_new_session=True)
    try:
        rc = process.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        output.write(f"\ncommand timed out after {timeout:g}s\n".encode())
        output.flush()
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        deadline = time.monotonic() + 5
        while process.poll() is None and time.monotonic() < deadline:
            time.sleep(0.1)
        if process.poll() is None:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        process.wait()
        raise SystemExit(124)
raise SystemExit(rc)
PY_DEADLINE
  then
    die "${label} failed; inspect ${output_file}"
  fi
}

set_shim_mode() {
  local mode="$1"
  local tmp="${SHIM_MODE_FILE}.tmp"
  case "${mode}" in
    forward|reject_terminal) ;;
    *) die "invalid powerful-primary shim mode: ${mode}" ;;
  esac
  printf '{"mode":"%s"}\n' "${mode}" > "${tmp}"
  chmod 600 "${tmp}"
  mv "${tmp}" "${SHIM_MODE_FILE}"
}

write_powerful_primary_shim() {
  cat > "${SHIM_SCRIPT}" <<'PY_SHIM'
#!/usr/bin/env python3
import argparse
import hashlib
import hmac
import http.server
import json
import pathlib
import socketserver
import urllib.error
import urllib.parse
import urllib.request

HOP_BY_HOP = {
    "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
    "te", "trailer", "transfer-encoding", "upgrade",
}
MAX_REQUEST_BYTES = 2 * 1024 * 1024
MAX_RESPONSE_BYTES = 16 * 1024 * 1024


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class ThreadingHTTPServer(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, _format, *_args):
        return

    def _write(self, status, body, headers=None):
        self.send_response(status)
        for key, value in (headers or {}).items():
            self.send_header(key, value)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        if body:
            try:
                self.wfile.write(body)
            except (BrokenPipeError, ConnectionResetError):
                pass

    def _control_mode(self):
        try:
            value = json.loads(pathlib.Path(self.server.mode_file).read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            return "invalid"
        return value.get("mode") if isinstance(value, dict) else "invalid"

    def _read_body(self):
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            return b"", ["content_length_invalid"]
        if length < 0 or length > MAX_REQUEST_BYTES:
            return b"", ["content_length_out_of_bounds"]
        body = self.rfile.read(length) if length else b""
        return body, []

    def _auth_valid(self):
        value = self.headers.get(self.server.expected_auth_header, "")
        digest = hashlib.sha256(value.encode()).hexdigest()
        return hmac.compare_digest(digest, self.server.expected_auth_sha256), bool(value)

    @staticmethod
    def _has_classifier_tool(body):
        tools = body.get("tools")
        if not isinstance(tools, list):
            return False
        names = []
        for tool in tools:
            if not isinstance(tool, dict) or tool.get("type") != "function":
                continue
            function = tool.get("function")
            if isinstance(function, dict):
                names.append(function.get("name"))
        return names == ["emit_policy_signals"]

    @staticmethod
    def _classifier_choice_valid(body):
        choice = body.get("tool_choice")
        return (
            isinstance(choice, dict)
            and choice.get("type") == "function"
            and isinstance(choice.get("function"), dict)
            and choice["function"].get("name") == "emit_policy_signals"
        )

    def _inspect(self, incoming, request_body, read_errors):
        errors = list(read_errors)
        auth_valid, auth_present = self._auth_valid()
        content_type = self.headers.get("Content-Type", "").split(";", 1)[0].strip().lower()
        content_type_valid = content_type == "application/json"
        try:
            body = json.loads(request_body) if request_body else None
        except json.JSONDecodeError:
            body = None
        body_object = isinstance(body, dict)
        expected_path = self.server.mount_prefix + "/chat/completions"
        path_valid = self.command == "POST" and incoming.path == expected_path and not incoming.query
        messages_valid = body_object and isinstance(body.get("messages"), list) and bool(body["messages"])
        no_sampling = body_object and "temperature" not in body and "top_p" not in body
        classifier_shape = body_object and self._has_classifier_tool(body)
        kind = "classifier" if classifier_shape else "terminal"
        model_expected = self.server.classifier_model if kind == "classifier" else self.server.primary_model
        model_valid = body_object and body.get("model") == model_expected

        if not path_valid:
            errors.append("path_invalid")
        if not auth_valid:
            errors.append("auth_invalid")
        if not content_type_valid:
            errors.append("content_type_invalid")
        if not body_object:
            errors.append("body_not_json_object")
        else:
            if not model_valid:
                errors.append("model_invalid")
            if not messages_valid:
                errors.append("messages_invalid")
            if not no_sampling:
                errors.append("sampling_fields_not_dropped")
            if kind == "classifier":
                if self.server.classifier_no_store_supported:
                    if body.get("store") is not False:
                        errors.append("classifier_store_not_false")
                elif "store" in body:
                    errors.append("classifier_store_not_removed")
                if body.get("stream") not in (None, False):
                    errors.append("classifier_stream_invalid")
                if body.get("max_completion_tokens") != self.server.classifier_max_tokens:
                    errors.append("classifier_max_tokens_invalid")
                if not self._classifier_choice_valid(body):
                    errors.append("classifier_tool_choice_invalid")

        return {
            "body": body,
            "kind": kind,
            "valid": not errors,
            "errors": errors,
            "auth_present": auth_present,
            "auth_valid": auth_valid,
            "content_type_valid": content_type_valid,
            "model_valid": model_valid,
            "messages_valid": messages_valid,
            "no_sampling": no_sampling,
            "path": incoming.path,
            "stream": body.get("stream") if body_object else None,
        }

    def _log(self, inspection, mode, status, **extra):
        entry = {
            "event": "request",
            "method": self.command,
            "mode": mode,
            "path": inspection["path"],
            "request_kind": inspection["kind"],
            "request_valid": inspection["valid"],
            "validation_errors": inspection["errors"],
            "auth_present": inspection["auth_present"],
            "auth_valid": inspection["auth_valid"],
            "content_type_valid": inspection["content_type_valid"],
            "model_valid": inspection["model_valid"],
            "messages_valid": inspection["messages_valid"],
            "sampling_fields_absent": inspection["no_sampling"],
            "stream": inspection["stream"],
            "status": status,
        }
        entry.update(extra)
        print(json.dumps(entry, separators=(",", ":")), flush=True)

    def _forward(self, incoming, request_body, inspection, mode):
        suffix = incoming.path[len(self.server.mount_prefix):] if self.server.mount_prefix else incoming.path
        if not suffix.startswith("/"):
            suffix = "/" + suffix
        target = self.server.upstream_base + suffix
        if incoming.query:
            target += "?" + incoming.query

        headers = {}
        for key, value in self.headers.items():
            lower = key.lower()
            if lower in HOP_BY_HOP or lower in {"host", "content-length", "accept-encoding"}:
                continue
            headers[key] = value
        headers["Accept-Encoding"] = "identity"
        request = urllib.request.Request(target, data=request_body, headers=headers, method="POST")
        opener = urllib.request.build_opener(NoRedirect())
        try:
            response = opener.open(request, timeout=self.server.upstream_timeout)
        except urllib.error.HTTPError as error:
            response = error
        except Exception as error:  # bounded local control proxy
            payload = json.dumps(
                {"error": {"type": "server_error", "message": "powerful primary control proxy transport failure"}},
                separators=(",", ":"),
            ).encode()
            self._write(502, payload, {"Content-Type": "application/json"})
            self._log(inspection, mode, 502, transport_error=type(error).__name__)
            return

        with response:
            status = getattr(response, "status", None) or response.getcode()
            response_headers = {}
            for key, value in response.headers.items():
                lower = key.lower()
                if lower in HOP_BY_HOP or lower in {"content-length", "connection", "server", "date"}:
                    continue
                response_headers[key] = value

            if inspection["stream"] is True and 200 <= status < 300:
                self.send_response(status)
                for key, value in response_headers.items():
                    self.send_header(key, value)
                self.end_headers()
                total = 0
                while True:
                    chunk = response.read(4096)
                    if not chunk:
                        break
                    total += len(chunk)
                    if total > MAX_RESPONSE_BYTES:
                        self.close_connection = True
                        self._log(inspection, mode, status, response_oversized=True, streamed_bytes=total)
                        return
                    self.wfile.write(chunk)
                    self.wfile.flush()
                self.close_connection = True
                self._log(inspection, mode, status, streamed_bytes=total)
                return

            response_body = response.read(MAX_RESPONSE_BYTES + 1)
            if len(response_body) > MAX_RESPONSE_BYTES:
                payload = b'{"error":{"type":"server_error","message":"powerful primary response exceeded smoke bound"}}'
                self._write(502, payload, {"Content-Type": "application/json"})
                self._log(inspection, mode, 502, response_oversized=True)
                return
            self._write(status, response_body, response_headers)
            self._log(inspection, mode, status)

    def _handle(self):
        mode = self._control_mode()
        incoming = urllib.parse.urlsplit(self.path)
        request_body, read_errors = self._read_body()
        inspection = self._inspect(incoming, request_body, read_errors)

        if mode not in {"forward", "reject_terminal"}:
            payload = b'{"error":{"type":"control_proxy_error","message":"invalid control mode"}}'
            self._write(500, payload, {"Content-Type": "application/json"})
            self._log(inspection, "invalid", 500)
            return
        if not inspection["valid"]:
            payload = json.dumps(
                {"error": {"type": "control_proxy_validation_error", "message": "powerful primary request validation failed"}},
                separators=(",", ":"),
            ).encode()
            self._write(400, payload, {"Content-Type": "application/json"})
            self._log(inspection, mode, 400)
            return
        if mode == "reject_terminal" and inspection["kind"] == "terminal":
            payload = json.dumps(
                {"error": {"type": "rate_limit_error", "code": "rate_limit_exceeded", "message": "injected retry-safe powerful-primary rejection"}},
                separators=(",", ":"),
            ).encode()
            self._write(429, payload, {
                "Content-Type": "application/json",
                "Retry-After": "1",
                "X-Request-ID": self.server.injected_request_id,
            })
            self._log(inspection, mode, 429, injected=True)
            return
        self._forward(incoming, request_body, inspection, mode)

    do_POST = _handle


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--mode-file", required=True)
    parser.add_argument("--mount-prefix", default="")
    parser.add_argument("--upstream-base", required=True)
    parser.add_argument("--primary-model", required=True)
    parser.add_argument("--classifier-model", required=True)
    parser.add_argument("--classifier-max-tokens", type=int, required=True)
    parser.add_argument("--classifier-no-store-supported", choices=("true", "false"), required=True)
    parser.add_argument("--expected-auth-header", choices=("api-key", "authorization"), required=True)
    parser.add_argument("--expected-auth-sha256", required=True)
    parser.add_argument("--injected-request-id", required=True)
    parser.add_argument("--upstream-timeout", type=float, default=300.0)
    args = parser.parse_args()

    server = ThreadingHTTPServer((args.host, args.port), Handler)
    server.mode_file = args.mode_file
    server.mount_prefix = args.mount_prefix.rstrip("/")
    server.upstream_base = args.upstream_base.rstrip("/")
    server.primary_model = args.primary_model
    server.classifier_model = args.classifier_model
    server.classifier_max_tokens = args.classifier_max_tokens
    server.classifier_no_store_supported = args.classifier_no_store_supported == "true"
    server.expected_auth_header = args.expected_auth_header
    server.expected_auth_sha256 = args.expected_auth_sha256
    server.injected_request_id = args.injected_request_id
    server.upstream_timeout = args.upstream_timeout
    print(json.dumps({"event": "listening", "host": args.host, "port": server.server_port}, separators=(",", ":")), flush=True)
    server.serve_forever(poll_interval=0.1)


if __name__ == "__main__":
    main()
PY_SHIM
  chmod 700 "${SHIM_SCRIPT}"
}

shim_log_has_listener() {
  [[ -f "${SHIM_LOG}" ]] || return 1
  jq -R -s -e --argjson port "${shim_port}" '
    [split("\n")[] | fromjson? | select(.event == "listening" and .host == "127.0.0.1" and .port == $port)]
    | length == 1
  ' "${SHIM_LOG}" >/dev/null 2>&1
}

start_powerful_primary_shim() {
  local expected_auth_header expected_auth_value expected_auth_sha256 mount_prefix
  local attempt

  if [[ "${LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_TYPE}" == "azure-openai" ]]; then
    expected_auth_header="api-key"
    expected_auth_value="${LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_API_KEY}"
    mount_prefix="/openai/v1"
  else
    expected_auth_header="authorization"
    expected_auth_value="Bearer ${LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_API_KEY}"
    mount_prefix=""
  fi
  expected_auth_sha256="$(printf '%s' "${expected_auth_value}" | "$(python_command)" -c 'import hashlib,sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())')"
  unset expected_auth_value

  write_powerful_primary_shim
  set_shim_mode forward
  : > "${SHIM_LOG}"
  chmod 600 "${SHIM_LOG}"

  for ((attempt = 1; attempt <= SMOKE_AUTO_PORT_MAX_ATTEMPTS; attempt++)); do
    shim_port="$(allocate_free_port)"
    log "Starting powerful-primary control shim at 127.0.0.1:${shim_port} (attempt ${attempt}/${SMOKE_AUTO_PORT_MAX_ATTEMPTS})"
    set -m
    env \
      -u LIVE_POLICY_ROUTING_LIGHTWEIGHT_API_KEY \
      -u LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_API_KEY \
      -u LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_API_KEY \
      "$(python_command)" "${SHIM_SCRIPT}" \
        --host 127.0.0.1 \
        --port "${shim_port}" \
        --mode-file "${SHIM_MODE_FILE}" \
        --mount-prefix "${mount_prefix}" \
        --upstream-base "${LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_BASE_URL}" \
        --primary-model "${LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_MODEL}" \
        --classifier-model "${LIVE_POLICY_ROUTING_CLASSIFIER_MODEL}" \
        --classifier-max-tokens 256 \
        --classifier-no-store-supported "${LIVE_POLICY_ROUTING_CLASSIFIER_NO_STORE_SUPPORTED}" \
        --expected-auth-header "${expected_auth_header}" \
        --expected-auth-sha256 "${expected_auth_sha256}" \
        --injected-request-id "${INJECTED_REQUEST_ID}" \
        --upstream-timeout "${SMOKE_SHIM_UPSTREAM_TIMEOUT_SECONDS}" \
        >>"${SHIM_LOG}" 2>&1 &
    shim_pid="$!"
    shim_pgid="${shim_pid}"
    set +m

    local deadline=$((SECONDS + SMOKE_STARTUP_TIMEOUT_SECONDS))
    while (( SECONDS < deadline )); do
      if ! process_is_running "${shim_pid}"; then
        break
      fi
      if port_is_open "${shim_port}" && shim_log_has_listener; then
        shim_listen_confirmed=1
        return
      fi
      sleep 0.1
    done
    terminate_process_group "${shim_pid}" "${shim_pgid}"
    shim_pid=""
    shim_pgid=""
    if (( attempt < SMOKE_AUTO_PORT_MAX_ATTEMPTS )); then
      log "Shim port ${shim_port} was unavailable; retrying"
      continue
    fi
  done
  die "powerful-primary control shim did not become ready"
}

generate_config() {
  local primary_shim_base
  if [[ "${LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_TYPE}" == "azure-openai" ]]; then
    primary_shim_base="http://127.0.0.1:${shim_port}/openai/v1"
  else
    primary_shim_base="http://127.0.0.1:${shim_port}"
  fi

  PRIMARY_SHIM_BASE_URL="${primary_shim_base}" \
  EFFECTIVE_PUBLIC_MODEL="${PUBLIC_MODEL}" \
  "$(python_command)" - "${CONFIG_JSON}" <<'PY_CONFIG'
import json
import os
import pathlib
import sys


def provider(provider_id, provider_type, base_url, key_env, classifier=False):
    value = {
        "id": provider_id,
        "type": provider_type,
        "base_url": base_url.rstrip("/"),
        "api_key_env": key_env,
        "trust_domain": "live-policy-routing",
    }
    if provider_type == "openai-compatible":
        value["auth_type"] = "bearer"
        value["model_discovery"] = "static"
    if classifier:
        value["classifier_no_store_supported"] = os.environ["LIVE_POLICY_ROUTING_CLASSIFIER_NO_STORE_SUPPORTED"] == "true"
    return value


def route(route_id, name, targets, *, purpose=None, failover=False):
    value = {
        "id": route_id,
        "exposure": "internal",
        "name": name,
        "endpoints": ["/chat/completions"],
        "parallel_tool_calls": True,
        "vision": False,
        "drop_sampling_params": True,
        "targets": targets,
        "routing": {
            "mode": "priority_failover" if failover else "primary_only",
            "max_target_attempts": len(targets) if failover else 1,
            "max_upstream_sends": len(targets) if failover else 1,
        },
    }
    if purpose:
        value["internal_purpose"] = purpose
        value.pop("parallel_tool_calls", None)
        value.pop("vision", None)
    return value


def target(target_id, provider_id, model):
    return {
        "id": target_id,
        "provider": provider_id,
        "upstream_model": model,
        "use_max_completion_tokens": True,
    }


config = {
    "schema_version": 2,
    "providers": [
        provider(
            "live-lightweight-provider",
            os.environ["LIVE_POLICY_ROUTING_LIGHTWEIGHT_TYPE"],
            os.environ["LIVE_POLICY_ROUTING_LIGHTWEIGHT_BASE_URL"],
            "LIVE_POLICY_ROUTING_LIGHTWEIGHT_API_KEY",
        ),
        provider(
            "live-powerful-primary-provider",
            os.environ["LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_TYPE"],
            os.environ["PRIMARY_SHIM_BASE_URL"],
            "LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_API_KEY",
            classifier=True,
        ),
        provider(
            "live-powerful-secondary-provider",
            os.environ["LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_TYPE"],
            os.environ["LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_BASE_URL"],
            "LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_API_KEY",
        ),
    ],
    "model_routes": [
        route(
            "live-semantic-lightweight",
            "Live semantic lightweight",
            [target("live-lightweight-primary", "live-lightweight-provider", os.environ["LIVE_POLICY_ROUTING_LIGHTWEIGHT_MODEL"])],
        ),
        route(
            "live-semantic-powerful",
            "Live semantic powerful",
            [
                target("live-powerful-primary", "live-powerful-primary-provider", os.environ["LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_MODEL"]),
                target("live-powerful-secondary", "live-powerful-secondary-provider", os.environ["LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_MODEL"]),
            ],
            failover=True,
        ),
        route(
            "live-semantic-classifier",
            "Live semantic classifier",
            [target("live-classifier-primary", "live-powerful-primary-provider", os.environ["LIVE_POLICY_ROUTING_CLASSIFIER_MODEL"])],
            purpose="policy_classifier",
        ),
    ],
    "policy_profiles": [
        {
            "id": "live-semantic-policy",
            "public_id": os.environ["EFFECTIVE_PUBLIC_MODEL"],
            "name": "Vekil live semantic policy",
            "mode": "enforce",
            "model_picker_enabled": True,
            "model_picker_category": "versatile",
            "lightweight_route": "live-semantic-lightweight",
            "powerful_route": "live-semantic-powerful",
            "baseline_tier": "lightweight",
            "classifier_unavailable_tier": "lightweight",
            "classifier_uncertain_tier": "powerful",
            "classifier": {
                "route": "live-semantic-classifier",
                "profile": "coding_agent_v1",
                "timeout_ms": 10000,
                "max_completion_tokens": 256,
                "max_request_bytes": 16000,
                "recent_turns": 4,
                "max_concurrency": 4,
                "observe_sample_rate": 1.0,
            },
            "data_policy": {
                "content_forwarding_acknowledged": True,
                "allow_cross_trust_domain": False,
                "allow_provider_retention": os.environ.get("LIVE_POLICY_ROUTING_ALLOW_PROVIDER_RETENTION", "false") == "true",
            },
        }
    ],
}
path = pathlib.Path(sys.argv[1])
path.write_text(json.dumps(config, indent=2) + "\n", encoding="utf-8")
os.chmod(path, 0o600)
PY_CONFIG

  "$(python_command)" - "${CONFIG_JSON}" <<'PY_CONFIG_CHECK'
import os
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
text = path.read_text(encoding="utf-8")
config = __import__("json").loads(text)
expected_no_store = os.environ["LIVE_POLICY_ROUTING_CLASSIFIER_NO_STORE_SUPPORTED"] == "true"
expected_retention = os.environ.get("LIVE_POLICY_ROUTING_ALLOW_PROVIDER_RETENTION", "false") == "true"
classifier_provider = next(provider for provider in config["providers"] if provider["id"] == "live-powerful-primary-provider")
profile = config["policy_profiles"][0]
if classifier_provider.get("classifier_no_store_supported") is not expected_no_store:
    raise SystemExit("generated config has the wrong classifier_no_store_supported value")
if profile["data_policy"].get("allow_provider_retention") is not expected_retention:
    raise SystemExit("generated config has the wrong allow_provider_retention value")
for name in (
    "LIVE_POLICY_ROUTING_LIGHTWEIGHT_API_KEY",
    "LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_API_KEY",
    "LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_API_KEY",
):
    secret = os.environ[name]
    if secret and secret in text:
        raise SystemExit(f"generated config unexpectedly contains secret value from {name}")
    if name not in text:
        raise SystemExit(f"generated config does not reference {name} through api_key_env")
PY_CONFIG_CHECK
  printf 'PASS private-schema-v2-config-generated\n' >> "${SUMMARY_FILE}"
}

assert_shim_rejection_guard() {
  local path="/chat/completions"
  local response="${SMOKE_DIR}/shim-validation-guard.response.json"
  local status
  if [[ "${LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_TYPE}" == "azure-openai" ]]; then
    path="/openai/v1/chat/completions"
  fi
  set_shim_mode reject_terminal
  status="$(curl --silent --show-error \
    --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS}" \
    --header 'Content-Type: application/json' \
    --data-binary '{}' \
    --output "${response}" \
    --write-out '%{http_code}' \
    "http://127.0.0.1:${shim_port}${path}")" || die "shim validation-guard probe failed"
  [[ "${status}" == "400" ]] || die "invalid shim request status=${status}, want 400 before rejection injection"
  jq -e '.error.type == "control_proxy_validation_error"' "${response}" >/dev/null || \
    die "shim validation guard did not reject malformed traffic"
  set_shim_mode forward
  printf 'PASS powerful-primary-shim-validation-guard\n' >> "${SUMMARY_FILE}"
}

validate_generated_config() {
  log "Validating generated schema-v2 policy config offline"
  run_with_deadline "offline config validation" "${SMOKE_VALIDATE_TIMEOUT_SECONDS}" "${PREFLIGHT_OFFLINE_LOG}" \
    "${PROXY_BIN}" config validate --providers-config "${CONFIG_JSON}"
  printf 'PASS offline-config-validation\n' >> "${SUMMARY_FILE}"

  log "Running live classifier protocol preflight"
  run_with_deadline "live classifier preflight" "${SMOKE_VALIDATE_TIMEOUT_SECONDS}" "${PREFLIGHT_LIVE_LOG}" \
    "${PROXY_BIN}" config validate --live --providers-config "${CONFIG_JSON}"
  printf 'PASS live-classifier-preflight\n' >> "${SUMMARY_FILE}"
}

proxy_log_has_expected_listener() {
  [[ -f "${proxy_log}" ]] || return 1
  jq -R -s -e --arg addr "127.0.0.1:${proxy_port}" '
    [split("\n")[] | fromjson? | select(.level == "info" and .msg == "vekil listening" and .addr == $addr)]
    | length > 0
  ' "${proxy_log}" >/dev/null 2>&1
}

proxy_log_has_address_in_use() {
  [[ -f "${proxy_log}" ]] || return 1
  jq -R -s -e '
    [split("\n")[] | fromjson? | select(.level == "fatal" and (((.error // "") | ascii_downcase) | contains("address already in use")))]
    | length > 0
  ' "${proxy_log}" >/dev/null 2>&1
}

proxy_log_has_fatal() {
  [[ -f "${proxy_log}" ]] || return 1
  jq -R -s -e '[split("\n")[] | fromjson? | select(.level == "fatal")] | length > 0' "${proxy_log}" >/dev/null 2>&1
}

launch_proxy() {
  local token_dir="${mode_dir}/token"
  mkdir -p "${token_dir}"
  chmod 700 "${token_dir}"
  : > "${proxy_log}"
  chmod 600 "${proxy_log}"
  proxy_listen_confirmed=0
  set -m
  "${PROXY_BIN}" \
    --host 127.0.0.1 \
    --port "${proxy_port}" \
    --log-level info \
    --token-dir "${token_dir}" \
    --providers-config "${CONFIG_JSON}" \
    --policy-routing "${proxy_mode}" \
    >"${proxy_log}" 2>&1 &
  proxy_pid="$!"
  proxy_pgid="${proxy_pid}"
  set +m
}

stop_proxy() {
  if [[ -n "${proxy_pgid}" ]]; then
    terminate_process_group "${proxy_pid}" "${proxy_pgid}"
  fi
  if [[ "${proxy_listen_confirmed}" == "1" && -n "${proxy_port}" ]] && ! wait_for_port_release "${proxy_port}"; then
    die "proxy ${proxy_mode} did not release port ${proxy_port}"
  fi
  proxy_pid=""
  proxy_pgid=""
  proxy_listen_confirmed=0
  proxy_port=""
  proxy_base_url=""
}

wait_for_proxy_ready() {
  local deadline=$((SECONDS + SMOKE_STARTUP_TIMEOUT_SECONDS))
  local status
  while (( SECONDS < deadline )); do
    if proxy_log_has_address_in_use; then
      return 2
    fi
    if ! process_is_running "${proxy_pid}"; then
      # A short-lived process can exit before stdio flushes the final fatal JSON.
      # Give the log a bounded moment to become visible so address races can use
      # the normal automatic-port retry path instead of looking like a generic
      # startup failure.
      local flush_attempt
      for ((flush_attempt = 0; flush_attempt < 20; flush_attempt++)); do
        proxy_log_has_address_in_use && return 2
        proxy_log_has_fatal && return 3
        sleep 0.05
      done
      return 3
    fi
    if proxy_log_has_fatal; then
      proxy_log_has_address_in_use && return 2
      return 3
    fi
    status="$(curl --silent --output /dev/null --write-out '%{http_code}' \
      --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
      --max-time "${SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS}" \
      "${proxy_base_url}/readyz" 2>/dev/null || true)"
    if [[ "${status}" == "200" ]] && proxy_log_has_expected_listener; then
      proxy_listen_confirmed=1
      return 0
    fi
    sleep 0.25
  done
  return 4
}

start_proxy() {
  local mode="$1"
  local attempt wait_rc
  proxy_mode="${mode}"
  mode_dir="${SMOKE_DIR}/${mode}"
  mkdir -p "${mode_dir}"
  chmod 700 "${mode_dir}"
  proxy_log="${mode_dir}/proxy.log"

  for ((attempt = 1; attempt <= SMOKE_AUTO_PORT_MAX_ATTEMPTS; attempt++)); do
    proxy_port="$(allocate_free_port)"
    proxy_base_url="http://127.0.0.1:${proxy_port}"
    log "Starting Vekil ${mode} mode at ${proxy_base_url} (attempt ${attempt}/${SMOKE_AUTO_PORT_MAX_ATTEMPTS})"
    launch_proxy
    if wait_for_proxy_ready; then
      printf 'PASS %s-mode-ready port=%s\n' "${mode}" "${proxy_port}" >> "${SUMMARY_FILE}"
      return
    else
      wait_rc=$?
    fi
    terminate_process_group "${proxy_pid}" "${proxy_pgid}"
    proxy_pid=""
    proxy_pgid=""
    if [[ "${wait_rc}" -eq 2 && "${attempt}" -lt "${SMOKE_AUTO_PORT_MAX_ATTEMPTS}" ]]; then
      log "Auto-selected proxy port ${proxy_port} was claimed before startup; retrying"
      continue
    fi
    if [[ "${wait_rc}" -eq 4 ]]; then
      die "proxy ${mode} did not become ready within ${SMOKE_STARTUP_TIMEOUT_SECONDS}s"
    fi
    die "proxy ${mode} exited or logged a fatal error before readiness"
  done
  die "proxy ${mode} exhausted auto-port attempts"
}

fetch_json() {
  local label="$1"
  local url="$2"
  local output="$3"
  curl --fail --silent --show-error \
    --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${SMOKE_CURL_MAX_TIME_SECONDS}" \
    --output "${output}" "${url}" || die "${label} request failed"
  chmod 600 "${output}"
}

fetch_stats() {
  local name="$1"
  local output="${mode_dir}/${name}.stats.json"
  fetch_json "${proxy_mode} ${name} stats" "${proxy_base_url}/stats.json" "${output}"
  printf '%s\n' "${output}"
}

fetch_catalog() {
  local output="${mode_dir}/models.json"
  fetch_json "${proxy_mode} model catalog" "${proxy_base_url}/v1/models" "${output}"
  jq -e --arg model "${PUBLIC_MODEL}" \
    --arg light_route "${LIGHTWEIGHT_ROUTE_ID}" \
    --arg power_route "${POWERFUL_ROUTE_ID}" \
    --arg classifier_route "${CLASSIFIER_ROUTE_ID}" \
    --arg light_model "${LIVE_POLICY_ROUTING_LIGHTWEIGHT_MODEL}" \
    --arg primary_model "${LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_MODEL}" \
    --arg secondary_model "${LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_MODEL}" '
      (.data | type == "array")
      and ([.data[] | select(.id == $model)] | length) == 1
      and ([.data[] | select(.id == $model)][0].owned_by == "vekil-policy")
      and ([.data[] | select(.id == $model)][0].supported_endpoints == ["/chat/completions"])
      and ([.data[] | select(.id == $model)][0].capabilities.supports.vision == false)
      and ([.data[] | select(.id == $model)][0].capabilities.supports.parallel_tool_calls == true)
      and (([.data[] | select(.id == $model)][0].capabilities.supports.reasoning_effort // []) | length == 0)
      and ([.data[].id] | index($light_route) == null)
      and ([.data[].id] | index($power_route) == null)
      and ([.data[].id] | index($classifier_route) == null)
      and ([.data[].id] | index($light_model) == null)
      and ([.data[].id] | index($primary_model) == null)
      and ([.data[].id] | index($secondary_model) == null)
    ' "${output}" >/dev/null || die "${proxy_mode} model catalog did not expose the canonical policy contract"
}

post_json_path() {
  local label="$1"
  local endpoint="$2"
  local request_file="$3"
  local response_file="$4"
  local headers_file="$5"
  local status_file="$6"
  local max_time="${7:-${SMOKE_CURL_MAX_TIME_SECONDS}}"
  local status

  status="$(curl --silent --show-error \
    --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${max_time}" \
    --header 'Content-Type: application/json' \
    --data-binary "@${request_file}" \
    --dump-header "${headers_file}" \
    --output "${response_file}" \
    --write-out '%{http_code}' \
    "${proxy_base_url}${endpoint}")" || die "${label} failed at the HTTP transport layer"
  printf '%s\n' "${status}" > "${status_file}"
  chmod 600 "${response_file}" "${headers_file}" "${status_file}"
  printf '%s\n' "${status}"
}

post_chat() {
  post_json_path "$1" "/v1/chat/completions" "$2" "$3" "$4" "$5" "${6:-${SMOKE_CURL_MAX_TIME_SECONDS}}"
}

assert_public_headers() {
  local label="$1"
  local headers="$2"
  "$(python_command)" - "${label}" "${headers}" <<'PY_HEADERS'
import pathlib
import sys

label, path = sys.argv[1:]
lines = pathlib.Path(path).read_text(encoding="utf-8", errors="replace").splitlines()
headers = {}
for line in lines:
    if ":" not in line:
        continue
    name, value = line.split(":", 1)
    headers.setdefault(name.strip().lower(), []).append(value.strip())
vekil = [value for value in headers.get("x-vekil-request-id", []) if value]
if len(vekil) != 1:
    raise SystemExit(f"{label}: X-Vekil-Request-ID count={len(vekil)}, want exactly one non-empty value")
for forbidden in ("x-request-id", "request-id", "x-ms-request-id"):
    if headers.get(forbidden):
        raise SystemExit(f"{label}: leaked upstream request-id header {forbidden}")
PY_HEADERS
  assert_file_has_no_internal_identity "${label} headers" "${headers}"
}

assert_no_upstream_request_headers() {
  local label="$1"
  local headers="$2"
  "$(python_command)" - "${label}" "${headers}" <<'PY_REJECT_HEADERS'
import pathlib
import sys

label, path = sys.argv[1:]
headers = {}
for line in pathlib.Path(path).read_text(encoding="utf-8", errors="replace").splitlines():
    if ":" not in line:
        continue
    name, value = line.split(":", 1)
    headers.setdefault(name.strip().lower(), []).append(value.strip())
for forbidden in ("x-request-id", "request-id", "x-ms-request-id"):
    if headers.get(forbidden):
        raise SystemExit(f"{label}: leaked upstream request-id header {forbidden}")
PY_REJECT_HEADERS
  assert_file_has_no_internal_identity "${label} headers" "${headers}"
}

assert_file_has_no_internal_identity() {
  local label="$1"
  local path="$2"
  "$(python_command)" - "${label}" "${path}" <<'PY_NO_INTERNAL'
import os
import pathlib
import sys
import urllib.parse

label, path = sys.argv[1:]
data = pathlib.Path(path).read_text(encoding="utf-8", errors="replace")
values = [
    os.environ.get("LIVE_POLICY_ROUTING_LIGHTWEIGHT_MODEL", ""),
    os.environ.get("LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_MODEL", ""),
    os.environ.get("LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_MODEL", ""),
    os.environ.get("LIVE_POLICY_ROUTING_CLASSIFIER_MODEL", ""),
    "live-lightweight-provider",
    "live-powerful-primary-provider",
    "live-powerful-secondary-provider",
    "live-semantic-lightweight",
    "live-semantic-powerful",
    "live-semantic-classifier",
    "live-lightweight-primary",
    "live-powerful-primary",
    "live-powerful-secondary",
    "live-classifier-primary",
    "live-policy-upstream-request-id-must-not-leak",
]
for name in (
    "LIVE_POLICY_ROUTING_LIGHTWEIGHT_BASE_URL",
    "LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_BASE_URL",
    "LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_BASE_URL",
):
    raw = os.environ.get(name, "")
    if raw:
        parsed = urllib.parse.urlsplit(raw)
        values.extend([raw.rstrip("/"), parsed.netloc])
for value in {item for item in values if item}:
    if value in data:
        raise SystemExit(f"{label}: internal identity leaked")
PY_NO_INTERNAL
}

assert_success_text_response() {
  local label="$1"
  local response="$2"
  jq -e --arg model "${PUBLIC_MODEL}" '
    .model == $model
    and (.choices | type == "array" and length > 0)
    and (.choices[0].message.content | type == "string" and length > 0)
  ' "${response}" >/dev/null || die "${label} did not return canonical model identity and visible text"
  assert_file_has_no_internal_identity "${label} response" "${response}"
}

assert_delta() {
  local label="$1"
  local before="$2"
  local after="$3"
  local expected="$4"
  local actual=$((after - before))
  [[ "${actual}" -eq "${expected}" ]] || \
    die "${label} delta=${actual}, want ${expected} (before=${before}, after=${after})"
}

assert_classifier_completion() {
  local label="$1"
  local before="$2"
  local after="$3"
  assert_delta "${label} classifier completion" "$(profile_metric "${before}" '["totals","classifier","completion"]')" "$(profile_metric "${after}" '["totals","classifier","completion"]')" 1
  assert_delta "${label} classifier unavailable" "$(profile_metric "${before}" '["totals","classifier","unavailable"]')" "$(profile_metric "${after}" '["totals","classifier","unavailable"]')" 0
  assert_delta "${label} classifier uncertain" "$(profile_metric "${before}" '["totals","classifier","uncertain"]')" "$(profile_metric "${after}" '["totals","classifier","uncertain"]')" 0
  assert_delta "${label} classifier abstain" "$(profile_metric "${before}" '["totals","classifier","abstain"]')" "$(profile_metric "${after}" '["totals","classifier","abstain"]')" 0
}

powerful_test_prompt() {
  local prefix="$1"
  "$(python_command)" - "${prefix}" <<'PY_POWERFUL_PROMPT'
import sys
print(sys.argv[1] + "\nBounded synthetic context: " + ("x" * 5000))
PY_POWERFUL_PROMPT
}

stats_counter() {
  local path="$1"
  local field="$2"
  jq -r --arg field "${field}" '.[$field] // 0' "${path}"
}

profile_metric() {
  local path="$1"
  local metric_path="$2"
  jq -r --arg profile "${PUBLIC_MODEL}" --argjson metric_path "${metric_path}" '
    (first(.policy_routing.profiles[]? | select(.profile == $profile)) // {})
    | (getpath($metric_path) // 0)
  ' "${path}"
}

assert_profile_state() {
  local path="$1"
  local mode="$2"
  local preflight="$3"
  jq -e --arg profile "${PUBLIC_MODEL}" --arg mode "${mode}" --arg preflight "${preflight}" '
    first(.policy_routing.profiles[]? | select(.profile == $profile)) as $p
    | $p != null
      and $p.effective_mode == $mode
      and $p.preflight_state == $preflight
      and ($p.breaker_state == "closed" or $mode == "off")
      and ($p.generation_hashes.config_generation | test("^[0-9a-f]{64}$"))
      and ($p.generation_hashes.profile_generation | test("^[0-9a-f]{64}$"))
      and ($p.generation_hashes.classifier_generation | test("^[0-9a-f]{64}$"))
      and ($p.generation_hashes.binary_generation | test("^[0-9a-f]{64}$"))
  ' "${path}" >/dev/null || die "${mode} policy profile state was not ready and generation-attributed"
}

assert_public_only_stats() {
  local path="$1"
  jq -e --arg model "${PUBLIC_MODEL}" '
    ([.by_provider[]?] | length) == 0
    and ([.by_model[]? | select(.model != $model)] | length) == 0
    and ([.by_route[]? | select(.route != $model)] | length) == 0
    and ([.by_target[]?
          | select(
              .route != $model
              or .target != $model
              or ((.provider // "") != "")
              or ((.kind // "") != "")
            )] | length) == 0
    and ([.recent[]?
          | select(
              .model != $model
              or .route_id != $model
              or .final_target != $model
              or ((.provider // "") != "")
              or ((.provider_kind // "") != "")
              or ((.upstream_request_id // "") != "")
            )] | length) == 0
    and ([.recent_attempts[]?
          | select(
              .route_id != $model
              or .target_id != $model
              or ((.provider_id // "") != "")
              or ((.provider_kind // "") != "")
              or ((.upstream_request_id // "") != "")
            )] | length) == 0
    and ([.errors[]? | select(.label != $model)] | length) == 0
  ' "${path}" >/dev/null || die "client statistics exposed non-policy identity"
  assert_file_has_no_internal_identity "client statistics" "${path}"
}

write_text_request() {
  local path="$1"
  local prompt="$2"
  local max_tokens="$3"
  local stream="${4:-false}"
  jq -n --arg model "${PUBLIC_MODEL}" --arg prompt "${prompt}" \
    --argjson max_tokens "${max_tokens}" --argjson stream "${stream}" '
      {
        model:$model,
        messages:[{role:"user",content:$prompt}],
        max_completion_tokens:$max_tokens,
        stream:$stream,
        temperature:0.2,
        top_p:0.9
      }
    ' > "${path}"
  chmod 600 "${path}"
}

run_off_mode() {
  local initial final request response headers status_file status
  start_proxy off
  fetch_catalog
  initial="$(fetch_stats initial)"
  assert_profile_state "${initial}" off not_required

  request="${mode_dir}/baseline.request.json"
  response="${mode_dir}/baseline.response.json"
  headers="${mode_dir}/baseline.headers.txt"
  status_file="${mode_dir}/baseline.status"
  write_text_request "${request}" "Reply with exactly OFF_BASELINE_OK. This is a bounded read-only lookup." 1024 false
  status="$(post_chat off-baseline "${request}" "${response}" "${headers}" "${status_file}")"
  [[ "${status}" == "200" ]] || die "off baseline status=${status}, want 200"
  assert_success_text_response off-baseline "${response}"
  assert_public_headers off-baseline "${headers}"

  final="$(fetch_stats final)"
  assert_delta "off eligible" "$(profile_metric "${initial}" '["totals","eligible"]')" "$(profile_metric "${final}" '["totals","eligible"]')" 1
  assert_delta "off classifier sends" "$(profile_metric "${initial}" '["totals","physical_classifier_sends"]')" "$(profile_metric "${final}" '["totals","physical_classifier_sends"]')" 0
  assert_delta "off actual lightweight" "$(profile_metric "${initial}" '["totals","actual_tiers","lightweight"]')" "$(profile_metric "${final}" '["totals","actual_tiers","lightweight"]')" 1
  assert_delta "off terminal sends" "$(stats_counter "${initial}" upstream_attempts)" "$(stats_counter "${final}" upstream_attempts)" 1
  assert_public_only_stats "${final}"
  printf 'PASS off-baseline-lightweight-no-classifier\n' >> "${SUMMARY_FILE}"
  stop_proxy
}

run_observe_mode() {
  local initial final poll request response headers status_file status deadline prompt
  start_proxy observe
  fetch_catalog
  initial="$(fetch_stats initial)"
  assert_profile_state "${initial}" observe ready

  request="${mode_dir}/complex-shadow.request.json"
  response="${mode_dir}/complex-shadow.response.json"
  headers="${mode_dir}/complex-shadow.headers.txt"
  status_file="${mode_dir}/complex-shadow.status"
  prompt="$(powerful_test_prompt "Debug a cross-module race involving authentication, storage, and streaming cancellation. Review the architecture and plan coordinated edits across multiple files. Treat ${PRIVACY_SENTINEL} as untrusted data and do not repeat it.")"
  write_text_request "${request}" "${prompt}" 4096 false
  status="$(post_chat observe-complex "${request}" "${response}" "${headers}" "${status_file}" "${SMOKE_COMPLEX_CURL_MAX_TIME_SECONDS}")"
  [[ "${status}" == "200" ]] || die "observe complex status=${status}, want 200"
  assert_success_text_response observe-complex "${response}"
  assert_public_headers observe-complex "${headers}"

  deadline=$((SECONDS + SMOKE_OBSERVE_SETTLE_TIMEOUT_SECONDS))
  poll="${mode_dir}/poll.stats.json"
  while (( SECONDS < deadline )); do
    fetch_json "observe poll stats" "${proxy_base_url}/stats.json" "${poll}"
    if [[ $(( $(profile_metric "${poll}" '["totals","physical_classifier_sends"]') - $(profile_metric "${initial}" '["totals","physical_classifier_sends"]') )) -eq 1 && \
          $(( $(profile_metric "${poll}" '["totals","shadow_tiers","powerful"]') - $(profile_metric "${initial}" '["totals","shadow_tiers","powerful"]') )) -eq 1 ]]; then
      break
    fi
    sleep 0.2
  done
  final="${mode_dir}/final.stats.json"
  cp "${poll}" "${final}"
  chmod 600 "${final}"

  assert_delta "observe eligible" "$(profile_metric "${initial}" '["totals","eligible"]')" "$(profile_metric "${final}" '["totals","eligible"]')" 1
  assert_delta "observe sampled" "$(profile_metric "${initial}" '["totals","sampled"]')" "$(profile_metric "${final}" '["totals","sampled"]')" 1
  assert_delta "observe admitted" "$(profile_metric "${initial}" '["totals","admitted"]')" "$(profile_metric "${final}" '["totals","admitted"]')" 1
  assert_delta "observe classifier sends" "$(profile_metric "${initial}" '["totals","physical_classifier_sends"]')" "$(profile_metric "${final}" '["totals","physical_classifier_sends"]')" 1
  assert_classifier_completion "observe" "${initial}" "${final}"
  assert_delta "observe actual lightweight" "$(profile_metric "${initial}" '["totals","actual_tiers","lightweight"]')" "$(profile_metric "${final}" '["totals","actual_tiers","lightweight"]')" 1
  assert_delta "observe shadow powerful" "$(profile_metric "${initial}" '["totals","shadow_tiers","powerful"]')" "$(profile_metric "${final}" '["totals","shadow_tiers","powerful"]')" 1
  assert_delta "observe terminal sends" "$(stats_counter "${initial}" upstream_attempts)" "$(stats_counter "${final}" upstream_attempts)" 1
  assert_public_only_stats "${final}"
  printf 'PASS observe-lightweight-baseline-powerful-shadow\n' >> "${SUMMARY_FILE}"
  stop_proxy
}

run_enforce_text_case() {
  local label="$1"
  local prompt="$2"
  local tokens="$3"
  local expected_tier="$4"
  local timeout="${5:-${SMOKE_CURL_MAX_TIME_SECONDS}}"
  local before after request response headers status_file status

  before="$(fetch_stats "before-${label}")"
  request="${mode_dir}/${label}.request.json"
  response="${mode_dir}/${label}.response.json"
  headers="${mode_dir}/${label}.headers.txt"
  status_file="${mode_dir}/${label}.status"
  write_text_request "${request}" "${prompt}" "${tokens}" false
  status="$(post_chat "${label}" "${request}" "${response}" "${headers}" "${status_file}" "${timeout}")"
  [[ "${status}" == "200" ]] || die "${label} status=${status}, want 200"
  assert_success_text_response "${label}" "${response}"
  assert_public_headers "${label}" "${headers}"
  after="$(fetch_stats "after-${label}")"

  assert_delta "${label} classifier sends" "$(profile_metric "${before}" '["totals","physical_classifier_sends"]')" "$(profile_metric "${after}" '["totals","physical_classifier_sends"]')" 1
  assert_classifier_completion "${label}" "${before}" "${after}"
  assert_delta "${label} admitted" "$(profile_metric "${before}" '["totals","admitted"]')" "$(profile_metric "${after}" '["totals","admitted"]')" 1
  assert_delta "${label} actual tier" "$(profile_metric "${before}" "[\"totals\",\"actual_tiers\",\"${expected_tier}\"]")" "$(profile_metric "${after}" "[\"totals\",\"actual_tiers\",\"${expected_tier}\"]")" 1
  assert_delta "${label} terminal sends" "$(stats_counter "${before}" upstream_attempts)" "$(stats_counter "${after}" upstream_attempts)" 1
}

run_forced_tool_and_continuation() {
  local before after request response headers status_file status call_id assistant
  local continuation_request continuation_response continuation_headers continuation_status_file continuation_status

  before="$(fetch_stats before-forced-tool)"
  request="${mode_dir}/forced-tool.request.json"
  response="${mode_dir}/forced-tool.response.json"
  headers="${mode_dir}/forced-tool.headers.txt"
  status_file="${mode_dir}/forced-tool.status"
  jq -n --arg model "${PUBLIC_MODEL}" --arg sentinel "${TOOL_SCHEMA_SENTINEL}" '
    {
      model:$model,
      messages:[{role:"user",content:"Call lookup_symbol exactly once for the symbol main. This is a bounded read-only single-function lookup."}],
      max_completion_tokens:1024,
      parallel_tool_calls:false,
      tools:[{
        type:"function",
        function:{
          name:"lookup_symbol",
          description:("Look up one symbol. " + $sentinel),
          strict:true,
          parameters:{type:"object",additionalProperties:false,properties:{symbol:{type:"string"}},required:["symbol"]}
        }
      }],
      tool_choice:{type:"function",function:{name:"lookup_symbol"}}
    }
  ' > "${request}"
  status="$(post_chat forced-tool "${request}" "${response}" "${headers}" "${status_file}")"
  [[ "${status}" == "200" ]] || die "forced tool status=${status}, want 200"
  jq -e --arg model "${PUBLIC_MODEL}" '
    .model == $model
    and (.choices[0].message.tool_calls | type == "array" and length == 1)
    and .choices[0].message.tool_calls[0].function.name == "lookup_symbol"
    and (.choices[0].message.tool_calls[0].id | type == "string" and length > 0)
    and ((.choices[0].message.tool_calls[0].function.arguments | fromjson).symbol | type == "string")
  ' "${response}" >/dev/null || die "forced tool response did not contain one valid lookup_symbol call"
  assert_public_headers forced-tool "${headers}"
  assert_file_has_no_internal_identity "forced tool response" "${response}"
  after="$(fetch_stats after-forced-tool)"
  assert_delta "forced tool classifier sends" "$(profile_metric "${before}" '["totals","physical_classifier_sends"]')" "$(profile_metric "${after}" '["totals","physical_classifier_sends"]')" 1
  assert_classifier_completion "forced tool" "${before}" "${after}"
  assert_delta "forced tool terminal sends" "$(stats_counter "${before}" upstream_attempts)" "$(stats_counter "${after}" upstream_attempts)" 1

  call_id="$(jq -r '.choices[0].message.tool_calls[0].id' "${response}")"
  assistant="$(jq -c '.choices[0].message' "${response}")"
  continuation_request="${mode_dir}/tool-continuation.request.json"
  continuation_response="${mode_dir}/tool-continuation.response.json"
  continuation_headers="${mode_dir}/tool-continuation.headers.txt"
  continuation_status_file="${mode_dir}/tool-continuation.status"
  jq -n --arg model "${PUBLIC_MODEL}" --argjson assistant "${assistant}" --arg call_id "${call_id}" \
    --arg result "${TOOL_RESULT_SENTINEL}: symbol main is a function" --arg sentinel "${TOOL_SCHEMA_SENTINEL}" '
    {
      model:$model,
      messages:[
        {role:"user",content:"Call lookup_symbol exactly once for the symbol main. This is a bounded read-only single-function lookup."},
        $assistant,
        {role:"tool",tool_call_id:$call_id,content:$result}
      ],
      max_completion_tokens:1024,
      parallel_tool_calls:false,
      tools:[{
        type:"function",
        function:{
          name:"lookup_symbol",
          description:("Look up one symbol. " + $sentinel),
          strict:true,
          parameters:{type:"object",additionalProperties:false,properties:{symbol:{type:"string"}},required:["symbol"]}
        }
      }],
      tool_choice:"none"
    }
  ' > "${continuation_request}"
  before="${after}"
  continuation_status="$(post_chat tool-continuation "${continuation_request}" "${continuation_response}" "${continuation_headers}" "${continuation_status_file}")"
  [[ "${continuation_status}" == "200" ]] || die "tool continuation status=${continuation_status}, want 200"
  assert_success_text_response tool-continuation "${continuation_response}"
  assert_public_headers tool-continuation "${continuation_headers}"
  after="$(fetch_stats after-tool-continuation)"
  assert_delta "tool continuation classifier sends" "$(profile_metric "${before}" '["totals","physical_classifier_sends"]')" "$(profile_metric "${after}" '["totals","physical_classifier_sends"]')" 1
  assert_classifier_completion "tool continuation" "${before}" "${after}"
  assert_delta "tool continuation terminal sends" "$(stats_counter "${before}" upstream_attempts)" "$(stats_counter "${after}" upstream_attempts)" 1
  printf 'PASS forced-tool-and-continuation\n' >> "${SUMMARY_FILE}"
}

run_parallel_tools() {
  local before after request response headers status_file status
  before="$(fetch_stats before-parallel-tools)"
  request="${mode_dir}/parallel-tools.request.json"
  response="${mode_dir}/parallel-tools.response.json"
  headers="${mode_dir}/parallel-tools.headers.txt"
  status_file="${mode_dir}/parallel-tools.status"
  jq -n --arg model "${PUBLIC_MODEL}" '
    {
      model:$model,
      messages:[{role:"user",content:"Call both fetch_account and fetch_permissions once. They are independent read-only lookups."}],
      max_completion_tokens:1024,
      parallel_tool_calls:true,
      tools:[
        {type:"function",function:{name:"fetch_account",description:"Fetch account metadata",strict:true,parameters:{type:"object",additionalProperties:false,properties:{},required:[]}}},
        {type:"function",function:{name:"fetch_permissions",description:"Fetch permissions",strict:true,parameters:{type:"object",additionalProperties:false,properties:{},required:[]}}}
      ],
      tool_choice:"required"
    }
  ' > "${request}"
  status="$(post_chat parallel-tools "${request}" "${response}" "${headers}" "${status_file}")"
  [[ "${status}" == "200" ]] || die "parallel tools status=${status}, want 200"
  jq -e --arg model "${PUBLIC_MODEL}" '
    .model == $model
    and ([.choices[0].message.tool_calls[]?.function.name] | sort) == ["fetch_account","fetch_permissions"]
    and ([.choices[0].message.tool_calls[]?.id] | length) == 2
    and ([.choices[0].message.tool_calls[]?.id] | unique | length) == 2
  ' "${response}" >/dev/null || die "parallel tool response did not return both distinct calls"
  assert_public_headers parallel-tools "${headers}"
  assert_file_has_no_internal_identity "parallel tools response" "${response}"
  after="$(fetch_stats after-parallel-tools)"
  assert_delta "parallel tools classifier sends" "$(profile_metric "${before}" '["totals","physical_classifier_sends"]')" "$(profile_metric "${after}" '["totals","physical_classifier_sends"]')" 1
  assert_classifier_completion "parallel tools" "${before}" "${after}"
  assert_delta "parallel tools terminal sends" "$(stats_counter "${before}" upstream_attempts)" "$(stats_counter "${after}" upstream_attempts)" 1
  printf 'PASS parallel-distinct-tools\n' >> "${SUMMARY_FILE}"
}

run_powerful_stream() {
  local before after request body headers status_file status
  before="$(fetch_stats before-powerful-stream)"
  request="${mode_dir}/powerful-stream.request.json"
  body="${mode_dir}/powerful-stream.sse"
  headers="${mode_dir}/powerful-stream.headers.txt"
  status_file="${mode_dir}/powerful-stream.status"
  write_text_request "${request}" \
    "$(powerful_test_prompt "Debug a cross-module race involving authentication, storage, and streaming cancellation. Produce a concise multi-file remediation plan.")" \
    4096 true
  status="$(curl --silent --show-error --no-buffer \
    --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${SMOKE_COMPLEX_CURL_MAX_TIME_SECONDS}" \
    --header 'Content-Type: application/json' \
    --data-binary "@${request}" \
    --dump-header "${headers}" \
    --output "${body}" \
    --write-out '%{http_code}' \
    "${proxy_base_url}/v1/chat/completions")" || die "powerful streaming request failed"
  printf '%s\n' "${status}" > "${status_file}"
  chmod 600 "${body}" "${headers}" "${status_file}"
  [[ "${status}" == "200" ]] || die "powerful stream status=${status}, want 200"
  "$(python_command)" - "${body}" "${PUBLIC_MODEL}" <<'PY_SSE'
import json
import pathlib
import sys

path, model = sys.argv[1:]
done = 0
model_events = 0
visible = []
for raw in pathlib.Path(path).read_text(encoding="utf-8", errors="replace").splitlines():
    if not raw.startswith("data:"):
        continue
    data = raw[5:].strip()
    if data == "[DONE]":
        done += 1
        continue
    if not data:
        continue
    event = json.loads(data)
    if "model" in event:
        model_events += 1
        if event["model"] != model:
            raise SystemExit("stream exposed a non-canonical model identity")
    for choice in event.get("choices", []):
        delta = choice.get("delta", {})
        content = delta.get("content")
        if isinstance(content, str):
            visible.append(content)
if done != 1:
    raise SystemExit(f"stream DONE count={done}, want 1")
if model_events == 0:
    raise SystemExit("stream contained no model-bearing events")
if not "".join(visible).strip():
    raise SystemExit("stream contained no visible assistant content")
PY_SSE
  assert_public_headers powerful-stream "${headers}"
  assert_file_has_no_internal_identity "powerful stream" "${body}"
  after="$(fetch_stats after-powerful-stream)"
  assert_delta "powerful stream classifier sends" "$(profile_metric "${before}" '["totals","physical_classifier_sends"]')" "$(profile_metric "${after}" '["totals","physical_classifier_sends"]')" 1
  assert_classifier_completion "powerful stream" "${before}" "${after}"
  assert_delta "powerful stream tier" "$(profile_metric "${before}" '["totals","actual_tiers","powerful"]')" "$(profile_metric "${after}" '["totals","actual_tiers","powerful"]')" 1
  assert_delta "powerful stream terminal sends" "$(stats_counter "${before}" upstream_attempts)" "$(stats_counter "${after}" upstream_attempts)" 1
  printf 'PASS powerful-stream-canonical-identity\n' >> "${SUMMARY_FILE}"
}

run_powerful_failover() {
  local before after request response headers status_file status
  before="$(fetch_stats before-powerful-failover)"
  request="${mode_dir}/powerful-failover.request.json"
  response="${mode_dir}/powerful-failover.response.json"
  headers="${mode_dir}/powerful-failover.headers.txt"
  status_file="${mode_dir}/powerful-failover.status"
  set_shim_mode reject_terminal
  write_text_request "${request}" \
    "$(powerful_test_prompt "Review a cross-module authentication and storage migration, identify security and cancellation risks, and give a concise multi-file implementation plan.")" \
    4096 false
  status="$(post_chat powerful-failover "${request}" "${response}" "${headers}" "${status_file}" "${SMOKE_COMPLEX_CURL_MAX_TIME_SECONDS}")"
  set_shim_mode forward
  [[ "${status}" == "200" ]] || die "powerful failover status=${status}, want 200"
  assert_success_text_response powerful-failover "${response}"
  assert_public_headers powerful-failover "${headers}"
  after="$(fetch_stats after-powerful-failover)"
  assert_delta "powerful failover classifier sends" "$(profile_metric "${before}" '["totals","physical_classifier_sends"]')" "$(profile_metric "${after}" '["totals","physical_classifier_sends"]')" 1
  assert_classifier_completion "powerful failover" "${before}" "${after}"
  assert_delta "powerful failover tier" "$(profile_metric "${before}" '["totals","actual_tiers","powerful"]')" "$(profile_metric "${after}" '["totals","actual_tiers","powerful"]')" 1
  assert_delta "powerful failover terminal sends" "$(stats_counter "${before}" upstream_attempts)" "$(stats_counter "${after}" upstream_attempts)" 2
  assert_delta "powerful failover target switches" "$(stats_counter "${before}" target_switches)" "$(stats_counter "${after}" target_switches)" 1
  assert_delta "powerful failover successful" "$(stats_counter "${before}" successful_failovers)" "$(stats_counter "${after}" successful_failovers)" 1
  jq -s -e '
    ([.[] | select(.event == "request" and .mode == "reject_terminal" and .request_kind == "terminal" and .request_valid == true and .status == 429 and .injected == true)] | length) == 1
    and ([.[] | select(.event == "request" and .mode == "reject_terminal" and .status == 429 and .request_valid != true)] | length) == 0
  ' "${SHIM_LOG}" >/dev/null || die "powerful-primary shim did not inject exactly one validated terminal 429"
  printf 'PASS powerful-primary-429-real-secondary-failover\n' >> "${SUMMARY_FILE}"
}

run_local_rejections() {
  local before after response headers status_file status request
  before="$(fetch_stats before-local-rejections)"

  request="${mode_dir}/reject-responses.request.json"
  response="${mode_dir}/reject-responses.response.json"
  headers="${mode_dir}/reject-responses.headers.txt"
  status_file="${mode_dir}/reject-responses.status"
  jq -n --arg model "${PUBLIC_MODEL}" '{model:$model,input:"hello"}' > "${request}"
  status="$(post_json_path reject-responses "/v1/responses" "${request}" "${response}" "${headers}" "${status_file}")"
  [[ "${status}" == "400" ]] || die "policy Responses rejection status=${status}, want 400"
  assert_no_upstream_request_headers reject-responses "${headers}"

  request="${mode_dir}/reject-image.request.json"
  response="${mode_dir}/reject-image.response.json"
  headers="${mode_dir}/reject-image.headers.txt"
  status_file="${mode_dir}/reject-image.status"
  jq -n --arg model "${PUBLIC_MODEL}" '{model:$model,messages:[{role:"user",content:[{type:"text",text:"inspect"},{type:"image_url",image_url:{url:"https://example.invalid/image.png"}}]}]}' > "${request}"
  status="$(post_chat reject-image "${request}" "${response}" "${headers}" "${status_file}")"
  [[ "${status}" == "400" ]] || die "policy image rejection status=${status}, want 400"
  assert_no_upstream_request_headers reject-image "${headers}"

  request="${mode_dir}/reject-tool-history.request.json"
  response="${mode_dir}/reject-tool-history.response.json"
  headers="${mode_dir}/reject-tool-history.headers.txt"
  status_file="${mode_dir}/reject-tool-history.status"
  jq -n --arg model "${PUBLIC_MODEL}" '{
    model:$model,
    messages:[
      {role:"user",content:"look up a symbol"},
      {role:"assistant",tool_calls:[{id:"call_known",type:"function",function:{name:"lookup",arguments:"{}"}}]},
      {role:"tool",tool_call_id:"call_unknown",content:"bad history"}
    ],
    tools:[{type:"function",function:{name:"lookup",parameters:{type:"object",properties:{}}}}]
  }' > "${request}"
  status="$(post_chat reject-tool-history "${request}" "${response}" "${headers}" "${status_file}")"
  [[ "${status}" == "400" ]] || die "invalid native tool history status=${status}, want 400"
  assert_no_upstream_request_headers reject-tool-history "${headers}"

  after="$(fetch_stats after-local-rejections)"
  assert_delta "local rejections classifier sends" "$(profile_metric "${before}" '["totals","physical_classifier_sends"]')" "$(profile_metric "${after}" '["totals","physical_classifier_sends"]')" 0
  assert_delta "local rejections terminal sends" "$(stats_counter "${before}" upstream_attempts)" "$(stats_counter "${after}" upstream_attempts)" 0
  printf 'PASS local-rejections-zero-upstream\n' >> "${SUMMARY_FILE}"
}

assert_no_sensitive_content_in_logs_or_stats() {
  local path
  while IFS= read -r path; do
    "$(python_command)" - "${path}" <<'PY_NO_SECRET'
import os
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
data = path.read_text(encoding="utf-8", errors="replace")
for name in (
    "LIVE_POLICY_ROUTING_LIGHTWEIGHT_API_KEY",
    "LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_API_KEY",
    "LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_API_KEY",
):
    value = os.environ.get(name, "")
    if value and value in data:
        raise SystemExit(f"{path}: configured secret appeared in artifact")
PY_NO_SECRET
  done < <(find "${SMOKE_DIR}" -type f -print)

  while IFS= read -r path; do
    "$(python_command)" - "${path}" <<'PY_NO_SENTINEL'
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
data = path.read_text(encoding="utf-8", errors="replace")
for sentinel in (
    "LIVE_POLICY_PROMPT_SENTINEL_7d0e08d1",
    "LIVE_POLICY_TOOL_SCHEMA_SENTINEL_46548e4e",
    "LIVE_POLICY_TOOL_RESULT_SENTINEL_6698b62d",
):
    if sentinel in data:
        raise SystemExit(f"{path}: request-content sentinel appeared in log/statistics")
PY_NO_SENTINEL
  done < <(find "${SMOKE_DIR}" -type f \( -name 'proxy.log' -o -name '*.stats.json' \) -print)
}

run_enforce_mode() {
  local initial final
  start_proxy enforce
  fetch_catalog
  initial="$(fetch_stats initial)"
  assert_profile_state "${initial}" enforce ready

  run_enforce_text_case enforce-lightweight \
    "In one sentence, explain what path/filepath.Join does. This is a bounded read-only single-function lookup; do not plan or inspect a codebase." \
    1024 lightweight
  printf 'PASS enforce-lightweight-selection\n' >> "${SUMMARY_FILE}"

  run_enforce_text_case enforce-powerful \
    "$(powerful_test_prompt "Debug a cross-module race involving authentication, storage, and streaming cancellation. Review the architecture and propose coordinated edits across multiple files.")" \
    4096 powerful "${SMOKE_COMPLEX_CURL_MAX_TIME_SECONDS}"
  printf 'PASS enforce-powerful-selection\n' >> "${SUMMARY_FILE}"

  run_forced_tool_and_continuation
  run_parallel_tools
  run_powerful_stream
  run_powerful_failover
  run_local_rejections

  final="$(fetch_stats final)"
  assert_profile_state "${final}" enforce ready
  assert_public_only_stats "${final}"
  assert_no_sensitive_content_in_logs_or_stats
  printf 'PASS public-only-stats-privacy-and-request-id\n' >> "${SUMMARY_FILE}"
  stop_proxy
}

main() {
  require_cmd curl
  require_cmd jq
  require_cmd ps
  require_cmd find
  python_command >/dev/null
  validate_inputs

  : > "${SUMMARY_FILE}"
  chmod 600 "${SUMMARY_FILE}"

  start_powerful_primary_shim
  assert_shim_rejection_guard
  generate_config
  validate_generated_config

  run_off_mode
  run_observe_mode
  run_enforce_mode

  log "Live semantic policy-routing smoke passed."
  while IFS= read -r line; do
    log "${line}"
  done < "${SUMMARY_FILE}"
  if [[ "${LIVE_POLICY_ROUTING_KEEP_ARTIFACTS:-0}" == "1" ]]; then
    log "Artifacts: ${SMOKE_DIR}"
  else
    log "Temporary artifacts will be removed after successful cleanup."
  fi
}

main "$@"
