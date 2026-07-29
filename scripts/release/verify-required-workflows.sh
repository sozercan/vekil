#!/usr/bin/env bash

# Verify that required GitHub Actions workflows succeeded for one exact commit.
#
# Usage: scripts/release/verify-required-workflows.sh <40-hex-commit> [workflow...]
#
# Workflows may be workflow display names, IDs, paths, or workflow filenames.
# When omitted, RELEASE_REQUIRED_WORKFLOWS supplies a comma/newline-separated
# list. RELEASE_REPOSITORY or GITHUB_REPOSITORY must be owner/name. gh uses
# GH_TOKEN/GITHUB_TOKEN in the normal way. The newest completed run for the
# exact head SHA of each required workflow must have conclusion=success.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/lib.sh
source "${SCRIPT_DIR}/lib.sh"

[[ "$#" -ge 1 ]] || release_die "usage: scripts/release/verify-required-workflows.sh <commit> [workflow names...]"
commit="$1"
shift
[[ "${commit}" =~ ^[0-9a-fA-F]{40}$ ]] || release_die "commit must be a full 40-hex Git commit ID"
commit="$(printf '%s' "${commit}" | tr '[:upper:]' '[:lower:]')"
repo="${RELEASE_REPOSITORY:-${GITHUB_REPOSITORY:-}}"
[[ "${repo}" =~ ^[^/[:space:]]+/[^/[:space:]]+$ ]] || release_die "RELEASE_REPOSITORY or GITHUB_REPOSITORY must be owner/name"
release_require_cmd gh
release_require_cmd python3

umask 077
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/vekil-required-workflows.XXXXXX")"
trap 'release_cleanup_dir "${tmp_dir}"' EXIT
workflows_file="${tmp_dir}/requested.txt"
results_file="${tmp_dir}/results.jsonl"
catalog_file="${tmp_dir}/catalog.json"
resolved_file="${tmp_dir}/resolved.tsv"
runs_file="${tmp_dir}/runs.json"

if [[ "$#" -gt 0 ]]; then
  printf '%s\n' "$@" >"${workflows_file}"
else
  [[ -n "${RELEASE_REQUIRED_WORKFLOWS:-}" ]] || release_die "at least one workflow name or RELEASE_REQUIRED_WORKFLOWS is required"
  python3 - "${RELEASE_REQUIRED_WORKFLOWS}" >"${workflows_file}" <<'PY'
import re
import sys

for value in re.split(r"[,\n]", sys.argv[1]):
    value = value.strip()
    if value:
        print(value)
PY
fi
[[ -s "${workflows_file}" ]] || release_die "required workflow list is empty"

gh api --method GET "repos/${repo}/actions/workflows" -f per_page=100 >"${catalog_file}"
python3 - "${catalog_file}" "${workflows_file}" "${resolved_file}" <<'PY'
import json
import os
import sys

catalog_path, query_path, output_path = sys.argv[1:]
with open(catalog_path, encoding="utf-8") as handle:
    catalog = json.load(handle)
workflows = catalog.get("workflows")
if not isinstance(workflows, list):
    raise SystemExit("GitHub workflow catalog response did not contain workflows")
if catalog.get("total_count", len(workflows)) > len(workflows):
    raise SystemExit("repository has more than 100 workflows; helper pagination must be extended")

with open(query_path, encoding="utf-8") as handle:
    queries = [line.strip() for line in handle if line.strip()]

resolved = []
for query in queries:
    matches = []
    for workflow in workflows:
        path = str(workflow.get("path", ""))
        candidates = {
            str(workflow.get("id", "")),
            str(workflow.get("name", "")),
            path,
            os.path.basename(path),
        }
        if query in candidates:
            matches.append(workflow)
    if len(matches) != 1:
        raise SystemExit(f"required workflow {query!r} resolved to {len(matches)} workflows")
    workflow = matches[0]
    resolved.append((str(workflow["id"]), str(workflow.get("name", query)), str(workflow.get("path", ""))))

if len({item[0] for item in resolved}) != len(resolved):
    raise SystemExit("required workflow list contains duplicates")
with open(output_path, "w", encoding="utf-8") as handle:
    for item in resolved:
        handle.write("\t".join(item) + "\n")
PY

while IFS=$'\t' read -r workflow_id workflow_name workflow_path; do
  [[ -n "${workflow_id}" ]] || continue
  gh api --method GET "repos/${repo}/actions/workflows/${workflow_id}/runs" \
    -f head_sha="${commit}" -f status=completed -f per_page=100 >"${runs_file}"
  python3 - "${runs_file}" "${commit}" "${workflow_id}" "${workflow_name}" "${workflow_path}" "${results_file}" <<'PY'
import json
import sys

runs_path, commit, workflow_id, workflow_name, workflow_path, results_path = sys.argv[1:]
with open(runs_path, encoding="utf-8") as handle:
    payload = json.load(handle)
runs = [
    run for run in payload.get("workflow_runs", [])
    if str(run.get("head_sha", "")).lower() == commit
    and run.get("status") == "completed"
]
if not runs:
    raise SystemExit(f"required workflow {workflow_name!r} has no completed run for {commit}")

def order(run):
    return (
        str(run.get("created_at", "")),
        int(run.get("run_attempt") or 0),
        int(run.get("id") or 0),
    )

latest = max(runs, key=order)
if latest.get("conclusion") != "success":
    raise SystemExit(
        f"newest completed run for {workflow_name!r} at {commit} concluded "
        f"{latest.get('conclusion')!r}, not 'success'"
    )
record = {
    "id": int(workflow_id),
    "name": workflow_name,
    "path": workflow_path,
    "run_id": latest.get("id"),
    "run_attempt": latest.get("run_attempt"),
    "url": latest.get("html_url", ""),
}
with open(results_path, "a", encoding="utf-8") as handle:
    handle.write(json.dumps(record, sort_keys=True, separators=(",", ":")) + "\n")
PY
  release_log "verified required workflow ${workflow_name} (${workflow_path}) for ${commit}"
done <"${resolved_file}"

verified_json="$(python3 - "${results_file}" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    values = [json.loads(line) for line in handle if line.strip()]
print(json.dumps(sorted(values, key=lambda item: (item["name"], item["id"])), sort_keys=True, separators=(",", ":")))
PY
)"
release_write_output commit "${commit}"
release_write_output verified_workflows "${verified_json}"
printf 'verified %s required workflow(s) for %s\n' "$(wc -l <"${results_file}" | tr -d ' ')" "${commit}"
