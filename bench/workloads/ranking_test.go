package workloads

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/testenv"
)

// Retain every other optional Store capability so this control changes only
// ranking adjacency hydration, not query planning or bundle/search features.
type fullRankingStore struct{ *store_sqlite.Store }

func (s fullRankingStore) GetRerankOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return s.GetOutEdgesByNodeIDs(ids)
}

func (s fullRankingStore) GetRerankInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return s.GetInEdgesByNodeIDs(ids)
}

func rankingEngines(tb testing.TB, f fixture) (full, projected *query.Engine) {
	tb.Helper()
	store, idx := openIndexer(tb, newRegistry())
	tb.Cleanup(func() { require.NoError(tb, store.Close()) })
	result, err := idx.Index(f.root)
	require.NoError(tb, err)
	require.Empty(tb, result.Errors)
	require.Empty(tb, result.FailedFiles)
	full = query.NewEngine(fullRankingStore{store})
	projected = query.NewEngine(store)
	full.SetSearch(idx.Search())
	projected.SetSearch(idx.Search())
	return full, projected
}

func TestRankingProjectionKeepsSearchResults(t *testing.T) {
	testenv.Sandbox(t)
	f := writeFixture(t, 4)
	full, projected := rankingEngines(t, f)
	for _, name := range append(f.names, "WorkloadFile", "DefinitelyAbsentWorkloadSymbol") {
		require.Equal(t, full.SearchSymbols(name, 20), projected.SearchSymbols(name, 20), name)
	}
}

// BenchmarkRankingProjection retains a full-hydration control beside the
// optimized path, in one process and over one indexed fixture. It complements
// before/after runs, whose timings can drift with machine load or temperature.
func BenchmarkRankingProjection(b *testing.B) {
	benchmarkEnvironment(b)
	f := writeFixture(b, 128)
	full, projected := rankingEngines(b, f)
	for _, variant := range []struct {
		name   string
		engine *query.Engine
	}{
		{"full", full},
		{"projected", projected},
	} {
		b.Run(variant.name, func(b *testing.B) {
			require.True(b, hasExactHit(variant.engine, f.names[0]))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !hasExactHit(variant.engine, f.names[i%len(f.names)]) {
					b.Fatal("search lost an indexed symbol")
				}
			}
		})
	}
}
