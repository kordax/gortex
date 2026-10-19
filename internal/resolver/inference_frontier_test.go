package resolver

import (
	"fmt"
	"iter"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type inferenceFrontierStore struct {
	*graph.Graph
	inIDs, outIDs, nodeIDs int
	maxBatch               int
}

func (s *inferenceFrontierStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.inIDs += len(ids)
	s.maxBatch = max(s.maxBatch, len(ids))
	return s.Graph.GetInEdgesByNodeIDs(ids)
}

func (s *inferenceFrontierStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.outIDs += len(ids)
	s.maxBatch = max(s.maxBatch, len(ids))
	return s.Graph.GetOutEdgesByNodeIDs(ids)
}

func (s *inferenceFrontierStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.nodeIDs += len(ids)
	s.maxBatch = max(s.maxBatch, len(ids))
	return s.Graph.GetNodesByIDs(ids)
}

func (s *inferenceFrontierStore) EdgesInScopeSeq(_, _ []string, _ ...graph.EdgeKind) iter.Seq[graph.ScopedEdgeRow] {
	panic("scoped inference must not scan a repository edge kind")
}

func populateInferenceFrontier(store graph.Store, ballast int) {
	nodes := []*graph.Node{
		{ID: "a::I", Kind: graph.KindInterface, RepoPrefix: "a", Meta: map[string]any{"methods": []string{"Run"}}},
		{ID: "a::I.Run", Kind: graph.KindMethod, Name: "Run", RepoPrefix: "a"},
		{ID: "a::Parent", Kind: graph.KindType, RepoPrefix: "a"},
		{ID: "a::Parent.Run", Kind: graph.KindMethod, Name: "Run", RepoPrefix: "a"},
		{ID: "a::Child", Kind: graph.KindType, RepoPrefix: "a", FilePath: "a/child.go", StartLine: 10},
		{ID: "a::Child.Run", Kind: graph.KindMethod, Name: "Run", RepoPrefix: "a", FilePath: "a/child.go", StartLine: 11},
		{ID: "b::Child", Kind: graph.KindType, RepoPrefix: "b"},
		{ID: "b::Child.Run", Kind: graph.KindMethod, Name: "Run", RepoPrefix: "b"},
	}
	edges := []*graph.Edge{
		{From: "a::I.Run", To: "a::I", Kind: graph.EdgeMemberOf},
		{From: "a::Parent.Run", To: "a::Parent", Kind: graph.EdgeMemberOf},
		{From: "a::Child.Run", To: "a::Child", Kind: graph.EdgeMemberOf},
		{From: "b::Child.Run", To: "b::Child", Kind: graph.EdgeMemberOf},
		{From: "a::Child", To: "a::Parent", Kind: graph.EdgeExtends, Origin: graph.OriginASTResolved},
		{From: "b::Child", To: "a::Parent", Kind: graph.EdgeExtends, Origin: graph.OriginLSPDispatch},
	}
	for i := 0; i < ballast; i++ {
		owner := fmt.Sprintf("a::Ballast%06d", i)
		method := owner + ".Other"
		nodes = append(nodes, &graph.Node{ID: owner, Kind: graph.KindType, RepoPrefix: "a"}, &graph.Node{ID: method, Kind: graph.KindMethod, Name: "Other", RepoPrefix: "a"})
		edges = append(edges, &graph.Edge{From: method, To: owner, Kind: graph.EdgeMemberOf})
	}
	store.AddBatch(nodes, edges)
}

func inferenceEdgePairs(store graph.Store, kind graph.EdgeKind) map[string]string {
	pairs := make(map[string]string)
	for edge := range store.EdgesByKind(kind) {
		pairs[inferencePairKey(edge.From, edge.To)] = edge.Origin
	}
	return pairs
}

func TestInferenceFrontierImplementsParity(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			types  map[string]bool
			ifaces map[string]bool
		}{
			{"type", map[string]bool{"a::Child": true}, nil},
			{"interface", map[string]bool{}, map[string]bool{"a::I": true}},
			{"mixed", map[string]bool{"a::Child": true, "a::Parent": false}, map[string]bool{"a::I": true}},
			{"false-and-missing", map[string]bool{"a::Child": false, "deleted": true}, map[string]bool{"a::I": false}},
			{"nil-preserves-global", nil, map[string]bool{"a::I": false}},
		} {
			t.Run(fmt.Sprintf("disk=%t/%s", disk, tc.name), func(t *testing.T) {
				newStore := func() graph.Store {
					if !disk {
						return graph.New()
					}
					store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, store.Close()) })
					return store
				}
				full, scoped := newStore(), newStore()
				populateInferenceFrontier(full, 40)
				populateInferenceFrontier(scoped, 40)
				New(full).InferImplements()
				want := inferenceEdgePairs(full, graph.EdgeImplements)
				for edge := range full.EdgesByKind(graph.EdgeImplements) {
					if tc.types != nil && !tc.types[edge.From] && !tc.ifaces[edge.To] {
						delete(want, inferencePairKey(edge.From, edge.To))
					}
				}
				r := New(scoped)
				assert.Equal(t, len(want), r.InferImplementsScoped(tc.types, tc.ifaces))
				assert.Equal(t, want, inferenceEdgePairs(scoped, graph.EdgeImplements))
				assert.Zero(t, r.InferImplementsScoped(tc.types, tc.ifaces), "warm pass must not duplicate edges")
			})
		}
	}
}

