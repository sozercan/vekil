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

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
ORIGINAL_HOME="${HOME}"

PROXY_BIN="${PROXY_BIN:-${REPO_ROOT}/vekil}"
PROXY_HOST="${PROXY_HOST:-127.0.0.1}"
PROXY_PORT="${PROXY_PORT:-1337}"
PROXY_BASE_URL="http://${PROXY_HOST}:${PROXY_PORT}"
START_PROXY="${START_PROXY:-1}"
TMP_PARENT="${LIVE_COMPACT_SMOKE_TMP_PARENT:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}}"
SMOKE_DIR="${LIVE_COMPACT_SMOKE_DIR:-$(mktemp -d "${TMP_PARENT%/}/live-compact-smoke.XXXXXX")}"
PROXY_LOG="${SMOKE_DIR}/proxy.log"
MODELS_JSON="${SMOKE_DIR}/models.json"
COMPACT_REQUEST_JSON="${SMOKE_DIR}/compact-request.json"
COMPACT_RESPONSE_JSON="${SMOKE_DIR}/compact-response.json"
COMPACTION_ITEM_JSON="${SMOKE_DIR}/compaction-item.json"
REPLAY_REQUEST_JSON="${SMOKE_DIR}/replay-request.json"
REPLAY_RESPONSE_JSON="${SMOKE_DIR}/replay-response.json"
REPLAY_MARKER="VEKIL_COMPACTION_REPLAY_OK"

if [[ -n "${COPILOT_GITHUB_TOKEN:-}" ]]; then
  PROXY_TOKEN_DIR="${PROXY_TOKEN_DIR:-${SMOKE_DIR}/proxy-token}"
else
  PROXY_TOKEN_DIR="${PROXY_TOKEN_DIR:-${ORIGINAL_HOME}/.config/vekil}"
fi

proxy_pid=""

cleanup() {
  if [[ -n "${proxy_pid}" ]] && kill -0 "${proxy_pid}" 2>/dev/null; then
    kill "${proxy_pid}" 2>/dev/null || true
    wait "${proxy_pid}" 2>/dev/null || true
  fi
}

trap cleanup EXIT

seed_access_token() {
  if [[ -z "${COPILOT_GITHUB_TOKEN:-}" ]]; then
    return 0
  fi

  printf '%s\n' "${COPILOT_GITHUB_TOKEN}" > "${PROXY_TOKEN_DIR}/access-token"
  chmod 600 "${PROXY_TOKEN_DIR}/access-token"
}

model_exists() {
  jq -e --arg model "$1" '.data[]? | select(.id == $model)' "${MODELS_JSON}" >/dev/null
}

pick_model() {
  local family="$1"
  shift

  local candidate
  for candidate in "$@"; do
    if model_exists "${candidate}"; then
      printf '%s\n' "${candidate}"
      return 0
    fi
  done

  log "Available models from ${PROXY_BASE_URL}/v1/models:"
  jq -r '.data[].id' "${MODELS_JSON}" >&2
  die "unable to find a ${family} model from preferred list: $*"
}

start_proxy() {
  [[ -x "${PROXY_BIN}" ]] || die "proxy binary not found or not executable: ${PROXY_BIN}"

  mkdir -p "${SMOKE_DIR}" "${PROXY_TOKEN_DIR}"
  seed_access_token

  log "Starting proxy at ${PROXY_BASE_URL}"
  "${PROXY_BIN}" \
    --host "${PROXY_HOST}" \
    --port "${PROXY_PORT}" \
    --token-dir "${PROXY_TOKEN_DIR}" \
    --log-level debug \
    >"${PROXY_LOG}" 2>&1 &
  proxy_pid="$!"
}

wait_for_ready() {
  local attempt
  for attempt in $(seq 1 60); do
    if curl -fsS "${PROXY_BASE_URL}/readyz" > "${SMOKE_DIR}/readyz.json"; then
      return 0
    fi
    sleep 2
  done

  if [[ -f "${PROXY_LOG}" ]]; then
    log "Proxy log from failed readiness check:"
    cat "${PROXY_LOG}" >&2
  fi

  die "proxy never became ready at ${PROXY_BASE_URL}"
}

fetch_models() {
  curl -fsS "${PROXY_BASE_URL}/v1/models" > "${MODELS_JSON}"
  jq -e '.data | length > 0' "${MODELS_JSON}" >/dev/null || die "no models returned by ${PROXY_BASE_URL}/v1/models"
}

