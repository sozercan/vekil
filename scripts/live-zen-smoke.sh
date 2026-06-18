#!/usr/bin/env bash

# Live smoke test for the OpenCode Zen free tier via Vekil.
#
# Starts the proxy with examples/opencode-zen-free.yaml on a NON-default port,
# waits for /readyz, lists /v1/models, then sends one tiny chat completion per
# configured free model.
#
# The OpenCode Zen free set rotates and individual promotions end without notice
# (an ended promo returns an error body such as "Free promotion has ended ...").
# This script therefore treats a promo-ended model as a SKIP, not a failure, and
# passes as long as at least one configured free model returns a completion. A
# proxy-side fault (config rejected, /readyz never ready, no models listed, or an
# HTTP transport error) is always a hard failure.
#
# Usage:
#   make build
#   scripts/live-zen-smoke.sh
#
# Overrides (env): PROXY_BIN, PROXY_HOST, PROXY_PORT, PROVIDERS_CONFIG, START_PROXY.

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

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PROXY_BIN="${PROXY_BIN:-${REPO_ROOT}/vekil}"
PROXY_HOST="${PROXY_HOST:-127.0.0.1}"
# Default to a NON-default port so this never collides with a 1337 instance.
PROXY_PORT="${PROXY_PORT:-8899}"
PROXY_BASE_URL="http://${PROXY_HOST}:${PROXY_PORT}"
PROVIDERS_CONFIG="${PROVIDERS_CONFIG:-${REPO_ROOT}/examples/opencode-zen-free.yaml}"
START_PROXY="${START_PROXY:-1}"
TMP_PARENT="${LIVE_ZEN_SMOKE_TMP_PARENT:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}}"
SMOKE_DIR="${LIVE_ZEN_SMOKE_DIR:-$(mktemp -d "${TMP_PARENT%/}/live-zen-smoke.XXXXXX")}"
if [[ "${SMOKE_DIR}" != /* ]]; then
  SMOKE_DIR="${PWD}/${SMOKE_DIR}"
fi
PROXY_LOG="${SMOKE_DIR}/proxy.log"
MODELS_JSON="${SMOKE_DIR}/models.json"
PROMPT="Reply with exactly the word: pong"

proxy_pid=""

cleanup() {
  if [[ -n "${proxy_pid}" ]] && kill -0 "${proxy_pid}" 2>/dev/null; then
    kill "${proxy_pid}" 2>/dev/null || true
    wait "${proxy_pid}" 2>/dev/null || true
  fi
}

trap cleanup EXIT

start_proxy() {
  [[ -x "${PROXY_BIN}" ]] || die "proxy binary not found or not executable: ${PROXY_BIN} (run: make build)"
  [[ -f "${PROVIDERS_CONFIG}" ]] || die "providers config not found: ${PROVIDERS_CONFIG}"

  mkdir -p "${SMOKE_DIR}"

  log "Starting proxy at ${PROXY_BASE_URL} with ${PROVIDERS_CONFIG}"
  "${PROXY_BIN}" \
    --host "${PROXY_HOST}" \
    --port "${PROXY_PORT}" \
    --providers-config "${PROVIDERS_CONFIG}" \
    >"${PROXY_LOG}" 2>&1 &
  proxy_pid="$!"
}

wait_for_ready() {
  local attempt
  for attempt in $(seq 1 30); do
    if curl -fsS "${PROXY_BASE_URL}/readyz" >"${SMOKE_DIR}/readyz.json" 2>/dev/null; then
      return 0
    fi
    sleep 1
  done

  if [[ -f "${PROXY_LOG}" ]]; then
    log "Proxy log from failed readiness check:"
    cat "${PROXY_LOG}" >&2
  fi
  die "proxy never became ready at ${PROXY_BASE_URL}"
}

fetch_models() {
  curl -fsS "${PROXY_BASE_URL}/v1/models" >"${MODELS_JSON}" || die "GET /v1/models failed"
  jq -e '.data | length > 0' "${MODELS_JSON}" >/dev/null || die "no models returned by ${PROXY_BASE_URL}/v1/models"
}

# Probe one model. Echoes one of: OK | SKIP | FAIL  plus a short reason.
probe_model() {
  local model="$1"
  local body_file="${SMOKE_DIR}/resp-${model//[^a-zA-Z0-9_.-]/_}.json"
  local code

  code="$(curl -s -o "${body_file}" -w '%{http_code}' --max-time 90 \
    -X POST "${PROXY_BASE_URL}/v1/chat/completions" \
    -H 'content-type: application/json' \
    -d "{\"model\":\"${model}\",\"messages\":[{\"role\":\"user\",\"content\":\"${PROMPT}\"}],\"max_tokens\":64}" \
    2>/dev/null)" || { printf 'FAIL transport-error\n'; return; }

  # Promo-ended free models come back as an error body (observed HTTP 401) whose
  # message contains "promotion has ended". Treat that as an upstream rotation
  # SKIP rather than a proxy failure.
  local errmsg
  errmsg="$(jq -r '.error.message? // empty' "${body_file}" 2>/dev/null || true)"
  if printf '%s' "${errmsg}" | grep -qi 'promotion has ended'; then
    printf 'SKIP promo-ended\n'
    return
  fi

  if [[ "${code}" != "200" ]]; then
    printf 'FAIL http-%s %s\n' "${code}" "${errmsg:0:80}"
    return
  fi

  if jq -e '.choices[0].message' "${body_file}" >/dev/null 2>&1; then
    local echoed finish
    echoed="$(jq -r '.model // "?"' "${body_file}")"
    finish="$(jq -r '.choices[0].finish_reason // "?"' "${body_file}")"
    printf 'OK echo=%s finish=%s\n' "${echoed}" "${finish}"
    return
  fi

  printf 'FAIL bad-shape\n'
}

main() {
  require_cmd curl
  require_cmd jq

  if [[ "${START_PROXY}" == "1" ]]; then
    start_proxy
    wait_for_ready
  else
    log "Using existing proxy at ${PROXY_BASE_URL}"
  fi

  fetch_models

  local models
  models="$(jq -r '.data[].id' "${MODELS_JSON}")"
  log "Models listed by proxy:"
  printf '%s\n' "${models}" | sed 's/^/    /' >&2

  local total=0 ok=0 skipped=0 failed=0
  local model result status
  while IFS= read -r model; do
    [[ -n "${model}" ]] || continue
    total=$((total + 1))
    result="$(probe_model "${model}")"
    status="${result%% *}"
    case "${status}" in
      OK)   ok=$((ok + 1)) ;;
      SKIP) skipped=$((skipped + 1)) ;;
      *)    failed=$((failed + 1)) ;;
    esac
    printf '%-5s %-26s %s\n' "${status}" "${model}" "${result#* }" >&2
  done <<< "${models}"

  log "Summary: ${ok} ok, ${skipped} skipped (promo ended), ${failed} failed, of ${total} listed."

  if [[ "${failed}" -gt 0 ]]; then
    die "${failed} model(s) failed through the proxy (see ${SMOKE_DIR})."
  fi
  if [[ "${ok}" -lt 1 ]]; then
    die "no free model returned a completion; the free set may have fully rotated. Re-check: curl -s https://opencode.ai/zen/v1/models -H 'authorization: Bearer public'"
  fi

  log "OpenCode Zen free-tier smoke passed."
  log "Artifacts: ${SMOKE_DIR}"
}

main "$@"
