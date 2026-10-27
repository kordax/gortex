package tstypes

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// loadAdjacency retains only the edge kinds an apply phase reads. Data-flow and
// reference edges dominate a repository's adjacency, so keeping them made the
// preload hold most of the graph for the whole pass.
func TestLoadAdjacencyRetainsOnlyApplicableKindsInStoreOrder(t *testing.T) {
	g := graph.New()
	for _, id := range []string{"a.go::T", "a.go::T.M", "a.go::F", "a.go::Base", "a.go::p", "a.go::v"} {
		g.AddNode(&graph.Node{ID: id, Name: id, Kind: graph.KindFunction, FilePath: "a.go", Language: "go"})
	}
	out := []*graph.Edge{
		{From: "a.go::T.M", To: "a.go::F", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 3},
		{From: "a.go::T.M", To: "a.go::v", Kind: graph.EdgeReads, FilePath: "a.go", Line: 4},
		{From: "a.go::T.M", To: "a.go::T", Kind: graph.EdgeMemberOf, FilePath: "a.go", Line: 2},
		{From: "a.go::T.M", To: "a.go::F", Kind: graph.EdgeReferences, FilePath: "a.go", Line: 5},
		{From: "a.go::T.M", To: "a.go::Base", Kind: graph.EdgeExtends, FilePath: "a.go", Line: 1},
		{From: "a.go::T.M", To: "a.go::v", Kind: graph.EdgeValueFlow, FilePath: "a.go", Line: 6},
	}
	for _, e := range out {
		g.AddEdge(e)
	}
	g.AddEdge(&graph.Edge{From: "a.go::p", To: "a.go::T.M", Kind: graph.EdgeParamOf, FilePath: "a.go", Line: 2})
	g.AddEdge(&graph.Edge{From: "a.go::F", To: "a.go::T.M", Kind: graph.EdgeArgOf, FilePath: "a.go", Line: 7})

	a := newApplier(g, GoSpec(), "go-ast-types")
	a.loadAdjacency([]string{"a.go::T.M"})

	var gotOut []graph.EdgeKind
	for _, e := range a.outEdges("a.go::T.M") {
		gotOut = append(gotOut, e.Kind)
	}
	wantOut := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeMemberOf, graph.EdgeExtends}
	if len(gotOut) != len(wantOut) {
		t.Fatalf("out kinds = %v, want %v", gotOut, wantOut)
	}
	for i := range wantOut {
		if gotOut[i] != wantOut[i] {
			t.Fatalf("out kinds = %v, want %v in store order", gotOut, wantOut)
		}
	}
	in := a.inEdges("a.go::T.M")
	if len(in) != 1 || in[0].Kind != graph.EdgeParamOf {
		t.Fatalf("in edges = %v, want only the param_of edge", in)
	}
	if !a.outLoaded["a.go::T.M"] || !a.inLoaded["a.go::T.M"] {
		t.Fatal("a filtered load must still mark the node loaded")
	}
}