write_compact_request() {
  local model="$1"

  jq -n \
    --arg model "${model}" \
    '{
      model: $model,
      input: [
        {
          type: "message",
          role: "user",
          content: [
            {
              type: "input_text",
              text: "We are testing live /v1/responses/compact through the local proxy. The task is in progress: create a compact checkpoint, then replay the returned compaction item through /v1/responses."
            }
          ]
        },
        {
          type: "message",
          role: "assistant",
          content: [
            {
              type: "output_text",
              text: "Acknowledged. The live compact smoke test should produce a concise checkpoint summary and an opaque compaction token."
            }
          ]
        }
      ]
    }' > "${COMPACT_REQUEST_JSON}"
}

post_json() {
  local endpoint="$1"
  local request_file="$2"
  local response_file="$3"
  local status

  status="$(curl -sS \
    -o "${response_file}" \
    -w '%{http_code}' \
    -X POST "${PROXY_BASE_URL}${endpoint}" \
    -H 'Content-Type: application/json' \
    --data-binary "@${request_file}")"

  if [[ "${status}" != "200" ]]; then
    if [[ -s "${response_file}" ]]; then
      log "${endpoint} response body:"
      cat "${response_file}" >&2
    fi
    die "${endpoint} returned HTTP ${status}"
  fi
}

assert_compact_response() {
  jq -e '
    ([.output[]? | select(.type == "compaction" and ((.encrypted_content // "") | length > 0))] | length > 0)
  ' "${COMPACT_RESPONSE_JSON}" >/dev/null || die "compact response did not contain a non-empty compaction item"

  jq -e '[.output[] | select(.type == "compaction" and ((.encrypted_content // "") | length > 0))] | .[0]' \
    "${COMPACT_RESPONSE_JSON}" > "${COMPACTION_ITEM_JSON}" || die "failed to extract compaction item"
}

write_replay_request() {
  local model="$1"

  jq -n \
    --arg model "${model}" \
    --arg marker "${REPLAY_MARKER}" \
    --slurpfile compaction "${COMPACTION_ITEM_JSON}" \
    '{
      model: $model,
      input: [
        $compaction[0],
        {
          type: "message",
          role: "user",
          content: [
            {
              type: "input_text",
              text: ("Reply with exactly " + $marker + " and no other text.")
            }
          ]
        }
      ]
    }' > "${REPLAY_REQUEST_JSON}"
}

responses_output_text() {
  jq -r '
    ([.output[]? | select(.type == "message") | .content[]? | select(.type == "output_text" or .type == "text") | .text] | join("\n"))
    | gsub("\r"; "")
    | sub("^[[:space:]]+"; "")
    | sub("[[:space:]]+$"; "")
  ' "$1"
}

assert_replay_response() {
  local replay_text
  replay_text="$(responses_output_text "${REPLAY_RESPONSE_JSON}")"

  if [[ "${replay_text}" != "${REPLAY_MARKER}" ]]; then
    printf 'expected replay response to equal %s after trimming whitespace\n' "${REPLAY_MARKER}" >&2
    printf 'actual normalized replay response:\n%s\n' "${replay_text}" >&2
    die "compaction replay output mismatch"
  fi

  if [[ "${START_PROXY}" == "1" ]] && [[ -f "${PROXY_LOG}" ]]; then
    grep -q 'rewrote compaction items' "${PROXY_LOG}" || die "proxy log did not show compaction item replay rewrite"
  fi
}

main() {
  require_cmd curl
  require_cmd jq

  mkdir -p "${SMOKE_DIR}"

  if [[ "${START_PROXY}" == "1" ]]; then
    start_proxy
    wait_for_ready
  else
    log "Using existing proxy at ${PROXY_BASE_URL}"
  fi

  fetch_models

  COMPACT_MODEL="$(pick_model "Codex/OpenAI" gpt-5.4 gpt-5.3-codex gpt-5.2-codex gpt-5.1-codex gpt-5.1 gpt-5-mini gpt-4.1 gpt-4o)"
  log "Selected compact model: ${COMPACT_MODEL}"

  log "Posting live compact request"
  write_compact_request "${COMPACT_MODEL}"
  post_json "/v1/responses/compact" "${COMPACT_REQUEST_JSON}" "${COMPACT_RESPONSE_JSON}"
  assert_compact_response

  log "Replaying returned compaction item through /v1/responses"
  write_replay_request "${COMPACT_MODEL}"
  post_json "/v1/responses" "${REPLAY_REQUEST_JSON}" "${REPLAY_RESPONSE_JSON}"
  assert_replay_response

  log "Live compaction smoke check passed."
  log "Artifacts: ${SMOKE_DIR}"
}

main "$@"
