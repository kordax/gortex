package store_sqlite

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Encoding happens after old rows are deleted inside the writer transaction.
// Holding it here makes the publication window deterministic for readers.
type filePublicationBarrier struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *filePublicationBarrier) MarshalJSON() ([]byte, error) {
	b.once.Do(func() { close(b.reached) })
	<-b.release
	return []byte("true"), nil
}

func replacementStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "publication.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func requireReplacementHit(t *testing.T, s *Store, token, id string) {
	t.Helper()
	hits, err := s.SearchSymbols(token, 10)
	require.NoError(t, err)
	if id == "" {
		require.Empty(t, hits, "query %s", token)
		return
	}
	require.Len(t, hits, 1, "query %s", token)
	assert.Equal(t, id, hits[0].NodeID)
}

func TestReplaceFilesAtomicSearchPublication(t *testing.T) {
	s := replacementStore(t)
	const marker = "repo/marker.go::Issue767PrimaryMarker"
	s.AddBatch([]*graph.Node{
		{ID: marker, Kind: graph.KindFunction, Name: "Issue767PrimaryMarker", FilePath: "repo/marker.go", EndLine: 2},
		{ID: "obsolete", Kind: graph.KindFunction, Name: "obsolete", FilePath: "repo/marker.go"},
		{ID: "sibling", Kind: graph.KindFunction, Name: "sibling", FilePath: "repo/sibling.go"},
	}, nil)
	require.NoError(t, s.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{
		{NodeID: marker, Tokens: "issue767primarymarker"},
		{NodeID: "obsolete", Tokens: "zqxobsolete"},
		{NodeID: "sibling", Tokens: "zqxsibling"},
	}))
	barrier := &filePublicationBarrier{reached: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	t.Cleanup(unblock)
	done := make(chan error, 1)
	go func() {
		done <- s.ReplaceFiles(graph.FileBatchReplacement{
			FilePaths: []string{"repo/marker.go"},
			Nodes: []*graph.Node{
				{ID: marker, Kind: graph.KindFunction, Name: "Issue767PrimaryMarker", FilePath: "repo/marker.go", EndLine: 3, Meta: map[string]any{"publication_barrier": barrier}},
				{ID: "added", Kind: graph.KindFunction, Name: "added", FilePath: "repo/marker.go"},
			},
			Edges: []*graph.Edge{{From: marker, To: "added", Kind: graph.EdgeCalls}},
			Symbols: []graph.SymbolFTSItem{
				{NodeID: marker, Tokens: "issue767primarymarker"},
				{NodeID: "added", Tokens: "zqxadded"},
			},
		})
	}()
	select {
	case <-barrier.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not reach its uncommitted publication window")
	}
	// Search and graph hydration must both read the old committed corpus.
	// Repeating observations does not retry a missing semantic result.
	for range 20 {
		requireReplacementHit(t, s, "issue767primarymarker", marker)
		requireReplacementHit(t, s, "zqxobsolete", "obsolete")
		requireReplacementHit(t, s, "zqxadded", "")
		node := s.GetNode(marker)
		require.NotNil(t, node)
		assert.Equal(t, 2, node.EndLine)
	}
	unblock()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not commit")
	}
	requireReplacementHit(t, s, "issue767primarymarker", marker)
	requireReplacementHit(t, s, "zqxobsolete", "")
	requireReplacementHit(t, s, "zqxadded", "added")
	requireReplacementHit(t, s, "zqxsibling", "sibling")
	assert.Equal(t, 3, s.GetNode(marker).EndLine)
	require.Len(t, s.GetOutEdges(marker), 1)
	assert.Equal(t, "added", s.GetOutEdges(marker)[0].To)
}

