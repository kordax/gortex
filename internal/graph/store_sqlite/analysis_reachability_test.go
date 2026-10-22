package store_sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestAnalysisReachabilityIndexOnReopen(t *testing.T) {
	path := filepathForAnalysisTest(t)
	store, err := Open(path)
	require.NoError(t, err)
	_, err = store.writerDB.Exec("DROP INDEX analysis_processes_by_entry_point")
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	var id, parent, unused int
	var detail string
	err = store.db.QueryRow("EXPLAIN QUERY PLAN SELECT entry_point FROM analysis_processes WHERE generation_id = ? AND entry_point IN (?, ?)", 1, "a", "b").Scan(&id, &parent, &unused, &detail)
	require.NoError(t, err)
	require.Contains(t, detail, "SEARCH")
	require.Contains(t, detail, "analysis_processes_by_entry_point")
	require.Contains(t, detail, "entry_point=?")
}

func TestAnalysisReachableNodesContext(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	revision := store.AnalysisMutationRevision()
	id, accepted, err := store.BeginAnalysisGeneration(revision, graph.AnalysisGenerationHeader{
		FormatVersion: 77, NodeCount: 3, ProcessCount: 1,
	})
	requireAnalysisAccepted(t)(accepted, err)
	requireAnalysisAccepted(t)(store.AppendAnalysisNodes(revision, id, []graph.AnalysisNodeMetric{
		{NodeID: "entry"}, {NodeID: "step"}, {NodeID: "dead"},
	}))
	requireAnalysisAccepted(t)(store.AppendAnalysisProcesses(revision, id, []graph.AnalysisProcessSummary{
		{ID: "process", EntryPoint: "entry", StepCount: 1},
	}, []graph.AnalysisProcessStep{{ProcessID: "process", NodeID: "step", Ordinal: 0}}))
	requireAnalysisAccepted(t)(store.PutAnalysisBlob(revision, id, graph.AnalysisBlob{Component: graph.AnalysisBlobAdjacency, Payload: []byte("adj")}))
	requireAnalysisAccepted(t)(store.PutAnalysisBlob(revision, id, graph.AnalysisBlob{Component: graph.AnalysisBlobLeiden, Payload: []byte("leiden")}))
	for component, count := range map[graph.AnalysisComponent]int{
		graph.AnalysisComponentNodes: 3, graph.AnalysisComponentCommunities: 0,
		graph.AnalysisComponentProcesses: 1, graph.AnalysisComponentConcepts: 0,
		graph.AnalysisComponentAdjacency: 1, graph.AnalysisComponentLeiden: 1,
	} {
		requireAnalysisAccepted(t)(store.SealAnalysisComponent(revision, id, component, count))
	}
	requireAnalysisAccepted(t)(store.ActivateAnalysisGeneration(revision, id))

	// A concurrent generation writer must not block this read-only capability.
	store.writeMu.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	rows, err := store.AnalysisReachableNodesContext(ctx, id, []string{"entry", "step", "dead", "missing"})
	cancel()
	store.writeMu.Unlock()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"entry", "step"}, rows, "entry points need no corresponding step row")

	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	_, err = store.AnalysisReachableNodesContext(ctx, id, []string{"entry"})
	require.ErrorIs(t, err, context.Canceled)
	_, err = store.AnalysisReachableNodesContext(t.Context(), id+1, []string{"entry"})
	require.ErrorIs(t, err, graph.ErrAnalysisGenerationInactive)
	_, err = store.AnalysisReachableNodesContext(t.Context(), id, make([]string, 1001))
	require.Error(t, err)
	rows, err = store.AnalysisReachableNodesContext(t.Context(), id, []string{"step"})
	require.NoError(t, err)
	require.Equal(t, []string{"step"}, rows, "errors must leave the store usable")
}
