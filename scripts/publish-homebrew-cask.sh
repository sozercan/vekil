#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 <version> <sha256> <tap-dir>" >&2
  exit 1
fi

version="$1"
sha256="$2"
tap_dir="$3"
cask_path="${tap_dir}/Casks/vekil.rb"

[[ "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?$ ]] || {
  echo "error: invalid release version: ${version}" >&2
  exit 1
}
[[ "${sha256}" =~ ^[0-9a-f]{64}$ ]] || {
  echo "error: sha256 must be 64 lowercase hexadecimal characters" >&2
  exit 1
}

mkdir -p "$(dirname "${cask_path}")"

if [[ -f "${cask_path}" ]]; then
  current_version="$(sed -n 's/^[[:space:]]*version "\([^"]*\)"/\1/p' "${cask_path}" | head -n 1)"
  if [[ -n "${current_version}" ]]; then
    current_sha256="$(sed -n 's/^[[:space:]]*sha256 "\([0-9a-f]*\)"/\1/p' "${cask_path}" | head -n 1)"
    if [[ "${current_version}" == "${version}" && "${current_sha256}" != "${sha256}" ]]; then
      echo "error: refusing to replace Homebrew cask ${current_version} with a different SHA-256" >&2
      exit 1
    fi
    python3 - "${current_version}" "${version}" <<'PY_VERSION'
import re
import sys

def parse(value: str):
    value = value.split("+", 1)[0]
    core, _, prerelease = value.partition("-")
    numbers = tuple(int(part) for part in core.split("."))
    pre = tuple((0, int(p)) if p.isdigit() else (1, p) for p in re.split(r"[.]", prerelease)) if prerelease else None
    return numbers, pre

def less(left, right):
    left_core, left_pre = parse(left)
    right_core, right_pre = parse(right)
    if left_core != right_core:
        return left_core < right_core
    if left_pre is None:
        return False
    if right_pre is None:
        return True
    return left_pre < right_pre

current, candidate = sys.argv[1], sys.argv[2]
if less(candidate, current):
    raise SystemExit(f"refusing to replace Homebrew cask {current} with older {candidate}")
PY_VERSION
  fi
fi

cat >"${cask_path}" <<EOF_CASK
cask "vekil" do
  version "${version}"
  sha256 "${sha256}"

  url "https://github.com/sozercan/vekil/releases/download/v#{version}/vekil-macos-universal.zip"
  name "Vekil"
  desc "Proxy Anthropic, Gemini, and OpenAI clients through configured providers"
  homepage "https://github.com/sozercan/vekil"

  depends_on macos: ">= :ventura"

  app "Vekil.app"

  zap trash: [
    "~/Library/Caches/com.vekil.menubar",
    "~/Library/LaunchAgents/com.vekil.menubar.plist",
    "~/Library/Preferences/com.vekil.menubar.plist",
    "~/Library/Saved Application State/com.vekil.menubar.savedState",
  ]
end
EOF_CASK
