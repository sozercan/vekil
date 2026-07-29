"""Shared deterministic release-manifest implementation."""

from __future__ import annotations

import hashlib
import json
import os
import re
import stat
from pathlib import Path
from typing import Any

SCHEMA = "https://sozercan.github.io/vekil/release-manifest/v1"
HEX_OBJECT_RE = re.compile(r"^[0-9a-f]{40}(?:[0-9a-f]{24})?$")
SHA256_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
TAG_RE = re.compile(r"^v([0-9]+)\.([0-9]+)\.([0-9]+)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$")
REPOSITORY_RE = re.compile(r"^[^/\s]+/[^/\s]+$")


class ManifestError(ValueError):
    """A release manifest or its inputs violate the contract."""


def validate_release_tag(tag: str) -> re.Match[str]:
    match = TAG_RE.fullmatch(tag)
    if not match:
        raise ManifestError("release tag must be vMAJOR.MINOR.PATCH[-PRERELEASE]")
    for numeric in match.group(1), match.group(2), match.group(3):
        if len(numeric) > 1 and numeric.startswith("0"):
            raise ManifestError("release tag numeric identifiers must not have leading zeroes")
    prerelease = match.group(4)
    if prerelease:
        for identifier in prerelease.split("."):
            if identifier.isdigit() and len(identifier) > 1 and identifier.startswith("0"):
                raise ManifestError("release tag numeric prerelease identifiers must not have leading zeroes")
    return match


def canonical_bytes(value: Any) -> bytes:
    return (json.dumps(value, indent=2, sort_keys=True, ensure_ascii=False) + "\n").encode("utf-8")


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def env_required(primary: str, fallback: str | None = None) -> str:
    value = os.environ.get(primary, "").strip()
    if not value and fallback:
        value = os.environ.get(fallback, "").strip()
    if not value:
        suffix = f" (or {fallback})" if fallback else ""
        raise ManifestError(f"required environment variable is empty: {primary}{suffix}")
    return value


def json_env(name: str, default: Any) -> Any:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        return json.loads(raw)
    except json.JSONDecodeError as exc:
        raise ManifestError(f"{name} is not valid JSON: {exc}") from exc


def checked_relative_path(value: str, field: str = "path") -> str:
    path = Path(value)
    if not value or path.is_absolute() or ".." in path.parts or "." in path.parts:
        raise ManifestError(f"{field} must be a normalized relative path: {value!r}")
    normalized = path.as_posix()
    if normalized != value.replace(os.sep, "/"):
        raise ManifestError(f"{field} must use normalized path separators: {value!r}")
    return normalized


def media_type_for(path: str) -> str:
    lower = path.lower()
    if lower.endswith((".spdx.json", ".cdx.json", ".json")):
        return "application/json"
    if lower.endswith(".xml"):
        return "application/xml"
    if lower.endswith(".zip"):
        return "application/zip"
    if lower.endswith(".tar.gz") or lower.endswith(".tgz"):
        return "application/gzip"
    if lower.endswith(".gz"):
        return "application/gzip"
    if lower.endswith((".txt", ".sha256", ".sha256sum")) or Path(lower).name.startswith("checksums"):
        return "text/plain"
    if lower.endswith(".pem"):
        return "application/x-pem-file"
    return "application/octet-stream"


def kind_for(path: str) -> str:
    lower = path.lower()
    name = Path(lower).name
    if lower.endswith((".spdx.json", ".cdx.json", ".sbom")) or "sbom" in name:
        return "sbom"
    if name == "appcast.xml":
        return "sparkle-appcast"
    if "macos" in lower and lower.endswith(".zip"):
        return "macos-archive"
    if name.startswith("checksums") or lower.endswith((".sha256", ".sha256sum")):
        return "checksums"
    if "attestation" in lower or "provenance" in lower:
        return "attestation"
    return "artifact"


