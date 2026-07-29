#!/usr/bin/env bash

# shellcheck disable=SC2016 # bash -c snippets intentionally expand positional parameters in the child shell.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
RELEASE_DIR="${REPO_ROOT}/scripts/release"
GOOD_FIXTURE="${RELEASE_DIR}/testdata/contract/good"
TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/vekil-release-tests.XXXXXX")"
PIDS=()

cleanup() {
  local pid
  set +u
  for pid in "${PIDS[@]}"; do
    kill "${pid}" 2>/dev/null || true
    wait "${pid}" 2>/dev/null || true
  done
  rm -rf -- "${TMP_ROOT}"
}
trap cleanup EXIT

pass_count=0
pass() {
  pass_count=$((pass_count + 1))
  printf 'ok %d - %s\n' "${pass_count}" "$1"
}

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

expect_failure() {
  local description="$1"
  local expected="$2"
  shift 2
  local output="${TMP_ROOT}/failure-${pass_count}.log"
  if "$@" >"${output}" 2>&1; then
    cat "${output}" >&2
    fail "${description}: command unexpectedly succeeded"
  fi
  if [[ -n "${expected}" ]] && ! grep -Fq -- "${expected}" "${output}"; then
    cat "${output}" >&2
    fail "${description}: expected diagnostic not found: ${expected}"
  fi
  pass "${description}"
}

wait_for_file() {
  local path="$1"
  local attempt
  for ((attempt = 0; attempt < 200; attempt++)); do
    [[ -s "${path}" ]] && return 0
    sleep 0.05
  done
  return 1
}

# Exact tool installation and version checks.
TOOLS_BIN="${TMP_ROOT}/bin"
TOOLS_GITHUB_PATH="${TMP_ROOT}/github-path"
TOOLS_GITHUB_OUTPUT="${TMP_ROOT}/github-output"
RELEASE_TOOLS_BIN_DIR="${TOOLS_BIN}" GITHUB_PATH="${TOOLS_GITHUB_PATH}" GITHUB_OUTPUT="${TOOLS_GITHUB_OUTPUT}" \
  "${RELEASE_DIR}/install-tools.sh" actionlint govulncheck gitleaks syft >/dev/null
TOOLS_BIN="$(cd "${TOOLS_BIN}" && pwd)"
export PATH="${TOOLS_BIN}:${PATH}"
grep -Fxq "${TOOLS_BIN}" "${TOOLS_GITHUB_PATH}" || fail "installer did not append GITHUB_PATH"
grep -Fxq "bin_dir=${TOOLS_BIN}" "${TOOLS_GITHUB_OUTPUT}" || fail "installer did not write GITHUB_OUTPUT"
[[ "$(actionlint -version | head -n 1)" == "1.7.12" ]] || fail "actionlint version"
[[ "$(gitleaks version)" == "8.30.1" ]] || fail "gitleaks version"
syft version | grep -Eq '^[[:space:]]*Version:[[:space:]]*1\.50\.0$' || fail "syft version"
govuln_version="$(govulncheck -version 2>&1)"
[[ "${govuln_version}" == *'govulncheck@v1.1.4'* ]] || fail "govulncheck version"
pass "install-tools installs every exact reviewed version"

# Positive workflow contract fixture.
"${RELEASE_DIR}/test-release-workflow.sh" \
  --workflow "${GOOD_FIXTURE}/.github/workflows/release.yaml" \
  --workflows-dir "${GOOD_FIXTURE}/.github/workflows" \
  --dockerfile "${GOOD_FIXTURE}/Dockerfile" \
  --actionlint "${TOOLS_BIN}/actionlint" >/dev/null
pass "release workflow positive contract fixture"

