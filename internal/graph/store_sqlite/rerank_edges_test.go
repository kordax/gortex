package store_sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

// Embedding only Reader deliberately hides the optional projection, exercising
// the full-edge fallback against the very same database and candidate set.
type fullRerankReader struct{ graph.Reader }

func openRerankStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "ranking.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func rankingEdgeProjection(e *graph.Edge) *graph.Edge {
	out := &graph.Edge{From: e.From, To: e.To, Kind: e.Kind, Confidence: e.Confidence, FilePath: e.FilePath, Line: e.Line}
	if e.Kind == graph.EdgeSimilarTo {
		out.Meta = e.Meta
	}
	return out
}

func TestRerankEdgesProjectionChunksAndMultiplicity(t *testing.T) {
	s := openRerankStore(t)
	var ids []string
	var nodes []*graph.Node
	var edges []*graph.Edge
	for i := 0; i < lookupChunkSize+2; i++ {
		id := fmt.Sprintf("repo/f.go::N%d", i)
		ids = append(ids, id)
		nodes = append(nodes, &graph.Node{ID: id, Name: fmt.Sprintf("N%d", i), Kind: graph.KindFunction, FilePath: "repo/f.go"})
		if i > 0 {
			edges = append(edges, &graph.Edge{From: id, To: ids[0], Kind: graph.EdgeCalls, FilePath: "repo/f.go", Line: i, Confidence: 0.4, Meta: map[string]any{"payload": "not used by ranking"}})
		}
	}
	edges = append(edges,
		&graph.Edge{From: ids[1], To: ids[0], Kind: graph.EdgeSimilarTo, Confidence: 0.1, Meta: map[string]any{"similarity": 0.9}},
		&graph.Edge{From: ids[0], To: ids[2], Kind: graph.EdgeSimilarTo, Confidence: 0.6},
		&graph.Edge{From: ids[0], To: ids[0], Kind: graph.EdgeCalls, Confidence: 0.8},
	)
	s.AddBatch(nodes, edges)
	ids = append(ids, "", ids[0], "missing")
	for _, direction := range []struct {
		name string
		full func([]string) map[string][]*graph.Edge
		fast func([]string) map[string][]*graph.Edge
	}{
		{"out", s.GetOutEdgesByNodeIDs, s.GetRerankOutEdgesByNodeIDs},
		{"in", s.GetInEdgesByNodeIDs, s.GetRerankInEdgesByNodeIDs},
	} {
		t.Run(direction.name, func(t *testing.T) {
			full, fast := direction.full(ids), direction.fast(ids)
			for _, id := range ids {
				if id == "" {
					continue
				}
				var want []*graph.Edge
				for _, e := range full[id] {
					want = append(want, rankingEdgeProjection(e))
				}
				assert.Equal(t, want, fast[id], id)
				assert.Contains(t, fast, id)
			}
			assert.NotContains(t, fast, "")
			assert.Nil(t, direction.fast(nil))
			assert.Nil(t, direction.fast([]string{"", ""}))
		})
	}
}

func TestRerankEdgesKeepScoresAndOrdering(t *testing.T) {
	s := openRerankStore(t)
	nodes := []*graph.Node{
		{ID: "r/a.go::Alpha", Name: "Alpha", Kind: graph.KindFunction, FilePath: "r/a.go"},
		{ID: "r/b.go::Beta", Name: "Beta", Kind: graph.KindFunction, FilePath: "r/b.go"},
		{ID: "r/c.go::Gamma", Name: "Gamma", Kind: graph.KindFunction, FilePath: "r/c.go"},
		{ID: "r/empty.go::Empty", Name: "Empty", Kind: graph.KindFunction, FilePath: "r/empty.go"},
	}
	s.AddBatch(nodes, []*graph.Edge{
		{From: nodes[0].ID, To: nodes[1].ID, Kind: graph.EdgeCalls, Meta: map[string]any{"payload": "ignored"}},
		{From: nodes[0].ID, To: nodes[1].ID, Kind: graph.EdgeSimilarTo, Confidence: 0.05, Meta: map[string]any{"similarity": 0.95}},
		{From: nodes[2].ID, To: nodes[0].ID, Kind: graph.EdgeSimilarTo, Confidence: 0.65},
		{From: nodes[1].ID, To: nodes[2].ID, Kind: graph.EdgeSimilarTo, Confidence: 0.8, Meta: map[string]any{"similarity": "legacy non-numeric"}},
		{From: nodes[1].ID, To: nodes[1].ID, Kind: graph.EdgeCalls},
	})
	for _, seeded := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial_seed=%t", seeded), func(t *testing.T) {
			run := func(reader graph.Reader) ([]*rerank.Candidate, *rerank.Context) {
				ctx := &rerank.Context{Graph: reader, Now: 1700000000}
				if seeded {
					ids := []string{nodes[0].ID}
					ctx.SeedEdgeCaches(s.GetInEdgesByNodeIDs(ids), s.GetOutEdgesByNodeIDs(ids), true)
				}
				var candidates []*rerank.Candidate
				for i, n := range nodes {
					candidates = append(candidates, &rerank.Candidate{Node: n, TextRank: i, VectorRank: -1})
				}
				return rerank.NewDefault().Rerank("Alpha", candidates, ctx), ctx
			}
			want, _ := run(fullRerankReader{s})
			got, ctx := run(s)
			assert.Equal(t, want, got, "every signal, score and result position must match full reads")
			assert.Equal(t, 1.0, ctx.EdgeCacheHitRate([]string{nodes[3].ID}), "empty adjacency is cached on both sides")
		})
	}
}

func TestRerankEdgesQueryPlansUseIndexedSearch(t *testing.T) {
	s := openRerankStore(t)
	for _, col := range []string{"from_id", "to_id"} {
		for _, generation := range []int64{0, 1} {
			t.Run(fmt.Sprintf("%s/gen=%d", col, generation), func(t *testing.T) {
				rows, err := s.db.Query("EXPLAIN QUERY PLAN "+rerankEdgesQuery(col, 2), string(graph.EdgeSimilarTo), "a", "b", generation)
				require.NoError(t, err)
				defer rows.Close()
				var plan []string
				for rows.Next() {
					var id, parent, unused int
					var detail string
					require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
					plan = append(plan, strings.ToUpper(detail))
				}
				require.NoError(t, rows.Err())
				joined := strings.Join(plan, "\n")
				assert.Contains(t, joined, "SEARCH EDGES", "plan must locate requested endpoints with an index")
				assert.NotContains(t, joined, "SCAN EDGES", "plan must not scan the edge corpus")
			})
		}
	}
}

func TestRerankEdgesFilterLegacyStructuralViolations(t *testing.T) {
	s := openRerankStore(t)
	from, to := "r/f.go::Method", "r/f.go::Type"
	s.AddBatch([]*graph.Node{
		{ID: from, Name: "Method", Kind: graph.KindMethod},
		{ID: to, Name: "Type", Kind: graph.KindType},
	}, []*graph.Edge{{From: from, To: to, Kind: graph.EdgeMemberOf}})
	invalid := to + "#param:x"
	_, err := s.writerDB.Exec(`UPDATE edges SET to_id = ? WHERE from_id = ? AND view_gen = ?`, invalid, from, s.viewGen)
	require.NoError(t, err)
	assert.Empty(t, s.GetRerankOutEdgesByNodeIDs([]string{from})[from])
	assert.Empty(t, s.GetRerankInEdgesByNodeIDs([]string{invalid})[invalid])
}
