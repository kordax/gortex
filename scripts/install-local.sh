#!/usr/bin/env bash
# Build and install this checkout, including uncommitted source changes.
set -euo pipefail

usage() {
    cat <<'USAGE'
Usage: bash scripts/install-local.sh [--dry-run] [--binary PATH | --rollback]

Default: build this checkout and install into $HOME/.local/bin/gortex.
  --dry-run      Build/validate and print the target without changing it.
  --binary PATH  Install an already-built trusted binary instead of building.
  --rollback     Swap the installed binary with gortex.previous.
  --help         Show this help.

GORTEX_INSTALL_DIR overrides the destination directory (for example /usr/local/bin).
Use your normal user for source builds. A trusted prebuilt binary can be installed
system-wide with sudo env GORTEX_INSTALL_DIR=/usr/local/bin bash scripts/install-local.sh --binary PATH.
GOTOOLCHAIN overrides the tested default toolchain, go1.27.3.
Requires Linux, Bash, coreutils and, for a source build, Git, Go and C/C++ tools.
The script does not modify configuration, download a published release, or
restart the daemon. A running daemon keeps its current executable until you
explicitly restart it.
USAGE
}

ui_terminal=0
ui_accent='' ui_success='' ui_error='' ui_bold='' ui_dim='' ui_reset=''
if [[ -t 2 && ${TERM:-dumb} != dumb ]]; then
    ui_terminal=1
    if [[ -z ${NO_COLOR:-} ]]; then
        ui_accent=$'\033[36m' ui_success=$'\033[32m' ui_error=$'\033[31m'
        ui_bold=$'\033[1m' ui_dim=$'\033[2m' ui_reset=$'\033[0m'
    fi
fi
step_number=0
step_total=6
step_title=Arguments
spinner_pid=''
log() { printf '    %s\n' "$*" >&2; }
step() {
    step_number=$((step_number + 1))
    step_title=$1
    printf '\n  %s%s[%s/%s] %s%s\n' "$ui_bold" "$ui_accent" "$step_number" "$step_total" "$step_title" "$ui_reset" >&2
}
ok() { printf '    %sOK%s  %s\n' "$ui_success" "$ui_reset" "$*" >&2; }
stop_spinner() {
    if [[ -n $spinner_pid ]]; then
        kill "$spinner_pid" 2>/dev/null || true
        wait "$spinner_pid" 2>/dev/null || true
        spinner_pid=''
        printf '\r\033[2K' >&2
    fi
}
start_spinner() {
    if ((ui_terminal)); then
        (
            trap - EXIT
            trap 'exit 0' TERM INT
            ui_frames="|/-\\"
            ui_frame_index=0
            ui_started=$SECONDS
            while :; do
                ui_elapsed=$((SECONDS - ui_started))
                printf '\r    %s%s%s Building checkout... %02d:%02d' "$ui_accent" "${ui_frames:ui_frame_index:1}" "$ui_reset" "$((ui_elapsed / 60))" "$((ui_elapsed % 60))" >&2
                ui_frame_index=$(((ui_frame_index + 1) % 4))
                sleep 0.1
            done
        ) &
        spinner_pid=$!
    fi
}
die() {
    stop_spinner
    printf '\n  %s%sFAILED%s  %s\n' "$ui_bold" "$ui_error" "$ui_reset" "$*" >&2
    log "Stopped at step $step_number/$step_total: $step_title."
    exit 1
}
cancel() {
    stop_spinner
    printf '\n  %sCANCELLED%s  %s\n' "$ui_dim" "$ui_reset" "$step_title" >&2
    log 'The running daemon was not restarted.'
    exit "$1"
}
complete_bar() {
    if ((ui_terminal)); then printf '    %s[########################] 100%%%s\n\n' "$ui_success" "$ui_reset" >&2; fi
}

