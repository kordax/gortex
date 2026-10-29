package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// A one-file edit used to pay for the Godot passes and the C# receiver gate on
// every repository: the passes carried no language gate, and the gate ran on
// any partial run. Measured on a Go-only edit they cost about a second of a
// two-second patch while producing no edge.
func TestFrontierLanguageGatesSkipPassesTheEditCannotFeed(t *testing.T) {
	t.Parallel()
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "a/main.go::Run", Kind: graph.KindFunction, Name: "Run", Language: "go", FilePath: "a/main.go", RepoPrefix: "a"},
		{ID: "a/game.gd::_ready", Kind: graph.KindMethod, Name: "_ready", Language: "gdscript", FilePath: "a/game.gd", RepoPrefix: "a"},
		{ID: "a/project.godot::Game", Kind: graph.KindType, Name: "Game", Language: "godot_project", FilePath: "a/project.godot", RepoPrefix: "a"},
		{ID: "a/Svc.cs::Svc.Do", Kind: graph.KindMethod, Name: "Do", Language: "csharp", FilePath: "a/Svc.cs", RepoPrefix: "a"},
	}, nil)
	scope := map[string]bool{"a": true}
	summary := func(files ...string) frameworkCandidateSummary {
		return summarizeFrameworkCandidatesCensus(g, scope, files, false)
	}
	godot := map[string]FrameworkSynthesizer{}
	for _, s := range defaultFrameworkSynthesizers() {
		switch s.Name() {
		case SynthGodotAutoload, SynthGodotPreloadAlias, SynthGodotConnection:
			godot[s.Name()] = s
		}
	}
	require.Len(t, godot, 3)

	goEdit := summary("a/main.go")
	for name, s := range godot {
		assert.False(t, shouldRunFrameworkSynthesizer(s, scope, goEdit), "%s must not run for a Go-only edit", name)
		assert.True(t, shouldRunFrameworkSynthesizer(s, scope, summary("a/game.gd")), "%s must run for a GDScript edit", name)
		assert.True(t, shouldRunFrameworkSynthesizer(s, scope, summary("a/project.godot")), "%s must run for a project.godot edit", name)
	}

	assert.False(t, csharpFrontierCanDemote([]string{"a/main.go"}, false, goEdit),
		"a frontier with no C# node offers the receiver gate no C# caller or target")
	assert.True(t, csharpFrontierCanDemote([]string{"a/Svc.cs"}, false, summary("a/Svc.cs")))
	assert.True(t, csharpFrontierCanDemote([]string{"a/main.go"}, true, goEdit),
		"a changed C# hierarchy re-judges calls outside the frontier")
	assert.True(t, csharpFrontierCanDemote(nil, false, summary()),
		"a repository-wide run keeps the gate")
}
