#!/usr/bin/env bash

# The callback must return SMOKE_OUTPUT_MISMATCH_STATUS only after a successful
# request whose output did not match. It should terminate the caller for all
# transport, response-shape, and usage failures.
SMOKE_OUTPUT_MISMATCH_STATUS=10

retry_output_mismatch() {
  local label="$1"
  local max_attempts="$2"
  shift 2
  local attempt rc
  local SMOKE_RETRY_ATTEMPT

  [[ "${max_attempts}" =~ ^[1-9][0-9]*$ ]] || return 2
  for ((attempt = 1; attempt <= max_attempts; attempt++)); do
    SMOKE_RETRY_ATTEMPT="${attempt}"
    if "$@"; then
      return 0
    else
      rc=$?
    fi
    [[ "${rc}" -eq "${SMOKE_OUTPUT_MISMATCH_STATUS}" ]] || return "${rc}"
    if (( attempt == max_attempts )); then
      return "${rc}"
    fi
    log "${label} output mismatch on attempt ${attempt}; retrying once"
  done
}
