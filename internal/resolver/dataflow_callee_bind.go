package resolver

import (
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// bindDataflowCalleeRefs lifts the callee side of dataflow edges
// (arg_of / value_flow) from an `unresolved::` placeholder onto the real node
// the callee denotes.
//
// The main resolver's resolveEdge already binds `calls` edges to these
// callees, but it keys the candidate lookup off the edge's From node's repo —
// and a dataflow edge's From is an `unresolved::` argument placeholder with no
// node, so the lookup is scoped to the empty repo, matches nothing in a
// multi-repo graph, and the callee never lifts. materializeDataflowParams
// (which refines a resolved callee to its param node) then skips the edge
// because its target is still an `unresolved::` stub. The result was ~half of
// all arg_of edges left dangling placeholder→placeholder even when the callee
// was defined in the same file.
//
// This pass closes that gap cheaply and without touching the hot per-edge
// resolver, via three call-site-local / index lookups (O(1) per edge):
//
//   - bare `unresolved::<name>` callee → the sole SAME-FILE function/method of
//     that name, else the sole SAME-PACKAGE function (Go package function names
//     are unique, so a same-dir function match is unambiguous). Matched by
//     FilePath / dir equality, so no repo-prefix logic — identical behaviour in
//     bare and prefixed graphs.
//   - `unresolved::*.<method>` callee → the method the call resolver already
//     bound for a `calls` / `references` edge at the SAME call site (file+line),
//     reusing the receiver-type work resolveMethodCall already did there.
//
// Ordering (see runFileAttributionPassesLocked): after bindBareNameScopeRefs
// (a same-scope local/param wins over a same-file function of the same name)
// and before attributeGoBuiltins (a bare `append`/`len` argument with no
// definition falls through to builtin attribution) and before
// materializeDataflowParams (which then refines a resolved function/method
// callee to its param node).
func (r *Resolver) bindDataflowCalleeRefs() {
	idx := r.buildCalleeIndex()
	for _, k := range []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences} {
		for e := range graph.EdgesLightSeq(r.graph, k) {
			idx.indexCallSite(e)
		}
	}
	if len(idx.byFile) == 0 && len(idx.bySite) == 0 {
		return
	}
	r.reindexAttributionEdgesBatched(
		[]graph.EdgeKind{graph.EdgeArgOf, graph.EdgeValueFlow},
		func(edge *graph.Edge) string {
			return bindDataflowCalleeEdge(edge, idx)
		},
	)
}

func (r *Resolver) buildCalleeIndex() *calleeIndex {
	idx := newCalleeIndex()
	for node := range graph.CallableBindingNodesSeq(r.graph, graph.KindFunction, graph.KindMethod) {
		if node.Name == "" || node.FilePath == "" {
			continue
		}
		indexName(idx.byFile, node.FilePath, node.Name, node.ID)
		if node.Kind == graph.KindFunction {
			indexName(idx.byDir, filePathDir(node.FilePath), node.Name, node.ID)
		}
	}
	return idx
}

func (r *Resolver) bindDataflowCalleeRefsFromCensus(census *postBareAttributionCensus) {
	if census == nil || census.callee == nil {
		return
	}
	idx := census.callee
	if len(idx.byFile) == 0 && len(idx.bySite) == 0 {
		return
	}
	r.reindexAttributionIdentitiesBatched(
		census.finder,
		census.dataflowCandidates,
		func(edge *graph.Edge) string {
			return bindDataflowCalleeEdge(edge, idx)
		},
	)
}

