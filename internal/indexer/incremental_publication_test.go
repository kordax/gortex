package indexer

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type observedFileReplacementStore struct {
	*store_sqlite.Store
	publicationErr error
	observe        atomic.Bool
	replacements   atomic.Int32
	ftsDeletes     atomic.Int32
	evictions      atomic.Int32
	reached        chan struct{}
	release        chan struct{}
}

func (s *observedFileReplacementStore) ReplaceFiles(replacement graph.FileBatchReplacement) error {
	if s.publicationErr != nil {
		return s.publicationErr
	}
	if s.observe.Load() {
		s.replacements.Add(1)
		close(s.reached)
		<-s.release
	}
	return s.Store.ReplaceFiles(replacement)
}

func (s *observedFileReplacementStore) BatchDeleteSymbolFTS(ids []string) error {
	if s.observe.Load() {
		s.ftsDeletes.Add(1)
	}
	return s.Store.BatchDeleteSymbolFTS(ids)
}

func (s *observedFileReplacementStore) EvictFiles(paths []string) (int, int) {
	if s.observe.Load() {
		s.evictions.Add(1)
	}
	return s.Store.EvictFiles(paths)
}

func TestIncrementalReindexPublishesStructuralFilesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "marker.go")
	const initial = "package fixture\n\nfunc Issue767PrimaryMarker() string { return \"primary\" }\n"
	writeFile(t, path, initial)
	writeFile(t, filepath.Join(dir, "caller.go"), "package fixture\n\nfunc Caller() string { return Issue767PrimaryMarker() }\n")
	base, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, base.Close()) })
	s := &observedFileReplacementStore{Store: base, reached: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(s.release) }) }
	t.Cleanup(unblock)
	idx := newTestIndexer(s)
	_, err = idx.Index(dir)
	require.NoError(t, err)
	idx.deferGlobalPasses.Store(true)
	before := idx.search.Search("Issue767PrimaryMarker", 10)
	require.Len(t, before, 1)
	markerID := before[0].ID
	s.observe.Store(true)
	writeFile(t, path, initial+"\nfunc AppendedMarker() int { return 1 }\n")
	future := time.Now().Add(3 * time.Second)
	require.NoError(t, os.Chtimes(path, future, future))
	done := make(chan error, 1)
	go func() {
		_, err := idx.IncrementalReindexPaths(dir, []string{path})
		done <- err
	}()
	select {
	case <-s.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("structural edit did not use atomic backend publication")
	}
	// The old corpus must remain valid until ReplaceFiles commits. An early
	// FTS delete or separate graph eviction would lose the unchanged marker.
	for range 20 {
		hits := idx.search.Search("Issue767PrimaryMarker", 10)
		require.Len(t, hits, 1)
		assert.Equal(t, markerID, hits[0].ID)
		require.NotNil(t, s.GetNode(markerID))
	}
	unblock()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("incremental reindex failed to finish")
	}
	assert.EqualValues(t, 1, s.replacements.Load())
	assert.Zero(t, s.ftsDeletes.Load(), "atomic publication used an earlier FTS deletion")
	assert.Zero(t, s.evictions.Load(), "atomic publication used a separate graph eviction")
	after := idx.search.Search("Issue767PrimaryMarker", 10)
	require.Len(t, after, 1)
	assert.Equal(t, markerID, after[0].ID)
	require.Len(t, idx.search.Search("AppendedMarker", 10), 1)
}

