#!/usr/bin/env bash

# Shared shell helpers for release scripts. This file is sourced, not executed.

release_log() {
  printf '==> %s\n' "$*" >&2
}

release_die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

release_require_cmd() {
  command -v "$1" >/dev/null 2>&1 || release_die "missing required command: $1"
}

release_write_output() {
  local key="$1"
  local value="$2"
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    printf '%s=%s\n' "${key}" "${value}" >>"${GITHUB_OUTPUT}"
  fi
}

release_sha256_file() {
  local path="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${path}" | awk '{print $1}'
    return
  fi
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${path}" | awk '{print $1}'
    return
  fi
  release_die "sha256sum or shasum is required"
}

release_cleanup_dir() {
  local path="$1"
  [[ -n "${path}" && "${path}" != "/" && -d "${path}" ]] || return 0
  rm -rf -- "${path}"
}
