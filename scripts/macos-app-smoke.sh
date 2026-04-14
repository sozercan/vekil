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

APP_PATH="${APP_PATH:-${REPO_ROOT}/Vekil.app}"
APP_BUNDLE_ID="${APP_BUNDLE_ID:-com.vekil.menubar}"
APP_EXECUTABLE="${APP_EXECUTABLE:-${APP_PATH}/Contents/MacOS/vekil-menubar}"

app_pid=""

cleanup() {
  osascript -e "tell application id \"${APP_BUNDLE_ID}\" to quit" >/dev/null 2>&1 || true

  if [[ -f "${APP_EXECUTABLE}" ]]; then
    pkill -f "${APP_EXECUTABLE}" >/dev/null 2>&1 || true
  fi
}

trap cleanup EXIT

wait_for_pid() {
  local attempt pid

  for attempt in $(seq 1 20); do
    pid="$(lsappinfo info -only pid -app "${APP_BUNDLE_ID}" 2>/dev/null | awk -F= '/"pid"=/{print $2}')"
    if [[ -n "${pid}" ]] && ps -p "${pid}" >/dev/null 2>&1; then
      printf '%s\n' "${pid}"
      return 0
    fi
    sleep 1
  done

  return 1
}

wait_for_exit() {
  local pid="$1"
  local attempt

  for attempt in $(seq 1 20); do
    if ! ps -p "${pid}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done

  return 1
}

main() {
  require_cmd make
  require_cmd open
  require_cmd osascript
  require_cmd lsappinfo
  require_cmd pgrep
  require_cmd pkill

  cd "${REPO_ROOT}"

  log "Building and validating Vekil.app"
  make test-app

  log "Ensuring no stale app instance is running"
  cleanup
  sleep 2

  log "Launching ${APP_PATH}"
  open "${APP_PATH}"

  app_pid="$(wait_for_pid)" || die "app did not appear in Launch Services after launch"
  log "App started with pid ${app_pid}"

  if ! ps -p "${app_pid}" -o command= | grep -Fq "${APP_EXECUTABLE}"; then
    ps -p "${app_pid}" -o pid=,ppid=,etime=,command=
    die "launched process did not match ${APP_EXECUTABLE}"
  fi

  sleep 3
  ps -p "${app_pid}" >/dev/null 2>&1 || die "app exited shortly after launch"

  log "Quitting ${APP_BUNDLE_ID}"
  osascript -e "tell application id \"${APP_BUNDLE_ID}\" to quit" >/dev/null 2>&1 || die "failed to quit app via AppleScript"
  wait_for_exit "${app_pid}" || die "app did not exit cleanly after quit"

  log "macOS app smoke test passed"
}

main "$@"
