#!/usr/bin/env bash

set -euo pipefail

[[ $# -eq 1 ]] || {
  echo "usage: $0 <stable-release-tag>" >&2
  exit 1
}

release_tag="$1"
[[ "${release_tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  echo "release tag must be stable semantic version text prefixed by v" >&2
  echo "prerelease tags require a persistent prerelease Sparkle feed" >&2
  exit 1
}
