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

mkdir -p "$(dirname "$cask_path")"

cat >"$cask_path" <<EOF
cask "vekil" do
  version "$version"
  sha256 "$sha256"

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
EOF
