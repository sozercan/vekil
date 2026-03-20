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

PROXY_BIN="${PROXY_BIN:-${REPO_ROOT}/copilot-proxy}"
PROXY_HOST="${PROXY_HOST:-127.0.0.1}"
PROXY_PORT="${PROXY_PORT:-1337}"
PROXY_BASE_URL="http://${PROXY_HOST}:${PROXY_PORT}"
START_PROXY="${START_PROXY:-1}"
TMP_PARENT="${LIVE_CLI_SMOKE_TMP_PARENT:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}}"
SMOKE_DIR="${LIVE_CLI_SMOKE_DIR:-$(mktemp -d "${TMP_PARENT%/}/live-cli-smoke.XXXXXX")}"
PROXY_LOG="${SMOKE_DIR}/proxy.log"
MODELS_JSON="${SMOKE_DIR}/models.json"
PROMPT="Read left.txt and right.txt in the current directory and reply with exactly the two file contents joined by a vertical bar, with no spaces or extra text."

if [[ -n "${COPILOT_GITHUB_TOKEN:-}" ]]; then
  PROXY_TOKEN_DIR="${PROXY_TOKEN_DIR:-${SMOKE_DIR}/proxy-token}"
else
  PROXY_TOKEN_DIR="${PROXY_TOKEN_DIR:-${ORIGINAL_HOME}/.config/copilot-proxy}"
fi

proxy_pid=""

cleanup() {
  if [[ -n "${proxy_pid}" ]] && kill -0 "${proxy_pid}" 2>/dev/null; then
    kill "${proxy_pid}" 2>/dev/null || true
    wait "${proxy_pid}" 2>/dev/null || true
  fi
}

trap cleanup EXIT

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

write_case_files() {
  local case_dir="$1"
  local left_value="$2"
  local right_value="$3"

  mkdir -p "${case_dir}"
  printf '%s\n' "${left_value}" > "${case_dir}/left.txt"
  printf '%s\n' "${right_value}" > "${case_dir}/right.txt"
}

assert_exact_output() {
  local client="$1"
  local expected="$2"
  local actual="$3"

  if [[ "${actual}" != "${expected}" ]]; then
    printf 'expected %s output: %s\n' "${client}" "${expected}" >&2
    printf 'actual %s output:   %s\n' "${client}" "${actual}" >&2
    die "${client} smoke output mismatch"
  fi
}

read_normalized_output() {
  tr -d '\r\n' < "$1"
}

