#!/usr/bin/env bash

# Live schema-v2 model-route smoke against two controlled Responses-capable
# OpenAI-compatible/Azure targets.
#
# The primary is reached through a loopback control proxy. The control proxy
# first forwards to the real primary, then injects an authoritative HTTP 429 so
# Vekil can safely exercise ordered failover to the real secondary. The smoke
# also verifies public model identity, exact response-state target pinning,
# unknown-state fail-closed behavior, and /stats.json attempt accounting.
#
# Required environment:
#   LIVE_PROVIDER_ROUTING_PRIMARY_TYPE       azure-openai|openai-compatible
#   LIVE_PROVIDER_ROUTING_PRIMARY_BASE_URL   API base ending before /responses
#   LIVE_PROVIDER_ROUTING_PRIMARY_API_KEY
#   LIVE_PROVIDER_ROUTING_PRIMARY_MODEL      physical model/deployment name
#   LIVE_PROVIDER_ROUTING_SECONDARY_TYPE
#   LIVE_PROVIDER_ROUTING_SECONDARY_BASE_URL
#   LIVE_PROVIDER_ROUTING_SECONDARY_API_KEY
#   LIVE_PROVIDER_ROUTING_SECONDARY_MODEL
#
# Both targets must implement the same native /responses contract. Azure base
# URLs must use API-key auth and the OpenAI v1 form ending in /openai/v1;
# generic OpenAI-compatible targets use bearer auth.

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
PROXY_HOST="${PROXY_HOST:-127.0.0.1}"
SMOKE_STARTUP_TIMEOUT_SECONDS="${SMOKE_STARTUP_TIMEOUT_SECONDS:-30}"
SMOKE_CURL_CONNECT_TIMEOUT_SECONDS="${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS:-5}"
SMOKE_CURL_MAX_TIME_SECONDS="${SMOKE_CURL_MAX_TIME_SECONDS:-120}"
SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS="${SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS:-5}"
SMOKE_PROCESS_TERM_GRACE_SECONDS="${SMOKE_PROCESS_TERM_GRACE_SECONDS:-5}"
SMOKE_PORT_RELEASE_TIMEOUT_SECONDS="${SMOKE_PORT_RELEASE_TIMEOUT_SECONDS:-5}"
SMOKE_SHIM_UPSTREAM_TIMEOUT_SECONDS="${SMOKE_SHIM_UPSTREAM_TIMEOUT_SECONDS:-120}"
SMOKE_PROXY_AUTO_PORT_MAX_ATTEMPTS="${SMOKE_PROXY_AUTO_PORT_MAX_ATTEMPTS:-3}"
PUBLIC_MODEL="${LIVE_PROVIDER_ROUTING_PUBLIC_MODEL:-vekil-live-routed-model}"
ROUTE_ID="${LIVE_PROVIDER_ROUTING_ROUTE_ID:-live-provider-routing}"
PRIMARY_TARGET_ID="primary"
SECONDARY_TARGET_ID="secondary"

