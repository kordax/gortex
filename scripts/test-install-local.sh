#!/usr/bin/env bash
# Contract tests use only disposable binaries and a private install prefix.
set -euo pipefail
installer_test_root=$(mktemp -d -- "${TMPDIR:-/tmp}/gortex-install-test.XXXXXX")
trap 'rm -rf -- "$installer_test_root"' EXIT
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
installer="$repo_root/scripts/install-local.sh"
prefix="$installer_test_root/prefix with spaces"
mkdir -p -- "$installer_test_root/source" "$installer_test_root/user-home"
# HOME is set only in the child environment as an additional test sandbox.
run_installer() {
    env HOME="$installer_test_root/user-home" GORTEX_INSTALL_DIR="$prefix" \
        bash "$installer" "$@" >"$installer_test_root/last.log" 2>&1
}
fail() { cat "$installer_test_root/last.log" >&2; printf 'FAIL: %s\n' "$*" >&2; exit 1; }
expect_failure() { if run_installer "$@"; then fail "unexpected success: $*"; fi; }
assert_version() {
    local actual
    actual=$("$1" version --short)
    [[ $actual == "$2" ]] || fail "version at $1 is $actual, expected $2"
}
assert_unlocked() { [[ ! -e "$prefix/.gortex-install.lock" ]] || fail 'owned install lock leaked'; }
write_binary() {
    local filename=$1 version=$2 bad_target=${3-}
    {
        printf '#!/usr/bin/env bash\nset -eu\n'
        if [[ -n $bad_target ]]; then
            printf "if [[ \$0 == %q ]]; then exit 73; fi\n" "$bad_target"
        fi
        printf "if [[ \$# == 1 && \$1 == version ]]; then printf 'gortex (fork by kordax)\\\\n  source: https://github.com/kordax/gortex\\\\n'; exit 0; fi\\n"
        printf "[[ \$# == 2 && \$1 == version && \$2 == --short ]] || exit 91\n"
        printf 'printf "%%s\\n" %q\n' "$version"
    } >"$filename"
    chmod 0755 -- "$filename"
}
write_binary "$installer_test_root/source/one" 'v1.0.0+test'
write_binary "$installer_test_root/source/two" 'v2.0.0+test'

# Dry-run validation never creates the destination directory.
run_installer --dry-run --binary "$installer_test_root/source/one" || fail 'dry run'
[[ ! -e $prefix ]] || fail 'dry run created destination'
run_installer --binary "$installer_test_root/source/one" || fail 'initial install'
assert_version "$prefix/gortex" 'v1.0.0+test'
[[ ! -e $prefix/gortex.previous ]] || fail 'initial install invented a backup'
assert_unlocked
# Merely appearing somewhere in PATH is insufficient: an older command can
# shadow the new fork. Report the actual resolution without changing either.
old_bin="$installer_test_root/old bin"
mkdir -p -- "$old_bin"
write_binary "$old_bin/gortex" 'v0.64.8+old'
PATH="$old_bin:$prefix:$PATH" run_installer --binary "$installer_test_root/source/two" || fail 'upgrade'
installer_output=$(<"$installer_test_root/last.log")
[[ $installer_output == *"PATH resolves gortex to $old_bin/gortex"* ]] || fail 'missing shadowed command path'
[[ $installer_output == *'export PATH='* && $installer_output == *'hash -r'* ]] || fail 'missing shell activation commands'
[[ $installer_output == *'fork by kordax'* && $installer_output == *'https://github.com/kordax/gortex'* ]] || fail 'missing validated fork identity'
assert_version "$old_bin/gortex" 'v0.64.8+old'
assert_version "$prefix/gortex" 'v2.0.0+test'
assert_version "$prefix/gortex.previous" 'v1.0.0+test'
run_installer --binary "$installer_test_root/source/two" || fail 'identical reinstall'
assert_version "$prefix/gortex.previous" 'v1.0.0+test'
run_installer --rollback || fail 'rollback'
assert_version "$prefix/gortex" 'v1.0.0+test'
assert_version "$prefix/gortex.previous" 'v2.0.0+test'
run_installer --rollback || fail 'rollback swap'
assert_version "$prefix/gortex" 'v2.0.0+test'
assert_version "$prefix/gortex.previous" 'v1.0.0+test'