func TestInferenceFrontierOverridesParity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope map[string]bool
		want  map[string]string
	}{
		{"child", map[string]bool{"a::Child": true}, map[string]string{inferencePairKey("a::Child.Run", "a::Parent.Run"): graph.OriginASTResolved}},
		{"cross-repo-parent", map[string]bool{"a::Parent": true}, map[string]string{inferencePairKey("a::Child.Run", "a::Parent.Run"): graph.OriginASTResolved, inferencePairKey("b::Child.Run", "a::Parent.Run"): graph.OriginLSPDispatch}},
		{"false-and-missing", map[string]bool{"a::Child": false, "deleted": true}, map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, disk := range []bool{false, true} {
				var store graph.Store = graph.New()
				if disk {
					s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, s.Close()) })
					store = s
				}
				populateInferenceFrontier(store, 40)
				r := New(store)
				assert.Equal(t, len(tc.want), r.InferOverridesScoped(tc.scope))
				assert.Equal(t, tc.want, inferenceEdgePairs(store, graph.EdgeOverrides))
				assert.Zero(t, r.InferOverridesScoped(tc.scope))
			}
		})
	}
}

func TestInferenceFrontierWorkIndependentOfUnrelatedMembers(t *testing.T) {
	var first [3]int
	for _, ballast := range []int{0, 4096} {
		g := graph.New()
		populateInferenceFrontier(g, ballast)
		store := &inferenceFrontierStore{Graph: g}
		r := New(store)
		assert.Equal(t, 1, r.InferImplementsScoped(map[string]bool{"a::Child": true}, nil))
		assert.Equal(t, 2, r.InferOverridesScoped(map[string]bool{"a::Child": true}))
		assert.LessOrEqual(t, store.maxBatch, inferenceMutationBatchSize)
		work := [3]int{store.inIDs, store.outIDs, store.nodeIDs}
		if ballast == 0 {
			first = work
		} else {
			assert.Equal(t, first, work, "unrelated repository members must not enter the frontier")
		}
	}
}

func TestInferenceFrontierOverridesPromotesOrigin(t *testing.T) {
	g := graph.New()
	populateInferenceFrontier(g, 0)
	g.AddBatch(nil, []*graph.Edge{{From: "a::Child.Run", To: "a::Parent.Run", Kind: graph.EdgeOverrides, Origin: graph.OriginASTInferred}})
	r := New(g)
	assert.Zero(t, r.InferOverridesScoped(map[string]bool{"a::Child": true}))
	assert.Equal(t, graph.OriginASTResolved, inferenceEdgePairs(g, graph.EdgeOverrides)[inferencePairKey("a::Child.Run", "a::Parent.Run")])
	assert.Zero(t, r.InferOverridesScoped(map[string]bool{"a::Child": true}))
}

