package resolver

import (
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// prepareIncrementalAttributionCache reuses the preloaded changed-file
// frontier across the scoped attribution passes. Package function candidates
// are loaded separately only when dataflow binding needs a sibling lookup.
func (r *Resolver) prepareIncrementalAttributionCache(frontier incrementalFileFrontier) {
	r.incrementalNodesByFile = make(map[string][]*graph.Node, len(frontier.nodesByFile))
	for _, path := range frontier.paths {
		// Record empty buckets too; absence would make the helper fall through
		// to a point query for files with no surviving nodes.
		r.incrementalNodesByFile[path] = frontier.nodesByFile[path]
	}
	r.incrementalOutByNode = frontier.outByNode
	r.incrementalCalleeByDir = nil
}

// runFileAttributionPassesForFilesLocked preserves the whole-graph pass order
// across a changed-file frontier. Each pass consumes the preloaded node/edge
// cache, and mutations are flushed by pass rather than by file.
func (r *Resolver) runFileAttributionPassesForFilesLocked(frontier incrementalFileFrontier) {
	rebound := false
	if rebinder, ok := r.graph.(graph.GoMethodReceiverBatchRebinder); ok {
		if _, err := rebinder.RebindGoMethodReceiversForFiles(frontier.paths); err == nil {
			rebound = true
		} else {
			r.logger.Warn("resolver: backend batch Go receiver rebind failed; using cached fallback", zap.Error(err))
		}
	}
	if !rebound {
		for _, path := range frontier.paths {
			r.rebindGoMethodReceiversForFile(path)
		}
	}
	for _, path := range frontier.paths {
		r.bindBareNameScopeRefsForFile(path)
	}
	for _, path := range frontier.paths {
		r.bindDataflowCalleeRefsForFile(path)
	}
	for _, path := range frontier.paths {
		r.bindGenericParamRefsForFile(path)
	}
	// Make all scope/dataflow/receiver rewrites visible in backend indexes
	// before the builtin and external materialisation passes inspect them.
	r.flushIncrementalAttributionReindexes()

	if !r.graphHasLanguage("go") {
		return
	}
	var candidates []*graph.Edge
	for _, path := range frontier.paths {
		candidates = append(candidates, r.fileOutEdges(path)...)
	}
	r.attributeGoBuiltinCandidates(candidates)
	seen := make(map[extKey]struct{})
	for _, edge := range candidates {
		collectGoExternalTarget(edge, seen)
	}
	r.materializeGoExternalSeen(seen)
}

func (r *Resolver) flushIncrementalAttributionReindexes() {
	batch := r.incrementalAttributionReindex
	// Release the resolver-owned backing array before entering the store. Every
	// emitted chunk is still referenced by batch until its write completes.
	r.incrementalAttributionReindex = nil
	r.noteImportEdgeReindexes(batch)
	for len(batch) > 0 {
		n := attributionReindexBatchSize
		if len(batch) < n {
			n = len(batch)
		}
		r.graph.ReindexEdges(batch[:n])
		batch = batch[n:]
	}
}

func (r *Resolver) clearIncrementalAttributionCache() {
	r.flushIncrementalAttributionReindexes()
	r.incrementalNodesByFile = nil
	r.incrementalOutByNode = nil
	r.incrementalCalleeByDir = nil
}

func (r *Resolver) persistAttributionReindexes(batch []graph.EdgeReindex) {
	if len(batch) == 0 {
		return
	}
	r.noteImportEdgeReindexes(batch)
	if r.incrementalNodesByFile == nil {
		for len(batch) > 0 {
			n := attributionReindexBatchSize
			if len(batch) < n {
				n = len(batch)
			}
			r.graph.ReindexEdges(batch[:n])
			batch = batch[n:]
		}
		return
	}

	for len(batch) > 0 {
		if len(r.incrementalAttributionReindex) >= attributionReindexBatchSize {
			r.flushIncrementalAttributionReindexes()
		}
		room := attributionReindexBatchSize - len(r.incrementalAttributionReindex)
		n := room
		if len(batch) < n {
			n = len(batch)
		}
		r.incrementalAttributionReindex = append(r.incrementalAttributionReindex, batch[:n]...)
		batch = batch[n:]
		if len(r.incrementalAttributionReindex) == attributionReindexBatchSize {
			r.flushIncrementalAttributionReindexes()
		}
	}
}

func (r *Resolver) incrementalFileNodes(filePath string) []*graph.Node {
	if r.incrementalNodesByFile != nil {
		if nodes, cached := r.incrementalNodesByFile[filePath]; cached {
			return nodes
		}
	}
	return r.graph.GetFileNodes(filePath)
}
