package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/testenv"
)

func TestCommandPackageDisablesQueryTelemetry(t *testing.T) {
	if got := os.Getenv("GORTEX_QUERY_LOG_DISABLE"); got != "1" {
		t.Fatalf("GORTEX_QUERY_LOG_DISABLE = %q, want 1", got)
	}
}

func snapshotProtectedUserState(t *testing.T) map[string]string {
	t.Helper()
	before := make(map[string]string)
	for _, path := range realUserStatePaths() {
		before[path] = fingerprint(path)
	}
	return before
}

func TestTelemetryAppendDoesNotChangeProtectedUserState(t *testing.T) {
	testenv.Sandbox(t)
	before := snapshotProtectedUserState(t)
	logPath := filepath.Join(platform.CacheDir(), "query-log.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o755))
	require.NoError(t, os.WriteFile(logPath, []byte("independent daemon retrieval event\n"), 0o600))
	assert.Empty(t, diffUserState(before), "a concurrently running daemon must not be mistaken for a test escaping its sandbox")
}

func TestUserStateGuardDetectsPersistentChanges(t *testing.T) {
	dirs := testenv.Sandbox(t)
	paths := []string{
		config.DefaultGlobalConfigPath(),
		filepath.Join(platform.ConfigDir(), "servers.toml"),
		filepath.Join(dirs.Home, ".codex", "config.toml"),
		filepath.Join(dirs.Home, ".codex", "AGENTS.md"),
		filepath.Join(dirs.Home, ".claude", "settings.json"),
		filepath.Join(dirs.Home, ".claude", "CLAUDE.md"),
		filepath.Join(dirs.Home, ".claude", "skills"),
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			filePath := path
			isSkills := filepath.Base(path) == "skills"
			if isSkills {
				filePath = filepath.Join(path, "installed-skill")
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(filePath), 0o755))
			require.NoError(t, os.WriteFile(filePath, []byte("before"), 0o600))
			before := snapshotProtectedUserState(t)
			require.Contains(t, before, path, "persistent user state must remain protected")
			if isSkills {
				require.NoError(t, os.Remove(filePath))
			} else {
				require.NoError(t, os.WriteFile(filePath, []byte("after"), 0o600))
			}
			assert.Contains(t, diffUserState(before), path, "the guard must still detect a persistent state change")
		})
	}
}
