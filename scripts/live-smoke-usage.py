#!/usr/bin/env python3
"""Capture numeric task usage before a live smoke's proxy is stopped."""

import argparse
import json
import os
from pathlib import Path
from http.client import HTTPException
import sys
from urllib.error import URLError
from urllib.request import urlopen


FIELDS = {
    "sends": ("sends",),
    "completed": ("completed",),
    "errors": ("errors",),
    "reported_usage_sends": ("reported_usage_sends",),
    "prompt_tokens": ("usage", "prompt_tokens"),
    "completion_tokens": ("usage", "completion_tokens"),
    "cached_tokens": ("usage", "cached_tokens"),
    "reasoning_tokens": ("usage", "reasoning_tokens"),
    "total_nano_aiu": ("copilot_usage", "total_nano_aiu"),
    "compute_units": ("copilot_usage", "compute_units"),
}


def counter(value):
    if type(value) is not int or value < 0:
        raise ValueError("invalid usage counter")
    return value


def extract(snapshot):
    task = snapshot["task_usage"]
    totals = task["totals"]
    metrics = {}
    for name, path in FIELDS.items():
        value = totals
        for key in path:
            value = value[key]
        metrics[name] = counter(value)
    return {
        "snapshots": 1,
        "available_snapshots": 1,
        "inflight": counter(task["inflight"]),
        "metrics": metrics,
    }


def capture(base_url):
    if base_url:
        try:
            with urlopen(base_url.rstrip("/") + "/stats.json", timeout=1) as response:
                # Never retain the catalog, identities, or request content in stats.
                body = response.read(2 * 1024 * 1024 + 1)
                if len(body) > 2 * 1024 * 1024:
                    raise ValueError("usage response too large")
            return extract(json.loads(body))
        except (URLError, OSError, HTTPException, ValueError, KeyError, TypeError):
            pass
    return {"snapshots": 1, "available_snapshots": 0, "inflight": 0, "metrics": None}


def combine(current, previous):
    """Add separate proxy lifetimes, never repeated cumulative snapshots."""
    available = current["available_snapshots"] + previous["available_snapshots"]
    metrics = None
    if available:
        metrics = {
            name: sum((record["metrics"] or {}).get(name, 0) for record in (current, previous))
            for name in FIELDS
        }
    return {
        "snapshots": current["snapshots"] + previous["snapshots"],
        "available_snapshots": available,
        "inflight": current["inflight"] + previous["inflight"],
        "metrics": metrics,
    }


def render(label, record):
    metrics = record["metrics"]
    if metrics is None:
        return f"\n### {label} usage\n\nUsage unavailable; no zero-cost result is inferred.\n"
    rows = "\n".join(f"| {name} | {value} |" for name, value in metrics.items())
    complete = record["available_snapshots"] == record["snapshots"] and record["inflight"] == 0
    status = "Complete proxy snapshots" if complete else "Partial proxy snapshots"
    return (
        f"\n### {label} usage\n\n"
        f"{status}: {record['available_snapshots']}/{record['snapshots']}; "
        f"inflight sends at capture: {record['inflight']}.\n\n"
        f"| Counter | Reported total |\n|---|---:|\n{rows}\n\n"
        "Token and billing totals include only provider-reported usage. "
        "Zero billing units do not establish that a request was free. "
        "Cached and reasoning tokens are subsets, not additional token totals.\n"
    )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default="")
    parser.add_argument("--label", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--previous", type=Path)
    parser.add_argument("--quiet", action="store_true")
    args = parser.parse_args()
    record = capture(args.url)
    if args.previous and args.previous.exists():
        record = combine(record, json.loads(args.previous.read_text()))
    args.output.write_text(json.dumps(record, indent=2) + "\n")
    args.output.chmod(0o600)
    if not args.quiet:
        label = " ".join(args.label.replace("|", " ").split())
        summary = render(label, record)
        print(summary, file=sys.stderr)
        if os.environ.get("GITHUB_STEP_SUMMARY"):
            with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as destination:
                destination.write(summary)


if __name__ == "__main__":
    main()