TMP_PARENT="${LIVE_PROVIDER_ROUTING_TMP_PARENT:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}}"
SMOKE_DIR="${LIVE_PROVIDER_ROUTING_SMOKE_DIR:-$(mktemp -d "${TMP_PARENT%/}/live-provider-routing-smoke.XXXXXX")}"
if [[ "${SMOKE_DIR}" != /* ]]; then
  SMOKE_DIR="${PWD}/${SMOKE_DIR}"
fi
umask 077
mkdir -p "${SMOKE_DIR}"
chmod 700 "${SMOKE_DIR}"

CONFIG_JSON="${SMOKE_DIR}/providers.json"
PROXY_LOG="${SMOKE_DIR}/proxy.log"
PROXY_TOKEN_DIR="${SMOKE_DIR}/token"
MODELS_JSON="${SMOKE_DIR}/models.json"
SUMMARY_FILE="${SMOKE_DIR}/summary.txt"
SHIM_SCRIPT="${SMOKE_DIR}/primary-shim.py"
SHIM_LOG="${SMOKE_DIR}/primary-shim.log"
SHIM_MODE_FILE="${SMOKE_DIR}/primary-shim.mode"
SHIM_PORT_FILE="${SMOKE_DIR}/primary-shim.port"

proxy_pid=""
proxy_pgid=""
proxy_listen_confirmed=0
shim_pid=""
shim_pgid=""
shim_listen_confirmed=0
SHIM_PORT=""
if [[ -n "${PROXY_PORT:-}" ]]; then
  PROXY_PORT_EXPLICIT=1
else
  PROXY_PORT_EXPLICIT=0
fi
PROXY_PORT="${PROXY_PORT:-}"
PROXY_BASE_URL=""

python_command() {
  if command -v python3 >/dev/null 2>&1; then
    command -v python3
    return
  fi
  if command -v python >/dev/null 2>&1; then
    command -v python
    return
  fi
  die "python3 (or python) is required for isolated port allocation and the primary control proxy"
}

connect_host() {
  case "$1" in
    0.0.0.0) printf '127.0.0.1\n' ;;
    ::|\[::\]) printf '::1\n' ;;
    *) printf '%s\n' "$1" ;;
  esac
}

allocate_free_port() {
  local python_bin host
  python_bin="$(python_command)"
  host="$(connect_host "$1")"
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
  local host="$1"
  local port="$2"
  local python_bin connect
  python_bin="$(python_command)"
  connect="$(connect_host "${host}")"
  "${python_bin}" - "${connect}" "${port}" <<'PY_CONNECT' >/dev/null 2>&1
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
  local host="$1"
  local port="$2"
  local deadline=$((SECONDS + SMOKE_PORT_RELEASE_TIMEOUT_SECONDS))
  while (( SECONDS < deadline )); do
    if ! port_is_open "${host}" "${port}"; then
      return 0
    fi
    sleep 0.1
  done
  ! port_is_open "${host}" "${port}"
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM

  if [[ -n "${proxy_pgid}" ]]; then
    terminate_process_group "${proxy_pid}" "${proxy_pgid}"
    proxy_pid=""
    proxy_pgid=""
  fi
  if [[ -n "${shim_pgid}" ]]; then
    terminate_process_group "${shim_pid}" "${shim_pgid}"
    shim_pid=""
    shim_pgid=""
  fi

  if [[ "${proxy_listen_confirmed}" == "1" ]] && ! wait_for_port_release "${PROXY_HOST}" "${PROXY_PORT}"; then
    printf 'error: proxy cleanup did not release %s:%s\n' "${PROXY_HOST}" "${PROXY_PORT}" >&2
    rc=1
  fi
  if [[ "${shim_listen_confirmed}" == "1" ]] && ! wait_for_port_release "127.0.0.1" "${SHIM_PORT}"; then
    printf 'error: primary control proxy cleanup did not release 127.0.0.1:%s\n' "${SHIM_PORT}" >&2
    rc=1
  fi
  exit "${rc}"
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

validate_inputs() {
  local name type base_url model type_var base_url_var model_var
  for name in \
    LIVE_PROVIDER_ROUTING_PRIMARY_TYPE \
    LIVE_PROVIDER_ROUTING_PRIMARY_BASE_URL \
    LIVE_PROVIDER_ROUTING_PRIMARY_API_KEY \
    LIVE_PROVIDER_ROUTING_PRIMARY_MODEL \
    LIVE_PROVIDER_ROUTING_SECONDARY_TYPE \
    LIVE_PROVIDER_ROUTING_SECONDARY_BASE_URL \
    LIVE_PROVIDER_ROUTING_SECONDARY_API_KEY \
    LIVE_PROVIDER_ROUTING_SECONDARY_MODEL; do
    require_env "${name}"
  done

  for name in PRIMARY SECONDARY; do
    type_var="LIVE_PROVIDER_ROUTING_${name}_TYPE"
    base_url_var="LIVE_PROVIDER_ROUTING_${name}_BASE_URL"
    model_var="LIVE_PROVIDER_ROUTING_${name}_MODEL"
    type="${!type_var}"
    base_url="${!base_url_var}"
    model="${!model_var}"
    case "${type}" in
      azure-openai|openai-compatible) ;;
      *) die "LIVE_PROVIDER_ROUTING_${name}_TYPE must be azure-openai or openai-compatible: ${type}" ;;
    esac
    [[ "${model}" != "${PUBLIC_MODEL}" ]] || die "LIVE_PROVIDER_ROUTING_${name}_MODEL must differ from the public smoke model ${PUBLIC_MODEL}"

    "$(python_command)" - "${name}" "${type}" "${base_url}" "${LIVE_PROVIDER_ROUTING_ALLOW_INSECURE_HTTP:-0}" <<'PY_VALIDATE_URL'
import sys
import urllib.parse

name, provider_type, raw, allow_http = sys.argv[1:]
parsed = urllib.parse.urlsplit(raw.rstrip("/"))
if parsed.scheme not in ({"https", "http"} if allow_http == "1" else {"https"}):
    raise SystemExit(f"LIVE_PROVIDER_ROUTING_{name}_BASE_URL must use HTTPS")
if not parsed.netloc or parsed.username or parsed.password or parsed.query or parsed.fragment:
    raise SystemExit(f"LIVE_PROVIDER_ROUTING_{name}_BASE_URL must be an absolute API base with no credentials, query, or fragment")
if provider_type == "azure-openai" and not parsed.path.rstrip("/").endswith("/openai/v1"):
    raise SystemExit(f"LIVE_PROVIDER_ROUTING_{name}_BASE_URL must end in /openai/v1 for azure-openai")
PY_VALIDATE_URL
  done

  [[ "${ROUTE_ID}" =~ ^[A-Za-z0-9]([A-Za-z0-9._-]{0,126}[A-Za-z0-9])?$ ]] || die "LIVE_PROVIDER_ROUTING_ROUTE_ID must be 1-128 bytes and use the schema-v2 operational ID character set: ${ROUTE_ID}"
  [[ -n "${PUBLIC_MODEL//[[:space:]]/}" ]] || die "LIVE_PROVIDER_ROUTING_PUBLIC_MODEL must not be empty"

  if [[ "${LIVE_PROVIDER_ROUTING_PRIMARY_TYPE}" == "${LIVE_PROVIDER_ROUTING_SECONDARY_TYPE}" && \
        "${LIVE_PROVIDER_ROUTING_PRIMARY_BASE_URL%/}" == "${LIVE_PROVIDER_ROUTING_SECONDARY_BASE_URL%/}" && \
        "${LIVE_PROVIDER_ROUTING_PRIMARY_MODEL}" == "${LIVE_PROVIDER_ROUTING_SECONDARY_MODEL}" ]]; then
    die "primary and secondary must identify two distinct controlled targets"
  fi
}

write_primary_shim() {
  cat > "${SHIM_SCRIPT}" <<'PY_SHIM'
#!/usr/bin/env python3
import argparse
import hashlib
import hmac
import http.server
import json
import os
import pathlib
import socketserver
import urllib.error
import urllib.parse
import urllib.request

HOP_BY_HOP = {
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
}


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
            self.wfile.write(body)

    def _control(self):
        try:
            control = json.loads(pathlib.Path(self.server.mode_file).read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            return {"mode": "invalid", "expect_previous_response_id": None}
        if not isinstance(control, dict):
            return {"mode": "invalid", "expect_previous_response_id": None}
        return control

    def _log(self, inspection, *, mode, status, **fields):
        entry = {
            "event": "request",
            "method": self.command,
            "mode": mode,
            "path": inspection["path"],
            "query_present": inspection["query_present"],
            "request_kind": inspection["request_kind"],
            "status": status,
            "auth_header": self.server.expected_auth_header,
            "authorization_present": inspection["authorization_present"],
            "api_key_present": inspection["api_key_present"],
            "auth_valid": inspection["auth_valid"],
            "content_type_valid": inspection["content_type_valid"],
            "body_json_object": inspection["body_json_object"],
            "model_valid": inspection["model_valid"],
            "stream_valid": inspection["stream_valid"],
            "max_output_tokens_valid": inspection["max_output_tokens_valid"],
            "input_valid": inspection["input_valid"],
            "previous_response_id_present": inspection["previous_response_id_present"],
            "previous_response_id_valid": inspection["previous_response_id_valid"],
            "expected_previous_response_id_present": inspection["expected_previous_response_id_present"],
            "request_shape_valid": inspection["request_shape_valid"],
            "valid_responses_post": inspection["valid_responses_post"],
            "valid_models_get": inspection["valid_models_get"],
            "validation_errors": inspection["validation_errors"],
        }
        entry.update(fields)
        print(json.dumps(entry, separators=(",", ":"), sort_keys=True), flush=True)

    def _read_body(self):
        errors = []
        raw_length = self.headers.get("Content-Length", "0")
        try:
            length = int(raw_length)
            if length < 0:
                raise ValueError
        except ValueError:
            length = 0
            errors.append("content_length_invalid")
        return (self.rfile.read(length) if length else b""), errors

    def _inspect(self, incoming, request_body, control, read_errors):
        prefix = self.server.mount_prefix
        responses_path = f"{prefix}/responses" if prefix else "/responses"
        models_path = f"{prefix}/models" if prefix else "/models"
        if incoming.path == responses_path:
            request_kind = "responses"
        elif incoming.path == models_path:
            request_kind = "models_discovery"
        else:
            request_kind = "other"

        authorization = self.headers.get("Authorization", "")
        api_key = self.headers.get("api-key", "")
        expected_value = api_key if self.server.expected_auth_header == "api-key" else authorization
        unexpected_value = authorization if self.server.expected_auth_header == "api-key" else api_key
        received_digest = hashlib.sha256(expected_value.encode("utf-8")).hexdigest()
        auth_valid = hmac.compare_digest(received_digest, self.server.expected_auth_sha256) and not unexpected_value

        content_type = self.headers.get("Content-Type", "")
        content_type_valid = content_type.lower().startswith("application/json")
        body = None
        body_parse_valid = False
        if request_body:
            try:
                body = json.loads(request_body)
                body_parse_valid = True
            except (UnicodeDecodeError, json.JSONDecodeError):
                pass
        body_json_object = body_parse_valid and isinstance(body, dict)

        model_valid = body_json_object and body.get("model") == self.server.expected_model
        stream_valid = body_json_object and body.get("stream") is False
        max_output_tokens_valid = body_json_object and body.get("max_output_tokens") == 1024
        input_valid = body_json_object and isinstance(body.get("input"), str) and bool(body["input"].strip())
        previous_response_id_present = body_json_object and "previous_response_id" in body
        previous_response_id_valid = (
            not previous_response_id_present
            or (isinstance(body.get("previous_response_id"), str) and bool(body["previous_response_id"].strip()))
        )
        expected_previous_response_id_present = control.get("expect_previous_response_id")

        response_errors = list(read_errors)
        if self.command != "POST":
            response_errors.append("method_not_post")
        if incoming.path != responses_path or incoming.query:
            response_errors.append("responses_path_invalid")
        if not auth_valid:
            response_errors.append("auth_invalid")
        if not content_type_valid:
            response_errors.append("content_type_invalid")
        if not body_json_object:
            response_errors.append("body_not_json_object")
        else:
            if not model_valid:
                response_errors.append("model_invalid")
            if not stream_valid:
                response_errors.append("stream_invalid")
            if not max_output_tokens_valid:
                response_errors.append("max_output_tokens_invalid")
            if not input_valid:
                response_errors.append("input_invalid")
            if not previous_response_id_valid:
                response_errors.append("previous_response_id_invalid")
        if not isinstance(expected_previous_response_id_present, bool):
            response_errors.append("previous_response_id_expectation_invalid")
        elif previous_response_id_present != expected_previous_response_id_present:
            response_errors.append("previous_response_id_presence_invalid")

        models_errors = list(read_errors)
        if self.command != "GET":
            models_errors.append("method_not_get")
        if incoming.path != models_path or incoming.query:
            models_errors.append("models_path_invalid")
        if not auth_valid:
            models_errors.append("auth_invalid")
        if request_body:
            models_errors.append("models_body_unexpected")

        if request_kind == "responses":
            validation_errors = response_errors
        elif request_kind == "models_discovery":
            validation_errors = models_errors
        else:
            validation_errors = ["unsupported_control_proxy_path"]

        request_shape_valid = (
            body_json_object
            and model_valid
            and stream_valid
            and max_output_tokens_valid
            and input_valid
            and previous_response_id_valid
            and isinstance(expected_previous_response_id_present, bool)
            and previous_response_id_present == expected_previous_response_id_present
        )
        return {
            "path": incoming.path,
            "query_present": bool(incoming.query),
            "request_kind": request_kind,
            "authorization_present": bool(authorization),
            "api_key_present": bool(api_key),
            "auth_valid": auth_valid,
            "content_type_valid": content_type_valid,
            "body_json_object": body_json_object,
            "model_valid": model_valid,
            "stream_valid": stream_valid,
            "max_output_tokens_valid": max_output_tokens_valid,
            "input_valid": input_valid,
            "previous_response_id_present": previous_response_id_present,
            "previous_response_id_valid": previous_response_id_valid,
            "expected_previous_response_id_present": expected_previous_response_id_present,
            "request_shape_valid": request_shape_valid,
            "valid_responses_post": request_kind == "responses" and not response_errors,
            "valid_models_get": request_kind == "models_discovery" and not models_errors,
            "validation_errors": validation_errors,
        }

    def _handle(self):
        control = self._control()
        mode = control.get("mode")
        incoming = urllib.parse.urlsplit(self.path)
        request_body, read_errors = self._read_body()
        inspection = self._inspect(incoming, request_body, control, read_errors)

        if mode not in {"forward", "reject"}:
            body = b'{"error":{"message":"invalid primary control proxy mode"}}'
            self._write(500, body, {"Content-Type": "application/json"})
            self._log(inspection, mode="invalid", status=500)
            return

        if inspection["request_kind"] == "responses" and not inspection["valid_responses_post"]:
            body = json.dumps(
                {
                    "error": {
                        "message": "primary control proxy request validation failed: "
                        + ",".join(inspection["validation_errors"]),
                        "type": "control_proxy_validation_error",
                    }
                },
                separators=(",", ":"),
            ).encode("utf-8")
            self._write(400, body, {"Content-Type": "application/json"})
            self._log(inspection, mode=mode, status=400)
            return

        if inspection["request_kind"] == "models_discovery" and not inspection["valid_models_get"]:
            body = b'{"error":{"message":"primary control proxy model-discovery validation failed"}}'
            self._write(400, body, {"Content-Type": "application/json"})
            self._log(inspection, mode=mode, status=400)
            return

        if inspection["request_kind"] == "other":
            body = b'{"error":{"message":"unexpected primary control proxy path"}}'
            self._write(404, body, {"Content-Type": "application/json"})
            self._log(inspection, mode=mode, status=404)
            return

        if mode == "reject" and inspection["valid_responses_post"]:
            body = json.dumps(
                {
                    "error": {
                        "message": "live provider-routing smoke injected retryable primary rejection",
                        "type": "rate_limit_error",
                        "code": "rate_limit_exceeded",
                    }
                },
                separators=(",", ":"),
            ).encode("utf-8")
            self._write(
                429,
                body,
                {"Content-Type": "application/json", "Retry-After": "1"},
            )
            self._log(inspection, mode=mode, status=429)
            return

        suffix = incoming.path[len(self.server.mount_prefix):] if self.server.mount_prefix else incoming.path
        if not suffix.startswith("/"):
            suffix = "/" + suffix
        target = self.server.upstream_base + suffix
        if incoming.query:
            target += "?" + incoming.query

        request_headers = {}
        for key, value in self.headers.items():
            lower = key.lower()
            if lower in HOP_BY_HOP or lower in {"host", "content-length", "accept-encoding"}:
                continue
            request_headers[key] = value
        request_headers["Accept-Encoding"] = "identity"

        request = urllib.request.Request(
            target,
            data=request_body or None,
            headers=request_headers,
            method=self.command,
        )
        opener = urllib.request.build_opener(NoRedirect())
        try:
            response = opener.open(request, timeout=self.server.upstream_timeout)
        except urllib.error.HTTPError as error:
            response = error
        except Exception as error:  # noqa: BLE001 - bounded local smoke proxy
            body = json.dumps(
                {"error": {"message": f"primary control proxy upstream transport failure: {type(error).__name__}"}},
                separators=(",", ":"),
            ).encode("utf-8")
            self._write(502, body, {"Content-Type": "application/json"})
            self._log(inspection, mode=mode, status=502, transport_error=type(error).__name__)
            return

        with response:
            response_body = response.read()
            status = getattr(response, "status", None) or response.getcode()
            self.send_response(status)
            for key, value in response.headers.items():
                lower = key.lower()
                if lower in HOP_BY_HOP or lower in {"content-length", "connection", "server", "date"}:
                    continue
                self.send_header(key, value)
            self.send_header("Content-Length", str(len(response_body)))
            self.send_header("Connection", "close")
            self.end_headers()
            if response_body:
                self.wfile.write(response_body)
            self._log(inspection, mode=mode, status=status)

    do_GET = _handle
    do_POST = _handle


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=0)
    parser.add_argument("--port-file", required=True)
    parser.add_argument("--mode-file", required=True)
    parser.add_argument("--mount-prefix", default="")
    parser.add_argument("--expected-model", required=True)
    parser.add_argument("--expected-auth-header", choices=("api-key", "authorization"), required=True)
    parser.add_argument("--expected-auth-sha256", required=True)
    parser.add_argument("--upstream-timeout", type=float, default=120.0)
    args = parser.parse_args()

    upstream_base = os.environ["LIVE_PROVIDER_ROUTING_PRIMARY_BASE_URL"].rstrip("/")
    mount_prefix = args.mount_prefix.rstrip("/")
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    server.upstream_base = upstream_base
    server.mount_prefix = mount_prefix
    server.mode_file = args.mode_file
    server.expected_model = args.expected_model
    server.expected_auth_header = args.expected_auth_header
    server.expected_auth_sha256 = args.expected_auth_sha256
    server.upstream_timeout = args.upstream_timeout

    port_path = pathlib.Path(args.port_file)
    port_path.write_text(str(server.server_port), encoding="utf-8")
    os.chmod(port_path, 0o600)
    print(json.dumps({"event": "listening", "host": args.host, "port": server.server_port}, separators=(",", ":")), flush=True)
    server.serve_forever(poll_interval=0.1)


if __name__ == "__main__":
    main()
PY_SHIM
  chmod 700 "${SHIM_SCRIPT}"
}

start_primary_shim() {
  local python_bin mount_prefix deadline expected_auth_header expected_auth_value expected_auth_sha256
  python_bin="$(python_command)"
  mount_prefix=""
  if [[ "${LIVE_PROVIDER_ROUTING_PRIMARY_TYPE}" == "azure-openai" ]]; then
    mount_prefix="/openai/v1"
    expected_auth_header="api-key"
    expected_auth_value="${LIVE_PROVIDER_ROUTING_PRIMARY_API_KEY}"
  else
    expected_auth_header="authorization"
    expected_auth_value="Bearer ${LIVE_PROVIDER_ROUTING_PRIMARY_API_KEY}"
  fi
  expected_auth_sha256="$(printf '%s' "${expected_auth_value}" | "${python_bin}" -c 'import hashlib, sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())')"
  unset expected_auth_value

  set_shim_mode forward false
  rm -f "${SHIM_PORT_FILE}"
  write_primary_shim

  log "Starting loopback primary control proxy"
  set -m
  env \
    -u LIVE_PROVIDER_ROUTING_PRIMARY_API_KEY \
    -u LIVE_PROVIDER_ROUTING_SECONDARY_API_KEY \
    "${python_bin}" "${SHIM_SCRIPT}" \
    --host 127.0.0.1 \
    --port 0 \
    --port-file "${SHIM_PORT_FILE}" \
    --mode-file "${SHIM_MODE_FILE}" \
    --mount-prefix "${mount_prefix}" \
    --expected-model "${LIVE_PROVIDER_ROUTING_PRIMARY_MODEL}" \
    --expected-auth-header "${expected_auth_header}" \
    --expected-auth-sha256 "${expected_auth_sha256}" \
    --upstream-timeout "${SMOKE_SHIM_UPSTREAM_TIMEOUT_SECONDS}" \
    >"${SHIM_LOG}" 2>&1 &
  shim_pid="$!"
  shim_pgid="${shim_pid}"
  set +m

  deadline=$((SECONDS + SMOKE_STARTUP_TIMEOUT_SECONDS))
  while (( SECONDS < deadline )); do
    if ! process_is_running "${shim_pid}"; then
      die "primary control proxy exited before listening; inspect ${SHIM_LOG}"
    fi
    if [[ -s "${SHIM_PORT_FILE}" ]]; then
      SHIM_PORT="$(cat "${SHIM_PORT_FILE}")"
      if [[ "${SHIM_PORT}" =~ ^[0-9]+$ ]] && port_is_open 127.0.0.1 "${SHIM_PORT}"; then
        shim_listen_confirmed=1
        return
      fi
    fi
    sleep 0.1
  done
  die "primary control proxy did not listen within ${SMOKE_STARTUP_TIMEOUT_SECONDS}s"
}

generate_config() {
  local primary_base
  if [[ "${LIVE_PROVIDER_ROUTING_PRIMARY_TYPE}" == "azure-openai" ]]; then
    primary_base="http://127.0.0.1:${SHIM_PORT}/openai/v1"
  else
    primary_base="http://127.0.0.1:${SHIM_PORT}"
  fi

  PRIMARY_SHIM_BASE_URL="${primary_base}" \
  EFFECTIVE_ROUTE_ID="${ROUTE_ID}" \
  EFFECTIVE_PUBLIC_MODEL="${PUBLIC_MODEL}" \
  "$(python_command)" - "${CONFIG_JSON}" <<'PY_CONFIG'
import json
import os
import pathlib
import sys


def provider(provider_id, provider_type, base_url, key_env, default=False):
    value = {
        "id": provider_id,
        "type": provider_type,
        "base_url": base_url.rstrip("/"),
        "api_key_env": key_env,
    }
    if default:
        value["default"] = True
    if provider_type == "openai-compatible":
        value["auth_type"] = "bearer"
        value["model_discovery"] = "static"
    return value


config = {
    "schema_version": 2,
    "providers": [
        provider(
            "live-primary",
            os.environ["LIVE_PROVIDER_ROUTING_PRIMARY_TYPE"],
            os.environ["PRIMARY_SHIM_BASE_URL"],
            "LIVE_PROVIDER_ROUTING_PRIMARY_API_KEY",
            default=True,
        ),
        provider(
            "live-secondary",
            os.environ["LIVE_PROVIDER_ROUTING_SECONDARY_TYPE"],
            os.environ["LIVE_PROVIDER_ROUTING_SECONDARY_BASE_URL"],
            "LIVE_PROVIDER_ROUTING_SECONDARY_API_KEY",
        ),
    ],
    "model_routes": [
        {
            "id": os.environ["EFFECTIVE_ROUTE_ID"],
            "public_id": os.environ["EFFECTIVE_PUBLIC_MODEL"],
            "name": "Vekil live routed model",
            "endpoints": ["/responses"],
            "targets": [
                {
                    "id": "primary",
                    "provider": "live-primary",
                    "upstream_model": os.environ["LIVE_PROVIDER_ROUTING_PRIMARY_MODEL"],
                },
                {
                    "id": "secondary",
                    "provider": "live-secondary",
                    "upstream_model": os.environ["LIVE_PROVIDER_ROUTING_SECONDARY_MODEL"],
                },
            ],
            "routing": {
                "mode": "priority_failover",
                "max_target_attempts": 2,
                "max_upstream_sends": 2,
            },
        }
    ],
}
path = pathlib.Path(sys.argv[1])
path.write_text(json.dumps(config, indent=2) + "\n", encoding="utf-8")
os.chmod(path, 0o600)
PY_CONFIG
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

proxy_log_has_address_in_use() {
  [[ -f "${PROXY_LOG}" ]] || return 1
  jq -R -s -e '
    [split("\n")[]
      | fromjson?
      | select(
          .level == "fatal"
          and (((.error // "") | ascii_downcase) | contains("address already in use"))
        )]
    | length > 0
  ' "${PROXY_LOG}" >/dev/null 2>&1
}

launch_proxy() {
  : > "${PROXY_LOG}"
  proxy_listen_confirmed=0
  set -m
  "${PROXY_BIN}" \
    --host "${PROXY_HOST}" \
    --port "${PROXY_PORT}" \
    --log-level info \
    --token-dir "${PROXY_TOKEN_DIR}" \
    --providers-config "${CONFIG_JSON}" \
    >"${PROXY_LOG}" 2>&1 &
  proxy_pid="$!"
  proxy_pgid="${proxy_pid}"
  set +m
}

stop_proxy_attempt() {
  terminate_process_group "${proxy_pid}" "${proxy_pgid}"
  proxy_pid=""
  proxy_pgid=""
  proxy_listen_confirmed=0
}

wait_for_ready() {
  local deadline=$((SECONDS + SMOKE_STARTUP_TIMEOUT_SECONDS))
  local status
  while (( SECONDS < deadline )); do
    if proxy_log_has_address_in_use; then
      return 2
    fi
    if ! process_is_running "${proxy_pid}"; then
      sleep 0.05
      if proxy_log_has_address_in_use; then
        return 2
      fi
      return 3
    fi
    if proxy_log_has_fatal; then
      return 3
    fi

    status="$(curl \
      --silent \
      --output /dev/null \
      --write-out '%{http_code}' \
      --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
      --max-time "${SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS}" \
      "${PROXY_BASE_URL}/readyz" 2>/dev/null || true)"
    if [[ "${status}" == "200" ]] && proxy_log_has_expected_listener; then
      proxy_listen_confirmed=1
      return 0
    fi
    sleep 0.25
  done
  return 4
}

start_proxy() {
  local max_attempts attempt wait_rc
  [[ -x "${PROXY_BIN}" ]] || die "proxy binary not found or not executable: ${PROXY_BIN} (run: make build)"

  if [[ "${PROXY_PORT_EXPLICIT}" == "1" ]]; then
    [[ "${PROXY_PORT}" =~ ^[0-9]+$ ]] || die "PROXY_PORT must be numeric: ${PROXY_PORT}"
    max_attempts=1
  else
    [[ "${SMOKE_PROXY_AUTO_PORT_MAX_ATTEMPTS}" =~ ^[1-9][0-9]*$ ]] || \
      die "SMOKE_PROXY_AUTO_PORT_MAX_ATTEMPTS must be a positive integer: ${SMOKE_PROXY_AUTO_PORT_MAX_ATTEMPTS}"
    max_attempts="${SMOKE_PROXY_AUTO_PORT_MAX_ATTEMPTS}"
  fi

  mkdir -p "${PROXY_TOKEN_DIR}"
  chmod 700 "${PROXY_TOKEN_DIR}"

  log "Validating generated schema-v2 provider config"
  "${PROXY_BIN}" config validate --providers-config "${CONFIG_JSON}" >/dev/null
  printf 'PASS schema-v2-config-validation\n' >> "${SUMMARY_FILE}"

  for ((attempt = 1; attempt <= max_attempts; attempt++)); do
    if [[ "${PROXY_PORT_EXPLICIT}" != "1" ]]; then
      PROXY_PORT="$(allocate_free_port "${PROXY_HOST}")"
    fi
    PROXY_BASE_URL="http://${PROXY_HOST}:${PROXY_PORT}"

    if [[ "${PROXY_PORT_EXPLICIT}" == "1" ]]; then
      log "Starting Vekil at ${PROXY_BASE_URL}"
    else
      log "Starting Vekil at ${PROXY_BASE_URL} (auto-port attempt ${attempt}/${max_attempts})"
    fi
    launch_proxy

    if wait_for_ready; then
      return 0
    else
      wait_rc=$?
    fi
    stop_proxy_attempt

    if [[ "${wait_rc}" -eq 2 ]]; then
      if [[ "${PROXY_PORT_EXPLICIT}" == "1" ]]; then
        die "proxy could not bind explicit PROXY_PORT ${PROXY_PORT}: address already in use"
      fi
      if (( attempt < max_attempts )); then
        log "Auto-selected proxy port ${PROXY_PORT} was claimed before startup; retrying with a new port"
        continue
      fi
      die "proxy auto-port startup exhausted ${max_attempts} attempts after address-in-use failures"
    fi
    if [[ "${wait_rc}" -eq 4 ]]; then
      die "proxy did not become ready within ${SMOKE_STARTUP_TIMEOUT_SECONDS}s; inspect ${PROXY_LOG}"
    fi
    die "proxy exited or logged a fatal startup error before readiness; inspect ${PROXY_LOG}"
  done
}

fetch_json() {
  local label="$1"
  local url="$2"
  local output="$3"
  if ! curl \
    --fail \
    --silent \
    --show-error \
    --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${SMOKE_CURL_MAX_TIME_SECONDS}" \
    --output "${output}" \
    "${url}"; then
    die "${label} request failed"
  fi
}

post_json() {
  local label="$1"
  local request_file="$2"
  local response_file="$3"
  local headers_file="$4"
  local status_file="$5"
  local status

  if ! status="$(curl \
    --silent \
    --show-error \
    --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${SMOKE_CURL_MAX_TIME_SECONDS}" \
    --header 'Content-Type: application/json' \
    --data-binary "@${request_file}" \
    --dump-header "${headers_file}" \
    --output "${response_file}" \
    --write-out '%{http_code}' \
    "${PROXY_BASE_URL}/v1/responses")"; then
    die "${label} request failed at the HTTP transport layer"
  fi
  printf '%s\n' "${status}" > "${status_file}"
  printf '%s\n' "${status}"
}

write_request() {
  local path="$1"
  local prompt="$2"
  local previous_response_id="${3:-}"
  if [[ -n "${previous_response_id}" ]]; then
    jq -n \
      --arg model "${PUBLIC_MODEL}" \
      --arg prompt "${prompt}" \
      --arg previous "${previous_response_id}" \
      '{model:$model,input:$prompt,previous_response_id:$previous,max_output_tokens:1024,stream:false}' \
      > "${path}"
  else
    jq -n \
      --arg model "${PUBLIC_MODEL}" \
      --arg prompt "${prompt}" \
      '{model:$model,input:$prompt,max_output_tokens:1024,stream:false}' \
      > "${path}"
  fi
}

assert_success_response() {
  local label="$1"
  local path="$2"
  jq -e --arg model "${PUBLIC_MODEL}" '
    .model == $model
    and ((.id // "") | type == "string" and length > 0)
    and ((.output // []) | type == "array")
    and ([.output[]?.content[]? | select(.type == "output_text") | .text] | join("") | length > 0)
  ' "${path}" >/dev/null || die "${label} response did not expose the public model and non-empty Responses output"
}

stats_counter() {
  local path="$1"
  local field="$2"
  jq -r --arg field "${field}" '.[$field] // 0' "${path}"
}

stats_target_attempts() {
  local path="$1"
  local target="$2"
  jq -r --arg route "${ROUTE_ID}" --arg target "${target}" '
    ([.by_target[]? | select(.route == $route and .target == $target) | .attempts] | add) // 0
  ' "${path}"
}

assert_delta() {
  local label="$1"
  local before="$2"
  local after="$3"
  local expected="$4"
  local actual=$((after - before))
  [[ "${actual}" -eq "${expected}" ]] || die "${label} delta = ${actual}, want ${expected} (before=${before}, after=${after})"
}

fetch_stats() {
  local label="$1"
  local path="${SMOKE_DIR}/${label}.stats.json"
  fetch_json "${label} stats" "${PROXY_BASE_URL}/stats.json" "${path}"
  printf '%s\n' "${path}"
}

set_shim_mode() {
  local mode="$1"
  local expect_previous_response_id="$2"
  local tmp="${SHIM_MODE_FILE}.tmp"
  case "${mode}" in
    forward|reject) ;;
    *) die "invalid primary control proxy mode: ${mode}" ;;
  esac
  case "${expect_previous_response_id}" in
    true|false) ;;
    *) die "invalid previous_response_id expectation: ${expect_previous_response_id}" ;;
  esac
  printf '{"mode":"%s","expect_previous_response_id":%s}\n' \
    "${mode}" "${expect_previous_response_id}" > "${tmp}"
  mv "${tmp}" "${SHIM_MODE_FILE}"
}

assert_reject_validation_guard() {
  local responses_path="/responses"
  local response="${SMOKE_DIR}/primary-shim-invalid.response.json"
  local status
  if [[ "${LIVE_PROVIDER_ROUTING_PRIMARY_TYPE}" == "azure-openai" ]]; then
    responses_path="/openai/v1/responses"
  fi

  set_shim_mode reject false
  status="$(curl \
    --silent \
    --show-error \
    --connect-timeout "${SMOKE_CURL_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS}" \
    --header 'Content-Type: application/json' \
    --data-binary '{}' \
    --output "${response}" \
    --write-out '%{http_code}' \
    "http://127.0.0.1:${SHIM_PORT}${responses_path}")" || die "primary control proxy validation probe failed"
  [[ "${status}" == "400" ]] || die "invalid primary control proxy probe status = ${status}, want 400 before rejection injection"
  jq -e '.error.type == "control_proxy_validation_error"' "${response}" >/dev/null || \
    die "invalid primary control proxy probe did not return a validation error"
  set_shim_mode forward false
}

assert_catalog_identity() {
  fetch_json "model catalog" "${PROXY_BASE_URL}/v1/models" "${MODELS_JSON}"
  jq -e \
    --arg model "${PUBLIC_MODEL}" \
    --arg primary "${LIVE_PROVIDER_ROUTING_PRIMARY_MODEL}" \
    --arg secondary "${LIVE_PROVIDER_ROUTING_SECONDARY_MODEL}" '
      (.data | type == "array" and length == 1)
      and ([.data[]? | select(.id == $model)] | length) == 1
      and ([.data[]? | select(.id == $model)][0].supported_endpoints == ["/responses"])
      and ([.data[]?.id] | index($primary) == null)
      and ([.data[]?.id] | index($secondary) == null)
    ' "${MODELS_JSON}" >/dev/null || die "model catalog did not expose exactly the public route identity"
  printf 'PASS public-model-catalog-identity\n' >> "${SUMMARY_FILE}"
}

run_healthy_primary_case() {
  local before="$1"
  local request="${SMOKE_DIR}/healthy-primary.request.json"
  local response="${SMOKE_DIR}/healthy-primary.response.json"
  local headers="${SMOKE_DIR}/healthy-primary.headers.txt"
  local status_file="${SMOKE_DIR}/healthy-primary.status"
  local status after

  write_request "${request}" "Reply with a short confirmation that the primary route is healthy."
  status="$(post_json healthy-primary "${request}" "${response}" "${headers}" "${status_file}")"
  [[ "${status}" == "200" ]] || die "healthy primary status = ${status}, want 200"
  assert_success_response healthy-primary "${response}"

  after="$(fetch_stats after-healthy-primary)"
  assert_delta "healthy primary upstream attempts" \
    "$(stats_counter "${before}" upstream_attempts)" \
    "$(stats_counter "${after}" upstream_attempts)" 1
  assert_delta "healthy primary target attempts" \
    "$(stats_target_attempts "${before}" "${PRIMARY_TARGET_ID}")" \
    "$(stats_target_attempts "${after}" "${PRIMARY_TARGET_ID}")" 1
  assert_delta "healthy secondary target attempts" \
    "$(stats_target_attempts "${before}" "${SECONDARY_TARGET_ID}")" \
    "$(stats_target_attempts "${after}" "${SECONDARY_TARGET_ID}")" 0
  assert_delta "healthy target switches" \
    "$(stats_counter "${before}" target_switches)" \
    "$(stats_counter "${after}" target_switches)" 0

  jq -r '.id' "${response}" > "${SMOKE_DIR}/healthy-primary.response-id.txt"
  printf 'PASS healthy-primary-real-target\n' >> "${SUMMARY_FILE}"
  printf '%s\n' "${after}"
}

run_failover_case() {
  local before="$1"
  local request="${SMOKE_DIR}/failover.request.json"
  local response="${SMOKE_DIR}/failover.response.json"
  local headers="${SMOKE_DIR}/failover.headers.txt"
  local status_file="${SMOKE_DIR}/failover.status"
  local status after

  set_shim_mode reject false
  write_request "${request}" "Reply with a short confirmation that the secondary route is healthy."
  status="$(post_json failover "${request}" "${response}" "${headers}" "${status_file}")"
  [[ "${status}" == "200" ]] || die "failover status = ${status}, want 200"
  assert_success_response failover "${response}"

  after="$(fetch_stats after-failover)"
  assert_delta "failover upstream attempts" \
    "$(stats_counter "${before}" upstream_attempts)" \
    "$(stats_counter "${after}" upstream_attempts)" 2
  assert_delta "failover primary target attempts" \
    "$(stats_target_attempts "${before}" "${PRIMARY_TARGET_ID}")" \
    "$(stats_target_attempts "${after}" "${PRIMARY_TARGET_ID}")" 1
  assert_delta "failover secondary target attempts" \
    "$(stats_target_attempts "${before}" "${SECONDARY_TARGET_ID}")" \
    "$(stats_target_attempts "${after}" "${SECONDARY_TARGET_ID}")" 1
  assert_delta "failover target switches" \
    "$(stats_counter "${before}" target_switches)" \
    "$(stats_counter "${after}" target_switches)" 1
  assert_delta "requests with failover" \
    "$(stats_counter "${before}" requests_with_failover)" \
    "$(stats_counter "${after}" requests_with_failover)" 1
  assert_delta "successful failovers" \
    "$(stats_counter "${before}" successful_failovers)" \
    "$(stats_counter "${after}" successful_failovers)" 1

  jq -e --arg route "${ROUTE_ID}" --arg target "${SECONDARY_TARGET_ID}" '
    [.recent[]?
      | select(
          .route_id == $route
          and .final_target == $target
          and .status == 200
          and .upstream_sends == 2
          and .target_switches == 1
        )]
    | length >= 1
  ' "${after}" >/dev/null || die "failover attempt topology was not visible in the recent request ledger"

  printf 'PASS safe-primary-429-failover\n' >> "${SUMMARY_FILE}"
  printf 'PASS route-attempt-observability\n' >> "${SUMMARY_FILE}"
  printf '%s\n' "${after}"
}

run_pinned_primary_case() {
  local before="$1"
  local response_id
  local request="${SMOKE_DIR}/pinned-primary.request.json"
  local response="${SMOKE_DIR}/pinned-primary.response.json"
  local headers="${SMOKE_DIR}/pinned-primary.headers.txt"
  local status_file="${SMOKE_DIR}/pinned-primary.status"
  local status after

  response_id="$(cat "${SMOKE_DIR}/healthy-primary.response-id.txt")"
  [[ -n "${response_id}" && "${response_id}" != "null" ]] || die "healthy primary response did not provide a bindable response id"
  set_shim_mode reject true
  write_request "${request}" "This request must remain pinned to the primary target." "${response_id}"
  status="$(post_json pinned-primary "${request}" "${response}" "${headers}" "${status_file}")"
  [[ "${status}" == "429" ]] || die "state-pinned primary status = ${status}, want injected 429"
  jq -e '.error.message | contains("injected retryable primary rejection")' "${response}" >/dev/null || \
    die "state-pinned primary request did not return the injected primary rejection"

  after="$(fetch_stats after-pinned-primary)"
  assert_delta "pinned primary upstream attempts" \
    "$(stats_counter "${before}" upstream_attempts)" \
    "$(stats_counter "${after}" upstream_attempts)" 1
  assert_delta "pinned primary target attempts" \
    "$(stats_target_attempts "${before}" "${PRIMARY_TARGET_ID}")" \
    "$(stats_target_attempts "${after}" "${PRIMARY_TARGET_ID}")" 1
  assert_delta "pinned secondary target attempts" \
    "$(stats_target_attempts "${before}" "${SECONDARY_TARGET_ID}")" \
    "$(stats_target_attempts "${after}" "${SECONDARY_TARGET_ID}")" 0
  assert_delta "pinned target switches" \
    "$(stats_counter "${before}" target_switches)" \
    "$(stats_counter "${after}" target_switches)" 0
  assert_delta "state binding hits" \
    "$(stats_counter "${before}" state_binding_hits)" \
    "$(stats_counter "${after}" state_binding_hits)" 1

  printf 'PASS exact-state-primary-pinning\n' >> "${SUMMARY_FILE}"
  printf '%s\n' "${after}"
}

run_unknown_state_case() {
  local before="$1"
  local request="${SMOKE_DIR}/unknown-state.request.json"
  local response="${SMOKE_DIR}/unknown-state.response.json"
  local headers="${SMOKE_DIR}/unknown-state.headers.txt"
  local status_file="${SMOKE_DIR}/unknown-state.status"
  local status after

  write_request "${request}" "This request must fail closed before dispatch." "resp_live_provider_routing_unknown_state"
  status="$(post_json unknown-state "${request}" "${response}" "${headers}" "${status_file}")"
  [[ "${status}" == "400" ]] || die "unknown-state status = ${status}, want 400"
  jq -e '.error.message | contains("unknown provider-bound state")' "${response}" >/dev/null || \
    die "unknown provider state did not produce the expected fail-closed error"

  after="$(fetch_stats after-unknown-state)"
  assert_delta "unknown-state upstream attempts" \
    "$(stats_counter "${before}" upstream_attempts)" \
    "$(stats_counter "${after}" upstream_attempts)" 0
  assert_delta "unknown-state primary attempts" \
    "$(stats_target_attempts "${before}" "${PRIMARY_TARGET_ID}")" \
    "$(stats_target_attempts "${after}" "${PRIMARY_TARGET_ID}")" 0
  assert_delta "unknown-state secondary attempts" \
    "$(stats_target_attempts "${before}" "${SECONDARY_TARGET_ID}")" \
    "$(stats_target_attempts "${after}" "${SECONDARY_TARGET_ID}")" 0
  assert_delta "state binding misses" \
    "$(stats_counter "${before}" state_binding_misses)" \
    "$(stats_counter "${after}" state_binding_misses)" 1

  printf 'PASS unknown-state-fail-closed\n' >> "${SUMMARY_FILE}"
  printf '%s\n' "${after}"
}

control_proxy_counts_match() {
  local responses_path="/responses"
  if [[ "${LIVE_PROVIDER_ROUTING_PRIMARY_TYPE}" == "azure-openai" ]]; then
    responses_path="/openai/v1/responses"
  fi
  jq -s -e --arg responses_path "${responses_path}" '
    [.[]
      | select(
          .event == "request"
          and .request_kind == "responses"
          and .method == "POST"
          and .path == $responses_path
          and .valid_responses_post == true
        )] as $valid_responses_posts
    | ([$valid_responses_posts[]
          | select(
              .mode == "forward"
              and .status == 200
              and .expected_previous_response_id_present == false
              and .previous_response_id_present == false
            )]
        | length) == 1
    and ([$valid_responses_posts[]
          | select(
              .mode == "reject"
              and .status == 429
              and .expected_previous_response_id_present == false
              and .previous_response_id_present == false
            )]
        | length) == 1
    and ([$valid_responses_posts[]
          | select(
              .mode == "reject"
              and .status == 429
              and .expected_previous_response_id_present == true
              and .previous_response_id_present == true
            )]
        | length) == 1
    and ([.[]
          | select(
              .event == "request"
              and .request_kind == "responses"
              and .mode == "reject"
              and .status == 400
              and .valid_responses_post != true
            )]
        | length) == 1
    and ([.[] | select(.event == "request" and .request_kind == "responses" and .status == 429 and .valid_responses_post != true)] | length) == 0
    and ([.[] | select(.event == "request" and .request_kind == "models_discovery")] | all(.valid_models_get == true))
  ' "${SHIM_LOG}" >/dev/null 2>&1
}

assert_control_proxy_counts() {
  local deadline=$((SECONDS + 5))
  while (( SECONDS < deadline )); do
    if control_proxy_counts_match; then
      printf 'PASS primary-control-proxy-attempts\n' >> "${SUMMARY_FILE}"
      return
    fi
    if ! process_is_running "${shim_pid}"; then
      break
    fi
    sleep 0.1
  done
  die "primary control proxy did not observe one valid healthy Responses POST and two valid injected Responses rejections"
}

main() {
  require_cmd curl
  require_cmd jq
  require_cmd ps
  python_command >/dev/null
  validate_inputs

  : > "${SUMMARY_FILE}"
  chmod 600 "${SUMMARY_FILE}"

  start_primary_shim
  assert_reject_validation_guard
  generate_config
  start_proxy
  assert_catalog_identity

  local initial after_healthy after_failover after_pinned final_stats
  initial="$(fetch_stats initial)"
  after_healthy="$(run_healthy_primary_case "${initial}")"
  after_failover="$(run_failover_case "${after_healthy}")"
  after_pinned="$(run_pinned_primary_case "${after_failover}")"
  final_stats="$(run_unknown_state_case "${after_pinned}")"
  assert_control_proxy_counts

  assert_delta "total upstream attempts" \
    "$(stats_counter "${initial}" upstream_attempts)" \
    "$(stats_counter "${final_stats}" upstream_attempts)" 4
  assert_delta "total primary target attempts" \
    "$(stats_target_attempts "${initial}" "${PRIMARY_TARGET_ID}")" \
    "$(stats_target_attempts "${final_stats}" "${PRIMARY_TARGET_ID}")" 3
  assert_delta "total secondary target attempts" \
    "$(stats_target_attempts "${initial}" "${SECONDARY_TARGET_ID}")" \
    "$(stats_target_attempts "${final_stats}" "${SECONDARY_TARGET_ID}")" 1

  log "Live provider-routing smoke passed."
  while IFS= read -r line; do
    log "${line}"
  done < "${SUMMARY_FILE}"
  log "Artifacts: ${SMOKE_DIR}"
}

main "$@"
