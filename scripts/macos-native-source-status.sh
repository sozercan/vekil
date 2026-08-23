#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CONFIG="${MACOS_APP_CONFIG:-${REPO_ROOT}/build-support/macos/app-config.json}"
MANIFEST_TOOL="${SCRIPT_DIR}/macos-release-manifest.py"
MODE="status"

case "${1:-}" in
  "") ;;
  --require) MODE="require" ;;
  --github-output) MODE="github-output" ;;
  *) echo "usage: $0 [--require|--github-output]" >&2; exit 2 ;;
esac

missing=()
invalid=()
missing_count=0
invalid_count=0
source_root_presence=0

swift_package="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key application.swift_package_path)"
go_helper_package="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key application.go_helper_package)"
sparkle_version="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key sparkle.version)"
sparkle_revision="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key sparkle.swift_package_revision)"
package_dir="${REPO_ROOT}/${swift_package}"
helper_dir="${REPO_ROOT}/${go_helper_package#./}"
appcontrol_dir="${REPO_ROOT}/internal/appcontrol"
macosruntime_dir="${REPO_ROOT}/internal/macosruntime"
for source_root in "${package_dir}" "${helper_dir}" "${appcontrol_dir}" "${macosruntime_dir}"; do
  if [[ -e "${source_root}" ]]; then
    source_root_presence=$((source_root_presence + 1))
  fi
done

if [[ ! -f "${package_dir}/Package.swift" ]]; then
  missing+=("${swift_package}/Package.swift")
  missing_count=$((missing_count + 1))
fi
if [[ ! -f "${package_dir}/Package.resolved" ]]; then
  missing+=("${swift_package}/Package.resolved")
  missing_count=$((missing_count + 1))
fi
if [[ ! -d "${package_dir}/Sources/Vekil" ]] || ! find "${package_dir}/Sources/Vekil" -type f -name '*.swift' -print -quit 2>/dev/null | grep -q .; then
  missing+=("${swift_package}/Sources/Vekil/*.swift"); missing_count=$((missing_count + 1))
fi
if [[ ! -d "${helper_dir}" ]] || ! find "${helper_dir}" -maxdepth 1 -type f -name '*.go' -print -quit 2>/dev/null | grep -q .; then
  missing+=("${go_helper_package}/*.go"); missing_count=$((missing_count + 1))
fi

if [[ ! -d "${appcontrol_dir}" ]] || ! find "${appcontrol_dir}" -maxdepth 1 -type f -name '*.go' -print -quit 2>/dev/null | grep -q .; then
  missing+=("internal/appcontrol/*.go"); missing_count=$((missing_count + 1))
fi
if [[ ! -d "${macosruntime_dir}" ]] || ! find "${macosruntime_dir}" -maxdepth 1 -type f -name '*.go' -print -quit 2>/dev/null | grep -q .; then
  missing+=("internal/macosruntime/*.go"); missing_count=$((missing_count + 1))
fi

if [[ -f "${package_dir}/Package.swift" ]]; then
  if ! python3 - "${package_dir}/Package.swift" "${sparkle_version}" <<'PY'
import re
import sys
from pathlib import Path

text = Path(sys.argv[1]).read_text(encoding="utf-8")
version = re.escape(sys.argv[2])
patterns = [
    rf'\.package\(\s*url:\s*"https://github\.com/sparkle-project/Sparkle(?:\.git)?"\s*,\s*exact:\s*"{version}"\s*\)',
    rf'\.package\(\s*url:\s*"https://github\.com/sparkle-project/Sparkle(?:\.git)?"\s*,\s*\.exact\("{version}"\)\s*\)',
]
if not any(re.search(pattern, text, re.MULTILINE) for pattern in patterns):
    raise SystemExit(1)
PY
  then
    invalid+=("${swift_package}/Package.swift must pin Sparkle with exact: \"${sparkle_version}\""); invalid_count=$((invalid_count + 1))
  fi
  if ! grep -Eq '\.macOS\(\.v13\)' "${package_dir}/Package.swift"; then
    invalid+=("${swift_package}/Package.swift must declare .macOS(.v13)"); invalid_count=$((invalid_count + 1))
  fi
fi

if [[ -f "${package_dir}/Package.resolved" ]]; then
  if ! python3 - "${package_dir}/Package.resolved" "${sparkle_version}" "${sparkle_revision}" <<'PY'
import json
import sys
from pathlib import Path

try:
    value = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8"))
except (OSError, json.JSONDecodeError):
    raise SystemExit(1)
expected = sys.argv[2]
expected_revision = sys.argv[3]
pins = value.get("pins")
if not isinstance(pins, list):
    pins = value.get("object", {}).get("pins", [])
matches = []
for pin in pins:
    identity = str(pin.get("identity") or pin.get("package") or "").lower()
    location = str(pin.get("location") or pin.get("repositoryURL") or "").lower()
    if identity == "sparkle" or "sparkle-project/sparkle" in location:
        state = pin.get("state") or {}
        matches.append((state.get("version"), state.get("revision")))
if matches != [(expected, expected_revision)]:
    raise SystemExit(1)
PY
  then
    invalid+=("${swift_package}/Package.resolved must contain exactly one Sparkle ${sparkle_version} pin at ${sparkle_revision}"); invalid_count=$((invalid_count + 1))
  fi
fi

ready=true
state=ready
if (( missing_count > 0 || invalid_count > 0 )); then
  ready=false
  if (( source_root_presence == 0 )); then
    state=absent
  else
    state=partial
  fi
fi

emit_details() {
  local item
  if (( missing_count > 0 )); then
    for item in "${missing[@]}"; do
      printf 'missing: %s\n' "${item}" >&2
    done
  fi
  if (( invalid_count > 0 )); then
    for item in "${invalid[@]}"; do
      printf 'invalid: %s\n' "${item}" >&2
    done
  fi
}

case "${MODE}" in
  github-output)
    if [[ -z "${GITHUB_OUTPUT:-}" ]]; then
      echo "error: GITHUB_OUTPUT is required with --github-output" >&2
      exit 2
    fi
    printf 'ready=%s\nstate=%s\n' "${ready}" "${state}" >>"${GITHUB_OUTPUT}"
    if [[ "${ready}" == true ]]; then
      echo "Native macOS sources are ready."
    elif [[ "${state}" == absent ]]; then
      emit_details
      echo "Native macOS source tree is absent; infrastructure-only checks will run." >&2
    else
      emit_details
      echo "error: native macOS source tree is partially present" >&2
      exit 1
    fi
    ;;
  require)
    if [[ "${ready}" != true ]]; then
      emit_details
      echo "error: native macOS source contract is incomplete" >&2
      exit 1
    fi
    echo "Native macOS source contract is ready."
    ;;
  status)
    if [[ "${ready}" == true ]]; then
      echo "ready"
    else
      emit_details
      echo "${state}"
      exit 1
    fi
    ;;
esac
