package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type receiptOracleStore struct {
	*store_sqlite.Store
	requested [][]string
}

func (s *receiptOracleStore) AnalysisMembershipsContext(ctx context.Context, generationID int64, ids []string) ([]graph.AnalysisMembership, error) {
	s.requested = append(s.requested, append([]string(nil), ids...))
	return s.Store.AnalysisMembershipsContext(ctx, generationID, ids)
}

func TestReviewReceiptAvailableLazyMatchesFullViewOracle(t *testing.T) {
	s, _, _ := busyAnalysisFixture(t)
	store := s.graph.(*store_sqlite.Store)
	// Restore the small fixture's algorithm blobs only to prepare a controlled
	// valid generation. The reader under test starts after all views are dropped.
	require.True(t, s.ensureNodeMetricsMaterialized())
	require.True(t, s.ensureAdjacencyMaterialized())
	require.True(t, s.ensureLeidenMaterialized())
	require.True(t, s.ensureAutoConceptsMaterialized())
	communities := &analysis.CommunityResult{
		Communities: []analysis.Community{
			{ID: "changed-community", Members: []string{"Alpha", "Beta", "Gamma"}, Size: 3},
			{ID: "caller-community", Members: []string{"Caller"}, Size: 1},
		},
		NodeToComm: map[string]string{"Alpha": "changed-community", "Beta": "changed-community", "Gamma": "changed-community", "Caller": "caller-community"},
	}
	processes := &analysis.ProcessResult{
		Processes: []analysis.Process{
			{ID: "changed-process", EntryPoint: "Alpha", Steps: []analysis.Step{{ID: "Alpha"}, {ID: "Beta"}}, StepCount: 2},
			{ID: "caller-process", EntryPoint: "Caller", Steps: []analysis.Step{{ID: "Caller"}, {ID: "Gamma"}}, StepCount: 2},
		},
		NodeToProcs: map[string][]string{"Alpha": {"changed-process"}, "Beta": {"changed-process"}, "Caller": {"caller-process"}, "Gamma": {"caller-process"}},
	}
	header, accepted, err := persistAnalysisGeneration(store, store.AnalysisMutationRevision(), persistedAnalysis{
		communities: communities, processes: processes, pageRank: s.pageRank,
		adjacency: s.adjacency, leiden: s.leidenCache, autoConcepts: s.autoConcepts, hits: s.hits,
	})
	require.NoError(t, err)
	require.True(t, accepted)
	s.analysisGeneration = header
	s.communities, s.processes = communities, processes

	ids := []string{"Alpha"}
	fullImpact := analysis.AnalyzeImpact(store, ids, communities, processes)
	assert.ElementsMatch(t, []string{"changed-community", "caller-community"}, fullImpact.AffectedCommunities)
	assert.ElementsMatch(t, []string{"changed-process", "caller-process"}, fullImpact.AffectedProcesses)
	wantRisk := analysis.ScorePRRisk(store, analysis.PRRiskInput{SymbolIDs: ids, NodeToComm: communities.NodeToComm, Communities: communities, Processes: processes})
	// PR risk's community axis counts changed symbols' communities. Impact's
	// affected-community labels include callers, but do not feed this score.
	require.Equal(t, 1, wantRisk.CommunitySpan)
	wantReceipt := analysis.BuildReviewReceipt(wantRisk, "NONE", false, true)
	require.Equal(t, wantReceipt, s.reviewReceipt(t.Context(), ids, nil, nil, nil, true))

	require.True(t, s.releaseTransientAnalysisIfIdle())
	queryStore := &receiptOracleStore{Store: store}
	s.graph, s.indexer = queryStore, nil
	lazyCommunities, lazyProcesses, err := s.analysisMembershipsForNodes(t.Context(), ids)
	require.NoError(t, err)
	require.NotNil(t, lazyCommunities)
	require.NotNil(t, lazyProcesses)
	assert.Empty(t, lazyCommunities.Communities, "the oracle must exercise maps without summary objects")
	assert.Empty(t, lazyProcesses.Processes)
	assert.NotContains(t, lazyCommunities.NodeToComm, "Caller")
	assert.NotContains(t, lazyProcesses.NodeToProcs, "Caller")
	gotRisk := analysis.ScorePRRisk(queryStore, analysis.PRRiskInput{SymbolIDs: ids, NodeToComm: lazyCommunities.NodeToComm, Communities: lazyCommunities, Processes: lazyProcesses})
	assert.Equal(t, wantRisk, gotRisk, "score, counts, factor values and reasons must all match the full-view oracle")
	gotReceipt := s.reviewReceipt(t.Context(), ids, nil, nil, nil, true)
	assert.Equal(t, wantReceipt, gotReceipt)
	assert.Empty(t, gotReceipt.AnalysisStatus)
	assert.Nil(t, gotReceipt.Complete)
	for _, requested := range queryStore.requested {
		assert.Equal(t, ids, requested, "the receipt only requires changed-node memberships")
	}
	require.NotEmpty(t, queryStore.requested)
	assertNoMaterializedAnalysis(t, s)
}
