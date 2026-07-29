#!/usr/bin/env bash

# Install exact reviewed release tools.
#
# Usage: scripts/release/install-tools.sh <actionlint|govulncheck|gitleaks|syft>...
#
# Binaries are installed in RELEASE_TOOLS_BIN_DIR, then GOBIN, then a
# runner-local directory (when GITHUB_PATH is set), then $HOME/.local/bin.
# The selected directory is appended to GITHUB_PATH and emitted as bin_dir via
# GITHUB_OUTPUT when those files are available. Executing a script cannot alter
# its parent's PATH, so local callers should export PATH="<bin_dir>:$PATH".

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/lib.sh
source "${SCRIPT_DIR}/lib.sh"
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/tool-versions.env"

usage() {
  cat >&2 <<'USAGE'
usage: scripts/release/install-tools.sh <tool>...

tools: actionlint govulncheck gitleaks syft
USAGE
  exit 2
}

[[ "$#" -gt 0 ]] || usage
release_require_cmd curl
release_require_cmd tar
release_require_cmd install

if [[ -n "${RELEASE_TOOLS_BIN_DIR:-}" ]]; then
  bin_dir="${RELEASE_TOOLS_BIN_DIR}"
elif [[ -n "${GOBIN:-}" ]]; then
  bin_dir="${GOBIN}"
elif [[ -n "${GITHUB_PATH:-}" ]]; then
  bin_dir="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/vekil-release-tools/bin"
else
  bin_dir="${HOME}/.local/bin"
fi
mkdir -p "${bin_dir}"
bin_dir="$(cd "${bin_dir}" && pwd)"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "${os}" in
  darwin | linux) ;;
  *) release_die "unsupported operating system: ${os}" ;;
esac

machine="$(uname -m)"
case "${machine}" in
  x86_64 | amd64) arch="amd64" ;;
  arm64 | aarch64) arch="arm64" ;;
  *) release_die "unsupported architecture: ${machine}" ;;
esac

checksum_for() {
  local tool_upper="$1"
  local os_upper arch_upper key value
  os_upper="$(printf '%s' "${os}" | tr '[:lower:]' '[:upper:]')"
  arch_upper="$(printf '%s' "${arch}" | tr '[:lower:]' '[:upper:]')"
  key="${tool_upper}_${os_upper}_${arch_upper}_SHA256"
  value="${!key:-}"
  [[ "${value}" =~ ^[0-9a-f]{64}$ ]] || release_die "missing reviewed checksum: ${key}"
  printf '%s\n' "${value}"
}

download_archive_tool() {
  local tool="$1"
  local version="$2"
  local asset="$3"
  local url="$4"
  local expected_sha="$5"
  local tmp_dir archive actual_sha binary

  tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/vekil-${tool}.XXXXXX")"
  archive="${tmp_dir}/${asset}"
  curl --fail --silent --show-error --location \
    --proto '=https' --proto-redir '=https' --tlsv1.2 --retry 3 \
    --connect-timeout 15 --max-time 300 \
    --output "${archive}" "${url}"
  actual_sha="$(release_sha256_file "${archive}")"
  if [[ "${actual_sha}" != "${expected_sha}" ]]; then
    release_cleanup_dir "${tmp_dir}"
    release_die "${tool} ${version} archive checksum mismatch"
  fi

  tar -xzf "${archive}" -C "${tmp_dir}"
  binary="${tmp_dir}/${tool}"
  [[ -f "${binary}" ]] || {
    release_cleanup_dir "${tmp_dir}"
    release_die "${tool} archive did not contain ${tool}"
  }
  install -m 0755 "${binary}" "${bin_dir}/${tool}"
  release_cleanup_dir "${tmp_dir}"
}

