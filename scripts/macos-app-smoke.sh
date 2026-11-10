#!/usr/bin/env bash

set -euo pipefail

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CONFIG="${MACOS_APP_CONFIG:-${REPO_ROOT}/build-support/macos/app-config.json}"
MANIFEST_TOOL="${SCRIPT_DIR}/macos-release-manifest.py"
APP_PATH="${APP_PATH:-${REPO_ROOT}/Vekil.app}"
RESOLVED_MANIFEST="${MACOS_RESOLVED_MANIFEST:-${REPO_ROOT}/.build/macos/vekil-macos-release.json}"
RELEASE_MODE="${MACOS_RELEASE:-0}"
STARTUP_TIMEOUT="${MACOS_SMOKE_STARTUP_TIMEOUT_SECONDS:-20}"
EXIT_TIMEOUT="${MACOS_SMOKE_EXIT_TIMEOUT_SECONDS:-20}"

[[ "$(uname -s)" == Darwin ]] || die "macOS app smoke requires Darwin"
for command in open osascript ps python3 pgrep; do require_cmd "${command}"; done
[[ -d "${APP_PATH}" ]] || die "app bundle not found: ${APP_PATH}"
[[ -f "${RESOLVED_MANIFEST}" ]] || die "release manifest not found: ${RESOLVED_MANIFEST}"

app_executable="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key application.executable)"
helper_executable="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key application.helper_executable)"
app_binary="${APP_PATH}/Contents/MacOS/${app_executable}"
helper_binary="${APP_PATH}/Contents/Helpers/${helper_executable}"

APP_PATH="${APP_PATH}" \
MACOS_APP_CONFIG="${CONFIG}" \
MACOS_RESOLVED_MANIFEST="${RESOLVED_MANIFEST}" \
MACOS_RELEASE="${RELEASE_MODE}" \
MACOS_REQUIRE_NOTARIZATION="${MACOS_REQUIRE_NOTARIZATION:-0}" \
  "${SCRIPT_DIR}/verify-macos-app.sh"

if [[ "${RELEASE_MODE}" == 1 ]]; then
  arches="${MACOS_SMOKE_ARCHES:-arm64 x86_64}"
else
  host_arch="$(uname -m)"
  [[ "${host_arch}" != amd64 ]] || host_arch=x86_64
  arches="${MACOS_SMOKE_ARCHES:-${host_arch}}"
fi

helper_args=(--helper "${helper_binary}" --manifest "${RESOLVED_MANIFEST}")
for arch in ${arches}; do helper_args+=(--arch "${arch}"); done
MACOS_RELEASE="${RELEASE_MODE}" "${SCRIPT_DIR}/macos-helper-smoke.py" "${helper_args[@]}"

smoke_root="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/vekil-app-smoke.XXXXXX")"
app_pid=""
helper_pid=""

list_app_pids() {
  local pid command
  for pid in $(pgrep -x "${app_executable}" 2>/dev/null || true); do
    command="$(ps -p "${pid}" -o command= 2>/dev/null || true)"
    if [[ "${command}" == "${app_binary}" ]]; then
      printf '%s\n' "${pid}"
    fi
  done
}

lookup_app_pid() {
  list_app_pids | head -n 1
}

