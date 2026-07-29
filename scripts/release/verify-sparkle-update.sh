#!/usr/bin/env bash

# Verify a Sparkle enclosure without executing Sparkle tooling.
#
# Usage:
#   verify-sparkle-update.sh <zip> <appcast> <public-key-base64> \
#     <expected-url> <expected-version>
#
# The appcast must contain exactly one matching enclosure. Its URL, parent
# item's sparkle:version child (or legacy enclosure version attribute), and
# length must match the supplied values and archive bytes. A trailing Sparkle
# 2 sparkle-signatures comment is required and its declared signed-prefix
# length and Ed25519 signature are verified as well. The raw Ed25519 public key
# and signatures are decoded with Python's standard library, then OpenSSL
# verifies both the ZIP and appcast prefix bytes.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/lib.sh
source "${SCRIPT_DIR}/lib.sh"

[[ "$#" -eq 5 ]] || release_die "usage: scripts/release/verify-sparkle-update.sh <zip> <appcast> <public-key-base64> <expected-url> <expected-version>"
archive="$1"
appcast="$2"
public_key_b64="$3"
expected_url="$4"
expected_version="$5"
[[ -f "${archive}" ]] || release_die "Sparkle archive not found: ${archive}"
[[ -f "${appcast}" ]] || release_die "Sparkle appcast not found: ${appcast}"
release_require_cmd python3
release_require_cmd openssl

umask 077
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/vekil-sparkle-verify.XXXXXX")"
trap 'release_cleanup_dir "${tmp_dir}"' EXIT
public_der="${tmp_dir}/public.der"
signature_file="${tmp_dir}/signature.bin"
feed_prefix_file="${tmp_dir}/appcast-prefix.xml"
feed_signature_file="${tmp_dir}/appcast-signature.bin"

python3 - "${archive}" "${appcast}" "${public_key_b64}" "${expected_url}" "${expected_version}" \
  "${public_der}" "${signature_file}" "${feed_prefix_file}" "${feed_signature_file}" <<'PY'
import base64
import binascii
import os
import re
import sys
import xml.etree.ElementTree as ET

(
    archive,
    appcast,
    public_key_b64,
    expected_url,
    expected_version,
    public_der,
    signature_file,
    feed_prefix_file,
    feed_signature_file,
) = sys.argv[1:]
namespace = "http://www.andymatuschak.org/xml-namespaces/sparkle"
version_key = f"{{{namespace}}}version"
signature_key = f"{{{namespace}}}edSignature"

try:
    root = ET.parse(appcast).getroot()
except (ET.ParseError, OSError) as exc:
    raise SystemExit(f"invalid Sparkle appcast XML: {exc}")

enclosures = []
for item in root.iter():
    if item.tag.rsplit("}", 1)[-1] != "item":
        continue
    item_versions = [
        (child.text or "").strip()
        for child in list(item)
        if child.tag == version_key
    ]
    if len(item_versions) > 1:
        raise SystemExit("Sparkle item contains more than one sparkle:version child")
    item_version = item_versions[0] if item_versions else ""
    for element in item.iter():
        if element.tag.rsplit("}", 1)[-1] != "enclosure":
            continue
        attribute_version = element.attrib.get(version_key, "").strip()
        if attribute_version and item_version and attribute_version != item_version:
            raise SystemExit("Sparkle item/enclosure version values disagree")
        resolved_version = attribute_version or item_version
        if element.attrib.get("url") == expected_url and resolved_version == expected_version:
            enclosures.append(element)
if len(enclosures) != 1:
    raise SystemExit(f"expected exactly one enclosure for URL/version, found {len(enclosures)}")
enclosure = enclosures[0]

length = enclosure.attrib.get("length", "")
try:
    declared_length = int(length)
except ValueError:
    raise SystemExit("Sparkle enclosure length is not an integer")
actual_length = os.path.getsize(archive)
if declared_length != actual_length:
    raise SystemExit(f"Sparkle enclosure length mismatch: declared={declared_length} actual={actual_length}")

signature_b64 = enclosure.attrib.get(signature_key, "")
if not signature_b64:
    raise SystemExit("Sparkle enclosure is missing sparkle:edSignature")
try:
    public_key = base64.b64decode(public_key_b64, validate=True)
    signature = base64.b64decode(signature_b64, validate=True)
except binascii.Error as exc:
    raise SystemExit(f"invalid base64 in Sparkle key/signature: {exc}")
if len(public_key) != 32:
    raise SystemExit(f"Sparkle Ed25519 public key must be 32 bytes, got {len(public_key)}")
if len(signature) != 64:
    raise SystemExit(f"Sparkle Ed25519 signature must be 64 bytes, got {len(signature)}")

# RFC 8410 SubjectPublicKeyInfo prefix for a raw Ed25519 public key.
spki = bytes.fromhex("302a300506032b6570032100") + public_key
try:
    appcast_bytes = open(appcast, "rb").read()
except OSError as exc:
    raise SystemExit(f"could not read Sparkle appcast bytes: {exc}")
if appcast_bytes.count(b"<!-- sparkle-signatures:") != 1:
    raise SystemExit("Sparkle appcast must contain exactly one trailing sparkle-signatures comment")
feed_match = re.search(
    rb"<!-- sparkle-signatures:\r?\nedSignature:[ \t]*([A-Za-z0-9+/=]+)\r?\n"
    rb"length:[ \t]*([0-9]+)\r?\n-->[ \t\r\n]*\Z",
    appcast_bytes,
)
if feed_match is None:
    raise SystemExit("Sparkle appcast has a malformed or non-trailing sparkle-signatures comment")
try:
    feed_signature = base64.b64decode(feed_match.group(1), validate=True)
except binascii.Error as exc:
    raise SystemExit(f"invalid base64 in Sparkle appcast signature: {exc}")
if len(feed_signature) != 64:
    raise SystemExit(f"Sparkle appcast Ed25519 signature must be 64 bytes, got {len(feed_signature)}")
signed_length = int(feed_match.group(2))
if signed_length != feed_match.start():
    raise SystemExit(
        f"Sparkle appcast signed length does not match signature comment offset: "
        f"declared={signed_length} actual={feed_match.start()}"
    )

with open(public_der, "wb") as handle:
    handle.write(spki)
with open(signature_file, "wb") as handle:
    handle.write(signature)
with open(feed_prefix_file, "wb") as handle:
    handle.write(appcast_bytes[:signed_length])
with open(feed_signature_file, "wb") as handle:
    handle.write(feed_signature)
PY

if ! openssl pkeyutl -verify -pubin -keyform DER -inkey "${public_der}" \
  -rawin -in "${archive}" -sigfile "${signature_file}" >/dev/null 2>&1; then
  release_die "Sparkle Ed25519 enclosure signature verification failed"
fi
if ! openssl pkeyutl -verify -pubin -keyform DER -inkey "${public_der}" \
  -rawin -in "${feed_prefix_file}" -sigfile "${feed_signature_file}" >/dev/null 2>&1; then
  release_die "Sparkle Ed25519 appcast feed signature verification failed"
fi
printf 'verified Sparkle update and signed feed (version=%s length=%s url=%s)\n' \
  "${expected_version}" "$(wc -c <"${archive}" | tr -d ' ')" "${expected_url}"
