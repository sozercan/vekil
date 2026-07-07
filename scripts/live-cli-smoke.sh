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
# SMOKE_PROVIDER selects the upstream under test:
#   copilot (default) -> zero-config GitHub Copilot, credentialed, used by the gated
#                        Live Copilot Smoke workflow.
#   zen               -> OpenCode Zen free tier via --providers-config; no credentials,
#                        runs on fork PRs. See main_zen below.
SMOKE_PROVIDER="${SMOKE_PROVIDER:-copilot}"
PROVIDERS_CONFIG="${PROVIDERS_CONFIG:-${REPO_ROOT}/examples/opencode-zen-free.yaml}"
if [[ "${SMOKE_PROVIDER}" == "zen" ]]; then
  PROXY_PORT="${PROXY_PORT:-8899}"
else
  PROXY_PORT="${PROXY_PORT:-1337}"
fi
PROXY_BASE_URL="http://${PROXY_HOST}:${PROXY_PORT}"
START_PROXY="${START_PROXY:-1}"
TMP_PARENT="${LIVE_CLI_SMOKE_TMP_PARENT:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}}"
SMOKE_DIR="${LIVE_CLI_SMOKE_DIR:-$(mktemp -d "${TMP_PARENT%/}/live-cli-smoke.XXXXXX")}"
if [[ "${SMOKE_DIR}" != /* ]]; then
  SMOKE_DIR="${PWD}/${SMOKE_DIR}"
fi
PROXY_LOG="${SMOKE_DIR}/proxy.log"
MODELS_JSON="${SMOKE_DIR}/models.json"
PROMPT="Read left.txt and right.txt in the current directory and reply with exactly the two file contents joined by a vertical bar, with no spaces, no markdown, no commentary, and no extra text. Output only the final string."

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

model_supports_endpoint() {
  local model="$1"
  local endpoint="$2"

  jq -e --arg model "${model}" --arg endpoint "${endpoint}" '
    .data[]?
    | select(.id == $model)
    | (.supported_endpoints // [])
    | index($endpoint)
  ' "${MODELS_JSON}" >/dev/null
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

pick_optional_gemini_model() {
  local candidate

  for candidate in "$@"; do
    if model_exists "${candidate}" && model_supports_endpoint "${candidate}" "/chat/completions"; then
      printf '%s\n' "${candidate}"
      return 0
    fi
  done

  # The Gemini CLI hits Gemini-native proxy routes, but Vekil translates
  # those requests to upstream OpenAI chat completions internally, so the
  # selected model must advertise /chat/completions support.
  candidate="$(jq -r '
    [
      .data[]?
      | select((.id | type) == "string")
      | select(.id | startswith("gemini-"))
      | select((.supported_endpoints // []) | index("/chat/completions"))
      | .id
    ][0] // ""
  ' "${MODELS_JSON}")"
  if [[ -n "${candidate}" ]]; then
    printf '%s\n' "${candidate}"
    return 0
  fi

  log "Skipping Gemini smoke: no Gemini model with /chat/completions support is listed by ${PROXY_BASE_URL}/v1/models."
  return 0
}

write_case_files() {
  local case_dir="$1"
  local client="$2"
  local fixture_name
  fixture_name="$(printf '%s' "${client}" | tr '[:lower:]' '[:upper:]')"
  local left_value="ZX_${fixture_name}_LEFT"
  local right_value="ZX_${fixture_name}_RIGHT"

  mkdir -p "${case_dir}"
  # Keep fixtures newline-free so exact-output assertions compare only the
  # requested payload, not editor-added file terminators.
  printf '%s' "${left_value}" > "${case_dir}/left.txt"
  printf '%s' "${right_value}" > "${case_dir}/right.txt"
  printf '%s|%s' "${left_value}" "${right_value}"
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
  awk 'NF { gsub(/\r/, "", $0); printf "%s", $0 }' "$1"
}

normalize_cli_output() {
  local client="$1"
  local actual="$2"

  # Some CLIs wrap exact fixture text in presentation-only numbered-list items,
  # e.g. `1. ZX_COPILOT_LEFT|1. ZX_COPILOT_RIGHT`. Strip only prefixes that
  # immediately precede the deterministic smoke fixture tokens.
  actual="$(printf '%s' "${actual}" | sed -E 's/(^|\|)[[:space:]]*[0-9]+[.)][[:space:]]*(ZX_[A-Z]+_(LEFT|RIGHT))/\1\2/g')"

  # Newer Gemini CLI builds may still emit JSON-shaped chunks even with
  # `-o text`, e.g. {"output":"LEFT"}|{"output":"RIGHT"}. Normalize that
  # wrapper before exact matching so the smoke tests keep validating proxy
  # translation rather than a CLI presentation detail.
  if [[ "${client}" == "gemini" && "${actual}" == *'{"output"'* ]]; then
    local parsed
    parsed="$(printf '%s' "${actual}" | jq -Rr 'split("|") | map((fromjson? // {}) | .output? // empty) | select(length > 0) | join("|")' 2>/dev/null || true)"
    if [[ -n "${parsed}" ]]; then
      actual="${parsed}"
    fi
  fi

  printf '%s' "${actual}"
}

start_proxy() {
  [[ -x "${PROXY_BIN}" ]] || die "proxy binary not found or not executable: ${PROXY_BIN}"

  mkdir -p "${SMOKE_DIR}" "${SMOKE_DIR}/cases" "${SMOKE_DIR}/homes" "${SMOKE_DIR}/outputs"

  if [[ "${SMOKE_PROVIDER}" == "zen" ]]; then
    [[ -f "${PROVIDERS_CONFIG}" ]] || die "providers config not found: ${PROVIDERS_CONFIG}"
    log "Starting proxy at ${PROXY_BASE_URL} with ${PROVIDERS_CONFIG} (no credentials)"
    "${PROXY_BIN}" \
      --host "${PROXY_HOST}" \
      --port "${PROXY_PORT}" \
      --providers-config "${PROVIDERS_CONFIG}" \
      >"${PROXY_LOG}" 2>&1 &
    proxy_pid="$!"
    return
  fi

  mkdir -p "${PROXY_TOKEN_DIR}"
  seed_access_token

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
  local expected
  local actual

  expected="$(write_case_files "${case_dir}" "codex")"
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
  printf '%s' "${actual}" > "${output_file}"
}

run_claude_smoke() {
  local case_dir="${SMOKE_DIR}/cases/claude"
  local home_dir="${SMOKE_DIR}/homes/claude-home"
  local output_file="${SMOKE_DIR}/outputs/claude.txt"
  local expected
  local actual

  expected="$(write_case_files "${case_dir}" "claude")"
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
  printf '%s' "${actual}" > "${output_file}"
}

run_gemini_smoke() {
  local case_dir="${SMOKE_DIR}/cases/gemini"
  local home_dir="${SMOKE_DIR}/homes/gemini-home"
  local output_file="${SMOKE_DIR}/outputs/gemini.txt"
  local expected
  local actual

  expected="$(write_case_files "${case_dir}" "gemini")"
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
    GEMINI_CLI_TRUST_WORKSPACE=true \
    gemini \
      -m "${GEMINI_MODEL}" \
      -p "${PROMPT}" \
      -o text \
      -y \
      > "${output_file}"
  )

  actual="$(normalize_cli_output "gemini" "$(read_normalized_output "${output_file}")")"
  assert_exact_output "gemini" "${expected}" "${actual}"
  printf '%s' "${actual}" > "${output_file}"
}

# ---------------------------------------------------------------------------
# Zen mode (SMOKE_PROVIDER=zen): credential-free OpenCode Zen free-tier smoke.
#
# The free tier rotates and is rate-limited per IP, so the contract is:
#   - hard FAIL only on a real proxy fault, or a model that a raw-chat canary
#     proves reachable yet whose CLI output is wrong (a translation regression);
#   - SKIP (try the next model) on promo-ended / 401 / 429 / 5xx / transport;
#   - exit 0 if >=1 harness passes OR every model is upstream-unreachable
#     (neutral skip — a Zen outage must not block unrelated PRs).
# Codex is intentionally excluded: codex CLI is /responses-only and always sends
# a nameless web_search tool that Zen free upstreams reject. Copilot CLI covers
# the same role via COPILOT_PROVIDER_WIRE_API=completions.
# ---------------------------------------------------------------------------

# Preference order for free models; intersected with the live /v1/models catalog.
# deepseek-v4-flash-free is first because it returns clean output (some weaker
# free models leak chain-of-thought), so it anchors the mismatch=FAIL check.
ZEN_MODEL_PREFS=(
  deepseek-v4-flash-free
  mimo-v2.5-free
  north-mini-code-free
  nemotron-3-ultra-free
  big-pickle
)

ATTEMPT_STATUS=""

# zen_canary <model> -> echoes: OK | SKIP <reason> | FAIL <reason>
# A raw /v1/chat/completions probe that decides reachability before we trust a
# CLI's output. Distinguishes a proxy-generated fault from an upstream outage.
zen_canary() {
  local model="$1"
  local body="${SMOKE_DIR}/canary-${model//[^a-zA-Z0-9_.-]/_}.json"
  local request="${SMOKE_DIR}/canary-req-${model//[^a-zA-Z0-9_.-]/_}.json"
  local code errmsg

  # Build the request body with jq so model IDs are always valid JSON (matches
  # probe_model in live-zen-smoke.sh and live-compact-smoke.sh).
  jq -n --arg model "${model}" \
    '{model: $model, max_tokens: 16, messages: [{role: "user", content: "ping"}]}' \
    > "${request}" || { printf 'SKIP request-build-error\n'; return; }

  code="$(curl -s -o "${body}" -w '%{http_code}' --max-time 60 \
    -X POST "${PROXY_BASE_URL}/v1/chat/completions" \
    -H 'content-type: application/json' \
    --data-binary "@${request}" \
    2>/dev/null)" || { printf 'SKIP transport-error\n'; return; }

  errmsg="$(jq -r '.error.message? // empty' "${body}" 2>/dev/null || true)"
  if printf '%s' "${errmsg}" | grep -qiE 'promotion has ended|not supported for format'; then
    printf 'SKIP promo-ended\n'; return
  fi
  if printf '%s' "${errmsg}" | grep -qiE 'does not support /|unknown model|no upstream'; then
    printf 'FAIL proxy:%s\n' "${errmsg:0:80}"; return
  fi
  case "${code}" in
    200) printf 'OK\n' ;;
    400) printf 'FAIL http-400:%s\n' "${errmsg:0:80}" ;;     # proxy-generated bad request
    401|403|429|5*) printf 'SKIP http-%s\n' "${code}" ;;
    *) printf 'SKIP http-%s\n' "${code}" ;;
  esac
}

# run_harness_iterated <client> <model>...  -> 0 pass, 2 all-skipped; dies on fault.
run_harness_iterated() {
  local client="$1"; shift
  local model verdict status
  for model in "$@"; do
    verdict="$(zen_canary "${model}")"
    status="${verdict%% *}"
    case "${status}" in
      SKIP) log "[${client}] skip ${model} (${verdict#* })"; continue ;;
      FAIL) die "[${client}] proxy fault on ${model}: ${verdict#* } (see ${PROXY_LOG})" ;;
    esac

    # Canary says reachable. Run the real CLI and classify the result.
    run_zen_harness_once "${client}" "${model}"
    case "${ATTEMPT_STATUS}" in
      PASS)
        log "[${client}] PASS ${model}"; return 0 ;;
      FAIL_WRONG)
        die "[${client}] ${model} reachable (canary 200) but output mismatch — translation regression" ;;
      SKIP_UPSTREAM)
        log "[${client}] skip ${model} (CLI/upstream error after reachable canary)"; continue ;;
    esac
  done
  log "[${client}] SKIP — no reachable free model"
  return 2
}

# run_zen_harness_once <client> <model> -> sets ATTEMPT_STATUS=PASS|FAIL_WRONG|SKIP_UPSTREAM
run_zen_harness_once() {
  local client="$1"
  local model="$2"
  local case_dir="${SMOKE_DIR}/cases/${client}"
  local output_file="${SMOKE_DIR}/outputs/${client}.txt"
  local expected actual rc

  rm -rf "${case_dir}"
  expected="$(write_case_files "${case_dir}" "${client}")"

  rc=0
  "run_${client}_zen" "${case_dir}" "${model}" "${output_file}" || rc=$?
  if [[ "${rc}" -ne 0 ]]; then
    ATTEMPT_STATUS="SKIP_UPSTREAM"; return 0
  fi

  actual="$(normalize_cli_output "${client}" "$(read_normalized_output "${output_file}")")"
  if [[ -z "${actual}" ]]; then
    ATTEMPT_STATUS="SKIP_UPSTREAM"; return 0
  fi
  if [[ "${actual}" == "${expected}" ]]; then
    ATTEMPT_STATUS="PASS"
  else
    printf 'expected %s output: %s\n' "${client}" "${expected}" >&2
    printf 'actual %s output:   %s\n' "${client}" "${actual}" >&2
    ATTEMPT_STATUS="FAIL_WRONG"
  fi
  return 0
}

# GitHub Copilot CLI in offline BYOK mode -> vekil -> Zen, OpenAI completions wire.
run_copilot_zen() {
  local case_dir="$1"
  local model="$2"
  local output_file="$3"
  local home_dir="${SMOKE_DIR}/homes/copilot-home"

  rm -rf "${home_dir}"; mkdir -p "${home_dir}"
  (
    cd "${case_dir}"
    HOME="${home_dir}" \
    COPILOT_PROVIDER_BASE_URL="${PROXY_BASE_URL}/v1" \
    COPILOT_PROVIDER_TYPE=openai \
    COPILOT_PROVIDER_WIRE_API=completions \
    COPILOT_MODEL="${model}" \
    COPILOT_OFFLINE=true \
    copilot --allow-all-tools -p "${PROMPT}" -s \
      > "${output_file}" 2>"${SMOKE_DIR}/outputs/copilot.err" < /dev/null
  )
}

run_claude_zen() {
  local case_dir="$1"
  local model="$2"
  local output_file="$3"
  local home_dir="${SMOKE_DIR}/homes/claude-home"

  rm -rf "${home_dir}"; mkdir -p "${home_dir}/.claude"
  cat > "${home_dir}/.claude/settings.json" <<EOF
{
  "env": {
    "ANTHROPIC_BASE_URL": "${PROXY_BASE_URL}",
    "ANTHROPIC_API_KEY": "dummy"
  },
  "skipDangerousModePermissionPrompt": true
}
EOF
  (
    cd "${case_dir}"
    HOME="${home_dir}" \
    ANTHROPIC_BASE_URL="${PROXY_BASE_URL}" \
    ANTHROPIC_API_KEY=dummy \
    claude \
      --dangerously-skip-permissions \
      --print \
      --output-format text \
      --model "${model}" \
      "${PROMPT}" \
      > "${output_file}" < /dev/null
  )
}

run_gemini_zen() {
  local case_dir="$1"
  local model="$2"
  local output_file="$3"
  local home_dir="${SMOKE_DIR}/homes/gemini-home"

  rm -rf "${home_dir}"; mkdir -p "${home_dir}/.gemini/tmp"
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
  (
    cd "${case_dir}"
    HOME="${home_dir}" \
    GEMINI_API_KEY=dummy \
    GOOGLE_GEMINI_BASE_URL="${PROXY_BASE_URL}" \
    GOOGLE_GENAI_API_VERSION=v1beta \
    GEMINI_CLI_NO_RELAUNCH=true \
    GEMINI_CLI_TRUST_WORKSPACE=true \
    gemini \
      -m "${model}" \
      -p "${PROMPT}" \
      -o text \
      -y \
      > "${output_file}" < /dev/null
  )
}

main_zen() {
  require_cmd curl
  require_cmd jq
  require_cmd copilot

  mkdir -p "${SMOKE_DIR}" "${SMOKE_DIR}/cases" "${SMOKE_DIR}/homes" "${SMOKE_DIR}/outputs"

  if [[ "${START_PROXY}" == "1" ]]; then
    start_proxy
    wait_for_ready
  else
    log "Using existing proxy at ${PROXY_BASE_URL}"
  fi

  fetch_models

  local candidates=() model
  for model in "${ZEN_MODEL_PREFS[@]}"; do
    if model_exists "${model}"; then
      candidates+=("${model}")
    fi
  done
  [[ "${#candidates[@]}" -gt 0 ]] || die "zen config lists no usable models (checked: ${ZEN_MODEL_PREFS[*]})"
  log "Zen candidate models: ${candidates[*]}"

  local any_pass=0 any_skip=0 rc

  # Copilot CLI is required; Claude and Gemini are optional (skipped if absent).
  local clients=(copilot)
  command -v claude >/dev/null 2>&1 && clients+=(claude) || log "claude not installed; skipping Claude harness"
  command -v gemini >/dev/null 2>&1 && clients+=(gemini) || log "gemini not installed; skipping Gemini harness"

  local client
  for client in "${clients[@]}"; do
    rc=0
    run_harness_iterated "${client}" "${candidates[@]}" || rc=$?
    case "${rc}" in
      0) any_pass=1 ;;
      2) any_skip=1 ;;
    esac
  done

  if [[ "${any_pass}" -eq 1 ]]; then
    log "Zen smoke passed (>=1 harness produced exact output through the proxy)."
    log "Artifacts: ${SMOKE_DIR}"
    return 0
  fi
  if [[ "${any_skip}" -eq 1 ]]; then
    log "Zen smoke NEUTRAL SKIP: no free model was reachable (rotation / rate limit / outage)."
    log "This is not a proxy failure; re-check the live free set with:"
    log "  curl -s https://opencode.ai/zen/v1/models -H 'authorization: Bearer public'"
    return 0
  fi
  die "Zen smoke produced neither a pass nor a skip (unexpected)"
}

main() {
  if [[ "${SMOKE_PROVIDER}" == "zen" ]]; then
    main_zen "$@"
    return
  fi

  require_cmd curl
  require_cmd jq
  require_cmd codex
  require_cmd claude

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
  GEMINI_MODEL="$(pick_optional_gemini_model gemini-3.1-pro-preview gemini-3-pro-preview gemini-2.5-pro gemini-3-flash-preview)"

  if [[ -n "${GEMINI_MODEL}" ]]; then
    require_cmd gemini
  fi

  log "Selected models:"
  log "  codex:  ${CODEX_MODEL}"
  log "  claude: ${CLAUDE_MODEL}"
  if [[ -n "${GEMINI_MODEL}" ]]; then
    log "  gemini: ${GEMINI_MODEL}"
  else
    log "  gemini: skipped (no supported Gemini model listed)"
  fi

  run_codex_smoke
  run_claude_smoke
  if [[ -n "${GEMINI_MODEL}" ]]; then
    run_gemini_smoke
  fi

  log "All enabled live CLI smoke checks passed."
  log "Artifacts: ${SMOKE_DIR}"
}

main "$@"
