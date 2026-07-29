#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/vekil-external-input.XXXXXX")"

cleanup() {
  rm -rf "${TMP_ROOT}"
}
trap cleanup EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

python3 - "${REPO_ROOT}" <<'PY'
from pathlib import Path
import re
import sys

root = Path(sys.argv[1])
dockerignore = set((root / ".dockerignore").read_text(encoding="utf-8").splitlines())
for required in (".git", ".github", ".build", "dist"):
    if required not in dockerignore:
        raise SystemExit(f".dockerignore: missing sensitive/generated path {required}")
for relative in ("Dockerfile", "Dockerfile.rtk"):
    path = root / relative
    lines = path.read_text(encoding="utf-8").splitlines()
    from_indexes = [index for index, line in enumerate(lines) if line.startswith("FROM ")]
    if not from_indexes:
        raise SystemExit(f"{relative}: no FROM lines found")
    for index in from_indexes:
        line = lines[index]
        image = line.split()[1]
        if not re.fullmatch(r"[^\s@]+@sha256:[0-9a-f]{64}", image):
            raise SystemExit(f"{relative}:{index + 1}: unpinned FROM input: {line}")
        if index == 0 or not lines[index - 1].startswith("# Base image tag: "):
            raise SystemExit(f"{relative}:{index + 1}: missing readable base-image tag comment")
        tag = image.split("@", 1)[0]
        if lines[index - 1] != f"# Base image tag: {tag}":
            raise SystemExit(f"{relative}:{index + 1}: base-image tag comment does not match {tag}")

makefile = (root / "Makefile").read_text(encoding="utf-8")
match = re.search(r"^SPARKLE_ARCHIVE_SHA256 := ([0-9a-f]{64})$", makefile, flags=re.MULTILINE)
if not match:
    raise SystemExit("Makefile: missing pinned Sparkle archive SHA-256")
if match.group(1) != "01e0f0ebf6614061ea816d414de50f937d64ffa6822ad572243031ca3676fe19":
    raise SystemExit("Makefile: unexpected Sparkle 2.9.0 archive SHA-256")
verify_index = makefile.find('"$(SPARKLE_ARCHIVE)" | shasum -a 256 -c -')
extract_index = makefile.find('tar -xf "$(SPARKLE_ARCHIVE)"')
if verify_index < 0 or extract_index < 0 or verify_index > extract_index:
    raise SystemExit("Makefile: Sparkle archive is not verified before extraction")

rtk_dockerfile = (root / "Dockerfile.rtk").read_text(encoding="utf-8")
if "checksums.txt" in rtk_dockerfile:
    raise SystemExit("Dockerfile.rtk still trusts a downloaded checksum manifest")
for architecture in ("amd64", "arm64"):
    if not re.search(rf'^\s*rtk_sha256_{architecture}="[0-9a-f]{{64}}";', rtk_dockerfile, flags=re.MULTILINE):
        raise SystemExit(f"Dockerfile.rtk: missing pinned {architecture} RTK SHA-256")
verify_index = rtk_dockerfile.find("sha256sum -c -")
extract_index = rtk_dockerfile.find("tar -xzf")
if verify_index < 0 or extract_index < 0 or verify_index > extract_index:
    raise SystemExit("Dockerfile.rtk: RTK archive is not verified before extraction")

dependabot = (root / ".github/dependabot.yml").read_text(encoding="utf-8")
if not re.search(r"^\s*- package-ecosystem: docker\s*$", dependabot, flags=re.MULTILINE):
    raise SystemExit("Dependabot: Docker ecosystem coverage is missing")

workflow_lines = []
for path in (root / ".github/workflows").glob("*.yaml"):
    workflow_lines.extend(path.read_text(encoding="utf-8").splitlines())

def action_blocks(action):
    blocks = []
    for index, line in enumerate(workflow_lines):
        if f"uses: {action}@" in line:
            blocks.append("\n".join(workflow_lines[index:index + 8]))
    return blocks

buildx_steps = action_blocks("docker/setup-buildx-action")
if not buildx_steps:
    raise SystemExit("workflows: no setup-buildx steps found")
for values in buildx_steps:
    if "version: v0.35.0" not in values:
        raise SystemExit("workflows: setup-buildx version is not exact")
    if not re.search(r"driver-opts: image=docker\.io/moby/buildkit:v0\.31\.2@sha256:[0-9a-f]{64}", values):
        raise SystemExit("workflows: BuildKit daemon image is not digest-pinned")
