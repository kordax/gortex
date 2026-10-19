package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/entrypoints"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

func TestIncrementalRestoresTypeDerivationsWithoutDeclarationChanges(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			for _, flags := range []DerivedInvalidationFlags{0, DerivedInvalidatesRuntime} {
				name := "literal-body"
				if flags != 0 {
					name = "call-body"
				}
				t.Run(name, func(t *testing.T) {
					var store graph.Store = graph.New()
					if backend == "sqlite" {
						sqlite, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
						require.NoError(t, err)
						t.Cleanup(func() { require.NoError(t, sqlite.Close()) })
						store = sqlite
					}
					// Model the graph immediately after successful file replacement:
					// extracted members and inheritance survive, derived edges and
					// inherited entry-point stamps need to be rebuilt.
					store.AddBatch([]*graph.Node{
						{ID: "repo::Worker", Kind: graph.KindType, FilePath: "repo/worker.go", RepoPrefix: "repo", Language: "go"},
						{ID: "repo::Worker.Run", Kind: graph.KindMethod, Name: "Run", FilePath: "repo/worker.go", RepoPrefix: "repo", Language: "go"},
						{ID: "repo::Runner", Kind: graph.KindInterface, FilePath: "repo/runner.go", RepoPrefix: "repo", Language: "go", Meta: map[string]any{"methods": []string{"Run"}}},
						{ID: "repo::Runner.Run", Kind: graph.KindMethod, Name: "Run", FilePath: "repo/runner.go", RepoPrefix: "repo", Language: "go"},
						{ID: "repo::Base", Kind: graph.KindType, FilePath: "repo/base.cs", RepoPrefix: "repo", Language: "csharp", Meta: map[string]any{entrypoints.MetaEntryPoint: true, entrypoints.MetaEntryKind: "aspnet:controller"}},
						{ID: "repo::Child", Kind: graph.KindType, FilePath: "repo/child.cs", RepoPrefix: "repo", Language: "csharp"},
					}, []*graph.Edge{
						{From: "repo::Worker.Run", To: "repo::Worker", Kind: graph.EdgeMemberOf, FilePath: "repo/worker.go"},
						{From: "repo::Runner.Run", To: "repo::Runner", Kind: graph.EdgeMemberOf, FilePath: "repo/runner.go"},
						{From: "repo::Child", To: "repo::Base", Kind: graph.EdgeExtends, FilePath: "repo/child.cs"},
					})
					mi := NewMultiIndexer(store, nil, nil, nil, zap.NewNop())
					plan := DerivedInvalidationPlan{
						Flags: flags, Files: []string{"repo/worker.go", "repo/child.cs"},
						TypeIDs: []string{"repo::Worker", "repo::Child"},
					}
					report := mi.RunIncrementalDerivedPasses(context.Background(), map[string]DerivedInvalidationPlan{"repo": plan})
					assert.Equal(t, 1, report.Implements)
					assert.Equal(t, 1, report.Overrides)
					assert.Equal(t, 1, report.EntryPointHierarchy)
					child := store.GetNode("repo::Child")
					require.NotNil(t, child)
					assert.Equal(t, "aspnet:controller", child.Meta[entrypoints.MetaEntryKind])
					kinds := make(map[graph.EdgeKind]string)
					for _, edge := range store.GetOutEdges("repo::Worker") {
						kinds[edge.Kind] = edge.To
					}
					assert.Equal(t, "repo::Runner", kinds[graph.EdgeImplements])
					for _, edge := range store.GetOutEdges("repo::Worker.Run") {
						kinds[edge.Kind] = edge.To
					}
					assert.Equal(t, "repo::Runner.Run", kinds[graph.EdgeOverrides])
					// A second coordinator pass cannot duplicate any derivation.
					report = mi.RunIncrementalDerivedPasses(context.Background(), map[string]DerivedInvalidationPlan{"repo": plan})
					assert.Zero(t, report.Implements)
					assert.Zero(t, report.Overrides)
					assert.Zero(t, report.EntryPointHierarchy)
				})
			}
		})
	}
}
