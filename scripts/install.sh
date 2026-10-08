#!/bin/sh
# Install yoho from GitHub Releases (curl -fsSL https://yoho.sh | sh).
# YOHO_VERSION pins a tag (default: latest). YOHO_INSTALL_DIR chooses the
# directory (/usr/local/bin when writable, else ~/.local/bin).
# Private repos: set GITHUB_TOKEN or GH_TOKEN. Never uses sudo.
set -eu

repo=yoho-build/yoho
token=${GITHUB_TOKEN:-${GH_TOKEN:-}}
case "$token" in
*[[:space:]]* | *\\* | *\"*) echo "yoho: GITHUB_TOKEN contains unsupported characters" >&2; exit 1 ;;
esac

os=$(uname -s)
arch=$(uname -m)
case $os in
Darwin) goos=darwin ;;
Linux) goos=linux ;;
*) echo "yoho: unsupported OS $os" >&2; exit 1 ;;
esac
case $arch in
arm64 | aarch64) goarch=arm64 ;;
x86_64 | amd64) goarch=amd64 ;;
*) echo "yoho: unsupported architecture $arch" >&2; exit 1 ;;
esac
asset="yoho-${goos}-${goarch}"

case "${YOHO_VERSION:-}" in
*"/"* | *".."* | *\\* | *[[:space:]]*) echo "yoho: invalid YOHO_VERSION" >&2; exit 1 ;;
esac

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

public_get() {
	if command -v curl >/dev/null 2>&1; then curl -fsSL -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then wget -q -O "$2" "$1"
	else echo "yoho: curl or wget is required" >&2; exit 1; fi
}

# curl >= 7.58 drops Authorization on a cross-host redirect (CVE-2018-1000007).
# The header is written mode 0600 so the token is not on the curl command line.
auth_setup() {
	command -v curl >/dev/null 2>&1 || { echo "yoho: GITHUB_TOKEN downloads need curl" >&2; exit 1; }
	cver=$(curl --version | awk 'NR==1 { print $2; exit }')
	major=${cver%%.*}; rest=${cver#*.}; minor=${rest%%.*}
	if ! { [ "$major" -gt 7 ] || { [ "$major" -eq 7 ] && [ "$minor" -ge 58 ]; }; } 2>/dev/null; then
		echo "yoho: GITHUB_TOKEN downloads need curl >= 7.58" >&2; exit 1
	fi
	(umask 077; printf 'header = "Authorization: Bearer %s"\n' "$token" > "$tmpdir/curl.cfg")
	chmod 0600 "$tmpdir/curl.cfg"
}

auth_get() {
	curl --config "$tmpdir/curl.cfg" -fsSL -L -H "Accept: $3" -o "$2" "$1"
}

# Pretty-printed release JSON, one field per line. Asset "url" (…/releases/assets/)
# is listed before "name". browser_download_url does not match that url.
asset_url() {
	awk -v want="$2" '
		/^[[:space:]]*"url":/ && /\/releases\/assets\// {
			u = $0
			sub(/^[^"]*"[^"]*"[[:space:]]*:[[:space:]]*"/, "", u); sub(/".*$/, "", u); pending = u; next
		}
		/^[[:space:]]*"name":/ && pending != "" {
			n = $0
			sub(/^[^"]*"[^"]*"[[:space:]]*:[[:space:]]*"/, "", n); sub(/".*$/, "", n)
			if (n == want) { print pending; exit }
			pending = ""
		}
	' "$1"
}

tag=${YOHO_VERSION:-}
if [ -n "$token" ]; then
	auth_setup
	api="https://api.github.com/repos/${repo}/releases/latest"
	if [ -n "$tag" ]; then api="https://api.github.com/repos/${repo}/releases/tags/${tag}"; fi
	auth_get "$api" "$tmpdir/release.json" "application/vnd.github+json"
	bin_url=$(asset_url "$tmpdir/release.json" "$asset")
	sum_url=$(asset_url "$tmpdir/release.json" "checksums.txt")
	if [ -z "$bin_url" ] || [ -z "$sum_url" ]; then
		echo "yoho: release ${tag:-latest} is missing $asset or checksums.txt" >&2; exit 1
	fi
	auth_get "$bin_url" "$tmpdir/$asset" "application/octet-stream"
	auth_get "$sum_url" "$tmpdir/checksums.txt" "application/octet-stream"
else
	base="https://github.com/${repo}/releases/latest/download"
	if [ -n "$tag" ]; then base="https://github.com/${repo}/releases/download/${tag}"; fi
	public_get "$base/$asset" "$tmpdir/$asset" &&
		public_get "$base/checksums.txt" "$tmpdir/checksums.txt" || {
		echo "yoho: release ${tag:-latest} not found; if the repo is private set GITHUB_TOKEN or GH_TOKEN" >&2
		exit 1
	}
fi

want=$(awk -v asset="$asset" '$1 ~ /^[0-9a-fA-F]{64}$/ {
	name = $NF; sub(/^\*/, "", name)
	if (name == asset) { print tolower($1); exit }
}' "$tmpdir/checksums.txt")
if [ -z "$want" ]; then echo "yoho: checksums.txt has no entry for $asset" >&2; exit 1; fi
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "$tmpdir/$asset" | awk '{ print tolower($1) }')
elif command -v shasum >/dev/null 2>&1; then
	got=$(shasum -a 256 "$tmpdir/$asset" | awk '{ print tolower($1) }')
else echo "yoho: sha256sum or shasum is required" >&2; exit 1
fi
if [ "$got" != "$want" ]; then echo "yoho: checksum mismatch for $asset" >&2; exit 1; fi

is_writable_dir() {
	d=$1
	if [ -d "$d" ]; then
		if [ -w "$d" ]; then return 0; fi
		return 1
	fi
	p=$(dirname "$d")
	if [ -d "$p" ] && [ -w "$p" ]; then return 0; fi
	return 1
}

if [ -n "${YOHO_INSTALL_DIR:-}" ]; then dest=$YOHO_INSTALL_DIR
elif is_writable_dir /usr/local/bin; then dest=/usr/local/bin
elif [ -n "${HOME:-}" ]; then dest="${HOME}/.local/bin"
else echo "yoho: HOME is not set; set YOHO_INSTALL_DIR" >&2; exit 1
fi
if ! is_writable_dir "$dest"; then
	trap - EXIT
	cmd="sudo mkdir -p '$dest' && sudo cp '$tmpdir/$asset' '$dest/yoho' && sudo chmod 0755 '$dest/yoho'"
	printf '%s\n' "yoho: $dest is not writable. sudo is not used automatically." "The verified binary is $tmpdir/$asset" "Install it with:" "  $cmd" >&2
	exit 1
fi
mkdir -p "$dest"
cp "$tmpdir/$asset" "$dest/yoho"
chmod 0755 "$dest/yoho"
case ":${PATH}:" in
*":${dest}:"*) ;;
*) printf 'yoho: %s is not on PATH\n  export PATH="%s:$PATH"\n' "$dest" "$dest" >&2 ;;
esac
installed=$("$dest/yoho" version) || { echo "yoho: installed $dest/yoho but could not read its version" >&2; exit 1; }
echo "Installed $dest/yoho ($installed)"
