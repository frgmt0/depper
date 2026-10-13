#!/bin/sh
# depper installer. Downloads a release archive, verifies its SHA-256
# checksum, and installs the binary. Never uses sudo.
#
# Environment:
#   DEPPER_VERSION       version to install, "0.2.0" or "v0.2.0" (default: latest)
#   DEPPER_INSTALL_DIR   install directory (default: /usr/local/bin if
#                        writable, otherwise $HOME/.local/bin)
#   DEPPER_DOWNLOAD_BASE testing hook: base URL (https:// or file://) that
#                        replaces https://github.com/frgmt0/depper/releases/download
#
# Archive names must stay in sync with .goreleaser.yaml.
set -eu

REPO="frgmt0/depper"
GITHUB="https://github.com/${REPO}"

err() {
    printf 'install.sh: error: %s\n' "$*" >&2
    exit 1
}

info() {
    printf '%s\n' "$*"
}

have() {
    command -v "$1" >/dev/null 2>&1
}

# --- platform -------------------------------------------------------------

case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) err "unsupported operating system: $(uname -s) (supported: linux, darwin)" ;;
esac

case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) err "unsupported architecture: $(uname -m) (supported: amd64, arm64)" ;;
esac

# --- downloader -----------------------------------------------------------

if have curl; then
    dl=curl
elif have wget; then
    dl=wget
else
    err "curl or wget is required"
fi

# fetch URL DEST
fetch() {
    case "$1" in
        file://*)
            cp "${1#file://}" "$2" || err "cannot read $1"
            return 0
            ;;
        https://*) ;;
        *) err "refusing non-https URL: $1" ;;
    esac
    if [ "$dl" = curl ]; then
        curl -fsSL --proto '=https' --tlsv1.2 -o "$2" "$1" || err "download failed: $1"
    else
        wget -q -O "$2" "$1" || err "download failed: $1"
    fi
}

# latest_version prints the latest release version without the leading v.
# It follows the redirect of /releases/latest, which needs no API token.
latest_version() {
    if [ "$dl" = curl ]; then
        url=$(curl -fsSL --proto '=https' --tlsv1.2 -o /dev/null -w '%{url_effective}' "${GITHUB}/releases/latest") ||
            err "cannot resolve the latest release; set DEPPER_VERSION"
    else
        url=$(wget -q --max-redirect=0 -S -O /dev/null "${GITHUB}/releases/latest" 2>&1 |
            sed -n 's/^ *[Ll]ocation: *//p' | tr -d '\r' | tail -n 1) || true
        [ -n "$url" ] || err "cannot resolve the latest release; set DEPPER_VERSION"
    fi
    tag=${url##*/}
    case "$tag" in
        v[0-9]*) printf '%s\n' "${tag#v}" ;;
        *) err "cannot determine the latest release (got '$url'); set DEPPER_VERSION" ;;
    esac
}

# --- version --------------------------------------------------------------

version=${DEPPER_VERSION:-}
if [ -z "$version" ]; then
    version=$(latest_version)
fi
version=${version#v}
case "$version" in
    '' | *[!0-9A-Za-z._+-]*) err "invalid version: $version" ;;
    [0-9]*) ;;
    *) err "invalid version: $version" ;;
esac

# --- install dir ----------------------------------------------------------

if [ -n "${DEPPER_INSTALL_DIR:-}" ]; then
    install_dir=$DEPPER_INSTALL_DIR
    mkdir -p "$install_dir" 2>/dev/null || true
    [ -d "$install_dir" ] && [ -w "$install_dir" ] ||
        err "cannot write to $install_dir"
elif [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    install_dir=/usr/local/bin
else
    [ -n "${HOME:-}" ] || err "HOME is not set; set DEPPER_INSTALL_DIR"
    install_dir=$HOME/.local/bin
    mkdir -p "$install_dir" || err "cannot create $install_dir"
    info "/usr/local/bin is not writable; installing to $install_dir"
    info "To install system-wide instead, run:"
    info "  curl -fsSL https://raw.githubusercontent.com/${REPO}/main/install.sh | sudo DEPPER_INSTALL_DIR=/usr/local/bin sh"
fi

# --- checksum tool --------------------------------------------------------

if have sha256sum; then
    sha_cmd="sha256sum"
elif have shasum; then
    sha_cmd="shasum -a 256"
else
    err "sha256sum or shasum is required"
fi

# --- download -------------------------------------------------------------

base=${DEPPER_DOWNLOAD_BASE:-${GITHUB}/releases/download}
base=${base%/}
case "$base" in
    https://* | file://*) ;;
    *) err "DEPPER_DOWNLOAD_BASE must start with https:// or file://" ;;
esac

archive="depper_${version}_${os}_${arch}.tar.gz"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/depper-install.XXXXXX") || err "cannot create a temporary directory"
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' HUP INT TERM

info "Installing depper ${version} (${os}/${arch})"
fetch "${base}/v${version}/${archive}" "$tmp/$archive"
fetch "${base}/v${version}/checksums.txt" "$tmp/checksums.txt"

# --- verify ---------------------------------------------------------------

expected=$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/checksums.txt")
[ -n "$expected" ] || err "$archive is not listed in checksums.txt"
actual=$($sha_cmd "$tmp/$archive" | awk '{ print $1 }')
if [ "$expected" != "$actual" ]; then
    err "checksum mismatch for $archive (expected $expected, got $actual); nothing was installed"
fi
info "Checksum verified"

# --- install --------------------------------------------------------------

mkdir "$tmp/x"
tar -xzf "$tmp/$archive" -C "$tmp/x" depper || err "cannot extract depper from $archive"
[ -f "$tmp/x/depper" ] || err "archive does not contain depper"
dest="$install_dir/depper"
# Stage next to the destination, then rename, so an existing binary is
# replaced atomically.
staged="$install_dir/.depper.new.$$"
trap 'rm -rf "$tmp"; rm -f "$staged"' EXIT
install -m 0755 "$tmp/x/depper" "$staged" || err "cannot write to $install_dir"
mv -f "$staged" "$dest" || err "cannot install to $dest"

info "Installed $dest"
"$dest" --version || err "installed binary failed to run"

# --- PATH hint ------------------------------------------------------------

case ":${PATH:-}:" in
    *":$install_dir:"*) ;;
    *)
        info ""
        shown=$install_dir
        if [ -n "${HOME:-}" ] && [ "$install_dir" = "$HOME/.local/bin" ]; then
            shown="\$HOME/.local/bin"
        fi
        info "$install_dir is not on your PATH. Add it:"
        info ""
        info "  bash or zsh (add to ~/.bashrc or ~/.zshrc):"
        info "    export PATH=\"$shown:\$PATH\""
        info ""
        info "  fish:"
        info "    fish_add_path $shown"
        ;;
esac
