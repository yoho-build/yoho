#!/bin/sh
# Install yoho from GitHub Releases.
#   curl -fsSL https://yoho.sh | sh
# YOHO_VERSION pins a tag (default: latest). YOHO_INSTALL_DIR chooses the
# directory (default: /usr/local/bin when writable, else ~/.local/bin).
# Never uses sudo. When the destination is not writable, prints a command.
set -eu

repo="yoho-build/yoho"

os=$(uname -s)
case "$os" in
Darwin) goos=darwin ;;
Linux) goos=linux ;;
*)
	echo "yoho: unsupported OS $os" >&2
	exit 1
	;;
esac

arch=$(uname -m)
case "$arch" in
arm64 | aarch64) goarch=arm64 ;;
x86_64 | amd64) goarch=amd64 ;;
*)
	echo "yoho: unsupported architecture $arch" >&2
	exit 1
	;;
esac

asset="yoho-${goos}-${goarch}"
if [ -n "${YOHO_VERSION:-}" ]; then
	base="https://github.com/${repo}/releases/download/${YOHO_VERSION}"
else
	base="https://github.com/${repo}/releases/latest/download"
fi

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

fetch() {
	url=$1
	dest=$2
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$url" -o "$dest"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$dest" "$url"
	else
		echo "yoho: curl or wget is required" >&2
		exit 1
	fi
}

fetch "${base}/${asset}" "$tmpdir/$asset"
fetch "${base}/checksums.txt" "$tmpdir/checksums.txt"

want=$(awk -v asset="$asset" '
	$1 ~ /^[0-9a-fA-F]{64}$/ {
		name = $NF
		sub(/^\*/, "", name)
		if (name == asset) { print tolower($1); exit }
	}
' "$tmpdir/checksums.txt")
if [ -z "$want" ]; then
	echo "yoho: checksums.txt has no entry for $asset" >&2
	exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "$tmpdir/$asset" | awk '{print tolower($1)}')
elif command -v shasum >/dev/null 2>&1; then
	got=$(shasum -a 256 "$tmpdir/$asset" | awk '{print tolower($1)}')
else
	echo "yoho: sha256sum or shasum is required" >&2
	exit 1
fi
if [ "$got" != "$want" ]; then
	echo "yoho: checksum mismatch for $asset" >&2
	exit 1
fi

# Return 0 when dir exists and is writable, or when its parent can be
# used to create it. Tests are inside if so set -e does not abort.
is_writable_dir() {
	dir=$1
	if [ -d "$dir" ]; then
		if [ -w "$dir" ]; then
			return 0
		fi
		return 1
	fi
	parent=$(dirname "$dir")
	if [ -d "$parent" ] && [ -w "$parent" ]; then
		return 0
	fi
	return 1
}

if [ -n "${YOHO_INSTALL_DIR:-}" ]; then
	dest=$YOHO_INSTALL_DIR
elif is_writable_dir /usr/local/bin; then
	dest=/usr/local/bin
else
	if [ -z "${HOME:-}" ]; then
		echo "yoho: HOME is not set; set YOHO_INSTALL_DIR" >&2
		exit 1
	fi
	dest="${HOME}/.local/bin"
fi

if ! is_writable_dir "$dest"; then
	trap - EXIT
	echo "yoho: $dest is not writable. sudo is not used automatically." >&2
	echo "The verified binary is $tmpdir/$asset" >&2
	echo "Install it with:" >&2
	echo "  sudo mkdir -p '$dest' && sudo cp '$tmpdir/$asset' '$dest/yoho' && sudo chmod 0755 '$dest/yoho'" >&2
	exit 1
fi

mkdir -p "$dest"
cp "$tmpdir/$asset" "$dest/yoho"
chmod 0755 "$dest/yoho"

case ":${PATH}:" in
*":${dest}:"*) ;;
*)
	echo "yoho: $dest is not on PATH" >&2
	echo "  export PATH=\"$dest:\$PATH\"" >&2
	;;
esac

echo "Installed $dest/yoho"
