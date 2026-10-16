#!/bin/sh
# Install a published kordax/gortex release without changing configuration or services.
# Usage: curl -fsSL https://raw.githubusercontent.com/kordax/gortex/main/scripts/install.sh | sh
# GORTEX_VERSION: release tag or latest. GORTEX_INSTALL_DIR: default ~/.local/bin.
# GORTEX_DOWNLOAD_BASE: HTTPS release endpoint override, intended for testing.
set -eu

repo=kordax/gortex
base=${GORTEX_DOWNLOAD_BASE:-https://github.com/$repo/releases}
version=${GORTEX_VERSION:-latest}
install_dir=${GORTEX_INSTALL_DIR:-$HOME/.local/bin}
work=
stage=
locked=
cleanup() {
    [ -z "$work" ] || rm -rf "$work"
    [ -z "$stage" ] || rm -rf "$stage"
    [ -z "$locked" ] || rmdir "$install_dir/.gortex-install.lock"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
fail() { printf 'gortex installer: %s\n' "$*" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || fail 'curl is required'
[ -z "${GORTEX_NO_VERIFY:-}" ] || fail 'checksum verification cannot be disabled'
case "$base" in https://*) ;; *) fail 'GORTEX_DOWNLOAD_BASE must use HTTPS' ;; esac
fetch() {
    curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
        --connect-timeout 15 --max-time 300 --output "$2" "$1"
}
if [ "$version" = latest ]; then
    resolved=$(curl --fail --silent --show-error --location --head --proto '=https' --proto-redir '=https' \
        --connect-timeout 15 --max-time 60 --output /dev/null --write-out '%{url_effective}' "$base/latest")
    case "$resolved" in "$base/tag/"*) version=${resolved#"$base/tag/"} ;; *) fail 'latest did not resolve to a release tag' ;; esac
fi
case "$version" in v*) ;; *) version=v$version ;; esac
printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || fail 'invalid release tag'
case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) fail 'supported systems: Linux and macOS' ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) fail 'supported architectures: amd64 and arm64' ;; esac
asset=gortex_${os}_${arch}.tar.gz
url=$base/download/$version
work=$(mktemp -d)
fetch "$url/$asset" "$work/$asset"
fetch "$url/checksums.txt" "$work/checksums.txt"
expected=$(awk -v f="$asset" '$2==f || $2=="*"f {print $1}' "$work/checksums.txt")
printf '%s\n' "$expected" | grep -Eq '^[0-9a-fA-F]{64}$' || fail 'missing, duplicate or invalid checksum entry'
if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "$work/$asset" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "$work/$asset" | awk '{print $1}')
else
    fail 'sha256sum or shasum is required'
fi
[ "$(printf '%s' "$expected" | tr 'A-F' 'a-f')" = "$actual" ] || fail 'archive checksum mismatch'
if command -v cosign >/dev/null 2>&1; then
    fetch "$url/$asset.sig" "$work/$asset.sig"
    fetch "$url/$asset.pem" "$work/$asset.pem"
    cosign verify-blob --certificate "$work/$asset.pem" --signature "$work/$asset.sig" \
        --certificate-identity "https://github.com/$repo/.github/workflows/release.yml@refs/tags/$version" \
        --certificate-oidc-issuer https://token.actions.githubusercontent.com "$work/$asset" >/dev/null
fi
# Only stream the binary. Archive paths, links and metadata cannot write to the host.
tar -xOzf "$work/$asset" gortex > "$work/gortex"
[ -s "$work/gortex" ] || fail 'archive has no regular gortex binary'
chmod 755 "$work/gortex"
# Startup may migrate configuration even for `version`; keep probes private.
mkdir -p "$work/probe/home" "$work/probe/config" "$work/probe/data" \
    "$work/probe/cache" "$work/probe/state" "$work/probe/runtime"
chmod 700 "$work/probe" "$work/probe/home" "$work/probe/config" "$work/probe/data" \
    "$work/probe/cache" "$work/probe/state" "$work/probe/runtime"
banner=$(HOME="$work/probe/home" XDG_CONFIG_HOME="$work/probe/config" \
    XDG_DATA_HOME="$work/probe/data" XDG_CACHE_HOME="$work/probe/cache" \
    XDG_STATE_HOME="$work/probe/state" XDG_RUNTIME_DIR="$work/probe/runtime" \
    GORTEX_SESSION_ID='' "$work/gortex" version) || fail 'downloaded binary cannot run'
printf '%s\n' "$banner" | grep -F 'fork by kordax' >/dev/null || fail 'binary is not the kordax fork'
reported=$(printf '%s\n' "$banner" | awk 'NR==1 {sub(/\+.*/, "", $2); print $2}')
[ "$reported" = "$version" ] || fail "binary version $reported differs from release $version"
mkdir -p "$install_dir"
install_dir=$(cd "$install_dir" && pwd -P)
mkdir "$install_dir/.gortex-install.lock" 2>/dev/null || fail 'another installation holds the destination lock'
locked=yes
for dest in "$install_dir/gortex" "$install_dir/gortex.previous"; do
    [ ! -L "$dest" ] || fail "refusing symlink: $dest"
    if [ -e "$dest" ] && [ ! -f "$dest" ]; then fail "refusing non-regular destination: $dest"; fi
done
stage=$(mktemp -d "$install_dir/.gortex-install.XXXXXX")
cp "$work/gortex" "$stage/gortex"
chmod 755 "$stage/gortex"
if [ -f "$install_dir/gortex" ]; then
    cp -p "$install_dir/gortex" "$stage/gortex.previous"
    mv -f "$stage/gortex.previous" "$install_dir/gortex.previous"
fi
mv -f "$stage/gortex" "$install_dir/gortex"
printf 'Installed %s/gortex\n%s\n' "$install_dir" "$banner"
resolved_binary=$(command -v gortex || true)
if [ "$resolved_binary" != "$install_dir/gortex" ]; then
    if [ -n "$resolved_binary" ]; then
        printf 'PATH currently resolves gortex to %s, which shadows this installation.\n' "$resolved_binary"
    else
        printf 'The installed gortex is not currently available through PATH.\n'
    fi
    escaped_install_dir=$(printf '%s' "$install_dir" | sed "s/'/'\\\\''/g")
    printf 'Run in your shell:\n'
    printf '  export PATH=\047%s\047:"%s"\n' "$escaped_install_dir" "\$PATH"
    printf '  hash -r\n'
fi
printf 'Check: command -v gortex; gortex version\n'
printf 'Configuration and daemon were not changed. Restart the existing managed service when ready.\n'