mutate_contract_case() {
  local case_name="$1"
  local case_dir="${TMP_ROOT}/contract-${case_name}"
  mkdir -p "${case_dir}"
  cp -R "${GOOD_FIXTURE}/." "${case_dir}/"
  python3 - "${case_dir}/.github/workflows/release.yaml" "${case_dir}/Dockerfile" "${case_name}" <<'PY'
from pathlib import Path
import sys

workflow_path, dockerfile_path, case = Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3]
workflow = workflow_path.read_text()
dockerfile = dockerfile_path.read_text()
mutations = {
    "unpinned-action": ("actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1", "actions/checkout@v4"),
    "top-write": ("permissions:\n  contents: read", "permissions:\n  contents: write"),
    "missing-job-permissions": ("  build-cli:\n    needs: verify-tag\n    runs-on: ubuntu-latest\n    permissions:\n      contents: read", "  build-cli:\n    needs: verify-tag\n    runs-on: ubuntu-latest"),
    "build-write": ("  build-cli:\n    needs: verify-tag\n    runs-on: ubuntu-latest\n    permissions:\n      contents: read", "  build-cli:\n    needs: verify-tag\n    runs-on: ubuntu-latest\n    permissions:\n      contents: write"),
    "clobber": ("gh release upload \"${GITHUB_REF_NAME}\" dist/*", "gh release upload \"${GITHUB_REF_NAME}\" dist/* --clobber"),
    "early-latest": ("  promote-latest:\n    needs: post-publish", "  promote-latest:\n    needs: build-cli"),
    "missing-environment": ("  publish-release:\n    needs: evidence\n    runs-on: ubuntu-latest\n    environment: release", "  publish-release:\n    needs: evidence\n    runs-on: ubuntu-latest"),
    "goreleaser-range": ("version: v2.17.1", "version: \"~> v2\""),
    "cancel-in-progress": ("cancel-in-progress: false", "cancel-in-progress: true"),
    "missing-promotion-concurrency": ("group: release-alias-promotion", "group: release-${GITHUB_REF_NAME}"),
    "persist-credentials": ("persist-credentials: false", "persist-credentials: true"),
    "missing-tag-stage": ("scripts/release/verify-release-tag.sh \"${GITHUB_REF_NAME}\"", "echo tag-check-skipped"),
    "missing-post-publish": ("scripts/release/verify-published-release.py --manifest", "echo post-publish-skipped --manifest"),
    "tap-main-push": ("\"HEAD:refs/heads/release-${GITHUB_REF_NAME}\"", "main"),
    "finalizer-rebuild": ("gh release create \"${GITHUB_REF_NAME}\" --draft", "go build ./...\n          gh release create \"${GITHUB_REF_NAME}\" --draft"),
}
if case == "docker-digest":
    dockerfile = dockerfile.replace("golang:1.26@sha256:" + "1" * 64, "golang:1.26", 1)
    dockerfile_path.write_text(dockerfile)
else:
    old, new = mutations[case]
    if old not in workflow:
        raise SystemExit(f"mutation marker not found for {case}")
    workflow_path.write_text(workflow.replace(old, new, 1))
PY
  printf '%s\n' "${case_dir}"
}

run_contract_case() {
  local case_name="$1"
  local case_dir
  case_dir="$(mutate_contract_case "${case_name}")"
  "${RELEASE_DIR}/test-release-workflow.sh" \
    --workflow "${case_dir}/.github/workflows/release.yaml" \
    --workflows-dir "${case_dir}/.github/workflows" \
    --dockerfile "${case_dir}/Dockerfile" \
    --actionlint "${TOOLS_BIN}/actionlint"
}

expect_failure "contract rejects unpinned action" "not pinned to a full commit SHA" run_contract_case unpinned-action
expect_failure "contract rejects top-level write permissions" "top-level permissions must be explicit and read-only" run_contract_case top-write
expect_failure "contract rejects missing job permissions" "must declare explicit job-level permissions" run_contract_case missing-job-permissions
expect_failure "contract rejects write permissions on build jobs" "read-only build/scan/verification job" run_contract_case build-write
expect_failure "contract rejects clobber uploads" "must not use --clobber" run_contract_case clobber
expect_failure "contract rejects early latest promotion" "must be promoted only after post-publish" run_contract_case early-latest
expect_failure "contract rejects unprotected publisher" "must use a protected release/homebrew environment" run_contract_case missing-environment
expect_failure "contract rejects non-exact GoReleaser" "must use exact version v2.17.1" run_contract_case goreleaser-range
expect_failure "contract rejects cancel-in-progress true" "cancel-in-progress: false" run_contract_case cancel-in-progress
expect_failure "contract rejects per-tag alias promotion concurrency" "queue:max global release-alias-promotion concurrency" run_contract_case missing-promotion-concurrency
expect_failure "contract rejects persisted checkout credentials" "must set persist-credentials: false" run_contract_case persist-credentials
expect_failure "contract rejects missing tag verification stage" "tag/preflight stage" run_contract_case missing-tag-stage
expect_failure "contract rejects missing post-publish verification" "post-publish stage" run_contract_case missing-post-publish
expect_failure "contract rejects direct tap main push" "must not push directly to main" run_contract_case tap-main-push
expect_failure "contract rejects finalizer rebuilds" "must not rebuild artifacts" run_contract_case finalizer-rebuild
expect_failure "contract rejects unpinned Docker base" "Docker base image is not pinned" run_contract_case docker-digest

"${RELEASE_DIR}/test-release-workflow.sh" --actionlint "${TOOLS_BIN}/actionlint" >/dev/null
pass "repository release workflows satisfy the integrated contract"