def load_tool_versions() -> dict[str, str]:
    configured = json_env("RELEASE_TOOLCHAIN_JSON", None)
    if configured is not None:
        if not isinstance(configured, dict) or not configured:
            raise ManifestError("RELEASE_TOOLCHAIN_JSON must be a non-empty JSON object")
        result = {}
        for key, value in configured.items():
            if not isinstance(key, str) or not key or not isinstance(value, str) or not value:
                raise ManifestError("RELEASE_TOOLCHAIN_JSON keys and values must be non-empty strings")
            result[key] = value
        return dict(sorted(result.items()))

    env_path = Path(__file__).with_name("tool-versions.env")
    wanted = {
        "ACTIONLINT_VERSION": "actionlint",
        "GOVULNCHECK_VERSION": "govulncheck",
        "GITLEAKS_VERSION": "gitleaks",
        "SYFT_VERSION": "syft",
        "GORELEASER_VERSION": "goreleaser",
    }
    result: dict[str, str] = {}
    for line in env_path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        if key in wanted:
            result[wanted[key]] = value.strip().strip("'\"")
    if set(result) != set(wanted.values()):
        raise ManifestError("tool-versions.env is missing one or more reviewed versions")
    go_version = os.environ.get("RELEASE_GO_VERSION", "").strip()
    if go_version:
        result["go"] = go_version
    return dict(sorted(result.items()))


def artifact_overrides() -> dict[str, dict[str, Any]]:
    value = json_env("RELEASE_ARTIFACT_METADATA_JSON", {})
    if not isinstance(value, dict):
        raise ManifestError("RELEASE_ARTIFACT_METADATA_JSON must be a JSON object keyed by relative path")
    result: dict[str, dict[str, Any]] = {}
    for path, metadata in value.items():
        if not isinstance(path, str) or not isinstance(metadata, dict):
            raise ManifestError("artifact metadata must map string paths to objects")
        path = checked_relative_path(path, "artifact metadata path")
        allowed = {"kind", "media_type", "sbom", "attestation_subject_sha256"}
        unknown = set(metadata) - allowed
        if unknown:
            raise ManifestError(f"unsupported artifact metadata keys for {path}: {sorted(unknown)}")
        normalized = dict(metadata)
        for key in ("kind", "media_type", "sbom", "attestation_subject_sha256"):
            if key in normalized and (not isinstance(normalized[key], str) or not normalized[key]):
                raise ManifestError(f"artifact metadata {key} for {path} must be a non-empty string")
        if "sbom" in normalized:
            normalized["sbom"] = checked_relative_path(normalized["sbom"], f"sbom for {path}")
        if "attestation_subject_sha256" in normalized and not re.fullmatch(
            r"[0-9a-f]{64}", normalized["attestation_subject_sha256"]
        ):
            raise ManifestError(f"attestation_subject_sha256 for {path} must be 64 lowercase hex characters")
        result[path] = normalized
    return result


def scan_artifacts(
    artifact_dir: Path, excluded: Path | None = None, *, use_env_overrides: bool = True
) -> list[dict[str, Any]]:
    root = artifact_dir.resolve(strict=True)
    if not root.is_dir():
        raise ManifestError(f"artifact directory is not a directory: {artifact_dir}")
    excluded_resolved = excluded.resolve() if excluded else None
    overrides = artifact_overrides() if use_env_overrides else {}
    artifacts: list[dict[str, Any]] = []
    seen_paths: set[str] = set()

    for current_root, dir_names, file_names in os.walk(root, followlinks=False):
        current = Path(current_root)
        for name in list(dir_names):
            candidate = current / name
            if candidate.is_symlink():
                raise ManifestError(f"artifact directory contains a symlinked directory: {candidate}")
        for name in file_names:
            path = current / name
            mode = path.lstat().st_mode
            if stat.S_ISLNK(mode):
                raise ManifestError(f"artifact directory contains a symlink: {path}")
            if not stat.S_ISREG(mode):
                raise ManifestError(f"artifact directory contains a non-regular file: {path}")
            if excluded_resolved and path.resolve() == excluded_resolved:
                continue
            relative = path.relative_to(root).as_posix()
            checked_relative_path(relative)
            metadata = overrides.get(relative, {})
            entry: dict[str, Any] = {
                "kind": metadata.get("kind", kind_for(relative)),
                "media_type": metadata.get("media_type", media_type_for(relative)),
                "name": Path(relative).name,
                "path": relative,
                "sha256": sha256_file(path),
                "size": path.stat().st_size,
            }
            for key in ("sbom", "attestation_subject_sha256"):
                if key in metadata:
                    entry[key] = metadata[key]
            artifacts.append(entry)
            seen_paths.add(relative)

    unknown = set(overrides) - seen_paths
    if unknown:
        raise ManifestError(f"artifact metadata references missing files: {sorted(unknown)}")
    artifacts.sort(key=lambda item: item["path"])
    if not artifacts:
        raise ManifestError("artifact directory contains no release files")
    return artifacts


