# Gortex by kordax

This fork has independent versions and updates at [kordax/gortex](https://github.com/kordax/gortex/releases). Upstream history and attribution remain intact. Future versions follow this fork's compatibility decisions; upstream changes can be reviewed and merged separately.

`gortex version` identifies the binary with `fork by kordax`, the fork repository, version and commit. Check `command -v gortex` too: an older installation earlier in `PATH` can still win. A running daemon keeps its previous executable until explicitly restarted.

## Current release: v1.1.0

This release adds daemon monitoring, opt-in Prometheus metrics, OpenTelemetry tracing, bounded profiles and performance benchmarks. It includes a locally built static Linux amd64 archive and Debian package. Other platforms, cosign signatures and SLSA attestations are not included. Local checks are required before publication; GitHub CI is not awaited for this maintainer-requested manual release.

Install the latest published release into `~/.local/bin` from this checkout:

```sh
sh scripts/install.sh
"$HOME/.local/bin/gortex" daemon restart
gortex version
```

The installer resolves the latest fork tag and verifies SHA-256. If cosign is installed, it also requires workflow signatures and refuses unsigned manual releases. Keep verification enabled; use the trusted-binary procedure below for an explicitly trusted manual build, substituting `v1.1.0` for `v1.0.0`.

For Debian or Ubuntu, download `gortex_linux_amd64.deb` from [v1.1.0](https://github.com/kordax/gortex/releases/tag/v1.1.0), verify it against `checksums.txt`, then install:

```sh
sudo apt install ./gortex_linux_amd64.deb
/usr/bin/gortex version
```

The package installs `/usr/bin/gortex` without restarting the daemon. An existing `~/.local/bin/gortex` earlier in PATH still takes precedence, and an existing service keeps its configured executable path. Use the archive installer when updating that user installation.

For a manual release, keep the release workflow disabled only during tag and asset publication, restore its prior state even on failure, and leave CI/security workflows enabled. Publish a draft as latest only after downloading its assets again and verifying checksums, the executable identity and package contents. Do not fabricate workflow signatures or attestations.

## Historical v1.0.0 installation

`v1.0.0` is an exceptional manual **Linux amd64** release, built and checked locally. Its published artifacts are `gortex_linux_amd64.tar.gz` and `checksums.txt`. It has no cosign signatures, SLSA provenance, macOS/Windows builds, arm64 builds or Linux package-manager artifacts. GitHub CI runs in the background; its completion is not a verification claim for this manual release.

Download and verify the release over HTTPS:

```sh
release_install_dir=$(mktemp -d)
release_url=https://github.com/kordax/gortex/releases/download/v1.0.0
curl --fail --show-error --location --proto '=https' --proto-redir '=https' \
  "$release_url/gortex_linux_amd64.tar.gz" -o "$release_install_dir/gortex_linux_amd64.tar.gz"
curl --fail --show-error --location --proto '=https' --proto-redir '=https' \
  "$release_url/checksums.txt" -o "$release_install_dir/checksums.txt"
(cd "$release_install_dir" && sha256sum --check --ignore-missing checksums.txt)
```

Continue only if checksum verification succeeds. SHA-256 detects corruption; a checksum downloaded from the same GitHub release is not an independent signature or build-provenance attestation.

From this release's source checkout, extract the trusted binary and install it for the current user:

```sh
tar -xOzf "$release_install_dir/gortex_linux_amd64.tar.gz" gortex > "$release_install_dir/gortex"
chmod 755 "$release_install_dir/gortex"
"$release_install_dir/gortex" version
bash scripts/install-local.sh --binary "$release_install_dir/gortex"
```

The source installer validates the fork identity and defaults to `~/.local/bin/gortex`. That directory makes the binary available to all your projects and shells once it precedes other Gortex directories in `PATH`. For a machine-wide installation, use this command instead of the last command above:

```sh
sudo env GORTEX_INSTALL_DIR=/usr/local/bin \
  bash scripts/install-local.sh --binary "$release_install_dir/gortex"
```

Both paths preserve `gortex.previous`, use a shared destination lock and atomic replacement, and refuse symlink or special-file destinations. They leave shell startup files, MCP configuration, repositories and the running daemon alone. Remove the temporary directory after installation:

```sh
rm -rf "$release_install_dir"
export PATH="$HOME/.local/bin:$PATH"
hash -r
command -v gortex
gortex version
```

Use `/usr/local/bin` in the PATH command if you chose the machine-wide installation. To activate the replacement explicitly, check the selected binary and restart through its absolute path:

```sh
~/.local/bin/gortex version
~/.local/bin/gortex daemon restart
```

Adjust that path to your installation directory. Development tests use a disposable `GORTEX_INSTALL_DIR` and must not replace or restart the maintainer's working installation.

The remote `scripts/install.sh` retains strict signature verification when cosign is present. It will refuse this unsigned manual `v1.0.0` on such machines. Do not remove cosign or bypass verification to make it pass: use the explicitly trusted binary installation above. `GORTEX_NO_VERIFY` remains rejected.

## Build this checkout

```sh
bash scripts/install-local.sh
```

This builds the current checkout, including working-tree changes. Use `--dry-run` to build and validate without replacing the installed binary, and `--rollback` to restore the previous binary. See `--help` for available options.

`gortex update` / `gortex upgrade` checks this fork's releases. Automatic installation of the unsigned `v1.0.0` follows the same cosign refusal described above; the trusted-binary path is the explicit installation method for this exceptional release. This fork does not publish into upstream Homebrew or Scoop repositories.

## Future automated releases

The normal release pipeline remains stricter than the one-off `v1.0.0` process. It requires successful `CI` and security push workflows for the exact tagged commit. It builds four Unix archives, a Windows amd64 zip and Linux `.deb`, `.rpm` and `.apk` packages for both architectures: 11 primary artifacts in total. **That set describes future automated releases, not the artifacts available for `v1.0.0`.**

The remote installer targets Linux/macOS amd64 and arm64 when the selected release contains those assets. It resolves `latest` to one tag, requires SHA-256 verification and, when cosign is installed, requires signatures matching this fork's tagged workflow identity. It preserves a previous binary and does not change services or configuration.

Automated archives retain stable names such as `gortex_linux_amd64.tar.gz`. Download an archive, its `.sig` and `.pem` from the same signed release, then verify using its actual tag:

```sh
cosign verify-blob \
  --certificate gortex_linux_amd64.tar.gz.pem \
  --signature gortex_linux_amd64.tar.gz.sig \
  --certificate-identity "https://github.com/kordax/gortex/.github/workflows/release.yml@refs/tags/$GORTEX_RELEASE_TAG" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  gortex_linux_amd64.tar.gz
```

Set `GORTEX_RELEASE_TAG` to the signed release being verified. This command does not apply to the unsigned `v1.0.0`.

Apple credentials are optional. Without this fork's Developer ID and notarization credentials, future macOS binaries use ad-hoc signing and are not notarized by Apple. Cosign authenticates the GitHub build identity; it does not replace Apple notarization. Local Linux checks alone do not establish macOS or Windows compatibility.

## Maintainer release procedure

For the manual `v1.0.0`, complete local lint, security checks and relevant tests before committing and publishing. Temporarily disable only the release workflow around tag publication to avoid a concurrent automated release. Restore it in a `finally`/trap path and verify its state is `active`, even if publishing fails. CI and security workflows stay enabled and run in the background. This is a documented one-off release path; it does not weaken workflow gates or signature requirements.

For subsequent automated releases:

1. Finish code, installer contracts, lint, security checks and tests. Commit the intended contents before tagging.
2. Push to `main` and wait for both `CI` and security workflows to succeed for that exact SHA, including platform tests.
3. Push a stable `vX.Y.Z` tag on that verified commit. The tag workflow checks both successful push runs before building.
4. GoReleaser creates a draft in `kordax/gortex`. Jobs assemble all platforms, checksums and signatures. Verification downloads the actual draft bytes, requires all 11 primary assets, validates signatures and SHA-256, and submits the complete subject list, including Windows, for SLSA provenance.
5. Only after verification and provenance succeed does the workflow publish the draft and update `latest`. A failed job leaves a draft for diagnosis. Do not manually publish a failed automated draft to bypass a gate.

The workflow uses this fork's `GITHUB_TOKEN` and GitHub OIDC. Upstream Homebrew/Scoop publishing is disabled for this fork. Apple and VirusTotal credentials are optional; no upstream private secret is required.