# SSH signed annotated release tag verification.
TAG_REPO="${TMP_ROOT}/tag-repo"
git init -q -b main "${TAG_REPO}"
git -C "${TAG_REPO}" config user.name "Release Tester"
git -C "${TAG_REPO}" config user.email "release@example.test"
printf 'main\n' >"${TAG_REPO}/file.txt"
git -C "${TAG_REPO}" add file.txt
git -C "${TAG_REPO}" commit -q -m initial
ssh-keygen -q -t ed25519 -N '' -f "${TAG_REPO}/approved-key"
git -C "${TAG_REPO}" config gpg.format ssh
git -C "${TAG_REPO}" config user.signingkey "${TAG_REPO}/approved-key"
git -C "${TAG_REPO}" tag -s -m release v1.2.3
git -C "${TAG_REPO}" update-ref refs/remotes/origin/main HEAD
approved_public_key="$(cat "${TAG_REPO}/approved-key.pub")"
approved_fingerprint="$(ssh-keygen -lf "${TAG_REPO}/approved-key.pub" -E sha256 | awk '{print $2}')"
tag_output="${TMP_ROOT}/tag-output"
(
  cd "${TAG_REPO}"
  GITHUB_OUTPUT="${tag_output}" \
  RELEASE_SIGNING_PUBLIC_KEY="${approved_public_key}" \
  RELEASE_SIGNING_PRINCIPAL="release@example.test" \
  RELEASE_SIGNING_FINGERPRINT="${approved_fingerprint}" \
  RELEASE_EXPECTED_COMMIT="$(git rev-parse HEAD)" \
    "${RELEASE_DIR}/verify-release-tag.sh" v1.2.3 >/dev/null
)
grep -Fq 'tag=v1.2.3' "${tag_output}" || fail "tag output"
grep -Fq 'version=1.2.3' "${tag_output}" || fail "version output"
grep -Fq 'prerelease=false' "${tag_output}" || fail "prerelease output"
pass "approved SSH signed annotated tag verifies and emits outputs"

git -C "${TAG_REPO}" tag v1.2.4
expect_failure "release tag rejects lightweight tag" "must be an annotated tag" bash -c \
  'cd "$1" && RELEASE_SIGNING_PUBLIC_KEY="$2" RELEASE_SIGNING_PRINCIPAL=release@example.test RELEASE_SIGNING_FINGERPRINT="$3" RELEASE_MAIN_REF=refs/remotes/origin/main "$4" v1.2.4' \
  _ "${TAG_REPO}" "${approved_public_key}" "${approved_fingerprint}" "${RELEASE_DIR}/verify-release-tag.sh"

git -C "${TAG_REPO}" tag -a -m unsigned v1.2.5
expect_failure "release tag rejects unsigned annotated tag" "signature verification failed" bash -c \
  'cd "$1" && RELEASE_SIGNING_PUBLIC_KEY="$2" RELEASE_SIGNING_PRINCIPAL=release@example.test RELEASE_SIGNING_FINGERPRINT="$3" RELEASE_MAIN_REF=refs/remotes/origin/main "$4" v1.2.5' \
  _ "${TAG_REPO}" "${approved_public_key}" "${approved_fingerprint}" "${RELEASE_DIR}/verify-release-tag.sh"

expect_failure "release tag rejects fingerprint mismatch" "fingerprint does not match" bash -c \
  'cd "$1" && RELEASE_SIGNING_PUBLIC_KEY="$2" RELEASE_SIGNING_PRINCIPAL=release@example.test RELEASE_SIGNING_FINGERPRINT=SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA RELEASE_MAIN_REF=refs/remotes/origin/main "$3" v1.2.3' \
  _ "${TAG_REPO}" "${approved_public_key}" "${RELEASE_DIR}/verify-release-tag.sh"

ssh-keygen -q -t ed25519 -N '' -f "${TAG_REPO}/other-key"
git -C "${TAG_REPO}" config user.signingkey "${TAG_REPO}/other-key"
git -C "${TAG_REPO}" tag -s -m other v1.2.6
expect_failure "release tag rejects unapproved signer" "signature verification failed" bash -c \
  'cd "$1" && RELEASE_SIGNING_PUBLIC_KEY="$2" RELEASE_SIGNING_PRINCIPAL=release@example.test RELEASE_SIGNING_FINGERPRINT="$3" RELEASE_MAIN_REF=refs/remotes/origin/main "$4" v1.2.6' \
  _ "${TAG_REPO}" "${approved_public_key}" "${approved_fingerprint}" "${RELEASE_DIR}/verify-release-tag.sh"

