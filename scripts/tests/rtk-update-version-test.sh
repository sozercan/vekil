#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
UPDATER="${REPO_ROOT}/scripts/update-rtk-version.sh"
TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/vekil-rtk-update.XXXXXX")"

cleanup() {
  rm -rf "${TMP_ROOT}"
}
trap cleanup EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  local path="$1"
  local expected="$2"
  grep -Fqx "${expected}" "${path}" || fail "${path} does not contain: ${expected}"
}

sha256_file() {
  shasum -a 256 "$1" | awk '{print $1}'
}

file_uri() {
  python3 - "$1" <<'PY'
from pathlib import Path
import sys
print(Path(sys.argv[1]).resolve().as_uri())
PY
}

write_dockerfile() {
  local path="$1"
  local amd64_sha="${2:-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}"
  local arm64_sha="${3:-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb}"
  cat >"${path}" <<EOF_DOCKERFILE
FROM scratch
ARG RTK_VERSION=0.43.0
RUN set -eux; \\
    rtk_sha256_amd64="${amd64_sha}"; \\
    rtk_sha256_arm64="${arm64_sha}"; \\
    echo done
EOF_DOCKERFILE
}

write_release_assets() {
  local directory="$1"
  local amd64_archive="${directory}/rtk-x86_64-unknown-linux-musl.tar.gz"
  local arm64_archive="${directory}/rtk-aarch64-unknown-linux-gnu.tar.gz"
  mkdir -p "${directory}"
  printf 'reviewed amd64 RTK fixture\n' >"${amd64_archive}"
  printf 'reviewed arm64 RTK fixture\n' >"${arm64_archive}"
  printf '%s  %s\n' "$(sha256_file "${amd64_archive}")" "$(basename "${amd64_archive}")" >"${directory}/checksums.txt"
  printf '%s  %s\n' "$(sha256_file "${arm64_archive}")" "$(basename "${arm64_archive}")" >>"${directory}/checksums.txt"
}

run_update() {
  local dockerfile="$1"
  local asset_directory="$2"
  local output_file="$3"
  env \
    GITHUB_OUTPUT="${output_file}" \
    RTK_UPDATE_DOCKERFILE="${dockerfile}" \
    RTK_VERSION_OVERRIDE="v0.44.0" \
    RTK_UPDATE_ASSET_BASE_URL="$(file_uri "${asset_directory}")" \
    "${UPDATER}"
}

valid_assets="${TMP_ROOT}/valid-assets"
write_release_assets "${valid_assets}"
expected_amd64="$(sha256_file "${valid_assets}/rtk-x86_64-unknown-linux-musl.tar.gz")"
expected_arm64="$(sha256_file "${valid_assets}/rtk-aarch64-unknown-linux-gnu.tar.gz")"

valid_dockerfile="${TMP_ROOT}/Dockerfile.valid"
valid_output="${TMP_ROOT}/valid-output"
write_dockerfile "${valid_dockerfile}"
run_update "${valid_dockerfile}" "${valid_assets}" "${valid_output}"
assert_contains "${valid_dockerfile}" "ARG RTK_VERSION=0.44.0"
grep -Fq "rtk_sha256_amd64=\"${expected_amd64}\";" "${valid_dockerfile}" || fail "amd64 hash was not updated"
grep -Fq "rtk_sha256_arm64=\"${expected_arm64}\";" "${valid_dockerfile}" || fail "arm64 hash was not updated"
assert_contains "${valid_output}" "current-version=0.43.0"
assert_contains "${valid_output}" "latest-version=0.44.0"
assert_contains "${valid_output}" "changed=true"

second_dockerfile="${TMP_ROOT}/Dockerfile.second"
second_output="${TMP_ROOT}/second-output"
write_dockerfile "${second_dockerfile}"
run_update "${second_dockerfile}" "${valid_assets}" "${second_output}"
cmp -s "${valid_dockerfile}" "${second_dockerfile}" || fail "identical inputs did not produce identical Dockerfiles"

no_fetch_output="${TMP_ROOT}/no-fetch-output"
env \
  GITHUB_OUTPUT="${no_fetch_output}" \
  RTK_UPDATE_DOCKERFILE="${valid_dockerfile}" \
  RTK_VERSION_OVERRIDE="0.44.0" \
  RTK_UPDATE_ASSET_BASE_URL="file:///does/not/exist" \
  "${UPDATER}"
assert_contains "${no_fetch_output}" "changed=false"

tampered_assets="${TMP_ROOT}/tampered-assets"
write_release_assets "${tampered_assets}"
printf 'tampered after checksum publication\n' >>"${tampered_assets}/rtk-x86_64-unknown-linux-musl.tar.gz"
tampered_dockerfile="${TMP_ROOT}/Dockerfile.tampered"
write_dockerfile "${tampered_dockerfile}"
cp "${tampered_dockerfile}" "${tampered_dockerfile}.before"
if run_update "${tampered_dockerfile}" "${tampered_assets}" "${TMP_ROOT}/tampered-output" >"${TMP_ROOT}/tampered.log" 2>&1; then
  fail "updater accepted an archive that did not match checksums.txt"
fi
cmp -s "${tampered_dockerfile}.before" "${tampered_dockerfile}" || fail "failed update partially modified the Dockerfile"
grep -Fq 'does not match checksums.txt' "${TMP_ROOT}/tampered.log" || fail "tampered archive failure was not explicit"

missing_metadata_assets="${TMP_ROOT}/missing-metadata-assets"
write_release_assets "${missing_metadata_assets}"
grep -v 'aarch64-unknown-linux-gnu' "${missing_metadata_assets}/checksums.txt" >"${missing_metadata_assets}/checksums.filtered"
mv "${missing_metadata_assets}/checksums.filtered" "${missing_metadata_assets}/checksums.txt"
missing_metadata_dockerfile="${TMP_ROOT}/Dockerfile.missing-metadata"
write_dockerfile "${missing_metadata_dockerfile}"
cp "${missing_metadata_dockerfile}" "${missing_metadata_dockerfile}.before"
if run_update "${missing_metadata_dockerfile}" "${missing_metadata_assets}" "${TMP_ROOT}/missing-output" >"${TMP_ROOT}/missing.log" 2>&1; then
  fail "updater accepted incomplete trust metadata"
fi
cmp -s "${missing_metadata_dockerfile}.before" "${missing_metadata_dockerfile}" || fail "missing metadata partially modified the Dockerfile"
grep -Fq 'missing required RTK assets' "${TMP_ROOT}/missing.log" || fail "missing checksum failure was not explicit"

malformed_dockerfile="${TMP_ROOT}/Dockerfile.malformed"
write_dockerfile "${malformed_dockerfile}" "not-a-sha256"
cp "${malformed_dockerfile}" "${malformed_dockerfile}.before"
if env \
  RTK_UPDATE_DOCKERFILE="${malformed_dockerfile}" \
  RTK_VERSION_OVERRIDE="0.44.0" \
  RTK_UPDATE_ASSET_BASE_URL="$(file_uri "${valid_assets}")" \
  "${UPDATER}" >"${TMP_ROOT}/malformed.log" 2>&1; then
  fail "updater accepted malformed reviewed trust metadata"
fi
cmp -s "${malformed_dockerfile}.before" "${malformed_dockerfile}" || fail "malformed metadata partially modified the Dockerfile"
grep -Fq 'invalid pinned RTK amd64 SHA-256' "${TMP_ROOT}/malformed.log" || fail "malformed pinned hash failure was not explicit"

printf 'PASS: RTK updater writes verified per-architecture hashes atomically and rejects tampering\n'
