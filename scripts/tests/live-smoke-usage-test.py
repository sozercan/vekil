#!/usr/bin/env python3

import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "live-smoke-usage.py"
spec = importlib.util.spec_from_file_location("smoke_usage", SCRIPT)
usage = importlib.util.module_from_spec(spec)
sys.dont_write_bytecode = True
spec.loader.exec_module(usage)

SNAPSHOT = {
    "recent_requests": [{"model": "PRIVATE_MODEL", "prompt": "PRIVATE_PROMPT"}],
    "task_usage": {
        "inflight": 0,
        "totals": {
            "sends": 3,
            "completed": 3,
            "errors": 1,
            "reported_usage_sends": 2,
            "usage": {
                "prompt_tokens": 100,
                "completion_tokens": 20,
                "cached_tokens": 60,
                "reasoning_tokens": 10,
            },
            "copilot_usage": {
                "total_nano_aiu": 150,
                "compute_units": 4,
                "token_details": [{"model": "PRIVATE_BILLING_MODEL"}],
            },
        },
        "by_kind": [{"kind": "classifier", "sends": 1}],
    },
}


class UsageTest(unittest.TestCase):
    def test_cli_captures_numeric_usage_and_publishes_summary(self):
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                self.server.paths.append(self.path)
                body = json.dumps(SNAPSHOT).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *_args):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        server.paths = []
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory() as directory:
                output = Path(directory) / "usage.json"
                summary = Path(directory) / "summary.md"
                previous = Path(directory) / "initial.json"
                previous.write_text(json.dumps(usage.extract(SNAPSHOT)))
                result = subprocess.run(
                    [sys.executable, str(SCRIPT), "--url", f"http://127.0.0.1:{server.server_port}",
                     "--label", "Carrier restart", "--output", str(output), "--previous", str(previous)],
                    env={**os.environ, "GITHUB_STEP_SUMMARY": str(summary)},
                    capture_output=True, text=True, timeout=10, check=True,
                )
                record = json.loads(output.read_text())
                self.assertEqual(server.paths, ["/stats.json"])
                self.assertEqual(record["snapshots"], 2)
                self.assertEqual(record["metrics"]["sends"], 6)
                self.assertEqual(record["metrics"]["total_nano_aiu"], 300)
                self.assertEqual(record["metrics"]["prompt_tokens"], 200)
                self.assertEqual(output.stat().st_mode & 0o777, 0o600)
                self.assertIn("Complete proxy snapshots: 2/2", summary.read_text())
                self.assertNotIn("PRIVATE_", output.read_text() + summary.read_text() + result.stderr)
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def test_missing_usage_stays_unknown(self):
        missing = usage.capture("")
        self.assertIsNone(missing["metrics"])
        self.assertIn("Usage unavailable", usage.render("Missing", missing))
        partial = usage.combine(missing, usage.extract(SNAPSHOT))
        self.assertEqual(partial["metrics"]["sends"], 3)
        self.assertIn("Partial proxy snapshots: 1/2", usage.render("Partial", partial))

    def test_inflight_is_explicit_and_subsets_are_not_added(self):
        snapshot = copy.deepcopy(SNAPSHOT)
        snapshot["task_usage"]["inflight"] = 1
        record = usage.extract(snapshot)
        self.assertEqual(record["metrics"]["prompt_tokens"], 100)
        self.assertEqual(record["metrics"]["completion_tokens"], 20)
        self.assertIn("Partial proxy snapshots", usage.render("Inflight", record))
        self.assertIn("inflight sends at capture: 1", usage.render("Inflight", record))

    def test_non_numeric_or_missing_counters_are_rejected(self):
        for value in (True, -1, "PRIVATE_COUNTER", 1.5, None):
            with self.subTest(value=value):
                snapshot = copy.deepcopy(SNAPSHOT)
                snapshot["task_usage"]["totals"]["sends"] = value
                with self.assertRaises(ValueError):
                    usage.extract(snapshot)
        with self.assertRaises(KeyError):
            usage.extract({})


if __name__ == "__main__":
    unittest.main()