git -C "${TAG_REPO}" config user.signingkey "${TAG_REPO}/approved-key"
git -C "${TAG_REPO}" checkout -q -b side
printf 'side\n' >>"${TAG_REPO}/file.txt"
git -C "${TAG_REPO}" commit -qam side
git -C "${TAG_REPO}" tag -s -m side v1.2.7
expect_failure "release tag rejects non-mainline commit" "not an ancestor" bash -c \
  'cd "$1" && RELEASE_SIGNING_PUBLIC_KEY="$2" RELEASE_SIGNING_PRINCIPAL=release@example.test RELEASE_SIGNING_FINGERPRINT="$3" RELEASE_MAIN_REF=refs/remotes/origin/main "$4" v1.2.7' \
  _ "${TAG_REPO}" "${approved_public_key}" "${approved_fingerprint}" "${RELEASE_DIR}/verify-release-tag.sh"
expect_failure "release tag rejects malformed version" "not valid vMAJOR" bash -c \
  'cd "$1" && RELEASE_SIGNING_PUBLIC_KEY="$2" RELEASE_SIGNING_PRINCIPAL=release@example.test RELEASE_SIGNING_FINGERPRINT="$3" RELEASE_MAIN_REF=refs/remotes/origin/main "$4" release-1.2.3' \
  _ "${TAG_REPO}" "${approved_public_key}" "${approved_fingerprint}" "${RELEASE_DIR}/verify-release-tag.sh"
expect_failure "release tag rejects SemVer leading zero" "leading zero" bash -c \
  'cd "$1" && RELEASE_SIGNING_PUBLIC_KEY="$2" RELEASE_SIGNING_PRINCIPAL=release@example.test RELEASE_SIGNING_FINGERPRINT="$3" RELEASE_MAIN_REF=refs/remotes/origin/main "$4" v01.2.3' \
  _ "${TAG_REPO}" "${approved_public_key}" "${approved_fingerprint}" "${RELEASE_DIR}/verify-release-tag.sh"

git -C "${TAG_REPO}" tag -a -m remote-state v1.2.8
remote_tag_object="$(git -C "${TAG_REPO}" rev-parse 'refs/tags/v1.2.8^{tag}')"
remote_tag_commit="$(git -C "${TAG_REPO}" rev-parse 'refs/tags/v1.2.8^{commit}')"
(
  cd "${TAG_REPO}"
  RELEASE_GIT_REMOTE=. "${RELEASE_DIR}/verify-remote-tag-state.sh" \
    v1.2.8 "${remote_tag_object}" "${remote_tag_commit}" >/dev/null
)
pass "remote tag state matches the preflight tag object and commit"
printf 'moved tag\n' >>"${TAG_REPO}/file.txt"
git -C "${TAG_REPO}" commit -qam moved-tag
git -C "${TAG_REPO}" tag -f -a -m moved v1.2.8
expect_failure "remote tag state rejects a moved tag" "changed after preflight" bash -c \
  'cd "$1" && RELEASE_GIT_REMOTE=. "$2" v1.2.8 "$3" "$4"' \
  _ "${TAG_REPO}" "${RELEASE_DIR}/verify-remote-tag-state.sh" "${remote_tag_object}" "${remote_tag_commit}"

# GitHub release lookup distinguishes explicit absence from API failure.
RELEASE_API_DIR="${TMP_ROOT}/release-api"
mkdir -p "${RELEASE_API_DIR}"
cat >"${RELEASE_API_DIR}/server.py" <<'PY'
import json
import pathlib
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

port_file = pathlib.Path(sys.argv[1])
class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        return
    def do_GET(self):
        if self.path.endswith("/v1.2.3"):
            data = json.dumps({"tag_name":"v1.2.3","draft":False}).encode()
            self.send_response(200)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        elif self.path.endswith("/v1.2.4"):
            self.send_response(404)
            self.send_header("content-length", "0")
            self.end_headers()
        else:
            self.send_response(503)
            self.send_header("content-length", "0")
            self.end_headers()
server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
port_file.write_text(str(server.server_address[1]))
server.serve_forever()
PY
python3 "${RELEASE_API_DIR}/server.py" "${RELEASE_API_DIR}/port" >"${RELEASE_API_DIR}/server.log" 2>&1 &
PIDS+=("$!")
wait_for_file "${RELEASE_API_DIR}/port" || fail "release API fixture did not start"
release_api_port="$(cat "${RELEASE_API_DIR}/port")"
RELEASE_GITHUB_API_URL="http://127.0.0.1:${release_api_port}" GITHUB_REPOSITORY=example/vekil \
  "${RELEASE_DIR}/query-github-release.sh" v1.2.3 "${RELEASE_API_DIR}/present.json" >/dev/null
[[ "$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["tag_name"])' "${RELEASE_API_DIR}/present.json")" == v1.2.3 ]] \
  || fail "release query did not preserve provider JSON"
pass "GitHub release lookup reports an existing release"
expect_failure "GitHub release lookup accepts only explicit 404 absence" "" env \
  RELEASE_GITHUB_API_URL="http://127.0.0.1:${release_api_port}" GITHUB_REPOSITORY=example/vekil \
  "${RELEASE_DIR}/query-github-release.sh" v1.2.4