def normalize_images(value: Any) -> list[dict[str, str]]:
    if not isinstance(value, list):
        raise ManifestError("RELEASE_IMAGES_JSON must be a JSON array")
    images: list[dict[str, str]] = []
    for item in value:
        if not isinstance(item, dict):
            raise ManifestError("each release image entry must be an object")
        repository = item.get("repository") or item.get("image")
        tag = item.get("tag")
        digest = item.get("digest")
        if not isinstance(repository, str) or not repository.startswith("ghcr.io/") or repository != repository.lower():
            raise ManifestError("image repository must be a lowercase ghcr.io/... string")
        if not isinstance(tag, str) or not re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}", tag):
            raise ManifestError(f"invalid image tag for {repository!r}")
        if not isinstance(digest, str) or not SHA256_RE.fullmatch(digest):
            raise ManifestError(f"invalid sha256 image digest for {repository}:{tag}")
        images.append({"repository": repository, "tag": tag, "digest": digest})
    images.sort(key=lambda item: (item["repository"], item["tag"], item["digest"]))
    if len({(item["repository"], item["tag"]) for item in images}) != len(images):
        raise ManifestError("release image repository/tag pairs must be unique")
    return images


def normalize_attestations(value: Any, artifacts: list[dict[str, Any]]) -> list[dict[str, str]]:
    if not isinstance(value, list):
        raise ManifestError("RELEASE_ATTESTATIONS_JSON must be a JSON array")
    artifact_hashes = {item["path"]: item["sha256"] for item in artifacts}
    result: list[dict[str, str]] = []
    for item in value:
        if not isinstance(item, dict):
            raise ManifestError("each attestation entry must be an object")
        subject = item.get("subject")
        digest = item.get("sha256") or item.get("digest")
        if not isinstance(subject, str):
            raise ManifestError("attestation subject must be an artifact path")
        subject = checked_relative_path(subject, "attestation subject")
        if isinstance(digest, str) and digest.startswith("sha256:"):
            digest = digest.removeprefix("sha256:")
        if not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest):
            raise ManifestError(f"attestation digest for {subject} must be 64 lowercase hex characters")
        if subject not in artifact_hashes:
            raise ManifestError(f"attestation subject is not a staged artifact: {subject}")
        if artifact_hashes[subject] != digest:
            raise ManifestError(f"attestation digest does not match staged artifact: {subject}")
        result.append({"subject": subject, "sha256": digest})
    result.sort(key=lambda item: item["subject"])
    if len({item["subject"] for item in result}) != len(result):
        raise ManifestError("attestation subjects must be unique")
    return result


def normalize_scans(value: Any) -> list[dict[str, Any]]:
    if not isinstance(value, list):
        raise ManifestError("RELEASE_SCANS_JSON must be a JSON array")
    result: list[dict[str, Any]] = []
    for item in value:
        if not isinstance(item, dict):
            raise ManifestError("each scan result must be an object")
        name = item.get("name")
        status_value = item.get("status")
        if not isinstance(name, str) or not name:
            raise ManifestError("scan name must be a non-empty string")
        if status_value not in {"passed", "skipped", "failed"}:
            raise ManifestError(f"scan {name!r} has unsupported status {status_value!r}")
        exceptions = item.get("exceptions", [])
        if not isinstance(exceptions, list) or any(not isinstance(value, str) or not value for value in exceptions):
            raise ManifestError(f"scan {name!r} exceptions must be a string array")
        result.append({"name": name, "status": status_value, "exceptions": sorted(set(exceptions))})
    result.sort(key=lambda item: item["name"])
    if len({item["name"] for item in result}) != len(result):
        raise ManifestError("scan names must be unique")
    return result