for values in action_blocks("docker/setup-qemu-action"):
    if not re.search(r"image: docker\.io/tonistiigi/binfmt:qemu-v10\.2\.3-68@sha256:[0-9a-f]{64}", values):
        raise SystemExit("workflows: QEMU binfmt image is not digest-pinned")
setup_docker_steps = action_blocks("docker/setup-docker-action")
for values in setup_docker_steps:
    if "version: v29.6.2" not in values:
        raise SystemExit("workflows: dry-run Docker Engine version is not exact")

ci = (root / ".github/workflows/ci.yaml").read_text(encoding="utf-8")
for variable in ("KIND_LINUX_AMD64_SHA256", "KUBECTL_LINUX_AMD64_SHA256"):
    if not re.search(rf"^\s+{variable}: [0-9a-f]{{64}}$", ci, flags=re.MULTILINE):
        raise SystemExit(f"CI: missing reviewed {variable}")
if "kind-linux-amd64.sha256sum" in ci or "bin/linux/amd64/kubectl.sha256" in ci:
    raise SystemExit("CI still trusts a checksum downloaded beside the executable")
PY

fixture_archive="${TMP_ROOT}/Sparkle-2.9.0.tar.xz"
python3 - "${fixture_archive}" <<'PY'
from io import BytesIO
from pathlib import Path
import sys
import tarfile

path = Path(sys.argv[1])
data = b"verified Sparkle fixture\n"
with tarfile.open(path, mode="w:xz") as archive:
    info = tarfile.TarInfo("Sparkle.framework/verified.txt")
    info.size = len(data)
    info.mode = 0o644
    archive.addfile(info, BytesIO(data))
PY
fixture_sha256="$(shasum -a 256 "${fixture_archive}" | awk '{print $1}')"
fixture_url="$(python3 - "${fixture_archive}" <<'PY'
from pathlib import Path
import sys
print(Path(sys.argv[1]).resolve().as_uri())
PY
)"

run_sparkle_make() {
  local build_dir="$1"
  local expected_sha256="$2"
  make --no-print-directory -f "${REPO_ROOT}/Makefile" \
    SPARKLE_VERSION=2.9.0 \
    SPARKLE_ARCHIVE_SHA256="${expected_sha256}" \
    SPARKLE_BUILD_DIR="${build_dir}" \
    SPARKLE_DOWNLOAD_URL="${fixture_url}" \
    "${build_dir}/unpacked/Sparkle.framework"
}

verified_build="${TMP_ROOT}/verified-build"
run_sparkle_make "${verified_build}" "${fixture_sha256}" >"${TMP_ROOT}/verified.log"
[[ -f "${verified_build}/unpacked/Sparkle.framework/verified.txt" ]] || fail "verified Sparkle archive was not extracted"

python3 - "${verified_build}/unpacked" <<'PY'
from pathlib import Path
import shutil
import sys
path = Path(sys.argv[1])
if path.exists():
    shutil.rmtree(path)
PY
printf 'tampered cached archive\n' >>"${verified_build}/Sparkle-2.9.0.tar.xz"
if run_sparkle_make "${verified_build}" "${fixture_sha256}" >"${TMP_ROOT}/tampered-cache.log" 2>&1; then
  fail "Makefile extracted a tampered cached Sparkle archive"
fi
[[ ! -e "${verified_build}/unpacked/Sparkle.framework" ]] || fail "tampered cached Sparkle archive reached extraction"
grep -Fq 'FAILED' "${TMP_ROOT}/tampered-cache.log" || fail "tampered cached Sparkle failure was not explicit"

wrong_metadata_build="${TMP_ROOT}/wrong-metadata-build"
wrong_sha256="${fixture_sha256%?}0"
if [[ "${wrong_sha256}" == "${fixture_sha256}" ]]; then
  wrong_sha256="${fixture_sha256%?}1"
fi
if run_sparkle_make "${wrong_metadata_build}" "${wrong_sha256}" >"${TMP_ROOT}/wrong-metadata.log" 2>&1; then
  fail "Makefile accepted changed Sparkle trust metadata"
fi
[[ ! -e "${wrong_metadata_build}/Sparkle-2.9.0.tar.xz" ]] || fail "failed Sparkle download was promoted into the trusted cache"
[[ ! -e "${wrong_metadata_build}/unpacked/Sparkle.framework" ]] || fail "changed Sparkle trust metadata reached extraction"

printf 'PASS: external Docker, Sparkle, RTK, and Dependabot inputs are pinned and verified\n'