func TestInferenceFrontierTypeBatches(t *testing.T) {
	g := graph.New()
	populateInferenceFrontier(g, 0)
	const count = inferenceMutationBatchSize + 17
	scope := make(map[string]bool)
	var nodes []*graph.Node
	var edges []*graph.Edge
	for i := 0; i < count; i++ {
		owner := fmt.Sprintf("a::Runner%04d", i)
		scope[owner] = true
		nodes = append(nodes, &graph.Node{ID: owner, Kind: graph.KindType, RepoPrefix: "a"}, &graph.Node{ID: owner + ".Run", Kind: graph.KindMethod, Name: "Run", RepoPrefix: "a"})
		edges = append(edges, &graph.Edge{From: owner + ".Run", To: owner, Kind: graph.EdgeMemberOf})
	}
	g.AddBatch(nodes, edges)
	store := &inferenceFrontierStore{Graph: g}
	r := New(store)
	assert.Equal(t, count, r.InferImplementsScoped(scope, nil))
	assert.LessOrEqual(t, store.maxBatch, inferenceMutationBatchSize)
	assert.Zero(t, r.InferImplementsScoped(scope, nil))
}

func TestInferenceFrontierDuplicateMethodWinnerSQLite(t *testing.T) {
	var results []map[string]string
	for _, scoped := range []bool{false, true} {
		store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		populateInferenceFrontier(store, 0)
		store.AddBatch([]*graph.Node{{ID: "a::Parent.AnotherRun", Name: "Run", Kind: graph.KindMethod, RepoPrefix: "a"}}, []*graph.Edge{{From: "a::Parent.AnotherRun", To: "a::Parent", Kind: graph.EdgeMemberOf}})
		if scoped {
			New(store).InferOverridesScoped(map[string]bool{"a::Parent": true})
		} else {
			New(store).InferOverrides()
		}
		results = append(results, inferenceEdgePairs(store, graph.EdgeOverrides))
	}
	require.Len(t, results[0], 2)
	assert.Equal(t, results[0], results[1])
}

func BenchmarkScopedInferenceSQLitePaired(b *testing.B) {
	for _, ballast := range []int{64, 2048} {
		for _, ifaceChanged := range []bool{false, true} {
			for _, pass := range []string{"implements", "overrides"} {
				for _, reference := range []bool{true, false} {
					name := fmt.Sprintf("members=%d/interface=%t/%s/reference=%t", ballast, ifaceChanged, pass, reference)
					b.Run(name, func(b *testing.B) {
						store, err := store_sqlite.Open(filepath.Join(b.TempDir(), "graph.db"))
						require.NoError(b, err)
						b.Cleanup(func() { require.NoError(b, store.Close()) })
						populateInferenceFrontier(store, ballast)
						var nodes []*graph.Node
						var edges []*graph.Edge
						for i := 0; i < ballast; i++ {
							owner := fmt.Sprintf("z::Unrelated%06d", i)
							nodes = append(nodes, &graph.Node{ID: owner, Kind: graph.KindType, RepoPrefix: "z"}, &graph.Node{ID: owner + ".Other", Kind: graph.KindMethod, Name: "Other", RepoPrefix: "z"})
							edges = append(edges, &graph.Edge{From: owner + ".Other", To: owner, Kind: graph.EdgeMemberOf})
						}
						store.AddBatch(nodes, edges)
						r := New(store)
						r.InferImplements()
						r.InferOverrides()
						scope := map[string]bool{"a::Child": true}
						if ifaceChanged {
							scope["a::I"] = true
						}
						var run func() int
						if pass == "implements" {
							if reference {
								run = func() int { return r.inferImplementationsByRepoScan(scope, scope) }
							} else {
								run = func() int { return r.InferImplementsScoped(scope, scope) }
							}
						} else if reference {
							run = func() int { return r.inferOverridesByRepoScan(scope) }
						} else {
							run = func() int { return r.InferOverridesScoped(scope) }
						}
						require.Zero(b, run())
						b.ReportAllocs()
						b.ResetTimer()
						for b.Loop() {
							run()
						}
					})
				}
			}
		}
	}
}

func BenchmarkScopedInferenceSQLiteBallast(b *testing.B) {
	for _, ballast := range []int{0, 4096, 32768} {
		b.Run(fmt.Sprintf("members=%d", ballast), func(b *testing.B) {
			store, err := store_sqlite.Open(filepath.Join(b.TempDir(), "graph.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, store.Close()) })
			populateInferenceFrontier(store, ballast)
			r := New(store)
			scope := map[string]bool{"a::Child": true}
			r.InferImplementsScoped(scope, nil)
			r.InferOverridesScoped(scope)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				r.InferImplementsScoped(scope, nil)
				r.InferOverridesScoped(scope)
			}
		})
	}
}
