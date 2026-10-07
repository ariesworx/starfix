#!/bin/sh
# Install sfx (or, with --server, starfixd) from a GitHub release, after
# checking the release's Ed25519 signature and the archive's sha256.
#
#   curl -fsSL https://raw.githubusercontent.com/ariesworx/starfix/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/ariesworx/starfix/main/install.sh | sh -s -- --version v0.1.0
#
# Run with -h for the options. Needs curl or wget, tar, and sha256sum or
# shasum; openssl 3 checks the signature. Never uses sudo.
set -eu

usage() {
	cat <<'EOF'
usage: install.sh [--version vX.Y.Z] [--dir DIR] [--server] [--require-signature]

  --version V          release to install (default: the latest; env STARFIX_VERSION)
  --dir DIR            where to put the binary (default: ~/.local/bin; env STARFIX_DIR)
  --server             install starfixd instead of sfx (Linux only)
  --require-signature  fail, rather than warn, when openssl cannot check the signature
  -h, --help           show this help

For testing and mirrors, STARFIX_BASE_URL replaces
https://github.com/ariesworx/starfix/releases; it must serve /latest (a
redirect to /tag/vX.Y.Z) and /download/vX.Y.Z/FILE.
EOF
}

# The release signing public key as a PEM SubjectPublicKeyInfo. It must
# match internal/release/keys.go (Lsz75EL8k/kqkFSEtvkCB6bVC8BKiuI3zzux00wv/Gs=);
# the body is base64 of 302a300506032b6570032100 followed by the 32 key bytes.
# Rotate it in the same pull request as keys.go (RELEASING.md).
RELEASE_PUBKEY_PEM='-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEALsz75EL8k/kqkFSEtvkCB6bVC8BKiuI3zzux00wv/Gs=
-----END PUBLIC KEY-----'

say() { printf '%s\n' "$*"; }
warn() { printf 'install.sh: warning: %s\n' "$*" >&2; }
die() { # message fix
	printf 'install.sh: %s\n' "$1" >&2
	if [ -n "${2:-}" ]; then printf 'fix: %s\n' "$2" >&2; fi
	exit 1
}
have() { _have=$(command -v "$1" 2>&1) && [ -n "$_have" ]; }

version=${STARFIX_VERSION:-}
dir=${STARFIX_DIR:-${HOME:?HOME is not set}/.local/bin}
bin=sfx
require_sig=0
while [ $# -gt 0 ]; do
	case $1 in
	--version) [ $# -ge 2 ] || die "--version needs a value" "--version v0.1.0"; version=$2; shift ;;
	--version=*) version=${1#--version=} ;;
	--dir) [ $# -ge 2 ] || die "--dir needs a value" "--dir \$HOME/.local/bin"; dir=$2; shift ;;
	--dir=*) dir=${1#--dir=} ;;
	--server) bin=starfixd ;;
	--require-signature) require_sig=1 ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; die "unknown option $1" "run with -h for the options" ;;
	esac
	shift
done

base=${STARFIX_BASE_URL:-https://github.com/ariesworx/starfix/releases}
base=${base%/}
pubkey=$RELEASE_PUBKEY_PEM
if [ -n "${STARFIX_PUBKEY_PEM:-}" ]; then
	pubkey=$STARFIX_PUBKEY_PEM
	warn "STARFIX_PUBKEY_PEM is set: trusting THAT key, not the release key."
	warn "Only tests should set it; unset it unless you made the release yourself."
fi

os=$(uname -s)
case $os in
Linux) os=linux ;;
Darwin) os=darwin ;;
MINGW* | MSYS* | CYGWIN* | Windows*)
	die "this script does not install on Windows" \
		"download sfx_<version>_windows_<arch>.zip from https://github.com/ariesworx/starfix/releases, or run this script in WSL" ;;
*) die "unsupported OS $os" "build from source: go install github.com/ariesworx/starfix/cmd/sfx@latest" ;;
esac
arch=$(uname -m)
case $arch in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) die "unsupported architecture $arch" "releases cover amd64 and arm64; build from source: go install github.com/ariesworx/starfix/cmd/$bin@latest" ;;
esac
if [ "$bin" = starfixd ] && [ "$os" != linux ]; then
	die "starfixd runs on Linux only" "run --server on the Linux server, or drop --server to install sfx"
fi

if have curl; then
	fetch() { curl -fsSL --retry 2 -o "$2" "$1"; }
elif have wget; then
	fetch() { wget -q -O "$2" "$1"; }
else
	die "neither curl nor wget is installed" "install curl"
fi
have tar || die "tar is not installed" "install tar"
if have sha256sum; then
	sha256() { sha256sum "$1" | awk '{print $1}'; }
elif have shasum; then
	sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
	die "neither sha256sum nor shasum is installed" "install coreutils"
fi

tmp=$(mktemp -d 2>&1) || die "cannot make a temporary directory: $tmp" "set TMPDIR to a writable directory"
cleanup() { rm -rf "$tmp"; [ -z "${dest:-}" ] || rm -f "$dir/.$bin.tmp"; }
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