func TestIncrementalReindexPublicationConnectionDoneLeavesFilesStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "marker.go")
	const initial = "package fixture\n\nfunc Issue767PrimaryMarker() string { return \"primary\" }\n"
	writeFile(t, path, initial)
	writeFile(t, filepath.Join(dir, "caller.go"), "package fixture\n\nfunc Caller() string { return Issue767PrimaryMarker() }\n")
	base, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, base.Close()) })
	s := &observedFileReplacementStore{Store: base}
	idx := newTestIndexer(s)
	_, err = idx.Index(dir)
	require.NoError(t, err)
	idx.deferGlobalPasses.Store(true)
	key := idx.relKey(path)
	markerID := idx.prefixPath(key) + "::Issue767PrimaryMarker"
	beforeIncoming := s.GetInEdges(markerID)
	callerID := idx.prefixPath("caller.go") + "::Caller"
	var callerRefFound bool
	for _, edge := range beforeIncoming {
		callerRefFound = callerRefFound || (edge.From == callerID && edge.To == markerID && graph.IsResolvableRefEdge(edge.Kind))
	}
	require.True(t, callerRefFound, "fixture must include a resolved incoming reference from caller.go")
	beforeIncomingJSON, err := json.Marshal(beforeIncoming)
	require.NoError(t, err)
	before, err := base.FileMetasByPaths(idx.repoPrefix, []string{key})
	require.NoError(t, err)
	require.Len(t, before, 1)
	writeFile(t, path, initial+"\nfunc AppendedMarker() int { return 1 }\n")
	future := time.Now().Add(3 * time.Second)
	require.NoError(t, os.Chtimes(path, future, future))
	s.publicationErr = sql.ErrConnDone
	require.NotPanics(t, func() {
		consumed, _, reparsed, failed, _ := idx.reindexIncrementalChunk([]string{path}, &reparsePendingEnrichmentBatch{}, false)
		assert.Equal(t, 1, consumed)
		assert.Empty(t, reparsed)
		assert.Equal(t, []string{path}, failed)
	})
	assert.ErrorIs(t, idx.fileIndexFailureError(path), sql.ErrConnDone)
	assert.True(t, idx.IsStale(key), "failed graph publication stamped a fresh read receipt")
	afterFailure, err := base.FileMetasByPaths(idx.repoPrefix, []string{key})
	require.NoError(t, err)
	assert.Equal(t, before, afterFailure, "failed graph publication updated file sidecars")
	afterIncoming := s.GetInEdges(markerID)
	afterIncomingJSON, err := json.Marshal(afterIncoming)
	require.NoError(t, err)
	assert.Equal(t, beforeIncomingJSON, afterIncomingJSON, "failed publication changed surviving callers' references")
	for _, edge := range afterIncoming {
		assert.Equal(t, markerID, edge.To)
	}
	require.Len(t, idx.search.Search("Issue767PrimaryMarker", 10), 1)
	addedID := idx.prefixPath(key) + "::AppendedMarker"
	assert.Nil(t, s.GetNode(addedID))
	for _, hit := range idx.search.Search("AppendedMarker", 10) {
		assert.NotEqual(t, addedID, hit.ID, "failed publication exposed the new symbol document")
	}

	// Reopening a usable publication path must retry the same disk version,
	// rather than classifying the failed write as an inert/fresh file.
	s.publicationErr = nil
	_, _, reparsed, failed, _ := idx.reindexIncrementalChunk([]string{path}, &reparsePendingEnrichmentBatch{}, false)
	assert.Equal(t, []string{path}, reparsed)
	assert.Empty(t, failed)
	assert.False(t, idx.IsStale(key))
	require.NotNil(t, s.GetNode(addedID))
	var foundAdded bool
	for _, hit := range idx.search.Search("AppendedMarker", 10) {
		foundAdded = foundAdded || hit.ID == addedID
	}
	assert.True(t, foundAdded, "retry did not publish the exact appended symbol")
	callerRefFound = false
	for _, edge := range s.GetInEdges(markerID) {
		callerRefFound = callerRefFound || (edge.From == callerID && edge.To == markerID && graph.IsResolvableRefEdge(edge.Kind))
	}
	assert.True(t, callerRefFound, "retry lost surviving callers' resolved references")
}

func TestIncomingRestubsForReplacementDoesNotMutatePriorView(t *testing.T) {
	const markerID = "repo/marker.go::Issue767PrimaryMarker"
	original := &graph.Edge{
		From: "repo/caller.go::Caller", To: markerID, Kind: graph.EdgeCalls,
		Origin: "semantic", Tier: "exact", Confidence: 0.95,
		Meta: map[string]any{"nested": map[string]any{"preserved": true}},
	}
	before, err := json.Marshal(original)
	require.NoError(t, err)
	stages := []*incrementalBatchStage{{priorNodes: []*graph.Node{{
		ID: markerID, Kind: graph.KindFunction, Name: "Issue767PrimaryMarker",
	}}}}
	parked := incomingRestubsForReplacement(stages, incrementalPriorView{
		inByNode: map[string][]*graph.Edge{markerID: {original}},
	})
	require.Len(t, parked, 1)
	assert.NotSame(t, original, parked[0])
	assert.Equal(t, graph.UnresolvedMarker+"Issue767PrimaryMarker", parked[0].To)
	assert.Empty(t, parked[0].Origin)
	assert.Equal(t, markerID, original.To)
	after, err := json.Marshal(original)
	require.NoError(t, err)
	assert.Equal(t, before, after, "preparing a transaction changed the captured prior graph")
}