def normalize_exception_ids(value: Any) -> list[str]:
    if not isinstance(value, list) or any(not isinstance(item, str) or not item for item in value):
        raise ManifestError("RELEASE_VULNERABILITY_EXCEPTIONS_JSON must be a JSON string array")
    return sorted(set(value))


def build_manifest(artifact_dir: Path, output: Path) -> dict[str, Any]:
    repository = env_required("RELEASE_REPOSITORY", "GITHUB_REPOSITORY")
    tag = env_required("RELEASE_TAG", "GITHUB_REF_NAME")
    commit = env_required("RELEASE_COMMIT", "GITHUB_SHA").lower()
    tag_object = env_required("RELEASE_TAG_OBJECT").lower()
    run_id = env_required("RELEASE_RUN_ID", "GITHUB_RUN_ID")
    workflow = env_required("RELEASE_WORKFLOW", "GITHUB_WORKFLOW_REF")

    if not REPOSITORY_RE.fullmatch(repository):
        raise ManifestError("release repository must be owner/name")
    tag_match = validate_release_tag(tag)
    if not HEX_OBJECT_RE.fullmatch(commit):
        raise ManifestError("release commit must be a full lowercase 40- or 64-hex object ID")
    if not HEX_OBJECT_RE.fullmatch(tag_object):
        raise ManifestError("release tag object must be a full lowercase 40- or 64-hex object ID")
    if not re.fullmatch(r"[1-9][0-9]*", run_id):
        raise ManifestError("release run ID must be a positive integer")

    artifacts = scan_artifacts(artifact_dir, output)
    images = normalize_images(json_env("RELEASE_IMAGES_JSON", []))
    attestations = normalize_attestations(json_env("RELEASE_ATTESTATIONS_JSON", []), artifacts)
    scans = normalize_scans(json_env("RELEASE_SCANS_JSON", []))
    exceptions = normalize_exception_ids(json_env("RELEASE_VULNERABILITY_EXCEPTIONS_JSON", []))

    return {
        "artifacts": artifacts,
        "attestations": attestations,
        "images": images,
        "release": {
            "commit": commit,
            "prerelease": tag_match.group(4) is not None,
            "repository": repository,
            "tag": tag,
            "tag_object": tag_object,
            "version": tag.removeprefix("v"),
        },
        "scans": scans,
        "schema": SCHEMA,
        "schema_version": 1,
        "toolchain": load_tool_versions(),
        "vulnerability_exceptions": exceptions,
        "workflow": {"identity": workflow, "run_id": run_id},
    }