// bindDataflowCalleeRefsForFile is the single-file scope of
// bindDataflowCalleeRefs, used on the incremental (fsnotify / edit_file)
// re-index path. It builds the same indexes restricted to the file's own
// nodes/edges (same-file), the package's functions via r.dirIndex (same-
// package — buildDirIndexes runs on the incremental resolve too, so the map is
// populated), and the file's own call sites — producing exactly the binds the
// whole-graph sweep would for the file, keeping incremental == full-index
// convergence, without scanning every function in the graph.
func (r *Resolver) bindDataflowCalleeRefsForFile(filePath string) {
	fileEdges := r.fileOutEdges(filePath)
	hasCandidates := false
	for _, edge := range fileEdges {
		if edge != nil && (edge.Kind == graph.EdgeArgOf || edge.Kind == graph.EdgeValueFlow) && dataflowCalleeTargetShape(edge.To) {
			hasCandidates = true
			break
		}
	}
	if !hasCandidates {
		return
	}
	idx := newCalleeIndex()
	for _, n := range r.incrementalFileNodes(filePath) {
		if n == nil || n.Name == "" || n.FilePath == "" {
			continue
		}
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			indexName(idx.byFile, n.FilePath, n.Name, n.ID)
		}
	}
	// Same-file candidates win, including ambiguity: an ambiguous local name
	// must never fall through to a unique sibling. Method targets use call-site
	// evidence, so neither case needs the package's function nodes.
	dir := filePathDir(filePath)
	for _, edge := range fileEdges {
		if edge == nil || (edge.Kind != graph.EdgeArgOf && edge.Kind != graph.EdgeValueFlow) || !dataflowCalleeTargetShape(edge.To) {
			continue
		}
		name := graph.UnresolvedName(edge.To)
		if !strings.HasPrefix(name, "*.") && filePathDir(edge.FilePath) == dir && len(idx.byFile[edge.FilePath][name]) == 0 {
			idx.byDir[dir] = r.incrementalPackageCallees(dir)
			break
		}
	}
	for _, e := range fileEdges {
		if e != nil && (e.Kind == graph.EdgeCalls || e.Kind == graph.EdgeReferences) {
			idx.indexCallSite(e)
		}
	}
	collector := newAttributionReindexCollector(r)
	for _, edge := range fileEdges {
		if edge == nil || (edge.Kind != graph.EdgeArgOf && edge.Kind != graph.EdgeValueFlow) {
			continue
		}
		collector.add(edge, bindDataflowCalleeEdge(edge, idx))
	}
	collector.flush()
}

// incrementalPackageCallees retains only function names and IDs for the active
// attribution pass. Stores without the compound finder retain one batched
// full-node read, rather than a query for every sibling file.
func (r *Resolver) incrementalPackageCallees(dir string) map[string][]string {
	if r.incrementalNodesByFile != nil {
		if cached, ok := r.incrementalCalleeByDir[dir]; ok {
			return cached
		}
	}
	fileNodes := r.dirIndex[dir]
	paths := make([]string, 0, len(fileNodes))
	for _, fileNode := range fileNodes {
		if fileNode.FilePath != "" {
			paths = append(paths, fileNode.FilePath)
		}
	}
	byFile := make(map[string][]*graph.Node, len(paths))
	if finder, ok := r.graph.(graph.NodesInFilesByKindFinder); ok && len(paths) > 0 {
		for _, node := range finder.NodesInFilesByKind(paths, []graph.NodeKind{graph.KindFunction}) {
			if node != nil {
				byFile[node.FilePath] = append(byFile[node.FilePath], node)
			}
		}
	} else {
		var missing []string
		for _, path := range paths {
			if nodes, cached := r.incrementalNodesByFile[path]; cached {
				byFile[path] = nodes
			} else {
				missing = append(missing, path)
			}
		}
		if len(missing) > 0 {
			fetched := r.graph.GetFileNodesByPaths(missing)
			for _, path := range missing {
				byFile[path] = fetched[path]
				if r.incrementalNodesByFile != nil {
					r.incrementalNodesByFile[path] = fetched[path]
				}
			}
		}
	}
	callees := make(map[string][]string)
	// Retain the directory frontier's multiplicity and the old ambiguity
	// rules, including legacy stores with multiple file nodes for one path.
	for _, fileNode := range fileNodes {
		for _, node := range byFile[fileNode.FilePath] {
			if node != nil && node.Kind == graph.KindFunction && node.Name != "" && node.FilePath != "" {
				callees[node.Name] = append(callees[node.Name], node.ID)
			}
		}
	}
	if r.incrementalNodesByFile != nil {
		if r.incrementalCalleeByDir == nil {
			r.incrementalCalleeByDir = make(map[string]map[string][]string)
		}
		r.incrementalCalleeByDir[dir] = callees
	}
	return callees
}

