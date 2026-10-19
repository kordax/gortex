package resolver

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

func inferenceFrontierIDs(scope map[string]bool) []string {
	ids := make([]string, 0, len(scope))
	for id, affected := range scope {
		if affected && id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func inferenceNodesByIDs(store graph.Store, ids []string) map[string]*graph.Node {
	nodes := make(map[string]*graph.Node, len(ids))
	for start := 0; start < len(ids); start += inferenceMutationBatchSize {
		for id, node := range store.GetNodesByIDs(ids[start:min(start+inferenceMutationBatchSize, len(ids))]) {
			nodes[id] = node
		}
	}
	return nodes
}

// inferenceMethodsForOwners reads only membership adjacency for the exact
// owner frontier. A type edit must not hydrate every method in its repository.
func inferenceMethodsForOwners(store graph.Store, ids []string) (map[string]map[string]*graph.Node, bool) {
	methods := make(map[string]map[string]*graph.Node)
	ambiguous := false
	for start := 0; start < len(ids); start += inferenceMutationBatchSize {
		chunk := ids[start:min(start+inferenceMutationBatchSize, len(ids))]
		incoming := store.GetInEdgesByNodeIDs(chunk)
		methodSet := make(map[string]bool)
		for _, ownerID := range chunk {
			for _, edge := range incoming[ownerID] {
				if edge != nil && edge.Kind == graph.EdgeMemberOf {
					methodSet[edge.From] = true
				}
			}
		}
		nodes := inferenceNodesByIDs(store, inferenceFrontierIDs(methodSet))
		for _, ownerID := range chunk {
			for _, edge := range incoming[ownerID] {
				if edge == nil || edge.Kind != graph.EdgeMemberOf {
					continue
				}
				method := nodes[edge.From]
				if method == nil || method.Kind != graph.KindMethod {
					continue
				}
				if methods[ownerID] == nil {
					methods[ownerID] = make(map[string]*graph.Node)
				}
				if previous := methods[ownerID][method.Name]; previous != nil && previous.ID != method.ID {
					ambiguous = true
				}
				methods[ownerID][method.Name] = method
			}
		}
	}
	return methods, ambiguous
}

func (r *Resolver) inferImplementationsForFrontier(scopeTypes, scopeIfaces map[string]bool) int {
	for _, node := range inferenceNodesByIDs(r.graph, inferenceFrontierIDs(scopeIfaces)) {
		if node != nil && node.Kind == graph.KindInterface {
			// A changed interface can affect all owners in its repository.
			// Keep the set-oriented reference projection for this inherently
			// broad case; batching every owner's adjacency costs more.
			return r.inferImplementationsByRepoScan(scopeTypes, scopeIfaces)
		}
	}
	repos := implementationInferenceRepos(r.graph, scopeTypes, scopeIfaces)
	if len(repos) == 0 {
		return 0
	}
	ifaces := collectImplementationInterfaces(r.graph, repos)
	if len(ifaces) == 0 {
		return 0
	}
	ownerSet := make(map[string]bool)
	for id, affected := range scopeTypes {
		if affected {
			ownerSet[id] = true
		}
	}
	ids := inferenceFrontierIDs(ownerSet)
	owners := inferenceNodesByIDs(r.graph, ids)
	liveIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		owner := owners[id]
		if owner != nil && (owner.Kind == graph.KindType || owner.Kind == graph.KindInterface) && len(ifaces[owner.RepoPrefix]) > 0 {
			liveIDs = append(liveIDs, id)
		}
	}
	methods, _ := inferenceMethodsForOwners(r.graph, liveIDs)
	types := make(map[string]map[string]*implementationType)
	for _, id := range liveIDs {
		owner := owners[id]
		if len(methods[id]) == 0 {
			continue
		}
		if types[owner.RepoPrefix] == nil {
			types[owner.RepoPrefix] = make(map[string]*implementationType)
		}
		names := make(map[string]struct{}, len(methods[id]))
		for name := range methods[id] {
			names[name] = struct{}{}
		}
		types[owner.RepoPrefix][id] = &implementationType{node: owner, methods: names}
	}
	existing := make(map[string]struct{})
	for start := 0; start < len(liveIDs); start += inferenceMutationBatchSize {
		for _, edges := range r.graph.GetOutEdgesByNodeIDs(liveIDs[start:min(start+inferenceMutationBatchSize, len(liveIDs))]) {
			for _, edge := range edges {
				if edge != nil && edge.Kind == graph.EdgeImplements {
					existing[inferencePairKey(edge.From, edge.To)] = struct{}{}
				}
			}
		}
	}
	added := 0
	batch := make([]*graph.Edge, 0, inferenceMutationBatchSize)
	flush := func() {
		if len(batch) > 0 {
			r.graph.AddBatch(nil, batch)
			batch = make([]*graph.Edge, 0, inferenceMutationBatchSize)
		}
	}
	visitImplementationMatches(types, ifaces, scopeTypes, scopeIfaces, func(owner *implementationType, iface implementationInterface) {
		key := inferencePairKey(owner.node.ID, iface.id)
		if _, found := existing[key]; found {
			return
		}
		existing[key] = struct{}{}
		batch = append(batch, &graph.Edge{
			From: owner.node.ID, To: iface.id, Kind: graph.EdgeImplements,
			FilePath: owner.node.FilePath, Line: owner.node.StartLine,
			Meta: map[string]any{"via": MetaViaMethodSetInference},
		})
		added++
		if len(batch) == inferenceMutationBatchSize {
			flush()
		}
	})
	flush()
	return added
}

func (r *Resolver) inferOverridesForFrontier(scope map[string]bool) int {
	ids := inferenceFrontierIDs(scope)
	if len(ids) == 0 {
		return 0
	}
	seen := make(map[graph.EdgeIdentity]bool)
	parents := make([]*graph.Edge, 0)
	ownerSet := make(map[string]bool)
	collect := func(chunk []string, rows map[string][]*graph.Edge) {
		for _, id := range chunk {
			for _, edge := range rows[id] {
				if edge == nil || !isOverrideParentKind(edge.Kind) || edge.From == edge.To {
					continue
				}
				key := graph.EdgeIdentityFor(edge)
				if seen[key] {
					continue
				}
				seen[key] = true
				parents = append(parents, edge)
				ownerSet[edge.From], ownerSet[edge.To] = true, true
			}
		}
	}
	for start := 0; start < len(ids); start += inferenceMutationBatchSize {
		chunk := ids[start:min(start+inferenceMutationBatchSize, len(ids))]
		collect(chunk, r.graph.GetInEdgesByNodeIDs(chunk))
		collect(chunk, r.graph.GetOutEdgesByNodeIDs(chunk))
	}
	if len(parents) == 0 {
		return 0
	}
	ownerIDs := inferenceFrontierIDs(ownerSet)
	owners := inferenceNodesByIDs(r.graph, ownerIDs)
	methods, ambiguous := inferenceMethodsForOwners(r.graph, ownerIDs)
	if ambiguous {
		// Adjacency has no public row-order guarantee. Overloaded names must
		// keep the reference projection's last-row winner, including SQLite
		// physical edge-ID ordering, rather than pick a different method.
		return r.inferOverridesByRepoScan(scope)
	}
	added := 0
	candidates := make([]overrideCandidate, 0, inferenceMutationBatchSize)
	flush := func() {
		added += r.flushOverrideCandidates(candidates)
		candidates = make([]overrideCandidate, 0, inferenceMutationBatchSize)
	}
	for _, edge := range parents {
		if !relevantOverrideParent(graph.ScopedEdgeRow{Edge: edge, Source: owners[edge.From], Target: owners[edge.To]}, scope) {
			continue
		}
		names := make([]string, 0, len(methods[edge.From]))
		for name := range methods[edge.From] {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			child, parent := methods[edge.From][name], methods[edge.To][name]
			if child == nil || parent == nil || child.ID == parent.ID {
				continue
			}
			candidates = append(candidates, overrideCandidate{from: child, to: parent, origin: overrideOrigin(edge.Origin)})
			if len(candidates) == inferenceMutationBatchSize {
				flush()
			}
		}
	}
	flush()
	return added
}
