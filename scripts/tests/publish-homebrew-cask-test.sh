#!/usr/bin/env bash

set -euo pipefail

export LC_ALL=C

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
RENDERER="${REPO_ROOT}/scripts/publish-homebrew-cask.sh"
TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/vekil-homebrew-cask-test.XXXXXX")"
SHA_LOWER="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
SHA_UPPER="0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF"

cleanup() {
  rm -rf "${TMP_ROOT}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

expect_failure() {
  local name="$1"
  shift
  if "$@" >"${TMP_ROOT}/${name}.stdout" 2>"${TMP_ROOT}/${name}.stderr"; then
    fail "${name}: command unexpectedly succeeded"
  fi
}

assert_file_equals() {
  local expected="$1"
  local actual="$2"
  local name="$3"
  if ! cmp -s "${expected}" "${actual}"; then
    printf '%s\n' "--- expected (${expected})" >&2
    cat "${expected}" >&2
    printf '%s\n' "--- actual (${actual})" >&2
    cat "${actual}" >&2
    fail "${name}: rendered content differs"
  fi
}

file_mode() {
  local path="$1"
  if stat -f '%Lp' "${path}" >/dev/null 2>&1; then
    stat -f '%Lp' "${path}"
  else
    stat -c '%a' "${path}"
  fi
}

sha256_file() {
  local path="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${path}" | awk '{print $1}'
  else
    shasum -a 256 "${path}" | awk '{print $1}'
  fi
}

[[ -x "${RENDERER}" ]] || fail "renderer is not executable"

valid_tap="${TMP_ROOT}/tap with spaces"
mkdir -p "${valid_tap}"
(
  umask 077
  "${RENDERER}" "1.2.3" "${SHA_UPPER}" "${valid_tap}"
)

cat >"${TMP_ROOT}/expected.rb" <<EOF_CASK
cask "vekil" do
  version "1.2.3"
  sha256 "${SHA_LOWER}"

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

cask_path="${valid_tap}/Casks/vekil.rb"
assert_file_equals "${TMP_ROOT}/expected.rb" "${cask_path}" "stable release"
[[ "$(file_mode "${cask_path}")" == "644" ]] || fail "rendered cask mode is not 0644"

before_hash="$(sha256_file "${cask_path}")"
chmod 0600 "${cask_path}"
"${RENDERER}" "1.2.3" "${SHA_LOWER}" "${valid_tap}"
after_hash="$(sha256_file "${cask_path}")"
[[ "${before_hash}" == "${after_hash}" ]] || fail "repeat render changed output bytes"
[[ "$(file_mode "${cask_path}")" == "644" ]] || fail "repeat render did not restore mode 0644"
leftover_tmp="$(find "${valid_tap}/Casks" -maxdepth 1 -name '.vekil.rb.tmp.*' -print -quit)"
[[ -z "${leftover_tmp}" ]] || fail "renderer left a temporary file behind"

prerelease_tap="${TMP_ROOT}/prerelease"
mkdir -p "${prerelease_tap}"
"${RENDERER}" "2.0.0-rc.1" "${SHA_LOWER}" "${prerelease_tap}"
grep -Fxq '  version "2.0.0-rc.1"' "${prerelease_tap}/Casks/vekil.rb" || fail "valid prerelease was not rendered"

expect_failure wrong-argument-count "${RENDERER}" "1.2.3" "${SHA_LOWER}"
invalid_index=0
for version in \
  "v1.2.3" \
  "1.2" \
  "01.2.3" \
  "1.02.3" \
  "1.2.03" \
  "1.2.3+build.1" \
  "1.2.3-rc.01" \
  '1.2.3"; system "false"; #'; do
  invalid_index=$((invalid_index + 1))
  case_dir="${TMP_ROOT}/invalid-version-${invalid_index}"
  mkdir -p "${case_dir}"
  expect_failure "invalid-version-${invalid_index}" "${RENDERER}" "${version}" "${SHA_LOWER}" "${case_dir}"
  [[ ! -e "${case_dir}/Casks/vekil.rb" ]] || fail "invalid version produced a cask: ${version}"
done

invalid_index=0
for checksum in \
  "abc123" \
  "${SHA_LOWER}0" \
  "g123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" \
  "${SHA_LOWER} "; do
  invalid_index=$((invalid_index + 1))
  case_dir="${TMP_ROOT}/invalid-sha-${invalid_index}"
  mkdir -p "${case_dir}"
  expect_failure "invalid-sha-${invalid_index}" "${RENDERER}" "1.2.3" "${checksum}" "${case_dir}"
  [[ ! -e "${case_dir}/Casks/vekil.rb" ]] || fail "invalid checksum produced a cask"
done

missing_tap="${TMP_ROOT}/missing"
expect_failure missing-tap "${RENDERER}" "1.2.3" "${SHA_LOWER}" "${missing_tap}"
[[ ! -e "${missing_tap}" ]] || fail "missing tap directory was created"

tap_file="${TMP_ROOT}/tap-file"
printf 'not a directory\n' >"${tap_file}"
expect_failure tap-is-file "${RENDERER}" "1.2.3" "${SHA_LOWER}" "${tap_file}"

sentinel_tap="${TMP_ROOT}/sentinel"
mkdir -p "${sentinel_tap}/Casks"
printf 'sentinel\n' >"${sentinel_tap}/Casks/vekil.rb"
expect_failure invalid-input-preserves-output "${RENDERER}" "not-a-version" "${SHA_LOWER}" "${sentinel_tap}"
[[ "$(cat "${sentinel_tap}/Casks/vekil.rb")" == "sentinel" ]] || fail "invalid input changed an existing cask"

symlink_tap="${TMP_ROOT}/symlink-casks"
symlink_target="${TMP_ROOT}/outside-casks"
mkdir -p "${symlink_tap}" "${symlink_target}"
ln -s "${symlink_target}" "${symlink_tap}/Casks"
expect_failure casks-symlink "${RENDERER}" "1.2.3" "${SHA_LOWER}" "${symlink_tap}"
[[ ! -e "${symlink_target}/vekil.rb" ]] || fail "renderer wrote through a Casks symlink"

target_symlink_tap="${TMP_ROOT}/target-symlink"
mkdir -p "${target_symlink_tap}/Casks"
target_file="${TMP_ROOT}/outside-vekil.rb"
printf 'outside\n' >"${target_file}"
ln -s "${target_file}" "${target_symlink_tap}/Casks/vekil.rb"
expect_failure target-symlink "${RENDERER}" "1.2.3" "${SHA_LOWER}" "${target_symlink_tap}"
[[ "$(cat "${target_file}")" == "outside" ]] || fail "renderer changed a symlink target"

if command -v ruby >/dev/null 2>&1; then
  ruby -c "${cask_path}" >/dev/null || fail "rendered cask is not valid Ruby syntax"
fi

printf 'PASS: publish-homebrew-cask renderer\n'
