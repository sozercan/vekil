#!/usr/bin/env bash

# Executable release contract. With no arguments it checks every workflow,
# .github/workflows/release.yaml, Dockerfile, and Dockerfile.rtk. See
# release_workflow_contract.py --help for fixture/test overrides.

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec python3 "${SCRIPT_DIR}/release_workflow_contract.py" "$@"
