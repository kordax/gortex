package mcp

import (
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Hides optional capabilities to retain the original full-reader control.
type contractParamFullStore struct{ graph.Store }

type contractParamAggregateStore struct{ graph.Store }

func (s contractParamAggregateStore) NodeFanCounts(ids []string, in, out []graph.EdgeKind) []graph.NodeFanRow {
	return s.Store.(graph.NodeFanAggregator).NodeFanCounts(ids, in, out)
}

func (contractParamAggregateStore) GetInEdges(string) []*graph.Edge {
	panic("parameter count must not hydrate incoming callers when the aggregate has a row")
}

func (contractParamAggregateStore) GetOutEdges(string) []*graph.Edge {
	panic("fan counts must not hydrate outgoing edges when the aggregate has a row")
}

func TestContractFanCountAggregateParity(t *testing.T) {
	base, err := store_sqlite.Open(filepath.Join(t.TempDir(), "fans.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, base.Close()) })
	for _, store := range []graph.Store{graph.New(), base, base.AtGeneration(1)} {
		seedContractParams(store, 6, 100)
		empty := &graph.Node{ID: "empty", Kind: graph.KindFunction}
		caller := &graph.Node{ID: "external-caller", Kind: graph.KindFunction}
		store.AddBatch([]*graph.Node{empty, caller}, []*graph.Edge{
			{From: caller.ID, To: "r/f.go::Target", Kind: graph.EdgeReferences},
			{From: "r/f.go::Target", To: caller.ID, Kind: graph.EdgeCalls},
			{From: "r/f.go::Target", To: "r/f.go::Target", Kind: graph.EdgeCalls},
			{From: "r/f.go::Target", To: empty.ID, Kind: graph.EdgeReferences},
		})
		nodes := []*graph.Node{{ID: "r/f.go::Target"}, {ID: "r/f.go::Target"}, empty, caller}
		wantIn, wantOut := computeFanInOut(contractParamFullStore{store}, nodes)
		gotIn, gotOut := computeFanInOut(contractParamAggregateStore{store}, nodes)
		assert.Equal(t, wantIn, gotIn)
		assert.Equal(t, wantOut, gotOut)
		assert.NotContains(t, gotOut, empty.ID, "zero-degree entries stay absent")
		// No aggregate row exists for this ID. Counts must still match
		// the old adjacency reader, including duplicate candidate entries.
		store.AddBatch(nil, []*graph.Edge{{From: caller.ID, To: "orphan", Kind: graph.EdgeCalls}})
		orphan := []*graph.Node{{ID: "orphan"}, {ID: "orphan"}}
		wantIn, wantOut = computeFanInOut(contractParamFullStore{store}, orphan)
		gotIn, gotOut = computeFanInOut(store, orphan)
		assert.Equal(t, wantIn, gotIn)
		assert.Equal(t, wantOut, gotOut)
	}
}

func seedContractParams(s graph.Store, params, callers int) {
	nodes := []*graph.Node{{ID: "r/f.go::Target", Name: "Target", Kind: graph.KindFunction}}
	var edges []*graph.Edge
	for i := 0; i < params; i++ {
		id := fmt.Sprintf("r/f.go::Target#param:p%d", i)
		nodes = append(nodes, &graph.Node{ID: id, Name: fmt.Sprintf("p%d", i), Kind: graph.KindParam})
		edges = append(edges, &graph.Edge{From: id, To: nodes[0].ID, Kind: graph.EdgeParamOf})
	}
	for i := 0; i < callers; i++ {
		id := fmt.Sprintf("r/c.go::Caller%d", i)
		nodes = append(nodes, &graph.Node{ID: id, Name: fmt.Sprintf("Caller%d", i), Kind: graph.KindFunction})
		edges = append(edges, &graph.Edge{From: id, To: nodes[0].ID, Kind: graph.EdgeCalls, Meta: map[string]any{"callee_name": "Target", "language": "go"}})
	}
	s.AddBatch(nodes, edges)
}

func TestContractParamCountUsesScopedAggregate(t *testing.T) {
	for _, params := range []int{0, 6} {
		t.Run(fmt.Sprintf("params=%d", params), func(t *testing.T) {
			g := graph.New()
			seedContractParams(g, params, 100)
			fast := &Server{graph: contractParamAggregateStore{g}}
			full := &Server{graph: contractParamFullStore{g}}
			assert.Equal(t, params, fast.paramCount(context.Background(), "r/f.go::Target"))
			assert.Equal(t, full.paramCount(context.Background(), "r/f.go::Target"), fast.paramCount(context.Background(), "r/f.go::Target"))
		})
	}
	assert.Zero(t, (&Server{}).paramCount(context.Background(), "missing"))
}

func TestContractParamCountSQLiteGenerationAndOrphanParity(t *testing.T) {
	base, err := store_sqlite.Open(filepath.Join(t.TempDir(), "params.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, base.Close()) })
	derived := base.AtGeneration(1)
	require.NotNil(t, derived)
	seedContractParams(base, 2, 30)
	seedContractParams(derived, 6, 60)
	for _, test := range []struct {
		store *store_sqlite.Store
		want  int
	}{{base, 2}, {derived, 6}} {
		fast := &Server{graph: contractParamAggregateStore{test.store}}
		full := &Server{graph: contractParamFullStore{test.store}}
		assert.Equal(t, test.want, fast.paramCount(context.Background(), "r/f.go::Target"))
		assert.Equal(t, full.paramCount(context.Background(), "r/f.go::Target"), fast.paramCount(context.Background(), "r/f.go::Target"))
	}
	base.AddBatch([]*graph.Node{{ID: "orphan-param", Kind: graph.KindParam}}, []*graph.Edge{{From: "orphan-param", To: "missing-owner", Kind: graph.EdgeParamOf}})
	// The SQL aggregate has no row for a missing owner. Preserve the legacy
	// full reader's count instead of silently changing orphan behavior.
	assert.Equal(t, 1, (&Server{graph: base}).paramCount(context.Background(), "missing-owner"))
}

func BenchmarkContractParamCountHighFanIn(b *testing.B) {
	previous := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(previous) })
	store, err := store_sqlite.Open(filepath.Join(b.TempDir(), "params.sqlite"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })
	seedContractParams(store, 6, 20000)
	for _, variant := range []struct {
		name  string
		store graph.Store
	}{{"full", contractParamFullStore{store}}, {"aggregate", store}} {
		b.Run(variant.name, func(b *testing.B) {
			server := &Server{graph: variant.store}
			b.ReportAllocs()
			for b.Loop() {
				if server.paramCount(context.Background(), "r/f.go::Target") != 6 {
					b.Fatal("parameter count changed")
				}
			}
		})
	}
}
