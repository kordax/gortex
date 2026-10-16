package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalInstallerContract(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("local source installer targets Linux")
	}
	bash, err := exec.LookPath("bash")
	require.NoError(t, err)
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "test-install-local.sh"))
	require.NoError(t, err)
	// Child-process file reads are invisible to Go's test cache. Register both
	// scripts in the parent so script-only fixes invalidate a cached success.
	for _, path := range []string{script, filepath.Join(filepath.Dir(script), "install-local.sh")} {
		_, err := os.ReadFile(path)
		require.NoError(t, err)
	}
	cmd := exec.CommandContext(t.Context(), bash, script)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	require.Contains(t, string(output), "Local installer contract tests passed.")
}
