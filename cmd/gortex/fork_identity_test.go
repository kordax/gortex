package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForkVersionIdentity(t *testing.T) {
	oldVersion, oldCommit, oldDate, oldShort := version, commit, date, versionShort
	t.Cleanup(func() { version, commit, date, versionShort = oldVersion, oldCommit, oldDate, oldShort })
	version, commit, date, versionShort = "1.0.0", "abc123.dirty", "2026-10-16T00:00:00Z", false
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	runVersion(cmd, nil)
	assert.Contains(t, out.String(), "gortex v1.0.0+abc123.dirty (fork by kordax)")
	assert.Contains(t, out.String(), "source:  https://github.com/kordax/gortex")
	assert.Contains(t, out.String(), "commit:  abc123.dirty")
	out.Reset()
	versionShort = true
	runVersion(cmd, nil)
	assert.Equal(t, "v1.0.0+abc123.dirty\n", out.String())
}

func TestForkUpdateDownloadFailureDoesNotExecutePartialScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fork automatic installation uses POSIX Bash; Windows updates are manual")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "must-not-exist")
	curl := "#!/usr/bin/env bash\nwhile (($#)); do\n if [[ $1 == --output ]]; then printf 'touch \"$FORK_UPDATE_MARKER\"\\n' >\"$2\"; exit 22; fi\n shift\ndone\nexit 90\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "curl"), []byte(curl), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FORK_UPDATE_MARKER", marker)
	command, manual := upgradeInstructions(InstallScript, "v1.0.0")
	require.False(t, manual)
	require.Error(t, upgradeExecCommand(context.Background(), InstallScript, command).Run())
	assert.NoFileExists(t, marker)
}

func TestForkUpdateRefusesPackageManagedRun(t *testing.T) {
	oldRun := upgradeRun
	upgradeRun = true
	t.Cleanup(func() { upgradeRun = oldRun })
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	// go test's executable lives outside ~/.local/bin. A pinned version avoids
	// any network lookup; refusal must happen before daemon lifecycle hooks.
	err := runUpgrade(cmd, []string{"v1.0.0"})
	require.ErrorContains(t, err, "refusing to replace")
	assert.Contains(t, out.String(), upgradeRepoURL+"/releases/latest")
	assert.NotContains(t, out.String(), "get.gortex.dev")
}

func TestVCSBuildCommit(t *testing.T) {
	for _, tc := range []struct{ name, revision, modified, want string }{
		{"clean", "abc123", "false", "abc123"},
		{"dirty", "abc123", "true", "abc123.dirty"},
		{"missing", "", "", ""},
		{"modified_without_revision", "", "true", "dirty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, vcsBuildCommit([]debug.BuildSetting{{Key: "vcs.revision", Value: tc.revision}, {Key: "vcs.modified", Value: tc.modified}}))
		})
	}
}

func TestForkUpdateRejectsForeignReleasesAndUnsafePins(t *testing.T) {
	assert.Empty(t, tagFromReleaseLocation("https://github.com/zzet/gortex/releases/tag/v99.0.0"))
	assert.Empty(t, tagFromReleaseLocation(upgradeRepoURL+"/releases/tag/main"))
	assert.Empty(t, tagFromReleaseLocation(upgradeRepoURL+"/releases/tag/v1.0.0/extra"))
	for _, method := range []InstallMethod{InstallScript, InstallGoInstall, InstallBrew, InstallScriptPS} {
		require.Error(t, validateUpgradePin(method, "v1.0.0'; touch /tmp/bad; '"))
		command, manual := upgradeInstructions(method, "v1.0.0'; touch /tmp/bad; '")
		assert.Empty(t, command)
		assert.True(t, manual)
	}
}
