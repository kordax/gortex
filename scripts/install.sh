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
trap 'cancel 130' INT
trap 'cancel 143' TERM
ui_terminal=0
ui_accent='' ui_success='' ui_error='' ui_bold='' ui_dim='' ui_reset=''
if [ -t 2 ] && [ "${TERM:-dumb}" != dumb ]; then
    ui_terminal=1
    if [ -z "${NO_COLOR:-}" ]; then
        ui_accent=$(printf '\033[36m')
        ui_success=$(printf '\033[32m')
        ui_error=$(printf '\033[31m')
        ui_bold=$(printf '\033[1m')
        ui_dim=$(printf '\033[2m')
        ui_reset=$(printf '\033[0m')
    fi
fi
step_number=0
step_title=Startup
log() {
    if [ "$ui_terminal" = 1 ]; then
        printf '    %s\n' "$*" >&2
    else
        printf 'gortex installer: %s\n' "$*" >&2
    fi
}
step() {
    step_number=$((step_number + 1))
    step_title=$1
    if [ "$ui_terminal" = 1 ]; then
        printf '\n  %s%s[%s/6] %s%s\n' "$ui_bold" "$ui_accent" "$step_number" "$step_title" "$ui_reset" >&2
    else
        log "[$step_number/6] $step_title"
    fi
}
ok() { printf '    %sOK%s  %s\n' "$ui_success" "$ui_reset" "$*" >&2; }
fail() {
    printf '\n  %s%sFAILED%s  %s\n' "$ui_bold" "$ui_error" "$ui_reset" "$*" >&2
    log "Stopped at step $step_number/6: $step_title."
    exit 1
}
cancel() {
    printf '\n  %sCANCELLED%s  %s\n' "$ui_dim" "$ui_reset" "$step_title" >&2
    log 'The running daemon was not restarted.'
    exit "$1"
}
if [ "$ui_terminal" = 1 ]; then
    printf '\n  %s%sG O R T E X%s  %sfork by kordax%s\n' "$ui_bold" "$ui_accent" "$ui_reset" "$ui_dim" "$ui_reset" >&2
    printf '  %sGitHub release installer%s\n\n' "$ui_dim" "$ui_reset" >&2
else
    log 'Installing Gortex (fork by kordax).'
fi
log "Destination: $install_dir/gortex"
step 'Resolve release'
command -v curl >/dev/null 2>&1 || fail 'curl is required'
[ -z "${GORTEX_NO_VERIFY:-}" ] || fail 'checksum verification cannot be disabled'
case "$base" in https://*) ;; *) fail 'GORTEX_DOWNLOAD_BASE must use HTTPS' ;; esac
fetch() {
    fetch_url=$1
    fetch_destination=$2
    fetch_name=${fetch_destination##*/}
    log "Downloading $fetch_name..."
    if [ "$ui_terminal" = 1 ]; then
        set -- --progress-bar
    else
        set -- --silent
    fi
    # Allow slow but active downloads; fail if no data arrives for 30 seconds.
    if fetch_stats=$(LC_ALL=C curl --fail --show-error --location "$@" --proto '=https' --proto-redir '=https' \
        --connect-timeout 15 --max-time 1800 --speed-limit 1 --speed-time 30 \
        --write-out '%{size_download} %{time_total} %{speed_download}' \
        --output "$fetch_destination" "$fetch_url"); then
        fetch_summary=$(printf '%s\n' "$fetch_stats" | LC_ALL=C awk '
            function size(n, unit) {
                unit = "B"
                if (n >= 1073741824) { n /= 1073741824; unit = "GiB" }
                else if (n >= 1048576) { n /= 1048576; unit = "MiB" }
                else if (n >= 1024) { n /= 1024; unit = "KiB" }
                return sprintf("%.1f %s", n, unit)
            }
            function elapsed(s) {
                return s >= 60 ? sprintf("%dm%02ds", s / 60, s % 60) : sprintf("%.2fs", s)
            }
            { printf "%s in %s (%s/s)", size($1), elapsed($2), size($3) }
        ')
        log "Downloaded $fetch_name: $fetch_summary"
    else
        fetch_status=$?
        fail "Download failed for $fetch_name (curl exit $fetch_status). Check the connection and retry; stalled transfers stop after 30 seconds."
    fi
}
if [ "$version" = latest ]; then
    log 'Resolving the latest fork release...'
    if resolved=$(curl --fail --silent --show-error --location --head --proto '=https' --proto-redir '=https' \
        --connect-timeout 15 --max-time 60 --speed-limit 1 --speed-time 30 \
        --output /dev/null --write-out '%{url_effective}' "$base/latest"); then
        :
    else
        resolve_status=$?
        fail "Could not resolve the latest fork release (curl exit $resolve_status)."
    fi
    case "$resolved" in "$base/tag/"*) version=${resolved#"$base/tag/"} ;; *) fail 'latest did not resolve to a release tag' ;; esac
fi
case "$version" in v*) ;; *) version=v$version ;; esac
printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || fail 'invalid release tag'
case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) fail 'supported systems: Linux and macOS' ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) fail 'supported architectures: amd64 and arm64' ;; esac
asset=gortex_${os}_${arch}.tar.gz
url=$base/download/$version
ok "$version | $os/$arch"
step 'Download release artifacts'
log 'A slow connection can take several minutes. Ctrl+C cancels the installation.'
work=$(mktemp -d)
fetch "$url/$asset" "$work/$asset"
fetch "$url/checksums.txt" "$work/checksums.txt"
step 'Verify checksums and signatures'
log "Verifying SHA-256 for $asset..."
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
ok 'SHA-256 verified.'
if command -v cosign >/dev/null 2>&1; then
    log 'Verifying the release workflow signature with cosign...'
    fetch "$url/$asset.sig" "$work/$asset.sig"
    fetch "$url/$asset.pem" "$work/$asset.pem"
    cosign verify-blob --certificate "$work/$asset.pem" --signature "$work/$asset.sig" \
        --certificate-identity "https://github.com/$repo/.github/workflows/release.yml@refs/tags/$version" \
        --certificate-oidc-issuer https://token.actions.githubusercontent.com "$work/$asset" >/dev/null \
        || fail 'Release workflow signature verification failed.'
    ok 'Release workflow signature verified.'
