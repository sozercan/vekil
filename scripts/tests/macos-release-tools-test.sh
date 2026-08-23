#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
MANIFEST_TOOL="${REPO_ROOT}/scripts/macos-release-manifest.py"
APPCAST_TOOL="${REPO_ROOT}/scripts/verify-sparkle-appcast.py"
CONFIG="${REPO_ROOT}/build-support/macos/app-config.json"
TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/vekil-release-tools-test.XXXXXX")"
trap 'rm -rf "${TMP_ROOT}"' EXIT

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

for command in base64 openssl tail; do
  command -v "${command}" >/dev/null 2>&1 || fail "missing required command: ${command}"
done

expect_failure() {
  if "$@" >"${TMP_ROOT}/unexpected.stdout" 2>"${TMP_ROOT}/unexpected.stderr"; then
    cat "${TMP_ROOT}/unexpected.stdout" "${TMP_ROOT}/unexpected.stderr" >&2 || true
    fail "command unexpectedly succeeded: $*"
  fi
}

"${MANIFEST_TOOL}" validate-config --config "${CONFIG}"
expect_failure "${MANIFEST_TOOL}" resolve \
  --config "${CONFIG}" \
  --output "${TMP_ROOT}/missing-build.json" \
  --marketing-version 0.15.0 \
  --source-commit 06a7fa15af95d611b73280be8823089073fbd9f0 \
  --release
expect_failure "${MANIFEST_TOOL}" resolve \
  --config "${CONFIG}" \
  --output "${TMP_ROOT}/nonnumeric-build.json" \
  --marketing-version 0.15.0 \
  --bundle-version 0.15.0 \
  --bundle-build-id invalid-version-test \
  --previous-bundle-version 14000 \
  --source-commit 06a7fa15af95d611b73280be8823089073fbd9f0 \
  --release
expect_failure "${MANIFEST_TOOL}" resolve \
  --config "${CONFIG}" \
  --output "${TMP_ROOT}/nonmonotonic-build.json" \
  --marketing-version 0.15.0 \
  --bundle-version 14000 \
  --bundle-build-id nonmonotonic-test \
  --previous-bundle-version 14000 \
  --source-commit 06a7fa15af95d611b73280be8823089073fbd9f0 \
  --release

manifest="${TMP_ROOT}/vekil-macos-release.json"
"${MANIFEST_TOOL}" resolve \
  --config "${CONFIG}" \
  --output "${manifest}" \
  --marketing-version 0.15.0 \
  --bundle-version 15001 \
  --bundle-build-id vekil-15001-testfixture \
  --previous-bundle-version 14000 \
  --source-commit 06a7fa15af95d611b73280be8823089073fbd9f0 \
  --release
"${MANIFEST_TOOL}" validate --manifest "${manifest}"
[[ "$("${MANIFEST_TOOL}" get --file "${manifest}" --key bundle_version)" == 15001 ]] || fail "numeric bundle version was not preserved"
[[ "$("${MANIFEST_TOOL}" compare-bundle-versions 15001 14000)" == 1 ]] || fail "bundle version comparison failed"
[[ "$("${MANIFEST_TOOL}" compare-system-versions 13.0 13.0.0)" == 0 ]] || fail "system version comparison failed"

openssl genpkey -algorithm Ed25519 -out "${TMP_ROOT}/test-key.pem" >/dev/null 2>&1
openssl pkey -in "${TMP_ROOT}/test-key.pem" -pubout -outform DER -out "${TMP_ROOT}/test-public.der"
public_key="$(tail -c 32 "${TMP_ROOT}/test-public.der" | base64 | tr -d '\r\n')"
python3 - "${manifest}" "${public_key}" <<'PY_SPARKLE_KEY'
import json
import sys
from pathlib import Path

path = Path(sys.argv[1])
value = json.loads(path.read_text())
value["sparkle"]["public_ed_key"] = sys.argv[2]
path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")
PY_SPARKLE_KEY
"${MANIFEST_TOOL}" validate --manifest "${manifest}"

native_plist="${TMP_ROOT}/Info.plist"
legacy_plist="${TMP_ROOT}/Legacy-Info.plist"
"${MANIFEST_TOOL}" plist --manifest "${manifest}" --output "${native_plist}"
"${MANIFEST_TOOL}" plist --legacy --manifest "${manifest}" --output "${legacy_plist}"
python3 - "${native_plist}" "${legacy_plist}" <<'PY'
import plistlib
import sys
from pathlib import Path

with Path(sys.argv[1]).open("rb") as handle:
    native = plistlib.load(handle)
with Path(sys.argv[2]).open("rb") as handle:
    legacy = plistlib.load(handle)
