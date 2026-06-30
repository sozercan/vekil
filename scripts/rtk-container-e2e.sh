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

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

require_cmd curl
require_cmd docker
require_cmd python3

TMP_PARENT="${RTK_CONTAINER_E2E_TMP_PARENT:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}}"
cleanup_smoke_dir=0
if [[ -n "${RTK_CONTAINER_E2E_DIR:-}" ]]; then
  SMOKE_DIR="${RTK_CONTAINER_E2E_DIR}"
else
  SMOKE_DIR="$(mktemp -d "${TMP_PARENT%/}/rtk-container-e2e.XXXXXX")"
  cleanup_smoke_dir=1
fi
if [[ "${SMOKE_DIR}" != /* ]]; then
  SMOKE_DIR="${PWD}/${SMOKE_DIR}"
fi

IMAGE="${RTK_CONTAINER_E2E_IMAGE:-vekil:rtk}"
UPSTREAM_PORT="${RTK_CONTAINER_E2E_UPSTREAM_PORT:-$(free_port)}"
PROXY_PORT="${RTK_CONTAINER_E2E_PROXY_PORT:-$(free_port)}"
CONTAINER_NAME="${RTK_CONTAINER_E2E_CONTAINER:-vekil-rtk-e2e-${RANDOM}-${RANDOM}}"
UPSTREAM_LOG="${SMOKE_DIR}/upstream.log"
PROXY_LOG="${SMOKE_DIR}/proxy.log"
PROVIDERS_CONFIG="${SMOKE_DIR}/providers.yaml"
REQUEST_JSON="${SMOKE_DIR}/request.json"
RESPONSE_JSON="${SMOKE_DIR}/response.json"
ORIGINAL_OUTPUT_FILE="${SMOKE_DIR}/original-output.txt"
UPSTREAM_SCRIPT="${SMOKE_DIR}/mock_responses_upstream.py"

upstream_pid=""

cleanup() {
  local rc=$?

  if [[ ${rc} -ne 0 ]]; then
    log "RTK container e2e failed; debug files are in ${SMOKE_DIR}"
    if [[ -f "${UPSTREAM_LOG}" ]]; then
      log "Mock upstream log"
      cat "${UPSTREAM_LOG}" >&2 || true
    fi
    log "Vekil container log"
    docker logs "${CONTAINER_NAME}" >&2 || true
    if [[ -f "${PROXY_LOG}" ]]; then
      cat "${PROXY_LOG}" >&2 || true
    fi
    if [[ -f "${RESPONSE_JSON}" ]]; then
      log "Proxy response"
      cat "${RESPONSE_JSON}" >&2 || true
    fi
  elif [[ "${cleanup_smoke_dir}" == "1" ]]; then
    rm -rf "${SMOKE_DIR}"
  fi

  docker rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true
  if [[ -n "${upstream_pid}" ]] && kill -0 "${upstream_pid}" 2>/dev/null; then
    kill "${upstream_pid}" 2>/dev/null || true
    wait "${upstream_pid}" 2>/dev/null || true
  fi

  exit "${rc}"
}
trap cleanup EXIT

mkdir -p "${SMOKE_DIR}"

log "Verifying RTK binary is present in ${IMAGE}"
docker run --rm --entrypoint /usr/local/bin/rtk "${IMAGE}" --version | grep -E '^rtk ' >/dev/null

cat > "${UPSTREAM_SCRIPT}" <<'PY'
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import sys

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/healthz":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
            return
        if self.path == "/v1/models":
            self._write_json({"object": "list", "data": [{"id": "rtk-e2e", "object": "model"}]})
            return
        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        length = int(self.headers.get("content-length", "0"))
        raw = self.rfile.read(length)
        try:
            payload = json.loads(raw)
        except Exception as exc:
            payload = {"decode_error": str(exc), "raw": raw.decode("utf-8", "replace")}
        self._write_json({
            "id": "resp_mock",
            "object": "response",
            "status": "completed",
            "model": payload.get("model", "rtk-e2e"),
            "input_echo": payload,
            "output": [],
        })

    def _write_json(self, body):
        data = json.dumps(body).encode("utf-8")
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, fmt, *args):
        print(fmt % args, file=sys.stderr)

if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1])), Handler).serve_forever()
PY

cat > "${ORIGINAL_OUTPUT_FILE}" <<'EOF_DIFF'
diff --git a/main.go b/main.go
index 1111111..2222222 100644
--- a/main.go
+++ b/main.go
@@ -1,7 +1,7 @@
 package main
 
 func main() {
-	println("old")
+	println("new")
 }
 
 func helper() {
-	println("unused old helper output")
+	println("unused new helper output")
 }
EOF_DIFF

log "Starting mock OpenAI-compatible upstream on 0.0.0.0:${UPSTREAM_PORT}"
python3 "${UPSTREAM_SCRIPT}" "${UPSTREAM_PORT}" >"${UPSTREAM_LOG}" 2>&1 &
upstream_pid=$!
for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:${UPSTREAM_PORT}/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done
curl -fsS "http://127.0.0.1:${UPSTREAM_PORT}/healthz" >/dev/null

cat > "${PROVIDERS_CONFIG}" <<EOF_CONFIG
providers:
  - id: mock
    type: openai-compatible
    default: true
    base_url: http://host.docker.internal:${UPSTREAM_PORT}/v1
    auth_type: none
    model_discovery: static
    models:
      - public_id: rtk-e2e
        endpoints:
          - /responses
tool_optimizers:
  enabled: true
  output_reduce:
    enabled: true
    min_input_bytes: 0
    max_input_bytes: 0
    timeout_ms: 5000
  providers:
    - id: rtk
      type: rtk_cli
      path: /usr/local/bin/rtk
      stages:
        - output_reduce
EOF_CONFIG

python3 - "${ORIGINAL_OUTPUT_FILE}" "${REQUEST_JSON}" <<'PY'
import json
import sys
original_output = open(sys.argv[1], encoding="utf-8").read()
request = {
    "model": "rtk-e2e",
    "input": [
        {
            "type": "function_call",
            "name": "shell_command",
            "call_id": "call_diff",
            "arguments": json.dumps({"command": "git diff"}),
        },
        {
            "type": "function_call_output",
            "call_id": "call_diff",
            "output": original_output,
        },
    ],
}
with open(sys.argv[2], "w", encoding="utf-8") as f:
    json.dump(request, f)
PY

log "Starting Vekil RTK variant on 127.0.0.1:${PROXY_PORT}"
docker rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true
docker run -d \
  --name "${CONTAINER_NAME}" \
  --add-host=host.docker.internal:host-gateway \
  -p "127.0.0.1:${PROXY_PORT}:1337" \
  -v "${PROVIDERS_CONFIG}:/config/providers.yaml:ro" \
  "${IMAGE}" \
  --providers-config /config/providers.yaml >"${PROXY_LOG}"

for _ in $(seq 1 100); do
  if curl -fsS "http://127.0.0.1:${PROXY_PORT}/readyz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done
curl -fsS "http://127.0.0.1:${PROXY_PORT}/readyz" >/dev/null

log "Posting Responses request with git diff tool output"
curl -fsS \
  -H 'content-type: application/json' \
  --data-binary "@${REQUEST_JSON}" \
  "http://127.0.0.1:${PROXY_PORT}/v1/responses" >"${RESPONSE_JSON}"

python3 - "${ORIGINAL_OUTPUT_FILE}" "${RESPONSE_JSON}" <<'PY'
import json
import sys

original = open(sys.argv[1], encoding="utf-8").read()
response = json.load(open(sys.argv[2], encoding="utf-8"))
try:
    reduced = response["input_echo"]["input"][1]["output"]
except Exception as exc:
    raise SystemExit(f"response did not echo reduced tool output: {exc}; response={response!r}")

if reduced == original:
    raise SystemExit("RTK output reducer returned the original git diff unchanged")
if len(reduced.encode("utf-8")) >= len(original.encode("utf-8")):
    raise SystemExit(
        f"RTK output was not smaller: original={len(original.encode('utf-8'))} reduced={len(reduced.encode('utf-8'))}"
    )
if "diff --git" in reduced or "index 1111111..2222222" in reduced:
    raise SystemExit(f"RTK output still contains raw git diff headers: {reduced!r}")
if "main.go" not in reduced or "+2 -2" not in reduced:
    raise SystemExit(f"RTK output did not look like reduced git diff summary: {reduced!r}")

print(
    "RTK reduced git diff tool output through Vekil: "
    f"{len(original.encode('utf-8'))} -> {len(reduced.encode('utf-8'))} bytes"
)
PY

log "RTK container e2e passed"