else
    log 'cosign is not installed; workflow signature verification is unavailable.'
fi
# Only stream the binary. Archive paths, links and metadata cannot write to the host.
step 'Validate the executable'
log 'Extracting the binary...'
tar -xOzf "$work/$asset" gortex > "$work/gortex" || fail 'Could not extract the gortex executable.'
[ -s "$work/gortex" ] || fail 'archive has no regular gortex binary'
chmod 755 "$work/gortex"
# Startup may migrate configuration even for `version`; keep probes private.
mkdir -p "$work/probe/home" "$work/probe/config" "$work/probe/data" \
    "$work/probe/cache" "$work/probe/state" "$work/probe/runtime"
chmod 700 "$work/probe" "$work/probe/home" "$work/probe/config" "$work/probe/data" \
    "$work/probe/cache" "$work/probe/state" "$work/probe/runtime"
log 'Checking the binary version and fork identity...'
banner=$(HOME="$work/probe/home" XDG_CONFIG_HOME="$work/probe/config" \
    XDG_DATA_HOME="$work/probe/data" XDG_CACHE_HOME="$work/probe/cache" \
    XDG_STATE_HOME="$work/probe/state" XDG_RUNTIME_DIR="$work/probe/runtime" \
    GORTEX_SESSION_ID='' "$work/gortex" version) || fail 'downloaded binary cannot run'
printf '%s\n' "$banner" | grep -F 'fork by kordax' >/dev/null || fail 'binary is not the kordax fork'
reported=$(printf '%s\n' "$banner" | awk 'NR==1 {sub(/\+.*/, "", $2); print $2}')
[ "$reported" = "$version" ] || fail "binary version $reported differs from release $version"
ok "$reported | fork by kordax"
step 'Install atomically'
log "Installing into $install_dir..."
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
    log "Saving the previous binary to $install_dir/gortex.previous..."
    cp -p "$install_dir/gortex" "$stage/gortex.previous"
    mv -f "$stage/gortex.previous" "$install_dir/gortex.previous"
fi
log 'Activating the new executable (the running daemon is unchanged)...'
mv -f "$stage/gortex" "$install_dir/gortex"
step 'Installation complete'
if [ "$ui_terminal" = 1 ]; then
    printf '    %s[########################] 100%%%s\n\n' "$ui_success" "$ui_reset" >&2
fi
ok "Installed $version"
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
printf '\nCheck: command -v gortex; gortex version\n'
if [ -f "$install_dir/gortex.previous" ]; then
    printf 'Previous binary: %s/gortex.previous\n' "$install_dir"
fi
escaped_binary=$(printf '%s/gortex' "$install_dir" | sed "s/'/'\\\\''/g")
printf 'Activate in the existing daemon:\n  \047%s\047 daemon restart\n' "$escaped_binary"
printf 'Configuration and running daemon were not changed.\n'
