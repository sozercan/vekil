#!/usr/bin/env python3
"""Verify Vekil's signed Sparkle appcast and macOS 13 eligibility metadata."""

from __future__ import annotations

import argparse
import base64
import json
import subprocess
import sys
import tempfile
import urllib.parse
import xml.etree.ElementTree as ET
from pathlib import Path
from typing import Any

SPARKLE = "http://www.andymatuschak.org/xml-namespaces/sparkle"
ED25519_SUBJECT_PUBLIC_KEY_INFO_PREFIX = bytes.fromhex("302a300506032b6570032100")


class VerificationError(ValueError):
    pass


def load_json(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise VerificationError(f"cannot read manifest {path}: {exc}") from exc
    if not isinstance(value, dict):
        raise VerificationError("manifest root must be an object")
    return value


def version_tuple(value: str) -> tuple[int, ...]:
    try:
        parts = tuple(int(part) for part in value.split("."))
    except ValueError as exc:
        raise VerificationError(f"invalid dotted version: {value!r}") from exc
    if not parts or any(part < 0 for part in parts):
        raise VerificationError(f"invalid dotted version: {value!r}")
    return parts + (0,) * (4 - len(parts))


def text(item: ET.Element, name: str) -> str:
    value = item.findtext(f"{{{SPARKLE}}}{name}")
    return (value or "").strip()


def decode_base64(value: str, label: str, expected_bytes: int) -> bytes:
    if not value:
        raise VerificationError(f"{label} is missing")
    try:
        decoded = base64.b64decode(value, validate=True)
    except ValueError as exc:
        raise VerificationError(f"{label} is not valid base64") from exc
    if len(decoded) != expected_bytes:
        raise VerificationError(f"{label} must decode to {expected_bytes} bytes")
    return decoded


def verify_artifact_signature(artifact: Path, signature: bytes, public_key: bytes) -> None:
    with tempfile.TemporaryDirectory(prefix="vekil-sparkle-verify-") as temporary:
        temporary_path = Path(temporary)
        public_key_path = temporary_path / "public-key.der"
        signature_path = temporary_path / "signature"
        public_key_path.write_bytes(ED25519_SUBJECT_PUBLIC_KEY_INFO_PREFIX + public_key)
        signature_path.write_bytes(signature)
        try:
            result = subprocess.run(
                [
                    "openssl",
                    "pkeyutl",
                    "-verify",
                    "-pubin",
                    "-inkey",
                    str(public_key_path),
                    "-keyform",
                    "DER",
                    "-rawin",
                    "-in",
                    str(artifact),
                    "-sigfile",
                    str(signature_path),
                ],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                check=False,
            )
        except OSError as exc:
            raise VerificationError(f"cannot run OpenSSL signature verification: {exc}") from exc
    if result.returncode != 0:
        raise VerificationError(
            "candidate enclosure Ed25519 signature does not verify against "
            "manifest sparkle.public_ed_key"
        )


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--appcast", required=True)
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--artifact")
    parser.add_argument("--expected-url-prefix")
    parser.add_argument("--require-legacy-compatible-entry", action="store_true")
    args = parser.parse_args()

    appcast_path = Path(args.appcast)
    manifest = load_json(Path(args.manifest))
    application = manifest.get("application") or {}
    legacy = manifest.get("legacy_shell") or {}
    sparkle = manifest.get("sparkle") or {}
    if not isinstance(application, dict) or not isinstance(legacy, dict) or not isinstance(sparkle, dict):
        raise VerificationError("manifest application, legacy_shell, and sparkle must be objects")

    bundle_version = str(manifest.get("bundle_version") or "")
    marketing_version = str(manifest.get("marketing_version") or "")
    minimum_version = str(application.get("minimum_system_version") or "")
    artifact_name = str(application.get("artifact_name") or "")
    if not all((bundle_version, marketing_version, minimum_version, artifact_name)):
        raise VerificationError("manifest is missing release identity fields")

    try:
        raw_xml = appcast_path.read_text(encoding="utf-8")
        root = ET.fromstring(raw_xml)
    except (OSError, UnicodeDecodeError, ET.ParseError) as exc:
        raise VerificationError(f"cannot parse appcast {appcast_path}: {exc}") from exc

    if "sparkle-signatures:" not in raw_xml:
        raise VerificationError("appcast-level Sparkle signature block is missing")
    signature_tail = raw_xml.rsplit("sparkle-signatures:", 1)[-1]
    appcast_signature = ""
    for line in signature_tail.splitlines():
        if line.strip().startswith("edSignature:"):
            appcast_signature = line.split(":", 1)[1].strip()
            break
    decode_base64(appcast_signature, "appcast Ed25519 signature", 64)

    items = root.findall("./channel/item")
    candidates = [item for item in items if text(item, "version") == bundle_version]
    if len(candidates) != 1:
        raise VerificationError(
            f"expected one candidate with sparkle:version {bundle_version}, found {len(candidates)}"
        )
    candidate = candidates[0]
    if text(candidate, "shortVersionString") != marketing_version:
        raise VerificationError("candidate sparkle:shortVersionString does not match manifest")
    if text(candidate, "minimumSystemVersion") != minimum_version:
        raise VerificationError(
            f"candidate minimumSystemVersion is {text(candidate, 'minimumSystemVersion')!r}; "
            f"expected {minimum_version!r}"
        )
    hardware = text(candidate, "hardwareRequirements")
    if hardware:
        raise VerificationError(
            f"universal candidate must not restrict sparkle:hardwareRequirements, found {hardware!r}"
        )

    enclosure = candidate.find("enclosure")
    if enclosure is None:
        raise VerificationError("candidate enclosure is missing")
    url = (enclosure.get("url") or "").strip()
    parsed = urllib.parse.urlparse(url)
    if parsed.scheme != "https":
        raise VerificationError("candidate enclosure URL must use HTTPS")
    if Path(parsed.path).name != artifact_name:
        raise VerificationError(f"candidate enclosure URL does not name {artifact_name}")
    if args.expected_url_prefix and not url.startswith(args.expected_url_prefix.rstrip("/") + "/"):
        raise VerificationError("candidate enclosure URL does not use the expected release prefix")
    candidate_signature = decode_base64(
        (enclosure.get(f"{{{SPARKLE}}}edSignature") or "").strip(),
        "candidate enclosure Ed25519 signature",
        64,
    )

    if args.artifact:
        artifact = Path(args.artifact)
        if artifact.name != artifact_name:
            raise VerificationError(f"artifact must be named {artifact_name}")
        try:
            expected_length = int(enclosure.get("length") or "")
        except ValueError as exc:
            raise VerificationError("candidate enclosure length is invalid") from exc
        actual_length = artifact.stat().st_size
        if expected_length != actual_length:
            raise VerificationError(
                f"candidate enclosure length {expected_length} does not match artifact {actual_length}"
            )
        public_key = decode_base64(
            str(sparkle.get("public_ed_key") or ""),
            "manifest sparkle.public_ed_key",
            32,
        )
        verify_artifact_signature(artifact, candidate_signature, public_key)

    fixture_expectations = {
        "10.13": False,
        "12.6": False,
        "13.0": True,
        "14.0": True,
    }
    candidate_minimum = version_tuple(text(candidate, "minimumSystemVersion"))
    for system_version, expected in fixture_expectations.items():
        eligible = version_tuple(system_version) >= candidate_minimum
        if eligible != expected:
            raise VerificationError(
                f"eligibility fixture {system_version} returned {eligible}; expected {expected}"
            )

    if args.require_legacy_compatible_entry:
        legacy_version = str(legacy.get("last_compatible_bundle_version") or "")
        legacy_minimum = str(legacy.get("minimum_system_version") or "")
        matches = [item for item in items if text(item, "version") == legacy_version]
        if not matches:
            raise VerificationError(
                f"appcast no longer preserves legacy compatible release {legacy_version}"
            )
        if all(version_tuple(text(item, "minimumSystemVersion")) > version_tuple("12.999") for item in matches):
            raise VerificationError("legacy release entry is not eligible for pre-macOS-13 users")
        if all(text(item, "minimumSystemVersion") != legacy_minimum for item in matches):
            raise VerificationError("legacy release minimumSystemVersion changed unexpectedly")

    print(
        f"verified appcast candidate {marketing_version} ({bundle_version}), "
        f"minimum macOS {minimum_version}, artifact {artifact_name}"
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except VerificationError as exc:
        print(f"error: {exc}", file=sys.stderr)
        raise SystemExit(1)
