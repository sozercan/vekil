#!/usr/bin/env python3
"""Offline verification of a deterministic Vekil release manifest."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

sys.dont_write_bytecode = True

from _manifest import ManifestError, canonical_bytes, scan_artifacts, validate_manifest_structure


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--artifact-dir", required=True, type=Path)
    parser.add_argument("--manifest", required=True, type=Path)
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    try:
        raw = args.manifest.read_bytes()
        manifest = json.loads(raw)
        validate_manifest_structure(manifest)
        if raw != canonical_bytes(manifest):
            raise ManifestError("manifest JSON is not in canonical deterministic form")
        actual = scan_artifacts(args.artifact_dir, args.manifest, use_env_overrides=False)
        expected_by_path = {item["path"]: item for item in manifest["artifacts"]}
        actual_by_path = {item["path"]: item for item in actual}
        missing = sorted(set(expected_by_path) - set(actual_by_path))
        extra = sorted(set(actual_by_path) - set(expected_by_path))
        integrity_fields = ("path", "name", "size", "sha256")
        changed = sorted(
            path for path in set(expected_by_path) & set(actual_by_path)
            if any(expected_by_path[path][field] != actual_by_path[path][field] for field in integrity_fields)
        )
        if missing or extra or changed:
            raise ManifestError(f"artifact verification failed (missing={missing}, extra={extra}, changed={changed})")
    except (ManifestError, OSError, json.JSONDecodeError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    print(f"verified {len(manifest['artifacts'])} staged artifact(s) offline against {args.manifest}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
