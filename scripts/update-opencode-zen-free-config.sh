#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
PARSER="${SCRIPT_DIR}/parse-opencode-zen-free-models.sh"
DEFAULT_CONFIG="${REPO_ROOT}/examples/opencode-zen-free.yaml"

log() {
  printf '==> %s\n' "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

write_output() {
  local key="$1"
  local value="$2"
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    printf '%s=%s\n' "${key}" "${value}" >> "${GITHUB_OUTPUT}"
  fi
}

if [[ "$#" -lt 1 || "$#" -gt 2 ]]; then
  printf 'usage: %s <zen.mdx|-> [config.yaml]\n' "$0" >&2
  exit 2
fi

source_path="$1"
config_path="${2:-${DEFAULT_CONFIG}}"

command -v python3 >/dev/null 2>&1 || die "missing required command: python3"
[[ -x "${PARSER}" ]] || die "Zen model parser is not executable: ${PARSER}"
[[ "${source_path}" == "-" || -f "${source_path}" ]] || die "OpenCode Zen documentation not found: ${source_path}"
[[ -f "${config_path}" ]] || die "OpenCode Zen example config not found: ${config_path}"

models_file="$(mktemp "${TMPDIR:-/tmp}/vekil-zen-free-models.XXXXXX")"
cleanup() {
  rm -f "${models_file}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

"${PARSER}" "${source_path}" > "${models_file}"

changed="$(python3 - "${models_file}" "${config_path}" <<'PY_RENDER'
from pathlib import Path
import json
import os
import re
import stat
import sys
import tempfile

models_path = Path(sys.argv[1])
config_path = Path(sys.argv[2])

begin_marker = "    # BEGIN GENERATED: OpenCode Zen free models"
end_marker = "    # END GENERATED: OpenCode Zen free models"
allowed_endpoints = {"/chat/completions", "/responses"}
model_id_pattern = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/-]*$")
max_models = 64

models = []
seen = set()
for line_number, raw_line in enumerate(models_path.read_text(encoding="utf-8").splitlines(), 1):
    columns = raw_line.split("\t")
    if len(columns) != 3:
        raise SystemExit(f"invalid parsed model row {line_number}: expected 3 tab-separated fields")
    model_id, _label, endpoint = columns
    if len(model_id) > 128 or not model_id_pattern.fullmatch(model_id):
        raise SystemExit(f"invalid model ID in parsed row {line_number}: {model_id}")
    if model_id in seen:
        raise SystemExit(f"duplicate free model ID: {model_id}")
    if endpoint not in allowed_endpoints:
        raise SystemExit(
            f"unsupported endpoint for openai-compatible Zen example: {model_id} -> {endpoint}"
        )
    seen.add(model_id)
    models.append((model_id, endpoint))

if not models:
    raise SystemExit("no OpenCode Zen free models were parsed")
if len(models) > max_models:
    raise SystemExit(f"parsed {len(models)} free models; maximum automatic update size is {max_models}")

models.sort(key=lambda item: (item[0].lower(), item[0]))

config_text = config_path.read_text(encoding="utf-8")
config_lines = config_text.splitlines(keepends=True)
begin_indexes = [
    index for index, line in enumerate(config_lines) if line.rstrip("\r\n") == begin_marker
]
end_indexes = [
    index for index, line in enumerate(config_lines) if line.rstrip("\r\n") == end_marker
]
if len(begin_indexes) != 1 or len(end_indexes) != 1:
    raise SystemExit("expected exactly one generated Zen model marker pair")

begin_index = begin_indexes[0]
end_index = end_indexes[0]
if begin_index >= end_index:
    raise SystemExit("generated Zen model markers are out of order")

rendered = ["    models:\n"]
for model_id, endpoint in models:
    rendered.extend(
        [
            f"      - public_id: {json.dumps(model_id)}\n",
            "        endpoints:\n",
            f"          - {endpoint}\n",
        ]
    )

updated_text = "".join(config_lines[: begin_index + 1] + rendered + config_lines[end_index:])
if updated_text == config_text:
    print("false")
    raise SystemExit(0)

mode = stat.S_IMODE(config_path.stat().st_mode)
file_descriptor, temporary_name = tempfile.mkstemp(
    prefix=f".{config_path.name}.", dir=config_path.parent
)
try:
    with os.fdopen(file_descriptor, "w", encoding="utf-8", newline="") as output:
        output.write(updated_text)
    os.chmod(temporary_name, mode)
    os.replace(temporary_name, config_path)
finally:
    try:
        os.unlink(temporary_name)
    except FileNotFoundError:
        pass

print("true")
PY_RENDER
)"

case "${changed}" in
  true)
    log "Updated ${config_path} from ${source_path}"
    ;;
  false)
    log "${config_path} is already current"
    ;;
  *)
    die "renderer returned an invalid changed state: ${changed}"
    ;;
esac

write_output changed "${changed}"