source_binary=''
rollback=0
dry_run=0
while (($#)); do
    case "$1" in
        --help|-h) usage; exit 0 ;;
        --dry-run) dry_run=1; shift ;;
        --rollback) rollback=1; shift ;;
        --binary)
            [[ $# -ge 2 && -n $2 ]] || die '--binary requires a path'
            source_binary=$2
            shift 2
            ;;
        *) die "unknown argument: $1" ;;
    esac
done
[[ $rollback == 0 || -z $source_binary ]] || die '--binary and --rollback cannot be combined'
[[ $(uname -s) == Linux ]] || die 'this local installer supports Linux'
if [[ ${GORTEX_INSTALL_DIR+x} && -z ${GORTEX_INSTALL_DIR-} ]]; then
    die 'GORTEX_INSTALL_DIR must not be empty'
fi
install_dir=${GORTEX_INSTALL_DIR:-${HOME:?HOME is required}/.local/bin}
[[ $install_dir == /* ]] || install_dir="$PWD/$install_dir"
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
work_dir=$(mktemp -d -- "${TMPDIR:-/tmp}/gortex-local-install.XXXXXX")
candidate="$work_dir/candidate"
stage_dir=''
lock_owned=0
published=0
committed=0
had_target=0
preserve_stage=0
target="$install_dir/gortex"
backup="$target.previous"
lock_dir="$install_dir/.gortex-install.lock"

cleanup() {
    local result=$?
    trap - EXIT
    stop_spinner
    if ((published && !committed)); then
        # Only undo the executable this invocation published. Retain a recovery
        # copy if another process has replaced the destination meanwhile.
        if [[ -f $target && ! -L $target ]] && cmp -s -- "$candidate" "$target"; then
            if ((had_target)); then
                if ! mv -fT -- "$stage_dir/old" "$target"; then
                    printf 'Automatic restore failed; previous binary: %s/old\n' "$stage_dir" >&2
                    preserve_stage=1
                fi
            else
                rm -f -- "$target"
            fi
        else
            printf 'Destination changed externally; recovery files retained in %s\n' "$stage_dir" >&2
            preserve_stage=1
        fi
    fi
    if [[ -n $stage_dir && $preserve_stage == 0 ]]; then rm -rf -- "$stage_dir"; fi
    if ((lock_owned)); then rmdir -- "$lock_dir"; fi
    rm -rf -- "$work_dir"
    exit "$result"
}
trap cleanup EXIT
trap 'cancel 130' INT
trap 'cancel 143' TERM
trap 'cancel 129' HUP

check_destination() {
    local path
    for path in "$target" "$backup"; do
        [[ ! -L $path ]] || die "refusing to replace a symlink: $path"
        if [[ -e $path && ! -f $path ]]; then die "not a regular file: $path"; fi
    done
}

report_command_path() {
    local resolved_command
    resolved_command=$(command -v gortex || true)
    if [[ $resolved_command != "$target" ]]; then
        printf 'PATH resolves gortex to %s; installed binary: %s\n' "${resolved_command:-(not found)}" "$target"
        printf "For this shell: export PATH=%q:\"\$PATH\"; hash -r\\n" "$install_dir"
    else
        printf 'PATH resolves gortex to the installed binary: %s\n' "$target"
        printf 'If this shell cached an older executable, run: hash -r\n'
    fi
}

if ((dry_run)); then step_total=3; fi
printf '\n  %s%sG O R T E X%s  %sfork by kordax%s\n' "$ui_bold" "$ui_accent" "$ui_reset" "$ui_dim" "$ui_reset" >&2
printf '  %sLocal build / binary installer%s\n\n' "$ui_dim" "$ui_reset" >&2
if ((rollback)); then
    log 'Source: saved previous binary (rollback)'
elif [[ -n $source_binary ]]; then
    log "Source: trusted local binary $source_binary"
else
    log "Source: local checkout $repo_root"
    log 'Build locally; no published release archive will be downloaded.'
fi
log "Destination: $target"
if ((dry_run)); then log 'Dry run: the destination will not be changed.'; fi
step 'Prepare destination'
if (( ! dry_run )); then
    mkdir -p -- "$install_dir"
    install_dir=$(cd -- "$install_dir" && pwd -P)
    target="$install_dir/gortex"
    backup="$target.previous"
    lock_dir="$install_dir/.gortex-install.lock"
    # No waiting and no automatic stale-lock removal: concurrent invocations
    # cannot overwrite one another's saved executable.
    (umask 077; mkdir -- "$lock_dir") 2>/dev/null || die "installation lock exists: $lock_dir"
    lock_owned=1
fi
check_destination

if ((rollback)); then source_binary=$backup; fi
step 'Prepare executable'
if [[ -n $source_binary ]]; then
    log "Copying $source_binary..."
    [[ -f $source_binary && -x $source_binary ]] || die "not an executable regular file: $source_binary"
    cp -L -- "$source_binary" "$candidate"
    chmod 0755 -- "$candidate"
else
    command -v go >/dev/null || die 'Go is required for a source build'
    command -v git >/dev/null || die 'Git is required for a source build'
    revision=$(git -C "$repo_root" rev-parse --short=12 HEAD)
    if [[ -n $(git -C "$repo_root" status --porcelain=v1 --untracked-files=normal) ]]; then
        revision+=.dirty
    fi
    build_date=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    log "Building checkout $revision with ${GOTOOLCHAIN:-go1.27.3}..."
    log 'The first build may download the Go toolchain and module dependencies.'
    start_spinner
    if (
        cd -- "$repo_root"
        GOTOOLCHAIN="${GOTOOLCHAIN:-go1.27.3}" CGO_ENABLED=1 go build \
            -mod=readonly -trimpath -buildvcs=true \
            -ldflags "-s -w -X main.commit=$revision -X main.date=$build_date" \
            -o "$candidate" ./cmd/gortex
    ) > "$work_dir/build.log" 2>&1; then
        stop_spinner
        ok 'Local build complete.'
    else
        stop_spinner
        cat "$work_dir/build.log" >&2
        die 'Local build failed; the installed binary was not changed.'
    fi
fi

# Version probes must not migrate the caller's configuration or write session
# accounting. Keep their HOME and XDG paths private even for a normal install.
mkdir -p -- "$work_dir/probe-home" "$work_dir/probe-runtime"
chmod 0700 -- "$work_dir/probe-runtime"
probe_version() {
    local binary=$1
    shift
    env HOME="$work_dir/probe-home" XDG_CONFIG_HOME="$work_dir/probe-home/config" \
        XDG_DATA_HOME="$work_dir/probe-home/data" XDG_CACHE_HOME="$work_dir/probe-home/cache" \
        XDG_STATE_HOME="$work_dir/probe-home/state" XDG_RUNTIME_DIR="$work_dir/probe-runtime" \
        GORTEX_SESSION_ID= "$binary" version "$@"
}
step 'Verify version and fork identity'
candidate_version=$(probe_version "$candidate" --short) || die 'candidate version check failed'
[[ $candidate_version =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$ ]] || die 'candidate returned an invalid version'
if (( ! rollback )); then
    candidate_identity=$(probe_version "$candidate") || die 'candidate identity check failed'
    [[ $candidate_identity == *'fork by kordax'* && $candidate_identity == *'https://github.com/kordax/gortex'* ]] || die 'candidate is not the kordax fork'
fi
ok "Validated $candidate_version"
log "Target: $target"
if (( ! rollback )); then printf '%s\n' "$candidate_identity" | sed 's/^/    /' >&2; fi
if ((dry_run)); then
    complete_bar
    printf 'Dry run complete; destination unchanged.\n'
    exit 0
fi
if [[ -f $target ]] && cmp -s -- "$candidate" "$target"; then
    step 'Stage candidate'
    log 'Skipped: the same binary is already installed.'
    step 'Verify installed executable'
    ok 'Installed bytes match; backup unchanged.'
    step 'Installation complete'
    complete_bar
    printf 'The same binary is already installed; backup unchanged.\n'
    report_command_path
    exit 0
fi

step 'Stage candidate and preserve current binary'
stage_dir=$(mktemp -d -- "$install_dir/.gortex-local-stage.XXXXXX")
install -m 0755 -- "$candidate" "$stage_dir/new"
cmp -s -- "$candidate" "$stage_dir/new" || die 'staged binary differs from candidate'
if [[ -f $target ]]; then
    log "Preserving the current executable for rollback..."
    cp -p -- "$target" "$stage_dir/old"
    had_target=1
fi
# GNU mv -T replaces the directory entry on the same filesystem. It neither
# truncates the running executable nor follows a leaf symlink as a directory.
step 'Install atomically and verify'
published=1
mv -fT -- "$stage_dir/new" "$target"
installed_version=$(probe_version "$target" --short) || die 'installed version check failed; restoring previous binary'
[[ $installed_version == "$candidate_version" ]] || die 'installed version differs; restoring previous binary'
if ((had_target)); then
    cp -p -- "$stage_dir/old" "$stage_dir/backup"
    mv -fT -- "$stage_dir/backup" "$backup"
fi
committed=1
step 'Installation complete'
complete_bar
ok "$installed_version"
printf 'Installed %s at %s\n' "$installed_version" "$target"
if ((had_target)); then printf 'Previous binary: %s\n' "$backup"; fi
printf 'Configuration and running daemon unchanged.\n'
printf 'To activate this binary in a running daemon: %q daemon restart\n' "$target"
report_command_path
