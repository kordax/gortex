#!/usr/bin/env bash
# Offline release installer contract. No network, services or real install paths.
set -euo pipefail
repo_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/tools" "$fixture/assets" "$fixture/home" "$fixture/payload" "$fixture/bin with spaces and 'quote"
installer=$repo_dir/scripts/install.sh
cat > "$fixture/tools/curl" <<'CURL'
#!/usr/bin/env bash
set -euo pipefail
out=
writeout=
speed_time= speed_limit= max_time=
progress=false
silent=false
url=
while (($#)); do
  case "$1" in
    --output) out=$2; shift 2 ;;
    --write-out) writeout=$2; shift 2 ;;
    --speed-time) speed_time=$2; shift 2 ;;
    --speed-limit) speed_limit=$2; shift 2 ;;
    --max-time) max_time=$2; shift 2 ;;
    --progress-bar) progress=true; shift ;;
    --silent) silent=true; shift ;;
    --proto|--proto-redir|--connect-timeout) shift 2 ;;
    --*) shift ;;
    *) url=$1; shift ;;
  esac
done
printf '%s\n' "$url" >> "$INSTALL_TEST_ROOT/requests"
[[ -n $speed_time && -n $speed_limit && -n $max_time ]]
((speed_time > 0 && speed_time <= 60 && speed_limit > 0 && max_time >= speed_time))
if [[ $writeout == '%{url_effective}' ]]; then
  if [[ ${INSTALL_TEST_REQUIRE_LOGS:-0} == 1 ]]; then
    grep -F 'Resolving the latest fork release' "$INSTALL_TEST_ROOT/output" >/dev/null
  fi
  printf 'https://github.com/kordax/gortex/releases/tag/v1.0.0'
else
  if [[ ${INSTALL_TEST_REQUIRE_LOGS:-0} == 1 ]]; then
    # Inspect while curl is only just starting, before a single payload byte
    # is copied. A silent installer fails here, not after installation ends.
    grep -F '[2/6] Download release artifacts' "$INSTALL_TEST_ROOT/output" >/dev/null
    grep -F "Downloading ${url##*/}..." "$INSTALL_TEST_ROOT/output" >/dev/null
  fi
  if [[ -t 2 && ${TERM:-dumb} != dumb ]]; then
    $progress && ! $silent
  else
    $silent && ! $progress
  fi
  if [[ ${INSTALL_TEST_DOWNLOAD_FAILURE:-0} == 1 ]]; then exit 28; fi
  cp "$INSTALL_TEST_ROOT/assets/${url##*/}" "$out"
  printf '%s 0.010 1000' "$(wc -c < "$out" | tr -d ' ')"
