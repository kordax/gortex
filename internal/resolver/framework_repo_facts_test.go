package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// A C# edit in a repository with no EF Core, MediatR or DI registration fact
// used to run all three passes over the whole repository; on the Unity game
// that is most of every patch. The memo must skip them there, and must never
// skip them where a fact exists, whether the fact is in the edit or elsewhere
// in the repository.
func TestFrameworkRepoFactsSkipPassesOnlyWithoutFacts(t *testing.T) {
	t.Parallel()
	g := graph.New()
	facts := NewFrameworkRepoFacts()
	scope := map[string]bool{"game": true}
	g.AddNode(&graph.Node{
		ID: "game/Player.cs::Player", Kind: graph.KindType, Name: "Player",
		FilePath: "game/Player.cs", Language: "csharp", RepoPrefix: "game",
	})
	edit := []string{"game/Player.cs"}

	for _, synth := range []string{SynthCSharpEFCoreModels, SynthMediatR, SynthCSharpDIIfaces} {
		assert.False(t, facts.allow(g, synth, scope, edit),
			"%s must not run in a repository that holds none of its facts", synth)
	}
	assert.True(t, facts.allow(g, "gin-middleware", scope, edit), "passes without a probe always run")
	assert.True(t, facts.allow(g, SynthCSharpEFCoreModels, scope, nil), "a repository-wide run always runs")

	// The edit that introduces a DbSet carries the fact itself.
	efAddDbSet(g, "game/Db.cs", "game", "", "Order", "Orders")
	assert.True(t, facts.allow(g, SynthCSharpEFCoreModels, scope, []string{"game/Db.cs"}))
	// A later edit elsewhere still runs: a new Order class must bind to it.
	assert.True(t, facts.allow(g, SynthCSharpEFCoreModels, scope, edit))
	assert.False(t, facts.allow(g, SynthMediatR, scope, edit))
}

// A fact that reaches the store without a frontier, through a full or
// repository-wide run, is found once that run forgets the memo.
func TestFrameworkRepoFactsForgottenByRepositoryRun(t *testing.T) {
	t.Parallel()
	g := graph.New()
	facts := NewFrameworkRepoFacts()
	scope := map[string]bool{"svc": true}
	g.AddNode(&graph.Node{
		ID: "svc/A.cs::A", Kind: graph.KindType, Name: "A",
		FilePath: "svc/A.cs", Language: "csharp", RepoPrefix: "svc",
	})
	edit := []string{"svc/A.cs"}
	assert.False(t, facts.allow(g, SynthMediatR, scope, edit))

	g.AddNode(&graph.Node{
		ID: "svc/H.cs::H.Handle", Kind: graph.KindMethod, Name: "Handle",
		FilePath: "svc/H.cs", Language: "csharp", RepoPrefix: "svc",
		Meta: map[string]any{"mediatr_request_type": "Ping", "mediatr_kind": "request"},
	})
	assert.False(t, facts.allow(g, SynthMediatR, scope, edit), "the memo holds until a run forgets it")
	facts.forget(scope)
	assert.True(t, facts.allow(g, SynthMediatR, scope, edit))
}