install_actionlint() {
  local asset="actionlint_${ACTIONLINT_VERSION}_${os}_${arch}.tar.gz"
  local url="https://github.com/rhysd/actionlint/releases/download/v${ACTIONLINT_VERSION}/${asset}"
  local version_output actual_version
  download_archive_tool actionlint "${ACTIONLINT_VERSION}" "${asset}" "${url}" "$(checksum_for ACTIONLINT)"
  version_output="$("${bin_dir}/actionlint" -version)"
  actual_version="${version_output%%$'\n'*}"
  [[ "${actual_version}" == "${ACTIONLINT_VERSION}" ]] || release_die "installed actionlint version did not match ${ACTIONLINT_VERSION}"
}

install_gitleaks() {
  local asset_arch="${arch}"
  [[ "${arch}" == "amd64" ]] && asset_arch="x64"
  local asset="gitleaks_${GITLEAKS_VERSION}_${os}_${asset_arch}.tar.gz"
  local url="https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}/${asset}"
  download_archive_tool gitleaks "${GITLEAKS_VERSION}" "${asset}" "${url}" "$(checksum_for GITLEAKS)"
  [[ "$("${bin_dir}/gitleaks" version)" == "${GITLEAKS_VERSION}" ]] || release_die "installed gitleaks version did not match ${GITLEAKS_VERSION}"
}

install_syft() {
  local asset="syft_${SYFT_VERSION}_${os}_${arch}.tar.gz"
  local url="https://github.com/anchore/syft/releases/download/v${SYFT_VERSION}/${asset}"
  local version_output actual_version
  download_archive_tool syft "${SYFT_VERSION}" "${asset}" "${url}" "$(checksum_for SYFT)"
  version_output="$("${bin_dir}/syft" version 2>&1)"
  actual_version="$(printf '%s\n' "${version_output}" | awk '/^[[:space:]]*Version:/ { print $2; exit }')"
  [[ "${actual_version}" == "${SYFT_VERSION}" ]] || release_die "installed syft version did not match ${SYFT_VERSION}"
}

install_govulncheck() {
  local tmp_dir metadata module_sum gomod_sum version_output
  release_require_cmd go
  release_require_cmd python3
  tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/vekil-govulncheck.XXXXXX")"
  metadata="${tmp_dir}/module.json"
  go mod download -json "${GOVULNCHECK_MODULE_ROOT}@${GOVULNCHECK_VERSION}" >"${metadata}"
  read -r module_sum gomod_sum < <(python3 - "${metadata}" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    value = json.load(handle)
print(value.get("Sum", ""), value.get("GoModSum", ""))
PY
)
  if [[ "${module_sum}" != "${GOVULNCHECK_MODULE_SUM}" || "${gomod_sum}" != "${GOVULNCHECK_GOMOD_SUM}" ]]; then
    release_cleanup_dir "${tmp_dir}"
    release_die "govulncheck module checksum mismatch for ${GOVULNCHECK_VERSION}"
  fi
  GOBIN="${bin_dir}" go install "${GOVULNCHECK_MODULE}@${GOVULNCHECK_VERSION}"
  version_output="$("${bin_dir}/govulncheck" -version 2>&1)"
  if [[ "${version_output}" != *"govulncheck@${GOVULNCHECK_VERSION}"* ]]; then
    release_cleanup_dir "${tmp_dir}"
    release_die "installed govulncheck version did not match ${GOVULNCHECK_VERSION}"
  fi
  release_cleanup_dir "${tmp_dir}"
}

seen=" "
for tool in "$@"; do
  case " ${seen} " in
    *" ${tool} "*) continue ;;
  esac
  case "${tool}" in
    actionlint) install_actionlint ;;
    govulncheck) install_govulncheck ;;
    gitleaks) install_gitleaks ;;
    syft) install_syft ;;
    *) usage ;;
  esac
  seen+="${tool} "
  release_log "installed ${tool} in ${bin_dir}"
done

if [[ -n "${GITHUB_PATH:-}" ]]; then
  printf '%s\n' "${bin_dir}" >>"${GITHUB_PATH}"
fi
release_write_output bin_dir "${bin_dir}"
printf '%s\n' "${bin_dir}"
