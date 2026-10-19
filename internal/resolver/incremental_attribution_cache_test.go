package resolver

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type attributionFileReadRecorder struct {
	graph.Store
	fullBatches [][]string
	pointReads  []string
}

func (s *attributionFileReadRecorder) GetFileNodes(path string) []*graph.Node {
	s.pointReads = append(s.pointReads, path)
	return s.Store.GetFileNodes(path)
}

func (s *attributionFileReadRecorder) GetFileNodesByPaths(paths []string) map[string][]*graph.Node {
	s.fullBatches = append(s.fullBatches, append([]string(nil), paths...))
	return s.Store.GetFileNodesByPaths(paths)
}

type attributionCallableReadRecorder struct {
	*attributionFileReadRecorder
	projectedPaths [][]string
	projectedKinds [][]graph.NodeKind
}

func (s *attributionCallableReadRecorder) NodesInFilesByKind(paths []string, kinds []graph.NodeKind) []*graph.Node {
	s.projectedPaths = append(s.projectedPaths, append([]string(nil), paths...))
	s.projectedKinds = append(s.projectedKinds, append([]graph.NodeKind(nil), kinds...))
	return s.Store.(graph.NodesInFilesByKindFinder).NodesInFilesByKind(paths, kinds)
}

func attributionCacheStore(t *testing.T, backend string) graph.Store {
	t.Helper()
	if backend == "memory" {
		return graph.New()
	}
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "attribution.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func attributionCacheFrontier(store graph.Store, paths ...string) incrementalFileFrontier {
	return incrementalFileFrontier{paths: paths, nodesByFile: store.GetFileNodesByPaths(paths)}
}

func addAttributionCacheFiles(store graph.Store, paths ...string) {
	for _, path := range paths {
		store.AddNode(&graph.Node{ID: path, Kind: graph.KindFile, FilePath: path, Language: "go"})
	}
}

func TestIncrementalAttributionCachePreservesEmptyFrontierBucket(t *testing.T) {
	store := &attributionFileReadRecorder{Store: graph.New()}
	r := New(store)
	const removed = "pkg/removed.go"
	r.prepareIncrementalAttributionCache(incrementalFileFrontier{
		paths: []string{removed}, nodesByFile: map[string][]*graph.Node{},
	})
	assert.Empty(t, r.incrementalFileNodes(removed))
	assert.Empty(t, r.incrementalFileNodes(removed))
	assert.Empty(t, store.pointReads, "a deleted file is a cached empty bucket, not a cache miss")
	assert.Empty(t, store.fullBatches)
	r.clearIncrementalAttributionCache()
}

func TestIncrementalAttributionCacheSkipsUnneededSiblingReads(t *testing.T) {
	const file = "pkg/changed.go"
	const sibling = "pkg/sibling.go"
	cases := []struct {
		name       string
		kind       graph.EdgeKind
		target     string
		sameFile   int
		methodSite bool
		want       string
	}{
		{name: "already resolved", kind: graph.EdgeArgOf, target: sibling + "::Target", want: sibling + "::Target"},
		{name: "non dataflow", kind: graph.EdgeReferences, target: "unresolved::Target", want: "unresolved::Target"},
		{name: "qualified", kind: graph.EdgeArgOf, target: "unresolved::pkg.Target", want: "unresolved::pkg.Target"},
		{name: "binding identity", kind: graph.EdgeValueFlow, target: "unresolved::Target#binding", want: "unresolved::Target#binding"},
		{name: "empty name", kind: graph.EdgeArgOf, target: "unresolved::", want: "unresolved::"},
		{name: "invalid method", kind: graph.EdgeValueFlow, target: "unresolved::*.Run.More", want: "unresolved::*.Run.More"},
		{name: "same file wins", kind: graph.EdgeArgOf, target: "unresolved::Target", sameFile: 1, want: file + "::Target0"},
		{name: "same file ambiguity blocks sibling", kind: graph.EdgeValueFlow, target: "unresolved::Target", sameFile: 2, want: "unresolved::Target"},
		{name: "method call site", kind: graph.EdgeValueFlow, target: "unresolved::*.Run", methodSite: true, want: sibling + "::Worker.Run"},
	}
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					store := attributionCacheStore(t, backend)
					legacyStore := attributionCacheStore(t, backend)
					for _, dst := range []graph.Store{store, legacyStore} {
						addAttributionCacheFiles(dst, file, sibling)
						dst.AddBatch([]*graph.Node{
							{ID: file + "::Owner", Name: "Owner", Kind: graph.KindFunction, FilePath: file},
							{ID: sibling + "::Target", Name: "Target", Kind: graph.KindFunction, FilePath: sibling},
							{ID: sibling + "::Worker.Run", Name: "Run", Kind: graph.KindMethod, FilePath: sibling},
						}, []*graph.Edge{{From: file + "::Owner", To: tc.target, Kind: tc.kind, FilePath: file, Line: 12}})
						if tc.sameFile > 0 {
							dst.AddNode(&graph.Node{ID: file + "::Target0", Name: "Target", Kind: graph.KindFunction, FilePath: file})
						}
						if tc.sameFile > 1 {
							dst.AddNode(&graph.Node{ID: file + "::Target1", Name: "Target", Kind: graph.KindMethod, FilePath: file})
						}
						if tc.methodSite {
							dst.AddEdge(&graph.Edge{From: file + "::Owner", To: sibling + "::Worker.Run", Kind: graph.EdgeCalls, FilePath: file, Line: 12})
						}
					}
					recorded := &attributionCallableReadRecorder{attributionFileReadRecorder: &attributionFileReadRecorder{Store: store}}
					r := New(recorded)
					r.buildDirIndexes()
					r.prepareIncrementalAttributionCache(attributionCacheFrontier(store, file))
					assert.Empty(t, recorded.fullBatches, "preparing a frontier must not eagerly read siblings")
					r.bindDataflowCalleeRefsForFile(file)
					r.clearIncrementalAttributionCache()
					assert.Empty(t, recorded.projectedPaths, "these candidates need no package function lookup")
					assert.Empty(t, recorded.fullBatches)
					assert.Empty(t, recorded.pointReads)
					New(legacyStore).bindDataflowCalleeRefs()
					for _, dst := range []graph.Store{store, legacyStore} {
						var matches []*graph.Edge
						for _, edge := range dst.GetOutEdges(file + "::Owner") {
							if edge.Kind == tc.kind {
								matches = append(matches, edge)
							}
						}
						require.Len(t, matches, 1)
						assert.Equal(t, tc.want, matches[0].To)
					}
				})
			}
		})
	}
}

