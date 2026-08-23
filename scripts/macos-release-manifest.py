#!/usr/bin/env python3
"""Resolve and validate the native macOS build manifest and Info.plist."""

from __future__ import annotations

import argparse
import json
import os
import plistlib
import re
import subprocess
import sys
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parent.parent
DEFAULT_CONFIG = ROOT / "build-support/macos/app-config.json"
BUNDLE_VERSION_RE = re.compile(r"^(?:0|[1-9][0-9]{0,17})$")
SYSTEM_VERSION_RE = re.compile(r"^[0-9]+(?:\.[0-9]+){0,3}$")
MARKETING_VERSION_RE = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$")
BUILD_ID_RE = re.compile(r"^[0-9A-Za-z][0-9A-Za-z._+-]{0,127}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")


class ManifestError(ValueError):
    pass


def load_json(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise ManifestError(f"file not found: {path}") from exc
    except json.JSONDecodeError as exc:
        raise ManifestError(f"invalid JSON in {path}: {exc}") from exc
    if not isinstance(value, dict):
        raise ManifestError(f"expected a JSON object in {path}")
    return value


def require_string(mapping: dict[str, Any], key: str, where: str) -> str:
    value = mapping.get(key)
    if not isinstance(value, str) or not value.strip():
        raise ManifestError(f"{where}.{key} must be a non-empty string")
    if value != value.strip():
        raise ManifestError(f"{where}.{key} must not contain surrounding whitespace")
    return value


def bundle_version_int(value: str) -> int:
    if not BUNDLE_VERSION_RE.fullmatch(value):
        raise ManifestError(
            f"invalid CFBundleVersion {value!r}; expected a numeric value of at most 18 digits"
        )
    return int(value)


def system_version_tuple(value: str) -> tuple[int, int, int, int]:
    if not SYSTEM_VERSION_RE.fullmatch(value):
        raise ManifestError(f"invalid dotted system/appcast version: {value!r}")
    parts = [int(part) for part in value.split(".")]
    return tuple((parts + [0, 0, 0, 0])[:4])  # type: ignore[return-value]


def git_output(*args: str) -> str:
    try:
        return subprocess.check_output(
            ["git", *args], cwd=ROOT, text=True, stderr=subprocess.DEVNULL
        ).strip()
    except (OSError, subprocess.CalledProcessError):
        return ""


def normalize_marketing_version(value: str) -> str:
    value = value.strip()
    if value.startswith("v"):
        value = value[1:]
    if not value:
        raise ManifestError("marketing version must not be empty")
    if any(char.isspace() for char in value):
        raise ManifestError("marketing version must not contain whitespace")
    return value


def validate_config(config: dict[str, Any]) -> None:
    if config.get("schema_version") != 1:
        raise ManifestError("app config schema_version must be 1")

    app = config.get("application")
    sparkle = config.get("sparkle")
    legacy = config.get("legacy_shell")
    if not isinstance(app, dict) or not isinstance(sparkle, dict) or not isinstance(legacy, dict):
        raise ManifestError("application, sparkle, and legacy_shell must be objects")

    app_keys = (
        "name",
        "bundle_name",
        "bundle_identifier",
        "executable",
        "helper_executable",
        "minimum_system_version",
        "icon_path",
        "swift_package_path",
        "swift_product",
        "go_helper_package",
        "artifact_name",
        "release_manifest_name",
    )
    for key in app_keys:
        require_string(app, key, "application")

    if app["bundle_name"] != "Vekil.app":
        raise ManifestError("application.bundle_name must remain Vekil.app")
    if app["bundle_identifier"] != "com.vekil.menubar":
        raise ManifestError("application.bundle_identifier must remain com.vekil.menubar")
    if app["executable"] != "Vekil":
        raise ManifestError("application.executable must be Vekil")
    if app["helper_executable"] != "vekil-runtime":
        raise ManifestError("application.helper_executable must be vekil-runtime")
    if system_version_tuple(app["minimum_system_version"]) != (13, 0, 0, 0):
        raise ManifestError("application.minimum_system_version must be 13.0")
    if app["swift_package_path"] != "mac/VekilApp":
        raise ManifestError("application.swift_package_path must be mac/VekilApp")
    if app["go_helper_package"] != "./cmd/macos-runtime":
        raise ManifestError("application.go_helper_package must be ./cmd/macos-runtime")
    if app["artifact_name"] != "vekil-macos-universal.zip":
        raise ManifestError("application.artifact_name must be vekil-macos-universal.zip")

    sparkle_keys = (
        "version",
        "archive_name",
        "archive_url",
        "archive_sha256",
        "framework_path",
        "generate_appcast_path",
        "feed_url",
        "public_ed_key",
        "swift_package_revision",
    )
    for key in sparkle_keys:
        require_string(sparkle, key, "sparkle")
    if sparkle["version"] != "2.9.4":
        raise ManifestError("Sparkle must be pinned exactly to 2.9.4")
    if sparkle["archive_name"] != "Sparkle-2.9.4.tar.xz":
        raise ManifestError("unexpected Sparkle archive name")
    if sparkle["archive_url"] != (
        "https://github.com/sparkle-project/Sparkle/releases/download/2.9.4/"
        "Sparkle-2.9.4.tar.xz"
    ):
        raise ManifestError("unexpected Sparkle archive URL")
    if not SHA256_RE.fullmatch(sparkle["archive_sha256"]):
        raise ManifestError("sparkle.archive_sha256 must be 64 lowercase hexadecimal characters")
    if not re.fullmatch(r"[0-9a-f]{40}", sparkle["swift_package_revision"]):
        raise ManifestError("sparkle.swift_package_revision must be a full lowercase Git SHA")
    if not re.fullmatch(r"[A-Za-z0-9+/]{43}=", sparkle["public_ed_key"]):
        raise ManifestError("sparkle.public_ed_key does not look like a Sparkle Ed25519 public key")
    if not sparkle["feed_url"].startswith("https://"):
        raise ManifestError("sparkle.feed_url must use HTTPS")

    legacy_keys = (
        "minimum_system_version",
        "last_compatible_marketing_version",
        "last_compatible_bundle_version",
        "last_compatible_artifact_url",
        "last_compatible_artifact_sha256",
        "last_compatible_appcast_url",
    )
    for key in legacy_keys:
        require_string(legacy, key, "legacy_shell")
    system_version_tuple(legacy["minimum_system_version"])
    system_version_tuple(legacy["last_compatible_bundle_version"])
    if not SHA256_RE.fullmatch(legacy["last_compatible_artifact_sha256"]):
        raise ManifestError("legacy_shell.last_compatible_artifact_sha256 is invalid")
    for key in ("last_compatible_artifact_url", "last_compatible_appcast_url"):
        if not legacy[key].startswith("https://"):
            raise ManifestError(f"legacy_shell.{key} must use HTTPS")


def resolve_manifest(args: argparse.Namespace) -> dict[str, Any]:
    config = load_json(Path(args.config))
    validate_config(config)

    raw_marketing = args.marketing_version or os.environ.get("VERSION") or ""
    if not raw_marketing:
        described = git_output("describe", "--tags", "--exact-match")
        raw_marketing = described or f"dev-{git_output('rev-parse', '--short', 'HEAD') or 'unknown'}"
    marketing_version = normalize_marketing_version(raw_marketing)

    explicit_bundle_version = args.bundle_version or os.environ.get("MACOS_BUNDLE_VERSION") or ""
    bundle_version = explicit_bundle_version.strip()
    release = bool(args.release or os.environ.get("MACOS_RELEASE") == "1")
    if release and not bundle_version:
        raise ManifestError("release builds require an explicit numeric MACOS_BUNDLE_VERSION")
    if not bundle_version:
        bundle_version = git_output("rev-list", "--count", "HEAD") or "1"
    bundle_version_int(bundle_version)
    if bundle_version == marketing_version:
        raise ManifestError("CFBundleVersion must be distinct from the marketing version")

    source_commit = (args.source_commit or git_output("rev-parse", "HEAD") or "unknown").strip()
    if source_commit != "unknown" and not re.fullmatch(r"[0-9a-f]{40}", source_commit):
        raise ManifestError("source commit must be a full lowercase 40-character Git SHA")

    explicit_bundle_build_id = (
        args.bundle_build_id or os.environ.get("MACOS_BUNDLE_BUILD_ID") or ""
    ).strip()
    if release and not explicit_bundle_build_id:
        raise ManifestError("release builds require an explicit MACOS_BUNDLE_BUILD_ID")
    bundle_build_id = explicit_bundle_build_id
    if not bundle_build_id:
        suffix = source_commit[:12] if source_commit != "unknown" else "unknown"
        bundle_build_id = f"vekil-{bundle_version}-{suffix}"
    if not BUILD_ID_RE.fullmatch(bundle_build_id):
        raise ManifestError(
            "bundle build ID must be 1-128 characters using letters, digits, '.', '_', '+', or '-'"
        )

    previous_bundle_version = (
        args.previous_bundle_version
        or os.environ.get("MACOS_PREVIOUS_BUNDLE_VERSION")
        or ""
    ).strip()
    if release and not previous_bundle_version:
        raise ManifestError(
            "release builds require an explicit numeric MACOS_PREVIOUS_BUNDLE_VERSION baseline"
        )
    if not previous_bundle_version:
        previous_bundle_version = "0"
    bundle_version_int(previous_bundle_version)

    if release:
        if not MARKETING_VERSION_RE.fullmatch(marketing_version):
            raise ManifestError(
                "release marketing version must be semantic version text such as 1.2.3 or 1.2.3-rc.1"
            )
        if bundle_version_int(bundle_version) <= bundle_version_int(previous_bundle_version):
            raise ManifestError(
                f"CFBundleVersion {bundle_version} must be greater than prior release "
                f"{previous_bundle_version}"
            )
        if source_commit == "unknown":
            raise ManifestError("release builds require a full source commit")

    return {
        "schema_version": 1,
        "marketing_version": marketing_version,
        "bundle_version": bundle_version,
        "bundle_build_id": bundle_build_id,
        "source_commit": source_commit,
        "release_build": release,
        "application": config["application"],
        "sparkle": config["sparkle"],
        "legacy_shell": config["legacy_shell"],
    }


def write_json(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def write_plist(args: argparse.Namespace) -> None:
    manifest = load_json(Path(args.manifest))
    validate_resolved_manifest(manifest)
    app = manifest["application"]
    sparkle = manifest["sparkle"]

    legacy = bool(args.legacy)
    executable = "vekil-menubar" if legacy else app["executable"]
    minimum_system_version = (
        manifest["legacy_shell"]["minimum_system_version"]
        if legacy
        else app["minimum_system_version"]
    )
    plist: dict[str, Any] = {
        "CFBundleDevelopmentRegion": "en",
        "CFBundleExecutable": executable,
        "CFBundleIdentifier": app["bundle_identifier"],
        "CFBundleIconFile": "Vekil.icns",
        "CFBundleInfoDictionaryVersion": "6.0",
        "CFBundleName": app["name"],
        "CFBundleDisplayName": app["name"],
        "CFBundlePackageType": "APPL",
        "CFBundleShortVersionString": manifest["marketing_version"],
        "CFBundleVersion": manifest["bundle_version"],
        "LSMinimumSystemVersion": minimum_system_version,
        "LSUIElement": True,
        "NSHighResolutionCapable": True,
        "SUFeedURL": sparkle["feed_url"],
        "SUPublicEDKey": sparkle["public_ed_key"],
        "SURequireSignedFeed": True,
        "SUVerifyUpdateBeforeExtraction": True,
        "VekilBundleBuildID": manifest["bundle_build_id"],
        "VekilReleaseManifest": app["release_manifest_name"],
    }
    if legacy:
        # Retain the old Go shell's updater behavior during the two-release rollback window.
        plist["SUEnableInstallerLauncherService"] = True
    else:
        # Update checks are user-initiated only, so Sparkle never shows its automatic-check
        # permission prompt at launch and cold/login/update launches stay menu-only.
        plist["SUEnableAutomaticChecks"] = False

    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    with output.open("wb") as handle:
        plistlib.dump(plist, handle, fmt=plistlib.FMT_XML, sort_keys=True)


def validate_resolved_manifest(manifest: dict[str, Any]) -> None:
    if manifest.get("schema_version") != 1:
        raise ManifestError("resolved manifest schema_version must be 1")
    for key in ("marketing_version", "bundle_version", "bundle_build_id", "source_commit"):
        require_string(manifest, key, "manifest")
    bundle_version_int(manifest["bundle_version"])
    if not BUILD_ID_RE.fullmatch(manifest["bundle_build_id"]):
        raise ManifestError("invalid manifest.bundle_build_id")
    config_view = {
        "schema_version": 1,
        "application": manifest.get("application"),
        "sparkle": manifest.get("sparkle"),
        "legacy_shell": manifest.get("legacy_shell"),
    }
    validate_config(config_view)


def get_value(document: dict[str, Any], dotted_key: str) -> Any:
    current: Any = document
    for part in dotted_key.split("."):
        if not isinstance(current, dict) or part not in current:
            raise ManifestError(f"missing JSON key: {dotted_key}")
        current = current[part]
    return current


def main() -> int:
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="command", required=True)

    validate_parser = subparsers.add_parser("validate-config")
    validate_parser.add_argument("--config", default=str(DEFAULT_CONFIG))

    resolve_parser = subparsers.add_parser("resolve")
    resolve_parser.add_argument("--config", default=str(DEFAULT_CONFIG))
    resolve_parser.add_argument("--output", required=True)
    resolve_parser.add_argument("--marketing-version")
    resolve_parser.add_argument("--bundle-version")
    resolve_parser.add_argument("--bundle-build-id")
    resolve_parser.add_argument("--source-commit")
    resolve_parser.add_argument("--previous-bundle-version")
    resolve_parser.add_argument("--release", action="store_true")

    plist_parser = subparsers.add_parser("plist")
    plist_parser.add_argument("--manifest", required=True)
    plist_parser.add_argument("--output", required=True)
    plist_parser.add_argument("--legacy", action="store_true")

    resolved_parser = subparsers.add_parser("validate")
    resolved_parser.add_argument("--manifest", required=True)

    get_parser = subparsers.add_parser("get")
    get_parser.add_argument("--file", default=str(DEFAULT_CONFIG))
    get_parser.add_argument("--key", required=True)

    compare_parser = subparsers.add_parser("compare-bundle-versions")
    compare_parser.add_argument("left")
    compare_parser.add_argument("right")

    compare_system_parser = subparsers.add_parser("compare-system-versions")
    compare_system_parser.add_argument("left")
    compare_system_parser.add_argument("right")

    args = parser.parse_args()
    try:
        if args.command == "validate-config":
            validate_config(load_json(Path(args.config)))
        elif args.command == "resolve":
            write_json(Path(args.output), resolve_manifest(args))
        elif args.command == "plist":
            write_plist(args)
        elif args.command == "validate":
            validate_resolved_manifest(load_json(Path(args.manifest)))
        elif args.command == "get":
            value = get_value(load_json(Path(args.file)), args.key)
            if isinstance(value, bool):
                print("true" if value else "false")
            elif isinstance(value, (dict, list)):
                print(json.dumps(value, sort_keys=True))
            else:
                print(value)
        elif args.command == "compare-bundle-versions":
            left = bundle_version_int(args.left)
            right = bundle_version_int(args.right)
            print(-1 if left < right else 1 if left > right else 0)
        elif args.command == "compare-system-versions":
            left = system_version_tuple(args.left)
            right = system_version_tuple(args.right)
            print(-1 if left < right else 1 if left > right else 0)
        else:
            raise AssertionError(args.command)
    except ManifestError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