def validate_manifest_structure(manifest: Any) -> None:
    if not isinstance(manifest, dict):
        raise ManifestError("manifest root must be an object")
    expected_keys = {
        "artifacts",
        "attestations",
        "images",
        "release",
        "scans",
        "schema",
        "schema_version",
        "toolchain",
        "vulnerability_exceptions",
        "workflow",
    }
    if set(manifest) != expected_keys:
        raise ManifestError(f"manifest keys do not match schema: {sorted(set(manifest) ^ expected_keys)}")
    if manifest.get("schema") != SCHEMA or manifest.get("schema_version") != 1:
        raise ManifestError("unsupported release manifest schema")

    release = manifest.get("release")
    if not isinstance(release, dict) or set(release) != {"commit", "prerelease", "repository", "tag", "tag_object", "version"}:
        raise ManifestError("release metadata does not match schema")
    try:
        tag_match = validate_release_tag(str(release.get("tag", "")))
    except ManifestError as exc:
        raise ManifestError("release tag/version metadata is invalid") from exc
    if release.get("version") != str(release.get("tag", "")).removeprefix("v"):
        raise ManifestError("release tag/version metadata is invalid")
    if release.get("prerelease") is not (tag_match.group(4) is not None):
        raise ManifestError("release prerelease flag does not match tag")
    if not REPOSITORY_RE.fullmatch(str(release.get("repository", ""))):
        raise ManifestError("release repository is invalid")
    if not HEX_OBJECT_RE.fullmatch(str(release.get("commit", ""))) or not HEX_OBJECT_RE.fullmatch(str(release.get("tag_object", ""))):
        raise ManifestError("release object IDs are invalid")

    workflow = manifest.get("workflow")
    if not isinstance(workflow, dict) or set(workflow) != {"identity", "run_id"}:
        raise ManifestError("workflow metadata does not match schema")
    if not isinstance(workflow["identity"], str) or not workflow["identity"] or not re.fullmatch(r"[1-9][0-9]*", str(workflow["run_id"])):
        raise ManifestError("workflow metadata is invalid")

    artifacts = manifest.get("artifacts")
    if not isinstance(artifacts, list) or not artifacts:
        raise ManifestError("manifest artifacts must be a non-empty array")
    paths: list[str] = []
    for artifact in artifacts:
        if not isinstance(artifact, dict):
            raise ManifestError("artifact entries must be objects")
        required = {"kind", "media_type", "name", "path", "sha256", "size"}
        optional = {"sbom", "attestation_subject_sha256"}
        if not required.issubset(artifact) or set(artifact) - required - optional:
            raise ManifestError("artifact entry keys do not match schema")
        path = checked_relative_path(str(artifact["path"]), "artifact path")
        paths.append(path)
        if artifact["name"] != Path(path).name:
            raise ManifestError(f"artifact name does not match path: {path}")
        if not isinstance(artifact["size"], int) or artifact["size"] < 0:
            raise ManifestError(f"artifact size is invalid: {path}")
        if not re.fullmatch(r"[0-9a-f]{64}", str(artifact["sha256"])):
            raise ManifestError(f"artifact sha256 is invalid: {path}")
        if not isinstance(artifact["kind"], str) or not artifact["kind"]:
            raise ManifestError(f"artifact kind is invalid: {path}")
        if not isinstance(artifact["media_type"], str) or not artifact["media_type"]:
            raise ManifestError(f"artifact media type is invalid: {path}")
        if "sbom" in artifact:
            checked_relative_path(str(artifact["sbom"]), f"sbom for {path}")
        if "attestation_subject_sha256" in artifact and not re.fullmatch(r"[0-9a-f]{64}", str(artifact["attestation_subject_sha256"])):
            raise ManifestError(f"artifact attestation subject digest is invalid: {path}")
    if paths != sorted(paths) or len(set(paths)) != len(paths):
        raise ManifestError("artifact paths must be unique and sorted")
    path_set = set(paths)
    for artifact in artifacts:
        if "sbom" in artifact and artifact["sbom"] not in path_set:
            raise ManifestError(f"artifact SBOM is not present in manifest: {artifact['sbom']}")
        if "attestation_subject_sha256" in artifact and artifact["attestation_subject_sha256"] != artifact["sha256"]:
            raise ManifestError(f"artifact attestation subject digest mismatch: {artifact['path']}")

    normalized_images = normalize_images(manifest.get("images"))
    if normalized_images != manifest.get("images"):
        raise ManifestError("image entries are not canonical")
    normalized_attestations = normalize_attestations(manifest.get("attestations"), artifacts)
    if normalized_attestations != manifest.get("attestations"):
        raise ManifestError("attestation entries are not canonical")
    normalized_scans = normalize_scans(manifest.get("scans"))
    if normalized_scans != manifest.get("scans"):
        raise ManifestError("scan entries are not canonical")
    normalized_exceptions = normalize_exception_ids(manifest.get("vulnerability_exceptions"))
    if normalized_exceptions != manifest.get("vulnerability_exceptions"):
        raise ManifestError("vulnerability exception IDs are not canonical")
    toolchain = manifest.get("toolchain")
    if not isinstance(toolchain, dict) or not toolchain or dict(sorted(toolchain.items())) != toolchain:
        raise ManifestError("toolchain must be a sorted non-empty string map")
    if any(not isinstance(key, str) or not key or not isinstance(value, str) or not value for key, value in toolchain.items()):
        raise ManifestError("toolchain keys and values must be non-empty strings")
