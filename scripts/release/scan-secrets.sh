#!/usr/bin/env bash

# Scan source or staged artifact paths with the reviewed gitleaks policy.
# Findings are always fully redacted from logs. Usage:
#   scripts/release/scan-secrets.sh <path>...

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=scripts/release/lib.sh
source "${SCRIPT_DIR}/lib.sh"

[[ "$#" -gt 0 ]] || release_die "usage: scripts/release/scan-secrets.sh <path>..."
release_require_cmd gitleaks
for source_path in "$@"; do
  [[ -e "${source_path}" ]] || release_die "secret-scan path does not exist: ${source_path}"
  release_log "scanning for secrets: ${source_path}"
  gitleaks dir --redact=100 --no-banner --no-color \
    --config "${REPO_ROOT}/.gitleaks.toml" "${source_path}"
done