start_proxy() {
  [[ -x "${PROXY_BIN}" ]] || die "proxy binary not found or not executable: ${PROXY_BIN}"

  mkdir -p "${SMOKE_DIR}" "${SMOKE_DIR}/cases" "${SMOKE_DIR}/homes" "${SMOKE_DIR}/outputs"
  mkdir -p "${PROXY_TOKEN_DIR}"

  log "Starting proxy at ${PROXY_BASE_URL}"
  "${PROXY_BIN}" \
    --host "${PROXY_HOST}" \
    --port "${PROXY_PORT}" \
    --token-dir "${PROXY_TOKEN_DIR}" \
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

run_codex_smoke() {
  local case_dir="${SMOKE_DIR}/cases/codex"
  local home_dir="${SMOKE_DIR}/homes/codex-home"
  local output_file="${SMOKE_DIR}/outputs/codex.txt"
  local left_value="ZX_COD_41A"
  local right_value="ZX_COD_88B"
  local expected="${left_value}|${right_value}"
  local actual

  write_case_files "${case_dir}" "${left_value}" "${right_value}"
  mkdir -p "${home_dir}/.codex"
  printf 'model = "%s"\nopenai_base_url = "%s"\n' "${CODEX_MODEL}" "${PROXY_BASE_URL}/v1" > "${home_dir}/.codex/config.toml"

  log "Running Codex smoke with model ${CODEX_MODEL}"
  HOME="${home_dir}" \
  OPENAI_API_KEY=dummy \
  OPENAI_BASE_URL="${PROXY_BASE_URL}/v1" \
  codex exec \
    --skip-git-repo-check \
    --cd "${case_dir}" \
    --dangerously-bypass-approvals-and-sandbox \
    -m "${CODEX_MODEL}" \
    --color never \
    -o "${output_file}" \
    "${PROMPT}"

  actual="$(read_normalized_output "${output_file}")"
  assert_exact_output "codex" "${expected}" "${actual}"
}

run_claude_smoke() {
  local case_dir="${SMOKE_DIR}/cases/claude"
  local home_dir="${SMOKE_DIR}/homes/claude-home"
  local output_file="${SMOKE_DIR}/outputs/claude.txt"
  local left_value="ZX_CLA_17Q"
  local right_value="ZX_CLA_52R"
  local expected="${left_value}|${right_value}"
  local actual

  write_case_files "${case_dir}" "${left_value}" "${right_value}"
  mkdir -p "${home_dir}/.claude"
  cat > "${home_dir}/.claude/settings.json" <<EOF
{
  "env": {
    "ANTHROPIC_BASE_URL": "${PROXY_BASE_URL}",
    "ANTHROPIC_API_KEY": "dummy"
  },
  "skipDangerousModePermissionPrompt": true
}
EOF

  log "Running Claude smoke with model ${CLAUDE_MODEL}"
  (
    cd "${case_dir}"
    HOME="${home_dir}" \
    ANTHROPIC_BASE_URL="${PROXY_BASE_URL}" \
    ANTHROPIC_API_KEY=dummy \
    claude \
      --dangerously-skip-permissions \
      --print \
      --output-format text \
      --model "${CLAUDE_MODEL}" \
      "${PROMPT}" \
      > "${output_file}"
  )

  actual="$(read_normalized_output "${output_file}")"
  assert_exact_output "claude" "${expected}" "${actual}"
}

run_gemini_smoke() {
  local case_dir="${SMOKE_DIR}/cases/gemini"
  local home_dir="${SMOKE_DIR}/homes/gemini-home"
  local output_file="${SMOKE_DIR}/outputs/gemini.txt"
  local left_value="ZX_GEM_73M"
  local right_value="ZX_GEM_94N"
  local expected="${left_value}|${right_value}"
  local actual

  write_case_files "${case_dir}" "${left_value}" "${right_value}"
  mkdir -p "${home_dir}/.gemini/tmp"
  printf '{"projects":{}}\n' > "${home_dir}/.gemini/projects.json"
  cat > "${home_dir}/.gemini/settings.json" <<EOF
{
  "security": {
    "auth": {
      "selectedType": "gemini-api-key"
    }
  }
}
EOF

  log "Running Gemini smoke with model ${GEMINI_MODEL}"
  (
    cd "${case_dir}"
    HOME="${home_dir}" \
    GEMINI_API_KEY=dummy \
    GOOGLE_GEMINI_BASE_URL="${PROXY_BASE_URL}" \
    GOOGLE_GENAI_API_VERSION=v1beta \
    GEMINI_CLI_NO_RELAUNCH=true \
    gemini \
      -m "${GEMINI_MODEL}" \
      -p "${PROMPT}" \
      -o text \
      -y \
      > "${output_file}"
  )

  actual="$(read_normalized_output "${output_file}")"
  assert_exact_output "gemini" "${expected}" "${actual}"
}

main() {
  require_cmd curl
  require_cmd jq
  require_cmd codex
  require_cmd claude
  require_cmd gemini

  mkdir -p "${SMOKE_DIR}" "${SMOKE_DIR}/cases" "${SMOKE_DIR}/homes" "${SMOKE_DIR}/outputs"

  if [[ "${START_PROXY}" == "1" ]]; then
    start_proxy
    wait_for_ready
  else
    log "Using existing proxy at ${PROXY_BASE_URL}"
  fi

  fetch_models

  CODEX_MODEL="$(pick_model "Codex/OpenAI" gpt-5.4 gpt-5.3-codex gpt-5.2-codex gpt-5.1-codex gpt-5.1 gpt-5-mini gpt-4.1 gpt-4o)"
  CLAUDE_MODEL="$(pick_model "Claude" claude-sonnet-4.6 claude-sonnet-4.5 claude-sonnet-4 claude-opus-4.6)"
  GEMINI_MODEL="$(pick_model "Gemini" gemini-3.1-pro-preview gemini-3-pro-preview gemini-2.5-pro gemini-3-flash-preview)"

  log "Selected models:"
  log "  codex:  ${CODEX_MODEL}"
  log "  claude: ${CLAUDE_MODEL}"
  log "  gemini: ${GEMINI_MODEL}"

  run_codex_smoke
  run_claude_smoke
  run_gemini_smoke

  log "All live CLI smoke checks passed."
  log "Artifacts: ${SMOKE_DIR}"
}

main "$@"
