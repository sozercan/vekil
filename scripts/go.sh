#!/usr/bin/env bash

set -euo pipefail

if command -v go >/dev/null 2>&1; then
	exec go "$@"
fi

die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

resolve_base_version() {
	if [[ -n "${GO_BOOTSTRAP_BASE_VERSION:-}" ]]; then
		printf '%s\n' "${GO_BOOTSTRAP_BASE_VERSION}"
		return 0
	fi

	local version
	version="$(sed -n 's/^go \([0-9][0-9.]*\)$/\1/p' "${REPO_ROOT}/go.mod" | head -n 1)"
	if [[ -z "${version}" ]]; then
		die "unable to determine Go version from go.mod"
	fi
	printf '%s\n' "${version}"
}

normalize_os() {
	case "$(uname -s)" in
	Linux) printf 'linux\n' ;;
	Darwin) printf 'darwin\n' ;;
	*) die "unsupported OS: $(uname -s)" ;;
	esac
}

normalize_arch() {
	case "$(uname -m)" in
	x86_64|amd64) printf 'amd64\n' ;;
	aarch64|arm64) printf 'arm64\n' ;;
	*) die "unsupported architecture: $(uname -m)" ;;
	esac
}

fetch_with_node() {
	local url="$1"
	local output="$2"
	node - "$url" "$output" <<'EOF'
const fs = require('fs');
const http = require('http');
const https = require('https');
const { URL } = require('url');

const [, , startUrl, output] = process.argv;

function download(url, redirectsLeft) {
  const client = url.startsWith('https:') ? https : http;
  client.get(url, (res) => {
    if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
      if (redirectsLeft === 0) {
        console.error(`too many redirects for ${startUrl}`);
        process.exit(1);
      }
      const nextUrl = new URL(res.headers.location, url).toString();
      res.resume();
      download(nextUrl, redirectsLeft - 1);
      return;
    }
    if (res.statusCode !== 200) {
      console.error(`failed to download ${startUrl}: HTTP ${res.statusCode}`);
      process.exit(1);
    }
    const out = fs.createWriteStream(output);
    res.pipe(out);
    out.on('finish', () => out.close(() => process.exit(0)));
  }).on('error', (err) => {
    console.error(err.message);
    process.exit(1);
  });
}

download(startUrl, 10);
EOF
}

fetch() {
	local url="$1"
	local output="$2"
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$url" -o "$output"
		return 0
	fi
	if command -v wget >/dev/null 2>&1; then
		wget -qO "$output" "$url"
		return 0
	fi
	if command -v node >/dev/null 2>&1; then
		fetch_with_node "$url" "$output"
		return 0
	fi
	die "missing downloader: install curl, wget, or node"
}

resolve_version() {
	if [[ -n "${GO_BOOTSTRAP_VERSION:-}" ]]; then
		printf '%s\n' "${GO_BOOTSTRAP_VERSION}"
		return 0
	fi

	local base_version="$1"
	local cache_dir="${REPO_ROOT}/.build/tools/go"
	local cache_file="${cache_dir}/resolved-${base_version}"
	if [[ -f "${cache_file}" ]]; then
		cat "${cache_file}"
		return 0
	fi

	mkdir -p "${cache_dir}"
	local json_file
	json_file="$(mktemp "${cache_dir}/go-dl.XXXXXX.json")"
	if fetch "https://go.dev/dl/?mode=json&include=all" "${json_file}"; then
		local version
		version="$(node - "${base_version}" "${json_file}" <<'EOF'
const fs = require('fs');

const [, , baseVersion, jsonPath] = process.argv;
const releases = JSON.parse(fs.readFileSync(jsonPath, 'utf8'));
const match = releases.find((release) => release.stable && release.version.startsWith(`go${baseVersion}.`));
if (match) {
  process.stdout.write(match.version.slice(2));
}
EOF
)"
		rm -f "${json_file}"
		if [[ -n "${version}" ]]; then
			printf '%s\n' "${version}" > "${cache_file}"
			printf '%s\n' "${version}"
			return 0
		fi
	else
		rm -f "${json_file}"
	fi

	printf '%s.0\n' "${base_version}" > "${cache_file}"
	printf '%s.0\n' "${base_version}"
}

BASE_VERSION="$(resolve_base_version)"
GO_VERSION="$(resolve_version "${BASE_VERSION}")"
GO_OS="$(normalize_os)"
GO_ARCH="$(normalize_arch)"
INSTALL_ROOT="${REPO_ROOT}/.build/tools/go/${GO_VERSION}/${GO_OS}-${GO_ARCH}"
GO_BIN="${INSTALL_ROOT}/go/bin/go"

if [[ ! -x "${GO_BIN}" ]]; then
	URL="${GO_BOOTSTRAP_URL:-https://go.dev/dl/go${GO_VERSION}.${GO_OS}-${GO_ARCH}.tar.gz}"
	mkdir -p "${REPO_ROOT}/.build"
	mkdir -p "${INSTALL_ROOT}"
	ARCHIVE="$(mktemp "${REPO_ROOT}/.build/go.${GO_VERSION}.${GO_OS}-${GO_ARCH}.XXXXXX.tar.gz")"
	printf 'bootstrapping Go %s for %s/%s\n' "${GO_VERSION}" "${GO_OS}" "${GO_ARCH}" >&2
	fetch "${URL}" "${ARCHIVE}"
	rm -rf "${INSTALL_ROOT}/go"
	tar -C "${INSTALL_ROOT}" -xzf "${ARCHIVE}"
	rm -f "${ARCHIVE}"
fi

export GOTOOLCHAIN=local
exec "${GO_BIN}" "$@"