func TestReplaceFilesFailureRollsBackGraphAndFTS(t *testing.T) {
	s := replacementStore(t)
	s.AddNode(&graph.Node{ID: "old", Kind: graph.KindFunction, Name: "old", FilePath: "repo/a.go"})
	require.NoError(t, s.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: "old", Tokens: "zqxoriginal"}}))
	err := s.ReplaceFiles(graph.FileBatchReplacement{
		FilePaths: []string{"repo/a.go"},
		Nodes: []*graph.Node{{
			ID: "new", Kind: graph.KindFunction, Name: "new", FilePath: "repo/a.go",
			Meta: map[string]any{"cannot_encode": func() {}},
		}},
		Symbols: []graph.SymbolFTSItem{{NodeID: "new", Tokens: "zqxreplacement"}},
	})
	require.Error(t, err)
	require.NotNil(t, s.GetNode("old"))
	assert.Nil(t, s.GetNode("new"))
	requireReplacementHit(t, s, "zqxoriginal", "old")
	requireReplacementHit(t, s, "zqxreplacement", "")
}

func TestReplaceFilesInitialPublicationAndGenerationIsolation(t *testing.T) {
	s := replacementStore(t)
	other := s.AtGeneration(7)
	for _, handle := range []*Store{s, other} {
		require.NoError(t, handle.ReplaceFiles(graph.FileBatchReplacement{
			FilePaths: []string{"repo/a.go"},
			Nodes:     []*graph.Node{{ID: "same", Kind: graph.KindFunction, Name: "same", FilePath: "repo/a.go"}},
			Symbols:   []graph.SymbolFTSItem{{NodeID: "same", Tokens: "zqxoriginal"}},
		}))
	}
	require.NoError(t, other.ReplaceFiles(graph.FileBatchReplacement{
		FilePaths: []string{"repo/a.go"},
		Nodes:     []*graph.Node{{ID: "different", Kind: graph.KindFunction, Name: "different", FilePath: "repo/a.go"}},
		Symbols:   []graph.SymbolFTSItem{{NodeID: "different", Tokens: "zqxreplacement"}},
	}))
	requireReplacementHit(t, s, "zqxoriginal", "same")
	requireReplacementHit(t, s, "zqxreplacement", "")
	requireReplacementHit(t, other, "zqxoriginal", "")
	requireReplacementHit(t, other, "zqxreplacement", "different")
	assert.NotNil(t, s.GetNode("same"))
	assert.Nil(t, other.GetNode("same"))
}

func TestReplaceFilesRetainsSharedContractDocument(t *testing.T) {
	s := replacementStore(t)
	s.AddBatch([]*graph.Node{
		{ID: "a", Kind: graph.KindFunction, Name: "a", FilePath: "repo/a.go", RepoPrefix: "repo"},
		{ID: "b", Kind: graph.KindFunction, Name: "b", FilePath: "repo/b.go", RepoPrefix: "repo"},
		{ID: "contract", Kind: graph.KindContract, Name: "contract", FilePath: "repo/a.go", RepoPrefix: "repo", Meta: map[string]any{"contract_owner_record": true}},
	}, []*graph.Edge{
		{From: "a", To: "contract", Kind: graph.EdgeProvides, FilePath: "repo/a.go"},
		{From: "b", To: "contract", Kind: graph.EdgeProvides, FilePath: "repo/b.go"},
	})
	require.NoError(t, s.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{
		{NodeID: "a", Tokens: "zqxremovedowner"},
		{NodeID: "contract", Tokens: "zqxsharedcontract"},
	}))
	require.NoError(t, s.ReplaceFiles(graph.FileBatchReplacement{
		FilePaths: []string{"repo/a.go"},
		Nodes:     []*graph.Node{{ID: "a", Kind: graph.KindFunction, Name: "a", FilePath: "repo/a.go", RepoPrefix: "repo"}},
		Symbols:   []graph.SymbolFTSItem{{NodeID: "a", Tokens: "zqxchangedowner"}},
	}))
	require.NotNil(t, s.GetNode("contract"))
	requireReplacementHit(t, s, "zqxsharedcontract", "contract")
	requireReplacementHit(t, s, "zqxremovedowner", "")
	requireReplacementHit(t, s, "zqxchangedowner", "a")
	require.Len(t, s.GetInEdges("contract"), 1)
	assert.Equal(t, "b", s.GetInEdges("contract")[0].From)
}