assert native["CFBundleExecutable"] == "Vekil"
assert native["CFBundleShortVersionString"] == "0.15.0"
assert native["CFBundleVersion"] == "15001"
assert native["LSMinimumSystemVersion"] == "13.0"
assert native["LSUIElement"] is True
assert native["SURequireSignedFeed"] is True
assert native["SUVerifyUpdateBeforeExtraction"] is True
assert native["VekilBundleBuildID"] == "vekil-15001-testfixture"
assert "SUEnableInstallerLauncherService" not in native
assert legacy["CFBundleExecutable"] == "vekil-menubar"
assert legacy["LSMinimumSystemVersion"] == "10.13"
assert legacy["SUEnableInstallerLauncherService"] is True
PY

artifact="${TMP_ROOT}/vekil-macos-universal.zip"
printf 'zip-fixture' >"${artifact}"
artifact_size="$(wc -c <"${artifact}" | tr -d ' ')"
openssl pkeyutl -sign \
  -inkey "${TMP_ROOT}/test-key.pem" \
  -rawin \
  -in "${artifact}" \
  -out "${TMP_ROOT}/artifact-signature"
signature="$(base64 <"${TMP_ROOT}/artifact-signature" | tr -d '\r\n')"
appcast="${TMP_ROOT}/appcast.xml"
cat >"${appcast}" <<EOF_APPCAST
<?xml version="1.0"?>
<rss xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle" version="2.0">
  <channel>
    <item>
      <title>0.15.0</title>
      <sparkle:version>15001</sparkle:version>
      <sparkle:shortVersionString>0.15.0</sparkle:shortVersionString>
      <sparkle:minimumSystemVersion>13.0</sparkle:minimumSystemVersion>
      <enclosure url="https://example.invalid/releases/v0.15.0/vekil-macos-universal.zip" length="${artifact_size}" type="application/octet-stream" sparkle:edSignature="${signature}"/>
    </item>
    <item>
      <title>0.14.0</title>
      <sparkle:version>0.14.0</sparkle:version>
      <sparkle:shortVersionString>0.14.0</sparkle:shortVersionString>
      <sparkle:minimumSystemVersion>10.13</sparkle:minimumSystemVersion>
      <sparkle:hardwareRequirements>arm64</sparkle:hardwareRequirements>
      <enclosure url="https://example.invalid/releases/v0.14.0/vekil-macos-arm64.zip" length="1" type="application/octet-stream" sparkle:edSignature="${signature}"/>
    </item>
  </channel>
</rss><!-- sparkle-signatures:
edSignature: ${signature}
length: 1
-->
EOF_APPCAST
"${APPCAST_TOOL}" \
  --appcast "${appcast}" \
  --manifest "${manifest}" \
  --artifact "${artifact}" \
  --expected-url-prefix https://example.invalid/releases/v0.15.0 \
  --require-legacy-compatible-entry >/dev/null

wrong_manifest="${TMP_ROOT}/wrong-key-manifest.json"
cp "${manifest}" "${wrong_manifest}"
openssl genpkey -algorithm Ed25519 -out "${TMP_ROOT}/wrong-key.pem" >/dev/null 2>&1
openssl pkey -in "${TMP_ROOT}/wrong-key.pem" -pubout -outform DER -out "${TMP_ROOT}/wrong-public.der"
wrong_public_key="$(tail -c 32 "${TMP_ROOT}/wrong-public.der" | base64 | tr -d '\r\n')"
python3 - "${wrong_manifest}" "${wrong_public_key}" <<'PY_WRONG_SPARKLE_KEY'
import json
import sys
from pathlib import Path

path = Path(sys.argv[1])
value = json.loads(path.read_text())
value["sparkle"]["public_ed_key"] = sys.argv[2]
path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")
PY_WRONG_SPARKLE_KEY
expect_failure "${APPCAST_TOOL}" \
  --appcast "${appcast}" \
  --manifest "${wrong_manifest}" \
  --artifact "${artifact}"

bad_appcast="${TMP_ROOT}/bad-appcast.xml"
python3 - "${appcast}" "${bad_appcast}" <<'PY_BAD_APPCAST'
from pathlib import Path
import sys
text = Path(sys.argv[1]).read_text()
old = "<sparkle:minimumSystemVersion>13.0</sparkle:minimumSystemVersion>"
new = "<sparkle:minimumSystemVersion>12.0</sparkle:minimumSystemVersion>"
if old not in text:
    raise SystemExit("candidate minimum version fixture not found")
Path(sys.argv[2]).write_text(text.replace(old, new, 1))
PY_BAD_APPCAST
expect_failure "${APPCAST_TOOL}" --appcast "${bad_appcast}" --manifest "${manifest}" --artifact "${artifact}"

