#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
SMOKE_SCRIPT="${REPO_ROOT}/scripts/live-provider-routing-smoke.sh"
EXAMPLE_CONFIG="${REPO_ROOT}/examples/provider-routing-failover.yaml"
WORKFLOW_FILE="${REPO_ROOT}/.github/workflows/live-provider-routing-smoke.yaml"
TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/vekil-provider-routing-smoke-test.XXXXXX")"
umask 077

MOCK_SERVER_PIDS=()
MOCK_SERVER_PORTS=()
MOCK_SERVER_PID=""
MOCK_SERVER_PORT=""
TEST_PROXY_BIN="${TMP_ROOT}/vekil"
TEST_PROXY_WRAPPER="${TMP_ROOT}/vekil-startup-wrapper.sh"
PROXY_WRAPPER_ATTEMPTS="${TMP_ROOT}/proxy-wrapper-attempts.txt"
PROXY_WRAPPER_COLLISION_MARKER="${TMP_ROOT}/proxy-wrapper-first-collision"
SMOKE_DIR="${TMP_ROOT}/smoke"
HARNESS_STDOUT="${TMP_ROOT}/harness.stdout"
HARNESS_STDERR="${TMP_ROOT}/harness.stderr"

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

redact() {
  sed -E \
    -e 's/(Authorization: (Bearer|token) )[[:graph:]]+/\1[REDACTED]/Ig' \
    -e 's/(api-key: )[[:graph:]]+/\1[REDACTED]/Ig' \
    -e 's/("encrypted_content"[[:space:]]*:[[:space:]]*")[^"]*(")/\1[REDACTED]\2/g' \
    -e 's/("previous_response_id"[[:space:]]*:[[:space:]]*")[^"]*(")/\1[REDACTED]\2/g' \
    -e 's/("id"[[:space:]]*:[[:space:]]*"resp_)[^"]*(")/\1[REDACTED]\2/g'
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
  for ((attempt = 0; attempt < 100; attempt++)); do
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

stop_mock_targets() {
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
  local file redacted index=0
  for file in \
    "${HARNESS_STDOUT}" \
    "${HARNESS_STDERR}" \
    "${SMOKE_DIR}/summary.txt" \
    "${SMOKE_DIR}/proxy.log" \
    "${SMOKE_DIR}/primary-shim.log" \
    "${TMP_ROOT}/primary/state.json" \
    "${TMP_ROOT}/secondary/state.json"; do
    if [[ -f "${file}" ]]; then
      printf '%s\n' "--- ${file} ---" >&2
      redacted="${TMP_ROOT}/diagnostic-${index}.redacted"
      redact < "${file}" > "${redacted}" || true
      head -c 32768 "${redacted}" >&2 || true
      rm -f "${redacted}"
      printf '\n' >&2
      index=$((index + 1))
    fi
  done
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  stop_mock_targets
  if [[ "${rc}" -ne 0 ]]; then
    dump_diagnostics
  fi
  if [[ "${KEEP_LIVE_PROVIDER_ROUTING_TEST_ARTIFACTS:-0}" == "1" ]]; then
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
  cat > "${TMP_ROOT}/mock_responses_server.py" <<'PY_SERVER'
import argparse
import json
import os
import pathlib
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

parser = argparse.ArgumentParser()
parser.add_argument("--host", default="127.0.0.1")
parser.add_argument("--port", type=int, default=0)
parser.add_argument("--port-file", required=True)
parser.add_argument("--state-file", required=True)
parser.add_argument("--role", choices=("primary", "secondary"), required=True)
parser.add_argument("--expected-path", required=True)
parser.add_argument("--expected-model", required=True)
parser.add_argument("--expected-auth-header", choices=("api-key", "authorization"), required=True)
parser.add_argument("--expected-auth-value", required=True)
args = parser.parse_args()

state_path = pathlib.Path(args.state_file)
state_lock = threading.Lock()
state = {
    "role": args.role,
    "expected_path": args.expected_path,
    "expected_model": args.expected_model,
    "requests": [],
    "errors": [],
}


def persist():
    temporary = state_path.with_suffix(state_path.suffix + ".tmp")
    temporary.write_text(json.dumps(state, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    os.replace(temporary, state_path)


def record_request(entry, validation_errors):
    with state_lock:
        state["requests"].append(entry)
        state["errors"].extend(validation_errors)
        persist()
        return len(state["requests"])


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, _format, *_args):
        return

    def send_json(self, status, payload):
        data = json.dumps(payload, separators=(",", ":")).encode("utf-8")
        self.send_response(status)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(data)))
        self.send_header("connection", "close")
        self.end_headers()
        self.wfile.write(data)

    def auth_validation(self):
        authorization = self.headers.get("authorization", "")
        api_key = self.headers.get("api-key", "")
        errors = []
        if args.expected_auth_header == "api-key":
            if api_key != args.expected_auth_value:
                errors.append("api-key header did not contain the configured primary credential")
            if authorization:
                errors.append("Azure-shaped primary unexpectedly received Authorization")
        else:
            if authorization != args.expected_auth_value:
                errors.append("Authorization header did not contain the configured bearer credential")
            if api_key:
                errors.append("generic secondary unexpectedly received api-key")
        return authorization, api_key, errors

    def do_GET(self):
        models_path = args.expected_path.removesuffix("/responses") + "/models"
        authorization, api_key, errors = self.auth_validation()
        if self.path == models_path:
            entry = {
                "method": "GET",
                "path": self.path,
                "metadata_probe": True,
                "authorization_present": bool(authorization),
                "api_key_present": bool(api_key),
                "auth_valid": not errors,
                "response_status": 200 if not errors else 400,
                "validation_errors": errors,
            }
            record_request(entry, errors)
            if errors:
                self.send_json(400, {"error": {"message": "invalid model-discovery authentication"}})
                return
            self.send_json(
                200,
                {
                    "object": "list",
                    "data": [
                        {
                            "id": args.expected_model,
                            "object": "model",
                            "created": 1700000000,
                            "owned_by": "controlled-local-target",
                            "supported_endpoints": ["/responses"],
                        }
                    ],
                },
            )
            return

        errors.append(f"unexpected GET {self.path}")
        record_request({"method": "GET", "path": self.path, "validation_errors": errors}, errors)
        self.send_json(404, {"error": {"message": errors[0], "type": "mock_validation_error"}})

    def do_POST(self):
        try:
            length = int(self.headers.get("content-length", "0"))
        except ValueError:
            length = 0
        raw = self.rfile.read(length) if length else b""
        errors = []
        try:
            body = json.loads(raw)
        except (UnicodeDecodeError, json.JSONDecodeError) as error:
            body = None
            errors.append(f"invalid JSON body: {type(error).__name__}")

        authorization, api_key, auth_errors = self.auth_validation()
        errors.extend(auth_errors)
        if self.path != args.expected_path:
            errors.append(f"path {self.path!r}, want {args.expected_path!r}")

        content_type = self.headers.get("content-type", "")
        if not content_type.lower().startswith("application/json"):
            errors.append(f"content-type {content_type!r}, want application/json")

        if not isinstance(body, dict):
            errors.append("request body must be a JSON object")
        else:
            if body.get("model") != args.expected_model:
                errors.append(f"model {body.get('model')!r}, want {args.expected_model!r}")
            if body.get("stream") is not False:
                errors.append(f"stream {body.get('stream')!r}, want false")
            if body.get("max_output_tokens") != 1024:
                errors.append(f"max_output_tokens {body.get('max_output_tokens')!r}, want 1024")
            if not isinstance(body.get("input"), str) or not body["input"].strip():
                errors.append("input must be a non-empty string")
            if "previous_response_id" in body:
                errors.append("provider target unexpectedly received previous_response_id")

        entry = {
            "method": "POST",
            "path": self.path,
            "content_type": content_type,
            "authorization_present": bool(authorization),
            "api_key_present": bool(api_key),
            "auth_valid": not auth_errors,
            "body": body,
            "validation_errors": errors,
        }
        sequence = record_request(entry, errors)
        if errors:
            self.send_json(
                400,
                {
                    "error": {
                        "message": "; ".join(errors),
                        "type": "mock_validation_error",
                    }
                },
            )
            return

        response_id = f"resp_local_{args.role}_{sequence}"
        text = f"{args.role} controlled target completed the Responses request"
        self.send_json(
            200,
            {
                "id": response_id,
                "object": "response",
                "created_at": 1700000000 + sequence,
                "status": "completed",
                "error": None,
                "incomplete_details": None,
                "model": args.expected_model,
                "output": [
                    {
                        "id": f"msg_local_{args.role}_{sequence}",
                        "type": "message",
                        "status": "completed",
                        "role": "assistant",
                        "content": [
                            {
                                "type": "output_text",
                                "text": text,
                                "annotations": [],
                            }
                        ],
                    }
                ],
                "parallel_tool_calls": True,
                "usage": {
                    "input_tokens": 8,
                    "output_tokens": 6,
                    "total_tokens": 14,
                },
            },
        )


state_path.parent.mkdir(parents=True, exist_ok=True)
persist()
server = ThreadingHTTPServer((args.host, args.port), Handler)
port_path = pathlib.Path(args.port_file)
port_path.write_text(str(server.server_port), encoding="utf-8")
os.chmod(port_path, 0o600)
server.serve_forever(poll_interval=0.1)
PY_SERVER
  chmod 700 "${TMP_ROOT}/mock_responses_server.py"
}

