#!/usr/bin/env bash

set -euo pipefail

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

require_cmd curl
require_cmd python3

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
PROXY_BIN="${PROXY_BIN:-${REPO_ROOT}/vekil}"
AUTH_FILE_1="${CODEX_POOL_AUTH_FILE_1:-}"
AUTH_FILE_2="${CODEX_POOL_AUTH_FILE_2:-}"
MODEL="${CODEX_POOL_MODEL:-}"
SMOKE_DIR="${LIVE_CODEX_POOL_SMOKE_DIR:-${TMPDIR:-/tmp}/vekil-live-codex-pool}"
HOST="${PROXY_HOST:-127.0.0.1}"
PORT="${PROXY_PORT:-}"

[[ -x "${PROXY_BIN}" ]] || die "proxy binary is not executable: ${PROXY_BIN}"
[[ -f "${AUTH_FILE_1}" ]] || die "CODEX_POOL_AUTH_FILE_1 must name a readable Codex auth.json"
[[ -f "${AUTH_FILE_2}" ]] || die "CODEX_POOL_AUTH_FILE_2 must name a readable Codex auth.json"
[[ "$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "${AUTH_FILE_1}")" != "$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "${AUTH_FILE_2}")" ]] || die "the two auth files must be distinct"

rm -rf "${SMOKE_DIR}"
mkdir -p "${SMOKE_DIR}"
CONFIG_JSON="${SMOKE_DIR}/providers.json"
PROXY_LOG="${SMOKE_DIR}/proxy.log"
STATS_JSON="${SMOKE_DIR}/stats.json"

if [[ -z "${PORT}" ]]; then
  PORT="$(python3 - "${HOST}" <<'PY'
import socket, sys
host = sys.argv[1]
family = socket.AF_INET6 if ':' in host else socket.AF_INET
with socket.socket(family, socket.SOCK_STREAM) as sock:
    sock.bind((host, 0))
    print(sock.getsockname()[1])
PY
)"
fi
BASE_URL="http://${HOST}:${PORT}"

python3 - "${AUTH_FILE_1}" "${AUTH_FILE_2}" "${CONFIG_JSON}" <<'PY'
import json, os, sys
first, second, output = sys.argv[1:]
config = {
    "schema_version": 2,
    "providers": [{
        "id": "codex",
        "type": "openai-codex",
        "default": True,
        "codex_accounts": {
            "strategy": "round_robin",
            "max_account_attempts": 0,
            "session_affinity": False,
            "session_affinity_ttl": "1h",
            "accounts": [
                {"id": "member-1", "auth_file": os.path.abspath(first)},
                {"id": "member-2", "auth_file": os.path.abspath(second)},
            ],
        },
    }],
}
with open(output, "w", encoding="utf-8") as handle:
    json.dump(config, handle)
PY

"${PROXY_BIN}" config validate --providers-config "${CONFIG_JSON}" >/dev/null

"${PROXY_BIN}" --host "${HOST}" --port "${PORT}" --providers-config "${CONFIG_JSON}" >"${PROXY_LOG}" 2>&1 &
PROXY_PID=$!
cleanup() {
  kill "${PROXY_PID}" 2>/dev/null || true
  wait "${PROXY_PID}" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

log "waiting for pooled Codex readiness"
for _ in $(seq 1 120); do
  if ! kill -0 "${PROXY_PID}" 2>/dev/null; then
    die "proxy exited before readiness; inspect ${PROXY_LOG}"
  fi
  if curl --fail --silent --show-error --connect-timeout 2 --max-time 5 "${BASE_URL}/readyz" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
curl --fail --silent --show-error --connect-timeout 2 --max-time 5 "${BASE_URL}/readyz" >/dev/null || die "proxy did not become ready; inspect ${PROXY_LOG}"

MODELS_JSON="${SMOKE_DIR}/models.json"
curl --fail --silent --show-error --connect-timeout 5 --max-time 30 "${BASE_URL}/v1/models" >"${MODELS_JSON}"
if [[ -z "${MODEL}" ]]; then
  MODEL="$(python3 - "${MODELS_JSON}" <<'PY'
import json, sys
with open(sys.argv[1], encoding='utf-8') as handle:
    payload = json.load(handle)
for item in payload.get('data', []):
    model = item.get('id', '')
    endpoints = item.get('supported_endpoints') or []
    if model and '/responses' in endpoints:
        print(model)
        break
PY
)"
fi
[[ -n "${MODEL}" ]] || die "no Responses-capable Codex model was discovered"
log "using model ${MODEL}; it must be advertised by both mounted accounts"

for index in 1 2; do
  request="${SMOKE_DIR}/request-${index}.json"
  response="${SMOKE_DIR}/response-${index}.json"
  python3 - "${MODEL}" "${index}" "${request}" <<'PY'
import json, sys
model, index, output = sys.argv[1:]
with open(output, 'w', encoding='utf-8') as handle:
    json.dump({"model": model, "input": f"Reply with the digit {index} only.", "store": False}, handle)
PY
  curl --fail --silent --show-error --connect-timeout 5 --max-time 120 \
    -H 'Content-Type: application/json' \
    --data-binary "@${request}" \
    "${BASE_URL}/v1/responses" >"${response}"
done

curl --fail --silent --show-error --connect-timeout 5 --max-time 15 "${BASE_URL}/stats.json" >"${STATS_JSON}"
python3 - "${STATS_JSON}" <<'PY'
import json, sys
with open(sys.argv[1], encoding='utf-8') as handle:
    payload = json.load(handle)
members = [row.get('access_member_id') for row in payload.get('recent_attempts', []) if row.get('provider_id') == 'codex']
missing = [member for member in ('member-1', 'member-2') if member not in members]
if missing:
    raise SystemExit(f"pooled attempt trace did not use both aliases; missing={missing}, observed={members}")
print("pooled Codex smoke passed with both configured account aliases")
PY