sha256="$(shasum -a 256 "${artifact}" | awk '{print $1}')"
sha_file="${artifact}.sha256"
printf '%s  %s\n' "${sha256}" "$(basename "${artifact}")" >"${sha_file}"
expect_failure env MACOS_RELEASE=1 "${REPO_ROOT}/scripts/verify-macos-release-gates.sh" "${artifact}" "${sha_file}"
MACOS_RELEASE=1 \
MACOS_N_MINUS_ONE_UPDATE_TESTED_SHA256="${sha256}" \
MACOS_HOMEBREW_INSTALL_TESTED_SHA256="${sha256}" \
MACOS_FORWARD_REVERT_TESTED_SHA256="${sha256}" \
  "${REPO_ROOT}/scripts/verify-macos-release-gates.sh" "${artifact}" "${sha_file}" >/dev/null
expect_failure env \
  MACOS_RELEASE=1 \
  MACOS_MANAGED_CONFIG_SHIPPING=1 \
  MACOS_N_MINUS_ONE_UPDATE_TESTED_SHA256="${sha256}" \
  MACOS_HOMEBREW_INSTALL_TESTED_SHA256="${sha256}" \
  MACOS_FORWARD_REVERT_TESTED_SHA256="${sha256}" \
  "${REPO_ROOT}/scripts/verify-macos-release-gates.sh" "${artifact}" "${sha_file}"

"${REPO_ROOT}/scripts/publish-homebrew-cask.sh" 0.15.0 "${sha256}" "${TMP_ROOT}/tap"
cask="${TMP_ROOT}/tap/Casks/vekil.rb"
grep -Fq 'vekil-macos-universal.zip' "${cask}" || fail "Homebrew cask does not use universal artifact"
grep -Fq 'depends_on macos: ">= :ventura"' "${cask}" || fail "Homebrew cask does not require macOS 13"
if grep -Eq 'depends_on arch|xattr' "${cask}"; then fail "Homebrew cask retains architecture restriction or quarantine bypass"; fi
if grep -Fq '.config/vekil' "${cask}"; then fail "Homebrew cask must not delete external configuration roots"; fi
if grep -Fq 'Application Support/vekil",' "${cask}"; then fail "Homebrew cask must not recursively delete Application Support"; fi
"${REPO_ROOT}/scripts/publish-homebrew-cask.sh" 0.16.0 "${sha256}" "${TMP_ROOT}/tap"
expect_failure "${REPO_ROOT}/scripts/publish-homebrew-cask.sh" 0.15.0 "${sha256}" "${TMP_ROOT}/tap"

status_output="${TMP_ROOT}/github-output"
set +e
GITHUB_OUTPUT="${status_output}" "${REPO_ROOT}/scripts/macos-native-source-status.sh" --github-output >/dev/null 2>&1
status_rc=$?
set -e
if ! grep -Eq '^ready=(true|false)$' "${status_output}"; then fail "source status did not write GitHub output"; fi
source_state="$(sed -n 's/^state=//p' "${status_output}")"
case "${source_state}" in
  ready|absent) [[ ${status_rc} -eq 0 ]] || fail "source status failed for ${source_state} state" ;;
  partial) [[ ${status_rc} -ne 0 ]] || fail "partial native source tree must fail closed" ;;
  *) fail "unexpected native source state: ${source_state}" ;;
esac

PYTHONPYCACHEPREFIX="${TMP_ROOT}/pycache" python3 -m py_compile \
  "${REPO_ROOT}/scripts/macos-release-manifest.py" \
  "${REPO_ROOT}/scripts/verify-sparkle-appcast.py" \
  "${REPO_ROOT}/scripts/macos-helper-smoke.py"

bash -n \
  "${REPO_ROOT}/scripts/build-macos-app.sh" \
  "${REPO_ROOT}/scripts/fetch-sparkle.sh" \
  "${REPO_ROOT}/scripts/generate-sparkle-appcast.sh" \
  "${REPO_ROOT}/scripts/macos-app-smoke.sh" \
  "${REPO_ROOT}/scripts/macos-native-source-status.sh" \
  "${REPO_ROOT}/scripts/notarize-macos-app.sh" \
  "${REPO_ROOT}/scripts/package-macos-app.sh" \
  "${REPO_ROOT}/scripts/publish-homebrew-cask.sh" \
  "${REPO_ROOT}/scripts/sign-macos-app.sh" \
  "${REPO_ROOT}/scripts/verify-macos-app.sh" \
  "${REPO_ROOT}/scripts/verify-macos-release-artifact.sh" \
  "${REPO_ROOT}/scripts/verify-macos-release-gates.sh" \
  "${REPO_ROOT}/scripts/tests/macos-appcast-generation-test.sh" \
  "${REPO_ROOT}/scripts/tests/macos-bundle-verification-test.sh"

printf 'macOS release tooling tests passed\n'
