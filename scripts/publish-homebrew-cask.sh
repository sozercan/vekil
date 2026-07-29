#!/usr/bin/env bash

set -euo pipefail

export LC_ALL=C
umask 022

usage() {
  printf 'usage: %s <version> <sha256> <tap-dir>\n' "$0" >&2
}

fail() {
  printf 'error: %s\n' "$1" >&2
  exit 1
}

validate_version() {
  local candidate="$1"
  local prerelease identifier

  if [[ ! "${candidate}" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]]; then
    fail "version must be SemVer without a leading v or build metadata"
  fi

  if [[ "${candidate}" != *-* ]]; then
    return
  fi

  prerelease="${candidate#*-}"
  while :; do
    identifier="${prerelease%%.*}"
    if [[ "${identifier}" =~ ^[0-9]+$ && ${#identifier} -gt 1 && "${identifier}" == 0* ]]; then
      fail "numeric prerelease identifiers must not contain leading zeroes"
    fi
    if [[ "${prerelease}" != *.* ]]; then
      break
    fi
    prerelease="${prerelease#*.}"
  done
}

if [[ $# -ne 3 ]]; then
  usage
  exit 1
fi

version="$1"
sha256="$2"
tap_dir="$3"

validate_version "${version}"

if [[ ! "${sha256}" =~ ^[0-9A-Fa-f]{64}$ ]]; then
  fail "sha256 must contain exactly 64 hexadecimal characters"
fi
sha256="$(printf '%s' "${sha256}" | tr 'A-F' 'a-f')"

if [[ ! -d "${tap_dir}" ]]; then
  fail "tap directory does not exist or is not a directory: ${tap_dir}"
fi

tap_dir="$(cd "${tap_dir}" && pwd -P)"
casks_dir="${tap_dir}/Casks"
cask_path="${casks_dir}/vekil.rb"

if [[ -L "${casks_dir}" ]]; then
  fail "refusing to write through symbolic link: ${casks_dir}"
fi
if [[ -e "${casks_dir}" && ! -d "${casks_dir}" ]]; then
  fail "cask path exists but is not a directory: ${casks_dir}"
fi
if [[ ! -e "${casks_dir}" ]]; then
  mkdir "${casks_dir}"
fi
if [[ -L "${cask_path}" ]]; then
  fail "refusing to replace symbolic link: ${cask_path}"
fi
if [[ -e "${cask_path}" && ! -f "${cask_path}" ]]; then
  fail "cask path exists but is not a regular file: ${cask_path}"
fi

tmp_path="$(mktemp "${casks_dir}/.vekil.rb.tmp.XXXXXX")"
cleanup() {
  if [[ -n "${tmp_path:-}" ]]; then
    rm -f "${tmp_path}"
  fi
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

cat >"${tmp_path}" <<EOF_CASK
cask "vekil" do
  version "${version}"
  sha256 "${sha256}"

  url "https://github.com/sozercan/vekil/releases/download/v#{version}/vekil-macos-arm64.zip"
  name "Vekil"
  desc "Proxy Anthropic, Gemini, and OpenAI clients through GitHub Copilot"
  homepage "https://github.com/sozercan/vekil"

  depends_on arch: :arm64

  app "Vekil.app"

  postflight do
    system_command "/usr/bin/xattr", args: ["-cr", "#{appdir}/Vekil.app"], sudo: false
  end

  zap trash: [
    "~/.config/vekil",
    "~/Library/LaunchAgents/com.vekil.menubar.plist",
  ]
end
EOF_CASK
chmod 0644 "${tmp_path}"

if [[ -f "${cask_path}" ]] && cmp -s "${tmp_path}" "${cask_path}"; then
  chmod 0644 "${cask_path}"
  rm -f "${tmp_path}"
  tmp_path=""
  exit 0
fi

mv -f "${tmp_path}" "${cask_path}"
tmp_path=""
