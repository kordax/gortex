package testenv

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSetHomeRedirectsUserHomeDir is the regression guard for the bug
// this package exists to prevent: t.Setenv("HOME", dir) alone leaves
// os.UserHomeDir resolving the developer's real profile on Windows.
// Asserting against os.UserHomeDir rather than against $HOME is what
// makes this test meaningful on every platform — it checks the lookup
// the production code actually performs, not the variable we happened
// to set.
func TestSetHomeRedirectsUserHomeDir(t *testing.T) {
	dir := t.TempDir()
	SetHome(t, dir)

	got, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir() after SetHome: %v", err)
	}
	if got != dir {
		t.Fatalf("os.UserHomeDir() = %s, want %s — the sandbox does not cover %s", got, dir, runtime.GOOS)
	}
}

// TestSandboxRedirectsEveryUserDirLookup pins all three stdlib user-dir
// lookups, because Gortex resolves cache and config paths through
// os.UserCacheDir in several packages as a fallback. On Windows those
// two read %AppData% / %LocalAppData% and ignore the XDG variables, so
// covering only XDG would leave them pointed at the real profile.
//
// It deliberately asserts exact sandbox paths instead of "not under the
// real home": on GitHub's Windows runner TEMP itself lives under the
// user profile, so a prefix check would be both wrong and green.
func TestSandboxRedirectsEveryUserDirLookup(t *testing.T) {
	d := Sandbox(t)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir(): %v", err)
	}
	if home != d.Home {
		t.Errorf("os.UserHomeDir() = %s, want %s", home, d.Home)
	}

	// Each platform derives these two differently, and only the generic
	// Unix branch consults XDG. Spelling the matrix out is the point of
	// the test: it proves the sandbox covers the lookup this platform
	// really performs, whichever root that turns out to be.
	var wantConfig, wantCache string
	switch runtime.GOOS {
	case "windows":
		wantConfig = filepath.Join(d.Home, "AppData", "Roaming")
		wantCache = filepath.Join(d.Home, "AppData", "Local")
	case "darwin", "ios":
		wantConfig = filepath.Join(d.Home, "Library", "Application Support")
		wantCache = filepath.Join(d.Home, "Library", "Caches")
	default:
		wantConfig = d.Config
		wantCache = d.Cache
	}

	gotConfig, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("os.UserConfigDir(): %v", err)
	}
	if gotConfig != wantConfig {
		t.Errorf("os.UserConfigDir() = %s, want %s", gotConfig, wantConfig)
	}

	gotCache, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("os.UserCacheDir(): %v", err)
	}
	if gotCache != wantCache {
		t.Errorf("os.UserCacheDir() = %s, want %s", gotCache, wantCache)
	}
}

// TestUnifiedHomeCollapsesToHomeLayout proves the variant used by tests
// that assert the default ~/.gortex layout both moves the home and
// clears every XDG override — an inherited XDG_CONFIG_HOME on the
// developer's machine would otherwise win over the temporary home.
func TestUnifiedHomeCollapsesToHomeLayout(t *testing.T) {
	for _, v := range xdgVars {
		t.Setenv(v, filepath.Join(t.TempDir(), "inherited"))
	}

	home := UnifiedHome(t)

	got, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir(): %v", err)
	}
	if got != home {
		t.Errorf("os.UserHomeDir() = %s, want %s", got, home)
	}
	for _, v := range xdgVars {
		if val := os.Getenv(v); val != "" {
			t.Errorf("%s = %q, want empty — an inherited override still wins", v, val)
		}
	}
}

// TestSandboxRestoresAmbientEnvironment proves the sandbox is scoped to
// the test that asked for it. t.Setenv unwinds at test end, so a helper
// that isolated one test must not silently isolate — or corrupt — the
// next one.
func TestSandboxRestoresAmbientEnvironment(t *testing.T) {
	before, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no resolvable home directory in this environment")
	}

	t.Run("sandboxed", func(t *testing.T) {
		d := Sandbox(t)
		if d.Home == before {
			t.Fatalf("sandbox home %s equals the ambient home — nothing was isolated", d.Home)
		}
	})

	after, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir() after the sandboxed subtest: %v", err)
	}
	if after != before {
		t.Fatalf("ambient home leaked: was %s, now %s", before, after)
	}
}

func TestSandboxesIsolateAndRestoreQueryLogOverride(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		name := "unset"
		if inherited {
			name = "set"
		}
		t.Run(name, func(t *testing.T) {
			external := filepath.Join(t.TempDir(), "external-query-log.jsonl")
			t.Setenv("GORTEX_QUERY_LOG", external)
			if !inherited {
				require.NoError(t, os.Unsetenv("GORTEX_QUERY_LOG"))
			}
			before, existed := os.LookupEnv("GORTEX_QUERY_LOG")
			t.Run("test sandbox", func(t *testing.T) {
				dirs := SandboxAt(t, t.TempDir())
				isolated := os.Getenv("GORTEX_QUERY_LOG")
				require.NotEmpty(t, isolated)
				assert.NotEqual(t, external, isolated)
				assert.Equal(t, filepath.Dir(dirs.Config), filepath.Dir(isolated), "telemetry must be inside the sandbox")
			})
			after, stillExists := os.LookupEnv("GORTEX_QUERY_LOG")
			assert.Equal(t, before, after)
			assert.Equal(t, existed, stillExists)

			var processLog string
			func() {
				restore, err := SandboxProcess()
				require.NoError(t, err)
				defer restore()
				processLog = os.Getenv("GORTEX_QUERY_LOG")
				require.NotEmpty(t, processLog)
				assert.NotEqual(t, external, processLog)
				assert.DirExists(t, filepath.Dir(processLog), "process telemetry must be in its private temporary tree")
			}()
			after, stillExists = os.LookupEnv("GORTEX_QUERY_LOG")
			assert.Equal(t, before, after)
			assert.Equal(t, existed, stillExists)
			assert.NoDirExists(t, filepath.Dir(processLog), "restoring the process must clean up its telemetry tree")
		})
	}
}
