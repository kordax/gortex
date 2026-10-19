package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestContractOwnerRefreshDetectsStaleAndFileAnchors(t *testing.T) {
	file := &graph.Node{ID: "a/config.env", Kind: graph.KindFile, FilePath: "a/config.env", RepoPrefix: "a"}
	consumer := contracts.Contract{ID: "env::TOKEN", Type: contracts.ContractEnv, Role: contracts.RoleConsumer, FilePath: file.FilePath, RepoPrefix: "a", Line: 2}
	owned := &graph.Edge{From: file.ID, To: consumer.ID, Kind: graph.EdgeConsumes, FilePath: file.FilePath, Line: 2}
	assert.False(t, contractOwnerRowsPresent([]contracts.Contract{consumer}, []*graph.Node{file}, nil), "symbol-less records need a real file ownership edge")
	assert.True(t, contractOwnerRowsPresent([]contracts.Contract{consumer}, []*graph.Node{file}, map[string][]*graph.Edge{file.ID: {owned}}))
	staleLine := *owned
	staleLine.Line = 1
	assert.False(t, contractOwnerRowsPresent([]contracts.Contract{consumer}, []*graph.Node{file}, map[string][]*graph.Edge{file.ID: {&staleLine}}))
	staleFile := *owned
	staleFile.FilePath = "a/stale.env"
	assert.False(t, contractOwnerRowsPresent([]contracts.Contract{consumer}, []*graph.Node{file}, map[string][]*graph.Edge{file.ID: {&staleFile}}))

	handler := &graph.Node{ID: "a::Handler", Kind: graph.KindFunction, RepoPrefix: "a"}
	provider := contracts.Contract{ID: "http::GET::/ready", Type: contracts.ContractHTTP, Role: contracts.RoleProvider, SymbolID: handler.ID, FilePath: "a/server.go", RepoPrefix: "a", Line: 10}
	provides := &graph.Edge{From: handler.ID, To: provider.ID, Kind: graph.EdgeProvides, FilePath: provider.FilePath, Line: provider.Line}
	route := *provides
	route.Kind = graph.EdgeHandlesRoute
	assert.False(t, contractOwnerRowsPresent([]contracts.Contract{provider}, []*graph.Node{handler}, map[string][]*graph.Edge{handler.ID: {provides}}), "an intact provides edge does not hide a lost route edge")
	assert.True(t, contractOwnerRowsPresent([]contracts.Contract{provider}, []*graph.Node{handler}, map[string][]*graph.Edge{handler.ID: {provides, &route}}))
}

func TestContractEnvOwnersSurviveStructuralPublication(t *testing.T) {
	root := t.TempDir()
	source := func(revision int, token string) []byte {
		return []byte(fmt.Sprintf("package workload\nimport \"os\"\ntype Holder struct { Revision%d int }\nfunc Capability() string { return os.Getenv(%q) }\n", revision, token))
	}
	path := filepath.Join(root, "capability.go")
	require.NoError(t, os.WriteFile(path, source(0, "TOKEN_NAME"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sibling.go"), []byte("package workload\nimport \"os\"\nfunc Sibling() string { return os.Getenv(\"TOKEN_NAME\") }\n"), 0600))
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	idx := New(store, registry, (config.Config{}).Index, zap.NewNop())
	result, err := idx.Index(root)
	require.NoError(t, err)
	require.Empty(t, result.Errors)
	consumers := func(token string) map[string]bool {
		found := make(map[string]bool)
		for edge := range store.EdgesByKind(graph.EdgeConsumes) {
			if edge.To != "env::"+token {
				continue
			}
			owner := store.GetNode(edge.From)
			require.NotNil(t, owner)
			found[owner.Name] = true
		}
		return found
	}
	want := map[string]bool{"Capability": true, "Sibling": true}
	require.Equal(t, want, consumers("TOKEN_NAME"))
	for revision := 1; revision <= 2; revision++ {
		require.NoError(t, os.WriteFile(path, source(revision, "TOKEN_NAME"), 0600))
		require.NoError(t, idx.IndexFile("capability.go"))
		assert.Equal(t, want, consumers("TOKEN_NAME"), "unchanged contract must survive structural publication %d", revision)
	}
	refresh := idx.refreshContractsForFiles([]string{"capability.go"})
	assert.False(t, refresh.Changed, "an intact equal contract remains a no-op")

	require.NoError(t, os.WriteFile(path, source(3, "NEW_TOKEN"), 0600))
	require.NoError(t, idx.IndexFile("capability.go"))
	assert.Equal(t, map[string]bool{"Sibling": true}, consumers("TOKEN_NAME"))
	assert.Equal(t, map[string]bool{"Capability": true}, consumers("NEW_TOKEN"), "republish fresh records, never the stale anchor")

	require.NoError(t, os.WriteFile(path, []byte("package workload\ntype Holder struct { Revision4 int }\nfunc Capability() string { return \"removed\" }\n"), 0600))
	require.NoError(t, idx.IndexFile("capability.go"))
	assert.Empty(t, consumers("NEW_TOKEN"))
	assert.Equal(t, map[string]bool{"Sibling": true}, consumers("TOKEN_NAME"), "shared canonical IDs retain sibling ownership")
}