// calleeIndex holds the per-pass lookup tables bindDataflowCallee* uses.
type calleeIndex struct {
	byFile map[string]map[string][]string // file -> name -> func/method ids
	byDir  map[string]map[string][]string // dir  -> name -> function ids
	bySite map[string][]string            // "<file>\x00<line>" -> resolved callee ids
}

func newCalleeIndex() *calleeIndex {
	return &calleeIndex{
		byFile: map[string]map[string][]string{},
		byDir:  map[string]map[string][]string{},
		bySite: map[string][]string{},
	}
}

// indexCallSite records a resolved calls/references edge under its call site so
// a `*.method` dataflow callee at the same site can reuse its bound target.
func (idx *calleeIndex) indexCallSite(e *graph.Edge) {
	if e == nil || e.Line <= 0 || e.FilePath == "" || graph.IsUnresolvedTarget(e.To) {
		return
	}
	k := siteKey(e.FilePath, e.Line)
	idx.bySite[k] = append(idx.bySite[k], e.To)
}

func siteKey(file string, line int) string {
	return file + "\x00" + strconv.Itoa(line)
}

func indexName(m map[string]map[string][]string, key, name, id string) {
	names := m[key]
	if names == nil {
		names = map[string][]string{}
		m[key] = names
	}
	names[name] = append(names[name], id)
}

// bindDataflowCalleeEdge rewrites e.To from an `unresolved::` callee placeholder
// to the real node it denotes, using idx. Returns the old To value when a
// rewrite happened (for the batched reindex) or "" when the edge was left alone
// (not an unresolved target, no unambiguous match, or a shape another pass owns).
func bindDataflowCalleeEdge(e *graph.Edge, idx *calleeIndex) string {
	if e == nil || !dataflowCalleeTargetShape(e.To) {
		return ""
	}
	name := graph.UnresolvedName(e.To)
	var chosen string
	switch {
	case strings.HasPrefix(name, "*."):
		// Method-call callee: reuse the target the call resolver bound for a
		// calls/references edge at the same call site.
		method := name[2:]
		if method == "" || strings.ContainsAny(method, ".*:#") {
			return ""
		}
		chosen = uniqueSiteCallee(idx.bySite[siteKey(e.FilePath, e.Line)], method)
	case name == "" || strings.ContainsAny(name, ".*:#"):
		// Qualified (a::b), extern, or per-binding (#...) shape — owned by
		// other passes.
		return ""
	default:
		// Bare identifier: same-file first, then same-package function.
		if ids := idx.byFile[e.FilePath][name]; len(ids) == 1 {
			chosen = ids[0]
		} else if len(ids) == 0 {
			if ids := idx.byDir[filePathDir(e.FilePath)][name]; len(ids) == 1 {
				chosen = ids[0]
			}
		}
	}
	if chosen == "" || chosen == e.To {
		return ""
	}
	oldTo := e.To
	e.To = chosen
	return oldTo
}

// dataflowCalleeTargetShape is the edge-only admission predicate shared by
// the light census and the authoritative binder. It deliberately excludes
// every shape bindDataflowCalleeEdge would reject before consulting an index,
// so retaining an identity can over-collect only on graph evidence, never on
// syntax; the exact-refetched edge still runs through the full binder.
func dataflowCalleeTargetShape(to string) bool {
	if !graph.IsUnresolvedTarget(to) {
		return false
	}
	name := graph.UnresolvedName(to)
	if strings.HasPrefix(name, "*.") {
		method := name[2:]
		return method != "" && !strings.ContainsAny(method, ".*:#")
	}
	return name != "" && !strings.ContainsAny(name, ".*:#")
}

// uniqueSiteCallee returns the sole resolved callee at a call site whose id
// names the given method (its id ends with ".<method>" or "::<method>"), or ""
// when there is no match or more than one.
func uniqueSiteCallee(callees []string, method string) string {
	var chosen string
	for _, id := range callees {
		if strings.HasSuffix(id, "."+method) || strings.HasSuffix(id, "::"+method) {
			if chosen != "" && chosen != id {
				return ""
			}
			chosen = id
		}
	}
	return chosen
}
