//go:build !windows

package platform

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertPrivateFilePermissions(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Zero(t, info.Mode().Perm()&0o077, "private files must not grant group or other access")
}
