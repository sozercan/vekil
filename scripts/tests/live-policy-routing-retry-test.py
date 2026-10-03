"""Exercise the real parallel-tools retry loop with deterministic responses."""

import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest


SOURCE = Path(__file__).resolve().parents[1] / "live-policy-routing-smoke.sh"


def shell_function(name):
    match = re.search(rf"^{name}\(\) \{{\n.*?^\}}$", SOURCE.read_text(), re.M | re.S)
    if match is None:
        raise AssertionError(f"missing function: {name}")
    return match.group(0)


HARNESS = r'''
set -euo pipefail
mode_dir="$1"
PUBLIC_MODEL=public-model
SUMMARY_FILE="${mode_dir}/summary"
LIVE_POLICY_ROUTING_RETRY_OUTPUT_MISMATCH=1
log() { :; }
die() { printf '%s\n' "$*" >&2; exit 1; }
fetch_stats() { printf '%s' "$1"; }
profile_metric() { [[ "$1" == before-* ]] && printf 0 || printf 1; }
stats_counter() { profile_metric "$@"; }
post_chat() {
  local n=0
  [[ ! -f "${mode_dir}/count" ]] || n=$(cat "${mode_dir}/count")
  n=$((n + 1))
  printf '%s' "$n" > "${mode_dir}/count"
  cp "${mode_dir}/response-${n}.json" "$3"
  : > "$4"
  if [[ "${CASE}" == http-error && "$n" == 1 ]]; then printf 500; else printf 200; fi
}
assert_public_headers() {
  if [[ "${CASE}" == header-leak && $(cat "${mode_dir}/count") == 1 ]]; then die 'header leak'; fi
}
assert_file_has_no_internal_identity() {
  if [[ "${CASE}" == body-leak && $(cat "${mode_dir}/count") == 1 ]]; then die 'body leak'; fi
}
parallel_tools_classifier_outcome_matches() {
  # Both infrastructure failure and unverified classifier output must fail.
  if [[ "${CASE}" == classifier-* && $(cat "${mode_dir}/count") == 1 ]]; then return 1; fi
}
'''


class ParallelToolsRetryTest(unittest.TestCase):
    def test_attempt_boundaries(self):
        cases = {
            "pass": (True, 1),
            "mismatch": (True, 2),
            "always-wrong": (False, 2),
            "header-leak": (False, 1),
            "body-leak": (False, 1),
            "wrong-model": (False, 1),
            "missing-usage": (False, 1),
            "http-error": (False, 1),
            "classifier-unavailable": (False, 1),
            "classifier-uncertain": (False, 1),
        }
        script = HARNESS + "\n" + shell_function("assert_delta")
        script += "\n" + shell_function("run_parallel_tools") + "\nrun_parallel_tools\n"
        for case, (success, sends) in cases.items():
            with self.subTest(case=case), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                for attempt in (1, 2):
                    names = ["fetch_account", "fetch_permissions"]
                    if case == "always-wrong" or attempt == 1 and case != "pass":
                        names = ["fetch_account"]
                    response = {
                        "model": "public-model",
                        "usage": {"total_tokens": 2},
                        "choices": [{"message": {"tool_calls": [
                            {"id": str(i), "function": {"name": name}}
                            for i, name in enumerate(names)
                        ]}}],
                    }
                    if attempt == 1 and case == "wrong-model":
                        response["model"] = "not-the-public-model"
                    if attempt == 1 and case == "missing-usage":
                        del response["usage"]
                    (root / f"response-{attempt}.json").write_text(json.dumps(response))
                result = subprocess.run(
                    ["bash", "-c", script, "retry-test", directory],
                    env={"PATH": os.environ["PATH"], "HOME": directory, "CASE": case},
                    capture_output=True, text=True, timeout=10,
                )
                self.assertEqual(result.returncode == 0, success, result.stderr)
                self.assertEqual(int((root / "count").read_text()), sends, result.stderr)


if __name__ == "__main__":
    unittest.main()