func TestIncrementalAttributionCacheSharesPackageLookupAndReleasesIt(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, projected := range []bool{false, true} {
			name := backend + "/fallback"
			if projected {
				name = backend + "/projection"
			}
			t.Run(name, func(t *testing.T) {
				store := attributionCacheStore(t, backend)
				const first, second, sibling, empty = "pkg/first.go", "pkg/second.go", "pkg/sibling.go", "pkg/empty.go"
				addAttributionCacheFiles(store, first, second, sibling, empty)
				store.AddNode(&graph.Node{ID: sibling + "::Target", Kind: graph.KindFunction, Name: "Target", FilePath: sibling})
				for _, file := range []string{first, second} {
					store.AddBatch([]*graph.Node{{ID: file + "::Owner", Kind: graph.KindFunction, Name: "Owner", FilePath: file}}, []*graph.Edge{
						{From: file + "::Owner", To: "unresolved::Target", Kind: graph.EdgeArgOf, FilePath: file, Line: 10},
						{From: file + "::Owner", To: "unresolved::Target", Kind: graph.EdgeValueFlow, FilePath: file, Line: 20},
					})
				}
				base := &attributionFileReadRecorder{Store: store}
				projection := &attributionCallableReadRecorder{attributionFileReadRecorder: base}
				var observed graph.Store = base
				if projected {
					observed = projection
				}
				r := New(observed)
				r.buildDirIndexes()
				frontier := attributionCacheFrontier(store, first, second)
				r.prepareIncrementalAttributionCache(frontier)
				assert.Empty(t, base.fullBatches)
				for _, file := range frontier.paths {
					r.bindDataflowCalleeRefsForFile(file)
				}
				r.clearIncrementalAttributionCache()
				if projected {
					require.Len(t, projection.projectedPaths, 1, "both changed files share one directory lookup")
					assert.ElementsMatch(t, []string{first, second, sibling, empty}, projection.projectedPaths[0])
					assert.Equal(t, [][]graph.NodeKind{{graph.KindFunction}}, projection.projectedKinds)
					assert.Empty(t, base.fullBatches)
				} else {
					require.Len(t, base.fullBatches, 1)
					assert.ElementsMatch(t, []string{sibling, empty}, base.fullBatches[0])
				}
				assert.Empty(t, base.pointReads, "empty files must not trigger repeated point reads")
				for _, file := range frontier.paths {
					edges := store.GetOutEdges(file + "::Owner")
					require.Len(t, edges, 2)
					for _, edge := range edges {
						assert.Equal(t, sibling+"::Target", edge.To)
					}
				}

				// A second package definition makes the name ambiguous. A new pass
				// must observe it even though the previous lookup saw an empty file.
				store.AddNode(&graph.Node{ID: empty + "::Target", Kind: graph.KindFunction, Name: "Target", FilePath: empty})
				store.AddBatch([]*graph.Node{{ID: first + "::Next", Kind: graph.KindFunction, Name: "Next", FilePath: first}}, []*graph.Edge{
					{From: first + "::Next", To: "unresolved::Target", Kind: graph.EdgeArgOf, FilePath: first, Line: 30},
				})
				r.prepareIncrementalAttributionCache(attributionCacheFrontier(store, first, second))
				r.bindDataflowCalleeRefsForFile(first)
				r.clearIncrementalAttributionCache()
				edges := store.GetOutEdges(first + "::Next")
				require.Len(t, edges, 1)
				assert.Equal(t, "unresolved::Target", edges[0].To)
				if projected {
					assert.Len(t, projection.projectedPaths, 2, "clearing must discard the package index")
				} else {
					assert.Len(t, base.fullBatches, 2)
				}
			})
		}
	}
}
