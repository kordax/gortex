package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// On SQLite the token's counts are full count(*) scans, so they are reused
// until a committed graph write advances the mutation clocks, and a write is
// seen at once rather than after the memo's time bound.
func TestCommunityTokenReusesCountsUntilTheGraphMoves(t *testing.T) {
	store, err := store_sqlite.Open(t.TempDir() + "/token.sqlite")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{
		{ID: "A", Name: "A", Kind: graph.KindFunction, FilePath: "a.go", Language: "go"},
		{ID: "B", Name: "B", Kind: graph.KindFunction, FilePath: "a.go", Language: "go"},
	}, []*graph.Edge{{From: "A", To: "B", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1}})
	s, _ := populateAnalysisForTest(store)

	first := s.currentCommunityToken()
	assert.Equal(t, 2, first.nodeCount)
	assert.Equal(t, first, s.currentCommunityToken(), "an unchanged graph keeps its token")

	store.AddBatch([]*graph.Node{
		{ID: "C", Name: "C", Kind: graph.KindFunction, FilePath: "c.go", Language: "go"},
	}, nil)
	moved := s.currentCommunityToken()
	assert.NotEqual(t, first, moved, "a committed write is visible immediately")
	assert.Equal(t, 3, moved.nodeCount)
	assert.Greater(t, moved.analysisRevision, first.analysisRevision)
}
