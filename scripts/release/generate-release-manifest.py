#!/usr/bin/env python3
"""Generate Vekil's deterministic release manifest from staged files and env."""

from __future__ import annotations

import argparse
import os
import sys
import tempfile
from pathlib import Path

sys.dont_write_bytecode = True

from _manifest import ManifestError, build_manifest, canonical_bytes


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--artifact-dir", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    try:
        manifest = build_manifest(args.artifact_dir, args.output)
        payload = canonical_bytes(manifest)
        args.output.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(dir=args.output.parent, prefix=f".{args.output.name}.", delete=False) as handle:
            handle.write(payload)
            temp_name = handle.name
        os.chmod(temp_name, 0o644)
        os.replace(temp_name, args.output)
    except (ManifestError, OSError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    print(f"wrote deterministic release manifest: {args.output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
