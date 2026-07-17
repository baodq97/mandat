#!/bin/sh
# Installs the mandat static binary in one command (design spec §4.10:
# "install time is one download"). POSIX sh, not bash: this script is meant
# to run as `curl -fsSL .../install.sh | sh` on whatever /bin/sh the target
# VM ships (often dash), so bashisms would silently break the one thing this
# script exists to make reliable.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/baodq97/mandat/main/install.sh | sh
#
# Overrides (env vars):
#   MANDAT_VERSION  install this tag instead of the latest release (e.g. v0.1.0)
#   BIN_DIR         install directory (default: $PREFIX/bin)
#   PREFIX          default: /usr/local (ignored if BIN_DIR is set)
set -eu

REPO="baodq97/mandat"
PREFIX="${PREFIX:-/usr/local}"
BIN_DIR="${BIN_DIR:-$PREFIX/bin}"

die() {
	printf 'install.sh: error: %s\n' "$1" >&2
	exit 1
}

require_cmd() {
	command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

# GitHub's unauthenticated REST API is capped at 60 req/hour per IP, which a
# widely-shared install script would burn through fast. Following the
# /releases/latest redirect to its resolved /releases/tag/<tag> URL gets the
# same answer without touching the API or needing a JSON parser.
resolve_latest_tag() {
	url="https://github.com/${REPO}/releases/latest"
	resolved=$(curl -fsSL -o /dev/null -w '%{url_effective}' "$url") ||
		die "could not resolve the latest release from $url"
	tag=$(printf '%s' "$resolved" | sed -n 's#.*/releases/tag/##p')
	[ -n "$tag" ] || die "could not parse a release tag out of: $resolved"
	printf '%s' "$tag"
}

# Directory may not exist yet on a fresh box (e.g. a from-scratch $BIN_DIR
# override); writability then depends on its parent, not the missing path.
dir_is_writable() {
	if [ -d "$1" ]; then
		[ -w "$1" ]
	else
		[ -w "$(dirname "$1")" ]
	fi
}

verify_checksum() {
	bin_path="$1"
	sum_path="$2"
	if command -v sha256sum >/dev/null 2>&1; then
		( cd "$(dirname "$bin_path")" && sha256sum -c "$(basename "$sum_path")" ) >/dev/null ||
			die "checksum verification failed for $(basename "$bin_path")"
	elif command -v shasum >/dev/null 2>&1; then
		expected=$(awk '{print $1}' "$sum_path")
		actual=$(shasum -a 256 "$bin_path" | awk '{print $1}')
		[ "$expected" = "$actual" ] ||
			die "checksum verification failed for $(basename "$bin_path")"
	else
		die "neither sha256sum nor shasum found; cannot verify download integrity"
	fi
}

require_cmd curl
require_cmd uname
require_cmd sed
require_cmd awk

[ "$(uname -s)" = "Linux" ] || die "mandat only ships linux binaries (detected: $(uname -s))"

case "$(uname -m)" in
	x86_64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported architecture: $(uname -m) (mandat ships linux/amd64 and linux/arm64 only)" ;;
esac

tag="${MANDAT_VERSION:-}"
if [ -z "$tag" ]; then
	tag=$(resolve_latest_tag)
fi

bin_name="mandat-linux-${arch}"
base_url="https://github.com/${REPO}/releases/download/${tag}"

tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT

echo "install.sh: downloading ${bin_name} (${tag})..."
curl -fsSL -o "${tmp_dir}/${bin_name}" "${base_url}/${bin_name}" ||
	die "download failed: ${base_url}/${bin_name} (does release ${tag} exist for linux/${arch}?)"
curl -fsSL -o "${tmp_dir}/${bin_name}.sha256" "${base_url}/${bin_name}.sha256" ||
	die "checksum download failed: ${base_url}/${bin_name}.sha256"

verify_checksum "${tmp_dir}/${bin_name}" "${tmp_dir}/${bin_name}.sha256"

sudo_cmd=""
if ! dir_is_writable "$BIN_DIR"; then
	require_cmd sudo
	sudo_cmd="sudo"
fi

# Stage-and-rename instead of cp: overwriting a RUNNING mandat (the always-on
# systemd unit) fails with ETXTBSY, while rename() swaps the directory entry
# atomically and the running process keeps its old inode until restart.
$sudo_cmd mkdir -p "$BIN_DIR"
$sudo_cmd cp "${tmp_dir}/${bin_name}" "${BIN_DIR}/.mandat.new"
$sudo_cmd chmod 0755 "${BIN_DIR}/.mandat.new"
$sudo_cmd mv -f "${BIN_DIR}/.mandat.new" "${BIN_DIR}/mandat"

echo "install.sh: installed to ${BIN_DIR}/mandat"
"${BIN_DIR}/mandat" version
echo "install.sh: next step: run \`mandat init\` to configure this VM."
