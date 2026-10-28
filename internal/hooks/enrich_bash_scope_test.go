package hooks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var bashScopeHit = []grepSymbolHit{{Name: "gortex", Kind: "contract", FilePath: "go.mod", Line: 1}}

// A word that also names a symbol must not deny a grep of a notes file the
// graph does not hold: the agent is searching text, not looking up the symbol.
func TestEnrichBash_GrepOfNonSourceFile_PassesThrough(t *testing.T) {
	redirectTelemetry(t)
	rec := stubProbe(t, bashScopeHit, nil)
	notes := filepath.Join(t.TempDir(), "AGENTS.md")
	require.NoError(t, os.WriteFile(notes, []byte("gortex\n"), 0o600))

	r := enrichBash(map[string]any{"command": "grep -niE 'gortex' " + notes + " | head -20"}, t.TempDir())
	assert.False(t, r.deny)
	assert.Empty(t, r.context)
	assert.Empty(t, rec.calls, "a proven non-source scope is not probed")
}

func TestEnrichBash_GrepOfHomeNotesFile_ExpandsTilde(t *testing.T) {
	redirectTelemetry(t)
	rec := stubProbe(t, bashScopeHit, nil)
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".codex"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".codex", "AGENTS.md"), []byte("gortex\n"), 0o600))

	r := enrichBash(map[string]any{"command": "[ -f x ] && { echo a; grep -niE 'gortex' ~/.codex/AGENTS.md | head; }"}, "")
	assert.False(t, r.deny, r.reason)
	assert.Empty(t, rec.calls)
}

func TestEnrichBash_GrepScopes(t *testing.T) {
	cases := []struct {
		name               string
		command            string
		hasSource, probeOK bool
		deny               bool
	}{
		{"directory proven to hold no source", "grep -rn gortex " + t.TempDir(), false, true, false},
		{"directory holding indexed source", "grep -rn gortex " + t.TempDir(), true, true, true},
		{"directory the daemon could not judge", "grep -rn gortex " + t.TempDir(), false, false, true},
		{"no explicit root", "grep -rn gortex", false, true, true},
		{"shell variable root", `grep -n gortex "$f"`, false, true, true},
		{"find in a non-source tree", "find " + t.TempDir() + " -name gortex", false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			redirectTelemetry(t)
			stubProbe(t, bashScopeHit, nil)
			stubScopeTracked(t, tc.hasSource, tc.probeOK)
			r := enrichBash(map[string]any{"command": tc.command}, "")
			assert.Equal(t, tc.deny, r.deny, r.reason)
		})
	}
}

func TestEnrichBash_GrepWithOneSourceRootStillProbes(t *testing.T) {
	redirectTelemetry(t)
	stubProbe(t, bashScopeHit, nil)
	notes := t.TempDir()
	old := scopeTrackedFn
	scopeTrackedFn = func(_, scope string) (bool, bool) { return scope != notes, true }
	t.Cleanup(func() { scopeTrackedFn = old })

	r := enrichBash(map[string]any{"command": "grep -rn gortex " + notes + " internal"}, "")
	assert.True(t, r.deny, "a source root among the scopes keeps the deny")
}