# Validation failure must not replace either installed executable.
write_binary "$installer_test_root/source/invalid" 'not-a-version'
expect_failure --binary "$installer_test_root/source/invalid"
expect_failure --binary "$installer_test_root/source/missing"
expect_failure --binary "$installer_test_root/source/one" --rollback
assert_version "$prefix/gortex" 'v2.0.0+test'
assert_version "$prefix/gortex.previous" 'v1.0.0+test'
assert_unlocked

# A valid SemVer alone cannot identify the distribution. Refuse upstream
# candidates while leaving both installed files untouched.
printf "#!/usr/bin/env bash\\nif [[ \$2 == --short ]]; then echo v1.0.0; else echo 'upstream gortex'; fi\\n" >"$installer_test_root/source/upstream"
chmod 0755 -- "$installer_test_root/source/upstream"
expect_failure --binary "$installer_test_root/source/upstream"
assert_version "$prefix/gortex" 'v2.0.0+test'
assert_version "$prefix/gortex.previous" 'v1.0.0+test'
assert_unlocked

# A binary can pass staging but fail from its final path. Restore the original
# executable and preserve the last successful backup in that case.
write_binary "$installer_test_root/source/broken-after-publish" 'v3.0.0+test' "$prefix/gortex"
expect_failure --binary "$installer_test_root/source/broken-after-publish"
assert_version "$prefix/gortex" 'v2.0.0+test'
assert_version "$prefix/gortex.previous" 'v1.0.0+test'
assert_unlocked

# Compilers failing before publication leave the current install alone.
mkdir -p -- "$installer_test_root/fake-tools"
printf '#!/usr/bin/env bash\nexit 42\n' >"$installer_test_root/fake-tools/go"
chmod 0755 -- "$installer_test_root/fake-tools/go"
if PATH="$installer_test_root/fake-tools:$PATH" run_installer; then fail 'compiler failure was ignored'; fi
assert_version "$prefix/gortex" 'v2.0.0+test'
assert_version "$prefix/gortex.previous" 'v1.0.0+test'
assert_unlocked

# Refuse a lock owned by another invocation, without removing it.
mkdir -- "$prefix/.gortex-install.lock"
expect_failure --binary "$installer_test_root/source/one"
[[ -d $prefix/.gortex-install.lock ]] || fail 'removed foreign lock'
rmdir -- "$prefix/.gortex-install.lock"

# Refuse managed symlinks and special files instead of following or reading them.
mv -- "$prefix/gortex" "$prefix/managed-target"
ln -s -- managed-target "$prefix/gortex"
expect_failure --binary "$installer_test_root/source/one"
[[ -L $prefix/gortex ]] || fail 'replaced symlink'
assert_version "$prefix/managed-target" 'v2.0.0+test'
rm -- "$prefix/gortex"
mv -- "$prefix/managed-target" "$prefix/gortex"
mv -- "$prefix/gortex.previous" "$prefix/saved-previous"
ln -s -- saved-previous "$prefix/gortex.previous"
expect_failure --binary "$installer_test_root/source/one"
[[ -L $prefix/gortex.previous ]] || fail 'replaced backup symlink'
rm -- "$prefix/gortex.previous"
mv -- "$prefix/saved-previous" "$prefix/gortex.previous"
mv -- "$prefix/gortex" "$prefix/saved-current"
mkfifo -- "$prefix/gortex"
expect_failure --binary "$installer_test_root/source/one"
[[ -p $prefix/gortex ]] || fail 'replaced special file'
rm -- "$prefix/gortex"
mv -- "$prefix/saved-current" "$prefix/gortex"
assert_version "$prefix/gortex" 'v2.0.0+test'
assert_unlocked
[[ ! -e $installer_test_root/user-home/.local ]] || fail 'installer ignored private prefix'
printf 'Local installer contract tests passed.\n'