fi
CURL
# Keep cosign behavior deterministic even if it is installed on the host.
cat > "$fixture/tools/cosign" <<'COSIGN'
#!/bin/sh
printf '%s\n' "$*" >> "$INSTALL_TEST_ROOT/signature-requests"
[ "${INSTALL_TEST_BAD_SIGNATURE:-0}" = 0 ]
COSIGN
chmod +x "$fixture/tools/curl" "$fixture/tools/cosign"
export INSTALL_TEST_ROOT=$fixture
export PATH="$fixture/tools:$PATH"
export HOME="$fixture/home"
export XDG_CONFIG_HOME="$fixture/caller-config" XDG_DATA_HOME="$fixture/caller-data"
export XDG_CACHE_HOME="$fixture/caller-cache" XDG_STATE_HOME="$fixture/caller-state"
export XDG_RUNTIME_DIR="$fixture/caller-runtime" GORTEX_SESSION_ID=caller-session
export GORTEX_INSTALL_DIR="$fixture/bin with spaces and 'quote"
case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) echo 'skip: POSIX installer targets Linux/macOS'; exit 0 ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo 'skip: unsupported host architecture'; exit 0 ;; esac
asset=gortex_${os}_${arch}.tar.gz
make_release() {
  local identity=${1:-fork by kordax} release_version=${2:-v1.0.0}
  cat > "$fixture/payload/gortex" <<'PROBE'
#!/bin/sh
set -eu
if [ "${INSTALL_TEST_PROBE_REQUIRED:-0}" = 1 ]; then
  [ -z "${GORTEX_SESSION_ID:-}" ] || exit 90
  for probe_dir in "$HOME" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME" "$XDG_CACHE_HOME" "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR"; do
    case "$probe_dir" in "$INSTALL_TEST_ROOT"/*) exit 91 ;; esac
    [ -d "$probe_dir" ] || exit 92
    [ "$(LC_ALL=C ls -ld "$probe_dir" | cut -c1-10)" = drwx------ ] || exit 93
    : > "$probe_dir/private-version-probe"
  done
fi
PROBE
  printf 'printf "gortex %s+fixture\\n%s\\n"\n' "$release_version" "$identity" >> "$fixture/payload/gortex"
  chmod +x "$fixture/payload/gortex"
  tar -czf "$fixture/assets/$asset" -C "$fixture/payload" gortex
  if command -v sha256sum >/dev/null; then
    (cd "$fixture/assets" && sha256sum "$asset") > "$fixture/assets/checksums.txt"
  else
    (cd "$fixture/assets" && shasum -a 256 "$asset") > "$fixture/assets/checksums.txt"
  fi
  : > "$fixture/assets/$asset.sig"
  : > "$fixture/assets/$asset.pem"
}
run_installer() { INSTALL_TEST_REQUIRE_LOGS=1 INSTALL_TEST_PROBE_REQUIRED=1 sh "$installer" > "$fixture/output" 2>&1; }
expect_failure() {
  if run_installer; then echo "FAIL: expected rejection: $*"; exit 1; fi
  [[ $("$GORTEX_INSTALL_DIR/gortex" version) == "$installed" ]] || { echo 'FAIL: rejected install changed destination'; exit 1; }
  [[ ! -d "$GORTEX_INSTALL_DIR/.gortex-install.lock" ]] || { echo 'FAIL: lock leaked'; exit 1; }
}
make_release
run_installer
for expected_step in '1/6] Resolve release' '2/6] Download release artifacts' '3/6] Verify checksums and signatures' '4/6] Validate the executable' '5/6] Install atomically' '6/6] Installation complete'; do
  grep -F "$expected_step" "$fixture/output" >/dev/null
done
grep -F 'Downloaded gortex_' "$fixture/output" >/dev/null
grep -E 'Downloaded .*: [0-9.]+ (B|KiB|MiB|GiB) in' "$fixture/output" >/dev/null
if grep -q $'\033' "$fixture/output"; then echo 'FAIL: ANSI escapes in redirected logs'; exit 1; fi
installed=$("$GORTEX_INSTALL_DIR/gortex" version)
[[ "$installed" == *'fork by kordax'* ]]
# An existing upstream binary still wins when the destination is later in PATH.
mkdir "$fixture/upstream"
printf '#!/bin/sh\nprintf "gortex v0.64.8 upstream\\n"\n' > "$fixture/upstream/gortex"
chmod +x "$fixture/upstream/gortex"
PATH="$fixture/upstream:$PATH:$GORTEX_INSTALL_DIR" run_installer
grep -F "PATH currently resolves gortex to $fixture/upstream/gortex" "$fixture/output" >/dev/null
grep -F 'fork by kordax' "$fixture/output" >/dev/null
grep -F 'hash -r' "$fixture/output" >/dev/null
# Execute only the suggested PATH assignment in a child shell; it must select the fork.
suggested_path=$(sed -n 's/^  \(export PATH=.*\)$/\1/p' "$fixture/output")
[[ -n "$suggested_path" ]]
resolved=$(PATH="$fixture/upstream:$PATH:$GORTEX_INSTALL_DIR" sh -c "$suggested_path; command -v gortex")
[[ "$resolved" == "$GORTEX_INSTALL_DIR/gortex" ]]
grep -F 'releases/download/v1.0.0/' "$fixture/requests" >/dev/null
if grep -F 'latest/download' "$fixture/requests"; then echo 'FAIL: unpinned latest download'; exit 1; fi
grep -F 'https://github.com/kordax/gortex/.github/workflows/release.yml@refs/tags/v1.0.0' "$fixture/signature-requests" >/dev/null
[[ ! -e "$HOME/.profile" && ! -e "$HOME/.bashrc" ]]
GORTEX_VERSION=1.0.0 run_installer
[[ $("$GORTEX_INSTALL_DIR/gortex.previous" version) == "$installed" ]]
INSTALL_TEST_DOWNLOAD_FAILURE=1 expect_failure 'stalled download'
grep -F "Download failed for $asset (curl exit 28)" "$fixture/output" >/dev/null
grep -F 'Stopped at step 2/6: Download release artifacts.' "$fixture/output" >/dev/null
if grep -q 'Installation complete' "$fixture/output"; then echo 'FAIL: failed download reported success'; exit 1; fi
printf '%064d  %s\n' 0 "$asset" > "$fixture/assets/checksums.txt"
expect_failure 'checksum mismatch'
make_release
rm "$fixture/assets/checksums.txt"
expect_failure 'missing checksum manifest'
make_release
: > "$fixture/assets/checksums.txt"
expect_failure 'missing checksum entry'
make_release
cat "$fixture/assets/checksums.txt" >> "$fixture/assets/checksums.duplicate"
cat "$fixture/assets/checksums.duplicate" >> "$fixture/assets/checksums.txt"
expect_failure 'duplicate checksum entry'
make_release upstream
expect_failure 'upstream identity'
make_release 'fork by kordax' v1.0.1
expect_failure 'wrong version'
make_release
INSTALL_TEST_BAD_SIGNATURE=1 expect_failure 'invalid cosign signature'
GORTEX_NO_VERIFY=1 expect_failure 'disabled verification'
GORTEX_VERSION='../../bad' expect_failure 'invalid version'
GORTEX_DOWNLOAD_BASE=http://example.invalid expect_failure 'insecure transport'
mv "$GORTEX_INSTALL_DIR/gortex.previous" "$fixture/previous"
ln -s "$fixture/previous" "$GORTEX_INSTALL_DIR/gortex.previous"
expect_failure 'symlink backup'
rm "$GORTEX_INSTALL_DIR/gortex.previous"
mkfifo "$GORTEX_INSTALL_DIR/gortex.previous"
expect_failure 'FIFO backup'
rm "$GORTEX_INSTALL_DIR/gortex.previous"
mkdir "$GORTEX_INSTALL_DIR/gortex.previous"
expect_failure 'directory backup'
rmdir "$GORTEX_INSTALL_DIR/gortex.previous"
mkdir "$GORTEX_INSTALL_DIR/.gortex-install.lock"
if run_installer; then echo 'FAIL: installed through a foreign lock'; exit 1; fi
[[ -d "$GORTEX_INSTALL_DIR/.gortex-install.lock" ]] || { echo 'FAIL: removed foreign lock'; exit 1; }
[[ $("$GORTEX_INSTALL_DIR/gortex" version) == "$installed" ]]
rmdir "$GORTEX_INSTALL_DIR/.gortex-install.lock"
tar -czf "$fixture/assets/$asset" -T /dev/null
if command -v sha256sum >/dev/null; then
  (cd "$fixture/assets" && sha256sum "$asset") > "$fixture/assets/checksums.txt"
else
  (cd "$fixture/assets" && shasum -a 256 "$asset") > "$fixture/assets/checksums.txt"
fi
expect_failure 'archive without binary'
printf 'release installer contracts passed\n'