# The latest release, from where /latest redirects (.../tag/vX.Y.Z).
if [ -z "$version" ]; then
	if have curl; then
		url=$(curl -fsSLI -o "$tmp/latest" -w '%{url_effective}' "$base/latest") || url=
	else
		url=$(wget -S --max-redirect=0 -O "$tmp/latest" "$base/latest" 2>&1 |
			sed -n 's/^ *[Ll]ocation: *\([^ ]*\).*/\1/p' | tail -n 1 | tr -d '\r') || url=
	fi
	version=${url##*/}
	[ -n "$version" ] || die "cannot find the latest release at $base/latest" "pass --version vX.Y.Z"
fi
case $version in v*) ;; *) version=v$version ;; esac
if ! printf '%s\n' "$version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]{1,32})?$'; then
	die "$version is not a release version" "pass --version vX.Y.Z"
fi

archive=${bin}_${version#v}_${os}_${arch}.tar.gz
dl=$base/download/$version
fetch "$dl/checksums.txt" "$tmp/checksums.txt" ||
	die "cannot download $dl/checksums.txt" "check that release $version exists at https://github.com/ariesworx/starfix/releases"
fetch "$dl/checksums.txt.sig" "$tmp/checksums.txt.sig" ||
	die "cannot download $dl/checksums.txt.sig" "check that release $version exists at https://github.com/ariesworx/starfix/releases"
fetch "$dl/$archive" "$tmp/$archive" ||
	die "cannot download $dl/$archive" "check that release $version has $archive"

# 1. The signature on checksums.txt. First prove openssl can verify
# Ed25519 at all (OpenSSL 3 can; LibreSSL and OpenSSL 1.1 cannot), so a
# failure below means a bad signature, never a missing feature.
sig_capable() {
	have openssl || return 1
	mkdir "$tmp/probe" && printf probe >"$tmp/probe/msg" &&
		openssl genpkey -algorithm ed25519 -out "$tmp/probe/key" 2>"$tmp/probe/err" &&
		openssl pkey -in "$tmp/probe/key" -pubout -out "$tmp/probe/pub" 2>>"$tmp/probe/err" &&
		openssl pkeyutl -sign -rawin -inkey "$tmp/probe/key" -in "$tmp/probe/msg" -out "$tmp/probe/sig" 2>>"$tmp/probe/err" &&
		openssl pkeyutl -verify -rawin -pubin -inkey "$tmp/probe/pub" -in "$tmp/probe/msg" -sigfile "$tmp/probe/sig" >"$tmp/probe/out" 2>&1
}
if sig_capable; then
	printf '%s\n' "$pubkey" >"$tmp/release.pem"
	if ! openssl base64 -d -A -in "$tmp/checksums.txt.sig" -out "$tmp/checksums.sig" 2>"$tmp/err" ||
		[ "$(wc -c <"$tmp/checksums.sig" | tr -d ' ')" != 64 ] ||
		! openssl pkeyutl -verify -rawin -pubin -inkey "$tmp/release.pem" \
			-in "$tmp/checksums.txt" -sigfile "$tmp/checksums.sig" >"$tmp/out" 2>&1; then
		die "the signature on $version's checksums.txt does not verify; nothing was installed" \
			"do not use this download; report it at https://github.com/ariesworx/starfix/issues"
	fi
elif [ "$require_sig" = 1 ]; then
	die "openssl cannot check Ed25519 signatures, and --require-signature is set" \
		"install OpenSSL 3 (brew install openssl@3 on macOS) and put it first on PATH"
else
	warn "signature not checked: openssl 3 with Ed25519 is not available (--require-signature makes this fatal)"
fi

# 2. The archive's sha256 against checksums.txt.
want=$(awk -v n="$archive" '$2 == n || $2 == "*" n {print $1; exit}' "$tmp/checksums.txt")
[ -n "$want" ] || die "$archive is not listed in $version's checksums.txt" "check that release $version has $archive"
got=$(sha256 "$tmp/$archive")
[ "$got" = "$want" ] || die "checksum mismatch for $archive (got $got, want $want); nothing was installed" \
	"retry; if it persists, report it at https://github.com/ariesworx/starfix/issues"

mkdir "$tmp/x"
tar -xzf "$tmp/$archive" -C "$tmp/x" "$bin" || die "cannot unpack $bin from $archive" "retry the download"
[ -f "$tmp/x/$bin" ] || die "$archive holds no $bin" "report it at https://github.com/ariesworx/starfix/issues"

mkdir -p "$dir" || die "cannot create $dir" "pass --dir with a directory you can write"
dest=$dir/$bin
cp "$tmp/x/$bin" "$dir/.$bin.tmp" || die "cannot write to $dir" "pass --dir with a directory you can write"
chmod 0755 "$dir/.$bin.tmp"
mv -f "$dir/.$bin.tmp" "$dest" || die "cannot replace $dest" "pass --dir with a directory you can write"

installed=$("$dest" version 2>&1) || installed="$bin $version"
say "installed $installed to $dest"
case ":${PATH:-}:" in
*":$dir:"*) ;;
*)
	case ${SHELL##*/} in
	zsh) rc=.zshrc ;;
	bash) rc=.bashrc ;;
	*) rc=.profile ;;
	esac
	say "$dir is not on your PATH; add this line to ~/$rc and open a new shell:"
	say "  export PATH=\"$dir:\$PATH\""
	;;
esac
