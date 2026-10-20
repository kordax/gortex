package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreatePrivateFileRefusesExistingEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.pprof")
	file, err := CreatePrivateFile(path)
	require.NoError(t, err)
	_, err = file.WriteString("private snapshot")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	assertPrivateFilePermissions(t, path)
	unexpected, err := CreatePrivateFile(path)
	if unexpected != nil {
		assert.NoError(t, unexpected.Close(), "close an unexpected existing-file handle before asserting refusal")
	}
	assert.Nil(t, unexpected)
	require.Error(t, err)
	assert.True(t, os.IsExist(err), "existing files must be refused: %v", err)
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "private snapshot", string(body))

	directory := t.TempDir()
	unexpected, err = CreatePrivateFile(directory)
	if unexpected != nil {
		assert.NoError(t, unexpected.Close(), "close an unexpected directory handle before asserting refusal")
	}
	assert.Nil(t, unexpected)
	require.Error(t, err, "an existing directory must not be replaced")
	assert.DirExists(t, directory)

	for _, dangling := range []bool{false, true} {
		name := "existing target"
		if dangling {
			name = "missing target"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "target")
			if !dangling {
				require.NoError(t, os.WriteFile(target, []byte("do not replace"), 0o600))
			}
			link := filepath.Join(root, "link")
			if err := os.Symlink(target, link); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("Windows symlink creation unavailable: %v", err)
				}
				require.NoError(t, err)
			}
			unexpected, err := CreatePrivateFile(link)
			if unexpected != nil {
				assert.NoError(t, unexpected.Close(), "close an unexpected target handle before asserting refusal")
			}
			assert.Nil(t, unexpected)
			require.Error(t, err, "existing symlinks must not be followed or replaced")
			got, err := os.Readlink(link)
			require.NoError(t, err)
			assert.Equal(t, target, got)
			if dangling {
				_, err = os.Stat(target)
				assert.True(t, os.IsNotExist(err), "a missing symlink target must stay missing")
			} else {
				body, err := os.ReadFile(target)
				require.NoError(t, err)
				assert.Equal(t, "do not replace", string(body))
			}
		})
	}
}