expect_failure "GitHub release lookup fails closed on API errors" "HTTP 503" env \
  RELEASE_GITHUB_API_URL="http://127.0.0.1:${release_api_port}" GITHUB_REPOSITORY=example/vekil \
  "${RELEASE_DIR}/query-github-release.sh" v1.2.5

# Exact required-workflow verification with a fake gh API.
FAKE_GH_DIR="${TMP_ROOT}/fake-gh"
mkdir -p "${FAKE_GH_DIR}"
cat >"${FAKE_GH_DIR}/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
args="$*"
commit="1111111111111111111111111111111111111111"
if [[ "${args}" == *'/actions/workflows '* ]]; then
  printf '%s\n' '{"total_count":2,"workflows":[{"id":1,"name":"CI","path":".github/workflows/ci.yaml"},{"id":2,"name":"CodeQL","path":".github/workflows/codeql.yaml"}]}'
elif [[ "${args}" == *'/actions/workflows/1/runs'* ]]; then
  conclusion="${FAKE_CI_CONCLUSION:-success}"
  printf '{"workflow_runs":[{"id":11,"head_sha":"%s","status":"completed","conclusion":"success","created_at":"2026-01-01T00:00:00Z","run_attempt":1},{"id":12,"head_sha":"%s","status":"completed","conclusion":"%s","created_at":"2026-01-02T00:00:00Z","run_attempt":1}]}\n' "${commit}" "${commit}" "${conclusion}"
elif [[ "${args}" == *'/actions/workflows/2/runs'* ]]; then
  printf '{"workflow_runs":[{"id":22,"head_sha":"%s","status":"completed","conclusion":"success","created_at":"2026-01-02T00:00:00Z","run_attempt":1}]}\n' "${commit}"
else
  printf 'unexpected fake gh call: %s\n' "${args}" >&2
  exit 1
fi
SH
chmod +x "${FAKE_GH_DIR}/gh"
PATH="${FAKE_GH_DIR}:${PATH}" GITHUB_REPOSITORY=example/vekil \
  "${RELEASE_DIR}/verify-required-workflows.sh" 1111111111111111111111111111111111111111 CI codeql.yaml >/dev/null
pass "required workflows verify newest successful runs for exact commit"
expect_failure "required workflows reject a newer failed run" "concluded 'failure'" env \
  PATH="${FAKE_GH_DIR}:${PATH}" GITHUB_REPOSITORY=example/vekil FAKE_CI_CONCLUSION=failure \
  "${RELEASE_DIR}/verify-required-workflows.sh" 1111111111111111111111111111111111111111 CI
expect_failure "required workflows reject runs from a different commit" "has no completed run" env \
  PATH="${FAKE_GH_DIR}:${PATH}" GITHUB_REPOSITORY=example/vekil \
  "${RELEASE_DIR}/verify-required-workflows.sh" 2222222222222222222222222222222222222222 CI

# GHCR tag absence via a deterministic local Registry-v2 stub.
REGISTRY_DIR="${TMP_ROOT}/registry"
mkdir -p "${REGISTRY_DIR}"
cat >"${REGISTRY_DIR}/server.py" <<'PY'
import json
import pathlib
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