write_proxy_wrapper() {
  cat > "${TEST_PROXY_WRAPPER}" <<'SH_WRAPPER'
#!/usr/bin/env bash
set -euo pipefail

: "${REAL_PROXY_BIN:?}"
: "${PROXY_WRAPPER_ATTEMPTS:?}"
: "${PROXY_WRAPPER_COLLISION_MARKER:?}"

if [[ "${1:-}" == "config" ]]; then
  exec "${REAL_PROXY_BIN}" "$@"
fi

host=""
port=""
args=("$@")
for ((index = 0; index < ${#args[@]}; index++)); do
  case "${args[index]}" in
    --host) host="${args[index + 1]}" ;;
    --port) port="${args[index + 1]}" ;;
  esac
done
[[ -n "${host}" && -n "${port}" ]]
printf '%s\n' "${port}" >> "${PROXY_WRAPPER_ATTEMPTS}"

if (set -o noclobber; : > "${PROXY_WRAPPER_COLLISION_MARKER}") 2>/dev/null; then
  exec python3 - "${REAL_PROXY_BIN}" "${host}" "${port}" "$@" <<'PY_COLLISION'
import socket
import subprocess
import sys

real_binary, host, port, *arguments = sys.argv[1:]
family = socket.AF_INET6 if ":" in host else socket.AF_INET
with socket.socket(family, socket.SOCK_STREAM) as listener:
    listener.bind((host, int(port)))
    listener.listen(1)
    raise SystemExit(subprocess.run([real_binary, *arguments], check=False).returncode)
PY_COLLISION
fi

exec "${REAL_PROXY_BIN}" "$@"
SH_WRAPPER
  chmod 700 "${TEST_PROXY_WRAPPER}"
}

start_mock_target() {
  local role="$1"
  local expected_path="$2"
  local expected_model="$3"
  local expected_auth_header="$4"
  local expected_auth_value="$5"
  local target_dir="${TMP_ROOT}/${role}"
  local port_file="${target_dir}/port"

  mkdir -p "${target_dir}"
  python3 "${TMP_ROOT}/mock_responses_server.py" \
    --port-file "${port_file}" \
    --state-file "${target_dir}/state.json" \
    --role "${role}" \
    --expected-path "${expected_path}" \
    --expected-model "${expected_model}" \
    --expected-auth-header "${expected_auth_header}" \
    --expected-auth-value "${expected_auth_value}" \
    >"${target_dir}/server.log" 2>&1 &
  MOCK_SERVER_PID=$!
  MOCK_SERVER_PIDS+=("${MOCK_SERVER_PID}")

  if ! wait_for_file "${port_file}"; then
    fail "${role} mock did not publish a listening port"
  fi
  MOCK_SERVER_PORT="$(cat "${port_file}")"
  [[ "${MOCK_SERVER_PORT}" =~ ^[0-9]+$ ]] || fail "${role} mock published invalid port ${MOCK_SERVER_PORT}"
  MOCK_SERVER_PORTS+=("${MOCK_SERVER_PORT}")
  if ! port_accepts_tcp "${MOCK_SERVER_PORT}"; then
    fail "${role} mock is not accepting TCP on 127.0.0.1:${MOCK_SERVER_PORT}"
  fi
}

assert_mock_state() {
  local role="$1"
  local expected_path="$2"
  local expected_model="$3"
  local expected_prompt="$4"
  local expected_auth_kind="$5"
  local expected_probe_count="$6"
  local expected_models_path="${expected_path%/responses}/models"
  local state_file="${TMP_ROOT}/${role}/state.json"

  jq -e \
    --arg role "${role}" \
    --arg path "${expected_path}" \
    --arg models_path "${expected_models_path}" \
    --arg model "${expected_model}" \
    --arg prompt "${expected_prompt}" \
    --arg auth_kind "${expected_auth_kind}" \
    --argjson probe_count "${expected_probe_count}" '
      (.requests | map(select(.method == "POST"))) as $posts
      | (.requests | map(select(.method == "GET"))) as $gets
      | .role == $role
      and .errors == []
      and ($posts | length == 1)
      and ($gets | length == $probe_count)
      and (
        $probe_count == 0
        or ($gets | all(
          .path == $models_path
          and .metadata_probe == true
          and .auth_valid == true
          and .response_status == 200
          and .validation_errors == []
        ))
      )
      and $posts[0].path == $path
      and $posts[0].auth_valid == true
      and $posts[0].validation_errors == []
      and $posts[0].body.model == $model
      and $posts[0].body.stream == false
      and $posts[0].body.max_output_tokens == 1024
      and ($posts[0].body.input | contains($prompt))
      and (($posts[0].body | has("previous_response_id")) | not)
      and (
        ($auth_kind == "api-key" and $posts[0].api_key_present == true and $posts[0].authorization_present == false)
        or
        ($auth_kind == "authorization" and $posts[0].authorization_present == true and $posts[0].api_key_present == false)
      )
    ' "${state_file}" >/dev/null || fail "${role} mock did not observe the expected routed request"
}

assert_control_proxy_log() {
  local primary_key="$1"
  local response_id
  response_id="$(cat "${SMOKE_DIR}/healthy-primary.response-id.txt")"

  jq -s -e '
    [.[] | select(.event == "request" and .request_kind == "responses" and .valid_responses_post == true)] as $posts
    | ([$posts[] | select(.mode == "forward" and .status == 200)] | length) == 1
    and ([$posts[] | select(.mode == "reject" and .status == 429)] | length) == 2
    and ([$posts[] | select(.mode == "reject" and .expected_previous_response_id_present == false and .previous_response_id_present == false)] | length) == 1
    and ([$posts[] | select(.mode == "reject" and .expected_previous_response_id_present == true and .previous_response_id_present == true)] | length) == 1
    and ($posts | all(
      .method == "POST"
      and .path == "/openai/v1/responses"
      and .auth_valid == true
      and .model_valid == true
      and .request_shape_valid == true
      and .validation_errors == []
      and (has("body") | not)
      and (has("previous_response_id") | not)
    ))
    and ([.[]
      | select(
          .request_kind == "responses"
          and .mode == "reject"
          and .status == 400
          and .valid_responses_post == false
          and (.validation_errors | index("auth_invalid") != null)
          and (.validation_errors | index("model_invalid") != null)
        )]
      | length) == 1
    and ([.[] | select(.request_kind == "responses" and .status == 429 and .valid_responses_post != true)] | length) == 0
    and ([.[] | select(.request_kind == "models_discovery" and .status == 200 and .valid_models_get == true)] | length) >= 1
  ' "${SMOKE_DIR}/primary-shim.log" >/dev/null || fail "primary control proxy log did not contain only validated Responses attempts"

  if grep -Fq -- "${primary_key}" "${SMOKE_DIR}/primary-shim.log"; then
    fail "primary control proxy log exposed the configured credential"
  fi
  if grep -Fq -- "${response_id}" "${SMOKE_DIR}/primary-shim.log"; then
    fail "primary control proxy log exposed the full previous_response_id"
  fi
}

assert_redaction_before_truncation() {
  local fixture="${TMP_ROOT}/redaction-fixture.txt"
  local unsafe="${TMP_ROOT}/redaction-unsafe.txt"
  local redacted="${TMP_ROOT}/redaction-full.txt"
  local safe="${TMP_ROOT}/redaction-safe.txt"
  local secret redact_line head_line
  local redact_pattern="redact < \"\$file\" > \"\$redacted\""
  local head_pattern="head -c 32768 \"\$redacted\""
  secret="resp_sensitive_cutoff_value_$(printf 'x%.0s' {1..512})"

  python3 - "${fixture}" "${secret}" <<'PY_REDACTION'
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
secret = sys.argv[2]
path.write_text("p" * 32600 + '\n{"previous_response_id":"' + secret + '"}\n', encoding="utf-8")
PY_REDACTION
  head -c 32768 "${fixture}" > "${unsafe}"
  grep -Fq 'resp_sensitive_cutoff_value_' "${unsafe}" || fail "redaction cutoff fixture did not cross the truncation boundary"

  redact < "${fixture}" > "${redacted}"
  head -c 32768 "${redacted}" > "${safe}"
  grep -Fq '[REDACTED]' "${safe}" || fail "redacted diagnostic did not retain a redaction marker before truncation"
  if grep -Fq 'resp_sensitive_cutoff_value_' "${safe}"; then
    fail "diagnostic truncation exposed an unterminated sensitive value"
  fi

  redact_line="$(grep -nF "${redact_pattern}" "${WORKFLOW_FILE}" | cut -d: -f1)"
  head_line="$(grep -nF "${head_pattern}" "${WORKFLOW_FILE}" | cut -d: -f1)"
  [[ -n "${redact_line}" && -n "${head_line}" && "${redact_line}" -lt "${head_line}" ]] || \
    fail "workflow diagnostics must fully redact each artifact before truncation"
}

main() {
  require_cmd curl
  require_cmd diff
  require_cmd go
  require_cmd jq
  require_cmd ps
  require_cmd python3

  [[ -x "${SMOKE_SCRIPT}" ]] || fail "live provider-routing harness is missing or not executable: ${SMOKE_SCRIPT}"
  [[ -f "${EXAMPLE_CONFIG}" ]] || fail "provider-routing example config is missing: ${EXAMPLE_CONFIG}"
  [[ -f "${WORKFLOW_FILE}" ]] || fail "live provider-routing workflow is missing: ${WORKFLOW_FILE}"
  assert_redaction_before_truncation

  log "Building a real Vekil binary for the process-level smoke"
  (cd "${REPO_ROOT}" && go build -o "${TEST_PROXY_BIN}" .)
  [[ -x "${TEST_PROXY_BIN}" ]] || fail "built Vekil binary is not executable: ${TEST_PROXY_BIN}"

  log "Validating the checked-in provider-routing example with dummy API keys"
  AZURE_PRIMARY_API_KEY=local-example-primary-key \
  AZURE_SECONDARY_API_KEY=local-example-secondary-key \
    "${TEST_PROXY_BIN}" config validate --providers-config "${EXAMPLE_CONFIG}" >/dev/null

  write_mock_server
  write_proxy_wrapper

  local primary_model="local-azure-deployment"
  local secondary_model="local-openai-model"
  local public_model="vekil-local-routed-model"
  local route_id="ci-provider-routing"
  local primary_key="local-primary-api-key"
  local secondary_key="local-secondary-api-key"
  local primary_port secondary_port

  log "Starting two controlled local Responses-compatible targets"
  start_mock_target primary "/openai/v1/responses" "${primary_model}" api-key "${primary_key}"
  primary_port="${MOCK_SERVER_PORT}"
  start_mock_target secondary "/v1/responses" "${secondary_model}" authorization "Bearer ${secondary_key}"
  secondary_port="${MOCK_SERVER_PORT}"

  log "Running the live provider-routing harness against the local targets"
  if ! env \
    PROXY_BIN="${TEST_PROXY_WRAPPER}" \
    REAL_PROXY_BIN="${TEST_PROXY_BIN}" \
    PROXY_WRAPPER_ATTEMPTS="${PROXY_WRAPPER_ATTEMPTS}" \
    PROXY_WRAPPER_COLLISION_MARKER="${PROXY_WRAPPER_COLLISION_MARKER}" \
    PROXY_HOST=127.0.0.1 \
    LIVE_PROVIDER_ROUTING_ALLOW_INSECURE_HTTP=1 \
    LIVE_PROVIDER_ROUTING_PRIMARY_TYPE=azure-openai \
    LIVE_PROVIDER_ROUTING_PRIMARY_BASE_URL="http://127.0.0.1:${primary_port}/openai/v1" \
    LIVE_PROVIDER_ROUTING_PRIMARY_API_KEY="${primary_key}" \
    LIVE_PROVIDER_ROUTING_PRIMARY_MODEL="${primary_model}" \
    LIVE_PROVIDER_ROUTING_SECONDARY_TYPE=openai-compatible \
    LIVE_PROVIDER_ROUTING_SECONDARY_BASE_URL="http://127.0.0.1:${secondary_port}/v1" \
    LIVE_PROVIDER_ROUTING_SECONDARY_API_KEY="${secondary_key}" \
    LIVE_PROVIDER_ROUTING_SECONDARY_MODEL="${secondary_model}" \
    LIVE_PROVIDER_ROUTING_PUBLIC_MODEL="${public_model}" \
    LIVE_PROVIDER_ROUTING_ROUTE_ID="${route_id}" \
    LIVE_PROVIDER_ROUTING_SMOKE_DIR="${SMOKE_DIR}" \
    SMOKE_STARTUP_TIMEOUT_SECONDS=10 \
    SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1 \
    SMOKE_CURL_MAX_TIME_SECONDS=10 \
    SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS=1 \
    SMOKE_PROCESS_TERM_GRACE_SECONDS=2 \
    SMOKE_PORT_RELEASE_TIMEOUT_SECONDS=3 \
    SMOKE_SHIM_UPSTREAM_TIMEOUT_SECONDS=5 \
    "${SMOKE_SCRIPT}" >"${HARNESS_STDOUT}" 2>"${HARNESS_STDERR}"; then
    fail "live provider-routing harness failed against controlled local targets"
  fi

  [[ "$(wc -l < "${PROXY_WRAPPER_ATTEMPTS}" | tr -d ' ')" == "2" ]] || \
    fail "auto-port startup did not retry exactly once after the injected address-in-use failure"
  [[ "$(sed -n '1p' "${PROXY_WRAPPER_ATTEMPTS}")" != "$(sed -n '2p' "${PROXY_WRAPPER_ATTEMPTS}")" ]] || \
    fail "auto-port retry reused the collided proxy port"

  cat > "${TMP_ROOT}/expected-summary.txt" <<'EOF_SUMMARY'
PASS schema-v2-config-validation
PASS public-model-catalog-identity
PASS healthy-primary-real-target
PASS safe-primary-429-failover
PASS route-attempt-observability
PASS exact-state-primary-pinning
PASS unknown-state-fail-closed
PASS primary-control-proxy-attempts
EOF_SUMMARY

  if ! diff -u "${TMP_ROOT}/expected-summary.txt" "${SMOKE_DIR}/summary.txt"; then
    fail "live provider-routing harness did not complete every self-check exactly once"
  fi

  jq -e \
    --arg public_model "${public_model}" \
    --arg route_id "${route_id}" \
    --arg primary_model "${primary_model}" \
    --arg secondary_model "${secondary_model}" '
      .schema_version == 2
      and (.providers | length == 2)
      and .providers[0].type == "azure-openai"
      and .providers[0].default == true
      and (.providers[0].base_url | startswith("http://127.0.0.1:"))
      and (.providers[0].base_url | endswith("/openai/v1"))
      and .providers[1].type == "openai-compatible"
      and .providers[1].auth_type == "bearer"
      and .providers[1].model_discovery == "static"
      and (.model_routes | length == 1)
      and .model_routes[0].id == $route_id
      and .model_routes[0].public_id == $public_model
      and .model_routes[0].endpoints == ["/responses"]
      and .model_routes[0].targets == [
        {"id":"primary","provider":"live-primary","upstream_model":$primary_model},
        {"id":"secondary","provider":"live-secondary","upstream_model":$secondary_model}
      ]
      and .model_routes[0].routing == {
        "mode":"priority_failover",
        "max_target_attempts":2,
        "max_upstream_sends":2
      }
    ' "${SMOKE_DIR}/providers.json" >/dev/null || fail "harness did not generate the expected schema-v2 route"

  assert_mock_state primary "/openai/v1/responses" "${primary_model}" "primary route is healthy" api-key 1
  assert_mock_state secondary "/v1/responses" "${secondary_model}" "secondary route is healthy" authorization 0
  assert_control_proxy_log "${primary_key}"

  local holder_port_file="${TMP_ROOT}/explicit-port-holder.port"
  local holder_pid holder_port attempts_before attempts_after
  python3 - "${holder_port_file}" <<'PY_HOLDER' &
import pathlib
import socket
import sys

with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
    listener.bind(("127.0.0.1", 0))
    listener.listen(8)
    pathlib.Path(sys.argv[1]).write_text(str(listener.getsockname()[1]), encoding="utf-8")
    while True:
        connection, _ = listener.accept()
        connection.close()
PY_HOLDER
  holder_pid=$!
  MOCK_SERVER_PIDS+=("${holder_pid}")
  wait_for_file "${holder_port_file}" || fail "explicit-port holder did not publish its port"
  holder_port="$(cat "${holder_port_file}")"
  MOCK_SERVER_PORTS+=("${holder_port}")
  attempts_before="$(wc -l < "${PROXY_WRAPPER_ATTEMPTS}" | tr -d ' ')"

  log "Verifying explicit PROXY_PORT fails without auto-port retry or external-listener cleanup"
  if env \
    PROXY_BIN="${TEST_PROXY_WRAPPER}" \
    REAL_PROXY_BIN="${TEST_PROXY_BIN}" \
    PROXY_WRAPPER_ATTEMPTS="${PROXY_WRAPPER_ATTEMPTS}" \
    PROXY_WRAPPER_COLLISION_MARKER="${PROXY_WRAPPER_COLLISION_MARKER}" \
    PROXY_HOST=127.0.0.1 \
    PROXY_PORT="${holder_port}" \
    LIVE_PROVIDER_ROUTING_ALLOW_INSECURE_HTTP=1 \
    LIVE_PROVIDER_ROUTING_PRIMARY_TYPE=azure-openai \
    LIVE_PROVIDER_ROUTING_PRIMARY_BASE_URL="http://127.0.0.1:${primary_port}/openai/v1" \
    LIVE_PROVIDER_ROUTING_PRIMARY_API_KEY="${primary_key}" \
    LIVE_PROVIDER_ROUTING_PRIMARY_MODEL="${primary_model}" \
    LIVE_PROVIDER_ROUTING_SECONDARY_TYPE=openai-compatible \
    LIVE_PROVIDER_ROUTING_SECONDARY_BASE_URL="http://127.0.0.1:${secondary_port}/v1" \
    LIVE_PROVIDER_ROUTING_SECONDARY_API_KEY="${secondary_key}" \
    LIVE_PROVIDER_ROUTING_SECONDARY_MODEL="${secondary_model}" \
    LIVE_PROVIDER_ROUTING_PUBLIC_MODEL="${public_model}" \
    LIVE_PROVIDER_ROUTING_ROUTE_ID="${route_id}" \
    LIVE_PROVIDER_ROUTING_SMOKE_DIR="${TMP_ROOT}/explicit-port-smoke" \
    SMOKE_STARTUP_TIMEOUT_SECONDS=5 \
    SMOKE_CURL_CONNECT_TIMEOUT_SECONDS=1 \
    SMOKE_READINESS_REQUEST_MAX_TIME_SECONDS=1 \
    SMOKE_PROCESS_TERM_GRACE_SECONDS=1 \
    SMOKE_PORT_RELEASE_TIMEOUT_SECONDS=1 \
    SMOKE_SHIM_UPSTREAM_TIMEOUT_SECONDS=5 \
    "${SMOKE_SCRIPT}" >"${TMP_ROOT}/explicit-port.stdout" 2>"${TMP_ROOT}/explicit-port.stderr"; then
    fail "explicit PROXY_PORT unexpectedly retried onto another port"
  fi
  attempts_after="$(wc -l < "${PROXY_WRAPPER_ATTEMPTS}" | tr -d ' ')"
  [[ "${attempts_after}" -eq $((attempts_before + 1)) ]] || fail "explicit PROXY_PORT triggered an auto-port retry"
  [[ "$(tail -n 1 "${PROXY_WRAPPER_ATTEMPTS}")" == "${holder_port}" ]] || fail "explicit PROXY_PORT was not preserved"
  grep -Fq -- "${holder_port}" "${TMP_ROOT}/explicit-port.stderr" || \
    fail "explicit PROXY_PORT failure did not identify the collided port"
  if ! process_is_running "${holder_pid}" || ! port_accepts_tcp "${holder_port}"; then
    fail "harness cleanup disturbed the external listener owning explicit PROXY_PORT"
  fi
  kill -TERM "${holder_pid}" 2>/dev/null || true
  wait "${holder_pid}" 2>/dev/null || true
  wait_for_port_release "${holder_port}" || fail "explicit-port holder did not release its port"

  stop_mock_targets
  local pid port
  for pid in "${MOCK_SERVER_PIDS[@]:-}"; do
    if [[ -n "${pid}" ]] && process_is_running "${pid}"; then
      fail "mock target PID ${pid} remained alive after cleanup"
    fi
  done
  for port in "${MOCK_SERVER_PORTS[@]:-}"; do
    if [[ -n "${port}" ]] && ! wait_for_port_release "${port}"; then
      fail "mock target port 127.0.0.1:${port} remained open after cleanup"
    fi
  done

  log "Deterministic live provider-routing process smoke passed"
}

main "$@"
