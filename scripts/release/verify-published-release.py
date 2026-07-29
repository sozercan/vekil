#!/usr/bin/env python3
"""Verify published GitHub release assets against a local release manifest."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import BinaryIO

sys.dont_write_bytecode = True

from _manifest import ManifestError, canonical_bytes, validate_manifest_structure


class SafeRedirectHandler(urllib.request.HTTPRedirectHandler):
    """Drop API authorization when a GitHub asset redirects cross-origin."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):  # noqa: ANN001
        redirected = super().redirect_request(req, fp, code, msg, headers, newurl)
        if redirected is not None:
            old = urllib.parse.urlsplit(req.full_url)
            new = urllib.parse.urlsplit(newurl)
            if (old.scheme, old.netloc) != (new.scheme, new.netloc):
                redirected.remove_header("Authorization")
        return redirected


OPENER = urllib.request.build_opener(SafeRedirectHandler())


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", required=True, type=Path)
    parser.add_argument("--repository", help="owner/name; defaults to manifest metadata")
    parser.add_argument("--tag", help="release tag; defaults to manifest metadata")
    parser.add_argument("--api-url", default=os.environ.get("GITHUB_API_URL", "https://api.github.com"))
    parser.add_argument(
        "--allow-extra-asset",
        action="append",
        default=[],
        help="asset name allowed in addition to manifest-declared files (repeatable)",
    )
    return parser.parse_args()


def request(url: str, accept: str) -> urllib.request.Request:
    headers = {
        "Accept": accept,
        "User-Agent": "vekil-release-verifier",
        "X-GitHub-Api-Version": "2022-11-28",
    }
    token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
    if token:
        headers["Authorization"] = f"Bearer {token}"
    return urllib.request.Request(url, headers=headers)


def read_json(url: str) -> dict:
    with OPENER.open(request(url, "application/vnd.github+json"), timeout=60) as response:
        return json.load(response)


def hash_stream(handle: BinaryIO) -> tuple[int, str, bytes | None]:
    digest = hashlib.sha256()
    size = 0
    retained = bytearray()
    retain = True
    while True:
        chunk = handle.read(1024 * 1024)
        if not chunk:
            break
        size += len(chunk)
        digest.update(chunk)
        if retain and len(retained) + len(chunk) <= 16 * 1024 * 1024:
            retained.extend(chunk)
        else:
            retain = False
            retained.clear()
    return size, digest.hexdigest(), bytes(retained) if retain else None


def main() -> int:
    args = parse_args()
    try:
        manifest_raw = args.manifest.read_bytes()
        manifest = json.loads(manifest_raw)
        validate_manifest_structure(manifest)
        if manifest_raw != canonical_bytes(manifest):
            raise ManifestError("local release manifest is not canonical")
        repository = args.repository or manifest["release"]["repository"]
        tag = args.tag or manifest["release"]["tag"]
        if repository != manifest["release"]["repository"] or tag != manifest["release"]["tag"]:
            raise ManifestError("requested repository/tag does not match manifest metadata")

        endpoint = (
            f"{args.api_url.rstrip('/')}/repos/{urllib.parse.quote(repository, safe='/')}"
            f"/releases/tags/{urllib.parse.quote(tag, safe='')}"
        )
        release = read_json(endpoint)
        if release.get("tag_name") != tag:
            raise ManifestError("GitHub release tag does not match manifest")
        if release.get("draft") is not False:
            raise ManifestError("GitHub release is still a draft")
        if bool(release.get("prerelease")) != bool(manifest["release"]["prerelease"]):
            raise ManifestError("GitHub release prerelease flag does not match manifest")

        assets = release.get("assets")
        if not isinstance(assets, list):
            raise ManifestError("GitHub release response did not contain assets")
        by_name: dict[str, dict] = {}
        for asset in assets:
            name = asset.get("name") if isinstance(asset, dict) else None
            if not isinstance(name, str) or not name:
                raise ManifestError("GitHub release asset has no name")
            if name in by_name:
                raise ManifestError(f"GitHub release contains duplicate asset name: {name}")
            by_name[name] = asset

        expected = {item["name"]: item for item in manifest["artifacts"]}
        if len(expected) != len(manifest["artifacts"]):
            raise ManifestError("manifest artifact basenames must be unique for GitHub release publication")
        manifest_name = args.manifest.name
        expected_names = set(expected) | {manifest_name}
        missing = sorted(expected_names - set(by_name))
        extras = sorted(set(by_name) - expected_names - set(args.allow_extra_asset))
        if missing or extras:
            raise ManifestError(f"published release asset set mismatch (missing={missing}, extra={extras})")

        for name in sorted(expected_names):
            asset = by_name[name]
            if name == manifest_name:
                expected_size = len(manifest_raw)
                expected_sha = hashlib.sha256(manifest_raw).hexdigest()
            else:
                expected_size = expected[name]["size"]
                expected_sha = expected[name]["sha256"]
            if asset.get("size") != expected_size:
                raise ManifestError(f"GitHub API asset size mismatch for {name}")
            api_digest = asset.get("digest")
            if api_digest is not None and api_digest != f"sha256:{expected_sha}":
                raise ManifestError(f"GitHub API asset digest mismatch for {name}")
            download_url = asset.get("url") or asset.get("browser_download_url")
            if not isinstance(download_url, str) or not download_url:
                raise ManifestError(f"GitHub asset has no download URL: {name}")
            parsed_download = urllib.parse.urlsplit(download_url)
            parsed_api = urllib.parse.urlsplit(args.api_url)
            if asset.get("url") and (parsed_download.scheme, parsed_download.netloc) != (parsed_api.scheme, parsed_api.netloc):
                raise ManifestError(f"GitHub API asset URL changed origin for {name}")
            with OPENER.open(request(download_url, "application/octet-stream"), timeout=120) as response:
                actual_size, actual_sha, retained = hash_stream(response)
            if actual_size != expected_size or actual_sha != expected_sha:
                raise ManifestError(f"downloaded release asset digest/size mismatch for {name}")
            if name == manifest_name and retained != manifest_raw:
                raise ManifestError("published release manifest bytes differ from the verified local manifest")
            print(f"verified published asset {name} size={actual_size} sha256={actual_sha}")
    except (ManifestError, OSError, json.JSONDecodeError, urllib.error.URLError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    print(f"verified published GitHub release {repository}@{tag}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
