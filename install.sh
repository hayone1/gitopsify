#!/bin/sh
# Installs the gitopsify binary from its GitHub releases. No Go needed.
#
#   curl -fsSL https://raw.githubusercontent.com/hayone1/gitopsify/main/install.sh | sh
#
# Environment variables (all optional):
#   GITOPSIFY_VERSION      version to install, e.g. v0.2.0 (default: the latest release)
#   GITOPSIFY_INSTALL_DIR  where to put the binary (default: /usr/local/bin when
#                          writable, else ~/.local/bin)
#   GITHUB_TOKEN           token for private repositories or to avoid API rate limits
#
# Arguments override the variables: install.sh [--version v0.2.0] [--dir ~/bin]
set -eu

REPO="hayone1/gitopsify"
BINARY="gitopsify"

version="${GITOPSIFY_VERSION:-}"
dir="${GITOPSIFY_INSTALL_DIR:-}"
while [ $# -gt 0 ]; do
  case "$1" in
    --version) version="$2"; shift 2 ;;
    --version=*) version="${1#*=}"; shift ;;
    --dir) dir="$2"; shift 2 ;;
    --dir=*) dir="${1#*=}"; shift ;;
    -h|--help) sed -n '2,15p' "$0" 2>/dev/null || true; exit 0 ;;
    *) echo "install.sh: unknown argument $1" >&2; exit 2 ;;
  esac
done

say() { printf '%s\n' "$*" >&2; }
die() { say "error: $*"; exit 1; }

# fetch URL OUT [ACCEPT]: GET with curl or wget. With ACCEPT (only used for
# api.github.com), GITHUB_TOKEN is sent too.
fetch() {
  url=$1 out=$2 accept=${3:-}
  if command -v curl >/dev/null 2>&1; then
    if [ -n "$accept" ] && [ -n "${GITHUB_TOKEN:-}" ]; then
      curl -fsSL --retry 3 -H "Accept: $accept" -H "Authorization: Bearer $GITHUB_TOKEN" -o "$out" "$url"
    elif [ -n "$accept" ]; then
      curl -fsSL --retry 3 -H "Accept: $accept" -o "$out" "$url"
    else
      curl -fsSL --retry 3 -o "$out" "$url"
    fi
  elif command -v wget >/dev/null 2>&1; then
    if [ -n "$accept" ] && [ -n "${GITHUB_TOKEN:-}" ]; then
      wget -q --header="Accept: $accept" --header="Authorization: Bearer $GITHUB_TOKEN" -O "$out" "$url"
    elif [ -n "$accept" ]; then
      wget -q --header="Accept: $accept" -O "$out" "$url"
    else
      wget -q -O "$out" "$url"
    fi
  else
    die "curl or wget is required"
  fi
}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ext=tar.gz ;;
  mingw*|msys*|cygwin*) os=windows; ext=zip ;;
  *) die "unsupported operating system $os; download a release from https://github.com/$REPO/releases" ;;
esac
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported architecture $arch; download a release from https://github.com/$REPO/releases" ;;
esac

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t gitopsify)
trap 'rm -rf "$tmp"' EXIT INT TERM

# Resolve the release.
if [ -z "$version" ]; then
  release_url="https://api.github.com/repos/$REPO/releases/latest"
else
  case "$version" in v*) ;; *) version="v$version" ;; esac
  release_url="https://api.github.com/repos/$REPO/releases/tags/$version"
fi
fetch "$release_url" "$tmp/release.json" "application/vnd.github+json" ||
  die "can't read $release_url (no release yet, or a private repository: set GITHUB_TOKEN)"
tag=$(sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' "$tmp/release.json" | head -n 1)
[ -n "$tag" ] || die "no release found at $release_url"
archive="${BINARY}_${tag#v}_${os}_${arch}.${ext}"

# Download an asset of the release by name: through the API (works for
# private repositories with a token), or the public download URL.
asset() {
  name=$1 out=$2
  if [ -n "${GITHUB_TOKEN:-}" ]; then
    # The asset's API URL is the last .../releases/assets/<id> URL before
    # its name, in pretty-printed and minified JSON alike.
    id=$(grep -o '"url": *"[^"]*/releases/assets/[0-9]*"\|"name": *"[^"]*"' "$tmp/release.json" |
      awk -v want="$name" '/releases\/assets\// { n = split($0, p, "/"); id = p[n]; gsub(/"/, "", id); next }
        { v = $0; sub(/^"name": *"/, "", v); sub(/"$/, "", v); if (v == want) { print id; exit } }')
    [ -n "$id" ] || die "release $tag has no asset $name"
    fetch "https://api.github.com/repos/$REPO/releases/assets/$id" "$out" "application/octet-stream"
  else
    fetch "https://github.com/$REPO/releases/download/$tag/$name" "$out"
  fi
}

say "Downloading $BINARY $tag for $os/$arch"
asset "$archive" "$tmp/$archive" || die "can't download $archive"
asset checksums.txt "$tmp/checksums.txt" || die "can't download checksums.txt"

want=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
[ -n "$want" ] || die "checksums.txt has no entry for $archive"
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$archive" | cut -d' ' -f1)
elif command -v shasum >/dev/null 2>&1; then
  got=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1)
else
  die "sha256sum or shasum is required to verify the download"
fi
[ "$want" = "$got" ] || die "checksum mismatch for $archive (want $want, got $got)"

case "$ext" in
  tar.gz) tar -xzf "$tmp/$archive" -C "$tmp" ;;
  zip) unzip -q -o "$tmp/$archive" -d "$tmp" ;;
esac
bin="$tmp/$BINARY"
[ "$os" = windows ] && bin="$bin.exe"
[ -f "$bin" ] || die "$archive does not contain $BINARY"

if [ -z "$dir" ]; then
  if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir="$HOME/.local/bin"; fi
fi
mkdir -p "$dir" 2>/dev/null || true
target="$dir/$(basename "$bin")"
if [ -w "$dir" ]; then
  install -m 0755 "$bin" "$target" 2>/dev/null || { cp "$bin" "$target" && chmod 0755 "$target"; }
elif command -v sudo >/dev/null 2>&1; then
  say "$dir is not writable; using sudo"
  sudo mkdir -p "$dir"
  sudo install -m 0755 "$bin" "$target"
else
  die "$dir is not writable; set GITOPSIFY_INSTALL_DIR to a directory you own"
fi

say "Installed $("$target" version 2>/dev/null || echo "$BINARY $tag") to $target"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) say "Add $dir to your PATH, for example: echo 'export PATH=\"$dir:\$PATH\"' >> ~/.profile" ;;
esac
say "Next: gitopsify doctor    (and gitopsify skills install, for AI coding agents)"