process_running() {
  local pid="$1"
  [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null
}

wait_for_app_pid() {
  local deadline pid
  deadline=$((SECONDS + STARTUP_TIMEOUT))
  while (( SECONDS < deadline )); do
    pid="$(lookup_app_pid || true)"
    if process_running "${pid}"; then
      printf '%s\n' "${pid}"
      return 0
    fi
    sleep 0.25
  done
  return 1
}

wait_for_child_helper() {
  local parent_pid="$1" deadline pid command
  deadline=$((SECONDS + STARTUP_TIMEOUT))
  while (( SECONDS < deadline )); do
    for pid in $(pgrep -P "${parent_pid}" 2>/dev/null || true); do
      command="$(ps -p "${pid}" -o command= 2>/dev/null || true)"
      if [[ "${command}" == *"${helper_binary}"* ]]; then
        printf '%s\n' "${pid}"
        return 0
      fi
    done
    sleep 0.25
  done
  return 1
}

wait_for_exit() {
  local pid="$1" deadline
  deadline=$((SECONDS + EXIT_TIMEOUT))
  while process_running "${pid}"; do
    (( SECONDS < deadline )) || return 1
    sleep 0.25
  done
}

cleanup() {
  if process_running "${app_pid}"; then
    osascript -e "tell application \"${APP_PATH}\" to quit" >/dev/null 2>&1 || true
    wait_for_exit "${app_pid}" || kill -TERM "${app_pid}" 2>/dev/null || true
  fi
  if process_running "${helper_pid}"; then
    kill -TERM "${helper_pid}" 2>/dev/null || true
  fi
  if [[ "${KEEP_MACOS_TEST_ARTIFACTS:-0}" == 1 ]]; then
    log "Preserving smoke artifacts at ${smoke_root}"
  else
    rm -rf "${smoke_root}"
  fi
}
trap cleanup EXIT

existing_pid="$(lookup_app_pid || true)"
if process_running "${existing_pid}"; then
  die "Vekil is already running as pid ${existing_pid}; quit it before running the smoke test"
fi

for arch in ${arches}; do
  app_pid=""
  helper_pid=""
  log_file="${smoke_root}/Vekil-${arch}.log"
  log "Launching exact packaged app as ${arch}"
  open -n -g --arch "${arch}" \
    --env "HOME=${smoke_root}/home-${arch}" \
    --env "XDG_CONFIG_HOME=${smoke_root}/home-${arch}/.config" \
    --env "VEKIL_TEST_ROOT=${smoke_root}/runtime-${arch}" \
    --stdout "${log_file}" \
    --stderr "${log_file}" \
    "${APP_PATH}"

  app_pid="$(wait_for_app_pid)" || {
    tail -n 200 "${log_file}" >&2 || true
    die "${arch} app did not appear in Launch Services"
  }
  command="$(ps -p "${app_pid}" -o command=)"
  [[ "${command}" == *"${app_binary}"* ]] || die "launched process is not the packaged ${app_executable}: ${command}"
  helper_pid="$(wait_for_child_helper "${app_pid}")" || {
    tail -n 200 "${log_file}" >&2 || true
    die "${arch} app did not launch packaged helper"
  }
  sleep 2
  process_running "${app_pid}" || die "${arch} app exited shortly after launch"
  process_running "${helper_pid}" || die "${arch} helper exited shortly after launch"

  log "Verifying second launch activates the singleton without another helper"
  open -n -g --arch "${arch}" \
    --env "HOME=${smoke_root}/home-${arch}" \
    --env "XDG_CONFIG_HOME=${smoke_root}/home-${arch}/.config" \
    --env "VEKIL_TEST_ROOT=${smoke_root}/runtime-${arch}" \
    --stdout "${log_file}" \
    --stderr "${log_file}" \
    "${APP_PATH}"
  sleep 2
  exact_app_pids="$(list_app_pids || true)"
  exact_app_count="$(printf '%s\n' "${exact_app_pids}" | awk 'NF { count++ } END { print count + 0 }')"
  exact_app_first="$(printf '%s\n' "${exact_app_pids}" | awk 'NF { print; exit }')"
  [[ "${exact_app_count}" -eq 1 && "${exact_app_first}" == "${app_pid}" ]] || \
    die "${arch} second launch left multiple packaged app instances: ${exact_app_pids:-none}"
  matching_helpers=0
  for pid in $(pgrep -P "${app_pid}" 2>/dev/null || true); do
    command="$(ps -p "${pid}" -o command= 2>/dev/null || true)"
    [[ "${command}" == *"${helper_binary}"* ]] && matching_helpers=$((matching_helpers + 1))
  done
  [[ "${matching_helpers}" -eq 1 ]] || die "${arch} second launch changed helper ownership (${matching_helpers} helpers)"

  log "Quitting ${arch} app"
  osascript -e "tell application \"${APP_PATH}\" to quit" >/dev/null
  wait_for_exit "${app_pid}" || die "${arch} app did not exit cleanly"
  wait_for_exit "${helper_pid}" || die "${arch} helper survived app quit"
  app_pid=""
  helper_pid=""
done

log "Native macOS app/helper launch and ownership smoke passed for: ${arches}"
