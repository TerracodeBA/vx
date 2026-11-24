#!/bin/sh
# Install the vx CLI. Served by vx-server at /install.sh:
#
#   curl -fsSL https://YOUR-VX-SERVER/install.sh | sh
#
# It downloads the vx archive for this OS and CPU from the latest GitHub
# release (or VX_VERSION, e.g. v0.3.0), checks it against the release's
# checksums.txt, and installs vx into ~/.local/bin (or VX_INSTALL_DIR).
# Nothing runs as root and nothing outside that directory changes.
set -eu

server='__VX_SERVER__'
releases="${VX_RELEASES_URL:-https://github.com/TerracodeBA/vx/releases}"
releases="${releases%/}"
dir="${VX_INSTALL_DIR:-$HOME/.local/bin}"
version="${VX_VERSION:-}"

say() { printf '%s\n' "$*"; }
fail() {
	printf 'vx install: %s\n' "$*" >&2
	exit 1
}

command -v curl >/dev/null 2>&1 || fail "needs curl; install it and run this again"
command -v tar >/dev/null 2>&1 || fail "needs tar; install it and run this again"

os=$(uname -s)
case "$os" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) fail "there is no vx build for $os here; on Windows download the zip from $releases, or build it: go install github.com/TerracodeBA/vx/cmd/vx@latest" ;;
esac
arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) fail "there is no vx build for $arch CPUs; build it: go install github.com/TerracodeBA/vx/cmd/vx@latest" ;;
esac

if [ -z "$version" ]; then
	# The latest release redirects to .../tag/vX.Y.Z; no API token needed.
	latest=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$releases/latest") ||
		fail "could not reach $releases/latest to find the latest vx release; set VX_VERSION=vX.Y.Z to pick one"
	version=${latest##*/}
fi
case "$version" in
v[0-9]*) ;;
[0-9]*) version="v$version" ;;
*) fail "could not tell the latest vx release from $releases/latest; set VX_VERSION=vX.Y.Z" ;;
esac
num=${version#v}
archive="vx_${num}_${os}_${arch}.tar.gz"

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t vx-install)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 130' INT TERM

say "Downloading vx $version for $os/$arch..."
curl -fsSL -o "$tmp/$archive" "$releases/download/$version/$archive" ||
	fail "could not download $releases/download/$version/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$releases/download/$version/checksums.txt" ||
	fail "could not download the checksums of vx $version, so the download cannot be verified; nothing was installed"

want=$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/checksums.txt")
[ -n "$want" ] || fail "checksums.txt of vx $version does not list $archive; nothing was installed"
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "$tmp/$archive" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
	got=$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')
else
	fail "needs sha256sum or shasum to verify the download; nothing was installed"
fi
[ "$got" = "$want" ] || fail "checksum mismatch for $archive (expected $want, got $got); nothing was installed"

mkdir -p "$tmp/x"
tar -xzf "$tmp/$archive" -C "$tmp/x" || fail "could not unpack $archive"
[ -f "$tmp/x/vx" ] || fail "$archive has no vx binary"

mkdir -p "$dir" || fail "could not create $dir; set VX_INSTALL_DIR to a directory you can write"
# Copy next to the target, then rename: replacing a running vx is safe.
cp "$tmp/x/vx" "$dir/.vx.new.$$" || fail "could not write to $dir; set VX_INSTALL_DIR to a directory you can write"
chmod 755 "$dir/.vx.new.$$"
mv -f "$dir/.vx.new.$$" "$dir/vx"
say "Installed vx $version to $dir/vx"

case ":$PATH:" in
*":$dir:"*)
	say "$dir is on your PATH."
	found=$(command -v vx 2>/dev/null || true)
	if [ -n "$found" ] && [ "$found" != "$dir/vx" ]; then
		say "Note: $found comes first on your PATH; remove it or move $dir ahead of it."
	fi
	;;
*)
	shown=$dir
	case "$dir" in "$HOME"/*) shown="\$HOME/${dir#"$HOME"/}" ;; esac
	case "${SHELL:-}" in
	*/zsh) rc="$HOME/.zshrc" ;;
	*/bash) rc="$HOME/.bashrc" ;;
	*) rc="$HOME/.profile" ;;
	esac
	say "$dir is not on your PATH. Add it, then open a new terminal:"
	case "${SHELL:-}" in
	*/fish) say "  fish_add_path $shown" ;;
	*) say "  echo 'export PATH=\"$shown:\$PATH\"' >> $rc" ;;
	esac
	;;
esac

say ""
if [ -n "$server" ]; then
	say "Next, log in (opens your browser):"
	say "  vx login --server $server"
else
	say "Next, log in (opens your browser):"
	say "  vx login --server https://YOUR-VX-SERVER"
fi
