package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReleaseInstallerContract(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("release shell installer targets Linux and macOS")
	}
	bash, err := exec.LookPath("bash")
	require.NoError(t, err)
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "test-install-release.sh"))
	require.NoError(t, err)
	// Register shell sources as test inputs so script-only edits invalidate Go's cache.
	for _, source := range []string{script, filepath.Join(filepath.Dir(script), "install.sh")} {
		contents, readErr := os.ReadFile(source)
		require.NoError(t, readErr)
		require.NotEmpty(t, contents)
	}
	cmd := exec.CommandContext(t.Context(), bash, script)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	require.Contains(t, string(output), "release installer contracts passed")
}
