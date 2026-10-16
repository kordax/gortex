// Package testenv redirects the per-user state a test can reach — the
// user home, the XDG base directories, and the daemon's runtime files —
// into temporary directories, so no test reads or writes the developer's
// real Gortex configuration.
//
// It exists because t.Setenv("HOME", dir) does not isolate a test on
// every platform. os.UserHomeDir reads $HOME on Unix but %USERPROFILE%
// on Windows, so a HOME-only override is a silent no-op there: the code
// under test keeps resolving the developer's real home and writes
// straight through to it. That asymmetry is invisible on the linux and
// macos CI matrix, which is exactly why it survived — on Windows the
// cmd/gortex suite was appending its temp directories to the real
// config.yaml as tracked repos, merging hooks into the real
// ~/.codex/config.toml, and deleting shipped skills out of the real
// ~/.claude/skills tree.
//
// Every helper here sets both variables, plus the AppData pair that
// os.UserConfigDir and os.UserCacheDir read on Windows, so whatever the
// current platform considers "the user's home" lands inside the sandbox.
package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// xdgVars is every XDG base-directory variable that can relocate Gortex
// state. platform.unifiedDir honours an absolute value over the home
// directory, so a test that wants the unified <home>/.gortex layout has
// to clear all of them, not just the one it happens to care about.
var xdgVars = []string{
	"XDG_CONFIG_HOME",
	"XDG_DATA_HOME",
	"XDG_CACHE_HOME",
	"XDG_STATE_HOME",
	"XDG_RUNTIME_DIR",
}

// Dirs are the sandbox roots handed to a test by Sandbox and SandboxAt.
type Dirs struct {
	Home    string // $HOME and %USERPROFILE%
	Config  string // $XDG_CONFIG_HOME
	Data    string // $XDG_DATA_HOME
	Cache   string // $XDG_CACHE_HOME
	Runtime string // $XDG_RUNTIME_DIR
}

// homeEnv is every variable that has to move for the current platform to
// stop resolving the developer's real home directory.
func homeEnv(home string) map[string]string {
	return map[string]string{
		"HOME":        home, // Unix: os.UserHomeDir
		"USERPROFILE": home, // Windows: os.UserHomeDir
		// os.UserConfigDir and os.UserCacheDir read these two on
		// Windows, where the XDG variables are ignored entirely.
		"AppData":      filepath.Join(home, "AppData", "Roaming"),
		"LocalAppData": filepath.Join(home, "AppData", "Local"),
	}
}

// sandboxEnv is homeEnv plus every base directory and daemon runtime
// path Gortex resolves, rooted at a scratch directory. Keeping the one
// list here is the point of the package: a second copy is how a variable
// gets forgotten and a suite starts writing through again.
func sandboxEnv(d Dirs, root string) map[string]string {
	env := homeEnv(d.Home)
	env["XDG_CONFIG_HOME"] = d.Config
	env["XDG_DATA_HOME"] = d.Data
	env["XDG_CACHE_HOME"] = d.Cache
	env["XDG_STATE_HOME"] = filepath.Join(root, "state")
	env["XDG_RUNTIME_DIR"] = d.Runtime
	// The daemon resolves each of these before falling back to the XDG
	// cache directory, so redirecting the cache root is not enough.
	env["GORTEX_DAEMON_SOCKET"] = filepath.Join(root, "daemon.sock")
	env["GORTEX_DAEMON_PIDFILE"] = filepath.Join(root, "daemon.pid")
	env["GORTEX_DAEMON_LOGFILE"] = filepath.Join(root, "daemon.log")
	env["GORTEX_DAEMON_SNAPSHOT"] = filepath.Join(root, "daemon.gob.gz")
	// An explicit telemetry destination also takes precedence over XDG.
	env["GORTEX_QUERY_LOG"] = filepath.Join(root, "query-log.jsonl")
	return env
}

// dirsUnder lays the sandbox roots out beneath a scratch directory.
func dirsUnder(home, root string) Dirs {
	return Dirs{
		Home:    home,
		Config:  filepath.Join(root, "config"),
		Data:    filepath.Join(root, "data"),
		Cache:   filepath.Join(root, "cache"),
		Runtime: filepath.Join(root, "runtime"),
	}
}

// SetHome points the current platform's user-home lookup at dir, so
// os.UserHomeDir resolves there on Unix and on Windows alike. Use it
// directly only when a test needs to move the home mid-run; otherwise
// prefer UnifiedHome or Sandbox, which also handle the XDG variables.
func SetHome(tb testing.TB, dir string) {
	tb.Helper()
	for k, v := range homeEnv(dir) {
		tb.Setenv(k, v)
	}
}

// UnifiedHome isolates the user home and clears every XDG override, so
// path resolution collapses into the unified <home>/.gortex layout. Use
// it for tests that assert that default layout; use Sandbox when the
// test only needs the state to go somewhere harmless.
func UnifiedHome(tb testing.TB) string {
	tb.Helper()
	home := tb.TempDir()
	SetHome(tb, home)
	for _, v := range xdgVars {
		tb.Setenv(v, "")
	}
	return home
}

// Sandbox isolates the user home and points every XDG base directory
// and daemon runtime file at a fresh temporary tree.
func Sandbox(tb testing.TB) Dirs {
	tb.Helper()
	return SandboxAt(tb, tb.TempDir())
}

// SandboxAt is Sandbox with a caller-supplied home directory, for tests
// that seed files into the home before running the code under test.
func SandboxAt(tb testing.TB, home string) Dirs {
	tb.Helper()
	root := tb.TempDir()
	d := dirsUnder(home, root)
	for k, v := range sandboxEnv(d, root) {
		tb.Setenv(k, v)
	}
	return d
}

// SandboxProcess isolates the whole process's per-user environment and
// returns a function that restores it and removes the scratch tree. It
// is the TestMain-time counterpart to Sandbox, for a package that wants
// every one of its tests sandboxed by default rather than trusting each
// to remember. A test that needs its own home can still override with
// t.Setenv, which unwinds to this sandbox rather than to the real home.
func SandboxProcess() (restore func(), err error) {
	root, err := os.MkdirTemp("", "gortex-testenv")
	if err != nil {
		return nil, fmt.Errorf("create sandbox root: %w", err)
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("create sandbox home: %w", err)
	}

	saved := map[string]*string{}
	for k, v := range sandboxEnv(dirsUnder(home, root), root) {
		if prev, ok := os.LookupEnv(k); ok {
			saved[k] = &prev
		} else {
			saved[k] = nil
		}
		if err := os.Setenv(k, v); err != nil {
			return nil, fmt.Errorf("set %s: %w", k, err)
		}
	}

	return func() {
		for k, prev := range saved {
			if prev == nil {
				_ = os.Unsetenv(k)
				continue
			}
			_ = os.Setenv(k, *prev)
		}
		_ = os.RemoveAll(root)
	}, nil
}