port_file = pathlib.Path(sys.argv[1])
class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        return
    def do_GET(self):
        if self.path.startswith("/token"):
            data = json.dumps({"token": "fixture-token"}).encode()
            self.send_response(200)
            self.send_header("content-length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        elif self.path.endswith("/manifests/absent"):
            self.send_response(404)
            self.send_header("content-length", "0")
            self.end_headers()
        elif self.path.endswith("/manifests/present"):
            self.send_response(200)
            self.send_header("docker-content-digest", "sha256:" + "4" * 64)
            self.send_header("content-length", "0")
            self.end_headers()
        else:
            self.send_response(503)
            self.send_header("content-length", "0")
            self.end_headers()
server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
port_file.write_text(str(server.server_address[1]))
server.serve_forever()
PY
python3 "${REGISTRY_DIR}/server.py" "${REGISTRY_DIR}/port" >"${REGISTRY_DIR}/server.log" 2>&1 &
PIDS+=("$!")
wait_for_file "${REGISTRY_DIR}/port" || fail "registry fixture did not start"
registry_port="$(cat "${REGISTRY_DIR}/port")"
registry_env=(
  RELEASE_GHCR_REGISTRY_URL="http://127.0.0.1:${registry_port}"
  RELEASE_GHCR_TOKEN_URL="http://127.0.0.1:${registry_port}/token"
)
env "${registry_env[@]}" "${RELEASE_DIR}/check-image-tag-absent.sh" ghcr.io/example/vekil:absent >/dev/null
pass "GHCR absence check accepts only a proven 404"
expect_failure "GHCR absence check rejects existing version tag" "already exists" env "${registry_env[@]}" \
  "${RELEASE_DIR}/check-image-tag-absent.sh" ghcr.io/example/vekil:present
expect_failure "GHCR absence check fails closed on registry errors" "absence was not proven" env "${registry_env[@]}" \
  "${RELEASE_DIR}/check-image-tag-absent.sh" ghcr.io/example/vekil:error

# Deterministic release manifest and offline tamper checks.
ARTIFACT_DIR="${TMP_ROOT}/artifacts"
mkdir -p "${ARTIFACT_DIR}"
printf 'vekil binary\n' >"${ARTIFACT_DIR}/vekil-linux-amd64"
printf '{"spdxVersion":"SPDX-2.3"}\n' >"${ARTIFACT_DIR}/vekil-linux-amd64.spdx.json"
MANIFEST="${ARTIFACT_DIR}/release-manifest.json"
export RELEASE_REPOSITORY=example/vekil
export RELEASE_TAG=v1.2.3
export RELEASE_COMMIT=1111111111111111111111111111111111111111
export RELEASE_TAG_OBJECT=2222222222222222222222222222222222222222
export RELEASE_RUN_ID=123
export RELEASE_WORKFLOW='example/vekil/.github/workflows/release.yaml@1111111111111111111111111111111111111111'
export RELEASE_IMAGES_JSON='[{"repository":"ghcr.io/example/vekil","tag":"v1.2.3","digest":"sha256:3333333333333333333333333333333333333333333333333333333333333333"}]'
export RELEASE_SCANS_JSON='[{"name":"gitleaks","status":"passed","exceptions":[]},{"name":"govulncheck","status":"passed","exceptions":[]}]'
export RELEASE_ARTIFACT_METADATA_JSON='{"vekil-linux-amd64":{"sbom":"vekil-linux-amd64.spdx.json","kind":"cli-binary"}}'
"${RELEASE_DIR}/generate-release-manifest.py" --artifact-dir "${ARTIFACT_DIR}" --output "${MANIFEST}" >/dev/null
cp "${MANIFEST}" "${TMP_ROOT}/manifest-first.json"
"${RELEASE_DIR}/generate-release-manifest.py" --artifact-dir "${ARTIFACT_DIR}" --output "${MANIFEST}" >/dev/null
cmp -s "${TMP_ROOT}/manifest-first.json" "${MANIFEST}" || fail "manifest is not deterministic"
"${RELEASE_DIR}/verify-release-manifest.py" --artifact-dir "${ARTIFACT_DIR}" --manifest "${MANIFEST}" >/dev/null
pass "release manifest is deterministic and verifies offline"
printf 'tamper\n' >>"${ARTIFACT_DIR}/vekil-linux-amd64"
expect_failure "offline manifest verification rejects tampered artifact" "changed=['vekil-linux-amd64']" \
  "${RELEASE_DIR}/verify-release-manifest.py" --artifact-dir "${ARTIFACT_DIR}" --manifest "${MANIFEST}"
printf 'vekil binary\n' >"${ARTIFACT_DIR}/vekil-linux-amd64"
printf 'extra\n' >"${ARTIFACT_DIR}/unexpected.txt"
expect_failure "offline manifest verification rejects extra artifact" "extra=['unexpected.txt']" \
  "${RELEASE_DIR}/verify-release-manifest.py" --artifact-dir "${ARTIFACT_DIR}" --manifest "${MANIFEST}"
rm "${ARTIFACT_DIR}/unexpected.txt"

# Reviewed vulnerability exception schema and expiry.
"${RELEASE_DIR}/validate-vulnerability-exceptions.py" --as-of 2026-07-28 >/dev/null
pass "empty reviewed vulnerability exception file is valid"
cat >"${TMP_ROOT}/expired-exceptions.json" <<'JSON'
{
  "exceptions": [
    {
      "compensating_controls": [
        "Runtime path is disabled"
      ],
      "component": "example/module",
      "expires_on": "2026-07-27",
      "id": "VULN-EXC-0001",
      "issue": "https://github.com/example/vekil/issues/1",
      "owner": "security@example.test",
      "rationale": "Temporary exception for fixture",
      "severity": "high",
      "vulnerability": "GO-TEST-0001"
    }
  ],
  "schema_version": 1
}
JSON
expect_failure "vulnerability exception validator rejects expired exception" "expired on 2026-07-27" \
  "${RELEASE_DIR}/validate-vulnerability-exceptions.py" --as-of 2026-07-28 "${TMP_ROOT}/expired-exceptions.json"

# Secret scanner policy: the one reviewed path is allowed, a relocated canary is
# rejected, and fully redacted output never includes the canary value.
allowlist_log="${TMP_ROOT}/gitleaks-allowlist.log"
if ! "${RELEASE_DIR}/scan-secrets.sh" "${RELEASE_DIR}/testdata/gitleaks" >"${allowlist_log}" 2>&1; then
  cat "${allowlist_log}" >&2
  fail "gitleaks reviewed fixture allowlist"
fi
pass "gitleaks reviewed fixture allowlist is path-scoped"
secret_value='VEKIL_RELEASE_CANARY_ThisMustRemainRedacted0001'
printf '%s\n' "${secret_value}" >"${TMP_ROOT}/leaky-artifact.txt"
secret_log="${TMP_ROOT}/gitleaks.log"
if "${RELEASE_DIR}/scan-secrets.sh" "${TMP_ROOT}/leaky-artifact.txt" >"${secret_log}" 2>&1; then
  fail "gitleaks synthetic canary unexpectedly passed"
fi
if grep -Fq "${secret_value}" "${secret_log}"; then
  fail "gitleaks output leaked the synthetic canary"
fi
pass "gitleaks rejects synthetic canary without logging secret material"

# Sparkle appcast URL/version/length/signature verification.
SPARKLE_DIR="${TMP_ROOT}/sparkle"
mkdir -p "${SPARKLE_DIR}"
printf 'signed archive bytes\n' >"${SPARKLE_DIR}/vekil.zip"
openssl genpkey -algorithm ED25519 -out "${SPARKLE_DIR}/private.pem" >/dev/null 2>&1
openssl pkey -in "${SPARKLE_DIR}/private.pem" -pubout -outform DER -out "${SPARKLE_DIR}/public.der"
tail -c 32 "${SPARKLE_DIR}/public.der" | base64 | tr -d '\n' >"${SPARKLE_DIR}/public.b64"
openssl pkeyutl -sign -inkey "${SPARKLE_DIR}/private.pem" -rawin -in "${SPARKLE_DIR}/vekil.zip" -out "${SPARKLE_DIR}/signature.bin"
sparkle_signature="$(base64 <"${SPARKLE_DIR}/signature.bin" | tr -d '\n')"
sparkle_length="$(wc -c <"${SPARKLE_DIR}/vekil.zip" | tr -d ' ')"
cat >"${SPARKLE_DIR}/appcast-prefix.xml" <<EOF_XML
<?xml version="1.0"?>
<rss version="2.0" xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle"><channel><item><sparkle:version>1.2.3</sparkle:version><enclosure url="https://example.test/vekil.zip" length="${sparkle_length}" sparkle:edSignature="${sparkle_signature}" /></item></channel></rss>
EOF_XML
openssl pkeyutl -sign -inkey "${SPARKLE_DIR}/private.pem" -rawin \
  -in "${SPARKLE_DIR}/appcast-prefix.xml" -out "${SPARKLE_DIR}/feed-signature.bin"
feed_signature="$(base64 <"${SPARKLE_DIR}/feed-signature.bin" | tr -d '\n')"
feed_length="$(wc -c <"${SPARKLE_DIR}/appcast-prefix.xml" | tr -d ' ')"
cat "${SPARKLE_DIR}/appcast-prefix.xml" >"${SPARKLE_DIR}/appcast.xml"
cat >>"${SPARKLE_DIR}/appcast.xml" <<EOF_SIGNATURE
<!-- sparkle-signatures:
edSignature: ${feed_signature}
length: ${feed_length}
-->
EOF_SIGNATURE
cp "${SPARKLE_DIR}/appcast.xml" "${SPARKLE_DIR}/appcast-original.xml"
"${RELEASE_DIR}/verify-sparkle-update.sh" "${SPARKLE_DIR}/vekil.zip" "${SPARKLE_DIR}/appcast.xml" \
  "$(cat "${SPARKLE_DIR}/public.b64")" https://example.test/vekil.zip 1.2.3 >/dev/null
pass "Sparkle verifier accepts exact URL/version/length plus archive and feed signatures"
expect_failure "Sparkle verifier rejects URL mismatch" "expected exactly one enclosure" \
  "${RELEASE_DIR}/verify-sparkle-update.sh" "${SPARKLE_DIR}/vekil.zip" "${SPARKLE_DIR}/appcast.xml" \
  "$(cat "${SPARKLE_DIR}/public.b64")" https://example.test/wrong.zip 1.2.3
printf 'tamper\n' >>"${SPARKLE_DIR}/vekil.zip"
expect_failure "Sparkle verifier rejects changed archive length/signature" "length mismatch" \
  "${RELEASE_DIR}/verify-sparkle-update.sh" "${SPARKLE_DIR}/vekil.zip" "${SPARKLE_DIR}/appcast.xml" \
  "$(cat "${SPARKLE_DIR}/public.b64")" https://example.test/vekil.zip 1.2.3
printf 'signed archive bytes\n' >"${SPARKLE_DIR}/vekil.zip"
python3 - "${SPARKLE_DIR}/vekil.zip" <<'PY'
from pathlib import Path
import sys
path = Path(sys.argv[1])
data = bytearray(path.read_bytes())
data[0] ^= 1
path.write_bytes(data)
PY
expect_failure "Sparkle verifier rejects same-length signature tamper" "signature verification failed" \
  "${RELEASE_DIR}/verify-sparkle-update.sh" "${SPARKLE_DIR}/vekil.zip" "${SPARKLE_DIR}/appcast.xml" \
  "$(cat "${SPARKLE_DIR}/public.b64")" https://example.test/vekil.zip 1.2.3
printf 'signed archive bytes\n' >"${SPARKLE_DIR}/vekil.zip"
python3 - "${SPARKLE_DIR}/appcast.xml" <<'PY'
from pathlib import Path
import sys
path = Path(sys.argv[1])
data = path.read_bytes()
old = b'version="2.0"'
new = b"version='2.0'"
if old not in data or len(old) != len(new):
    raise SystemExit("feed tamper marker missing")
path.write_bytes(data.replace(old, new, 1))
PY
expect_failure "Sparkle verifier rejects same-length signed feed tamper" "appcast feed signature verification failed" \
  "${RELEASE_DIR}/verify-sparkle-update.sh" "${SPARKLE_DIR}/vekil.zip" "${SPARKLE_DIR}/appcast.xml" \
  "$(cat "${SPARKLE_DIR}/public.b64")" https://example.test/vekil.zip 1.2.3
cp "${SPARKLE_DIR}/appcast-original.xml" "${SPARKLE_DIR}/appcast.xml"

# Published release API and downloaded bytes are compared with the manifest.
PUBLISH_DIR="${TMP_ROOT}/published"
mkdir -p "${PUBLISH_DIR}"
cp "${ARTIFACT_DIR}/vekil-linux-amd64" "${PUBLISH_DIR}/"
cp "${ARTIFACT_DIR}/vekil-linux-amd64.spdx.json" "${PUBLISH_DIR}/"
cp "${MANIFEST}" "${PUBLISH_DIR}/release-manifest.json"
cat >"${PUBLISH_DIR}/server.py" <<'PY'
import hashlib
import json
import pathlib
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import unquote

root = pathlib.Path(sys.argv[1])
port_file = pathlib.Path(sys.argv[2])
class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        return
    def do_GET(self):
        if self.path == "/repos/example/vekil/releases/tags/v1.2.3":
            assets = []
            for path in sorted(root.iterdir()):
                if not path.is_file() or path.name in {"server.py", "server.log", "port"}:
                    continue
                data = path.read_bytes()
                assets.append({
                    "name": path.name,
                    "size": len(data),
                    "digest": "sha256:" + hashlib.sha256(data).hexdigest(),
                    "url": f"http://127.0.0.1:{self.server.server_address[1]}/asset/{path.name}",
                })
            data = json.dumps({"tag_name":"v1.2.3","draft":False,"prerelease":False,"assets":assets}).encode()
            self.send_response(200)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        if self.path.startswith("/asset/"):
            path = root / unquote(self.path.removeprefix("/asset/"))
            if path.is_file():
                data = path.read_bytes()
                self.send_response(200)
                self.send_header("content-length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)
                return
        self.send_response(404)
        self.send_header("content-length", "0")
        self.end_headers()
server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
port_file.write_text(str(server.server_address[1]))
server.serve_forever()
PY
python3 "${PUBLISH_DIR}/server.py" "${PUBLISH_DIR}" "${PUBLISH_DIR}/port" >"${PUBLISH_DIR}/server.log" 2>&1 &
PIDS+=("$!")
wait_for_file "${PUBLISH_DIR}/port" || fail "published-release fixture did not start"
publish_port="$(cat "${PUBLISH_DIR}/port")"
"${RELEASE_DIR}/verify-published-release.py" --manifest "${PUBLISH_DIR}/release-manifest.json" \
  --api-url "http://127.0.0.1:${publish_port}" >/dev/null
pass "published release verifier rehashes every API asset"
printf 'tamper\n' >>"${PUBLISH_DIR}/vekil-linux-amd64"
expect_failure "published release verifier rejects API size/digest drift" "asset size mismatch" \
  "${RELEASE_DIR}/verify-published-release.py" --manifest "${PUBLISH_DIR}/release-manifest.json" \
  --api-url "http://127.0.0.1:${publish_port}"

printf '1..%d\n' "${pass_count}"
