package resolver

import (
	"sync"

	"github.com/zzet/gortex/internal/graph"
)

// The .NET framework passes read every candidate row of their repository on
// each run: EF Core joins all types, fields and file actions, MediatR indexes
// all methods, DI walks all provides edges. A one-file edit in a repository
// that never uses the framework paid the whole scan, up to a second a pass on
// a large store, for no edge. Their facts live in node and edge metadata, which
// the light candidate census does not read.
//
// A changed-file run therefore asks two cheaper questions. Does the frontier
// itself carry a fact of the framework? Then the pass runs and the repository
// is remembered as using it. Otherwise, does the repository hold any fact at
// all? That answer is computed once per repository with a full-scope read and
// kept: a repository without facts can only gain one through a file that a
// later frontier will carry. A positive answer is never withdrawn, which is the
// safe direction, and a repository-wide or full run forgets the repository so
// the next frontier recomputes it.
type frameworkRepoFactProbe func(g graph.Store, prefixes, filePaths []string) bool

var frameworkRepoFactProbes = map[string]frameworkRepoFactProbe{
	SynthCSharpEFCoreModels: csharpEFFactsInScope,
	SynthMediatR:            mediatrFactsInScope,
	SynthCSharpDIIfaces:     csharpDIFactsInScope,
}

// FrameworkRepoFacts remembers, per repository, which probed passes found a
// fact. The indexer that owns the store owns one instance for the store's
// lifetime; a run without one never skips a pass.
type FrameworkRepoFacts struct {
	mu      sync.Mutex
	present map[frameworkRepoFactKey]bool
}

type frameworkRepoFactKey struct {
	synth string
	repo  string
}

// NewFrameworkRepoFacts returns an empty memo.
func NewFrameworkRepoFacts() *FrameworkRepoFacts {
	return &FrameworkRepoFacts{present: map[frameworkRepoFactKey]bool{}}
}

// allow reports whether a changed-file run must execute the named pass.
// Passes without a probe, repository-wide runs and runs without a memo always
// run.
func (f *FrameworkRepoFacts) allow(g graph.Store, synth string, scope map[string]bool, filePaths []string) bool {
	probe := frameworkRepoFactProbes[synth]
	if f == nil || g == nil || probe == nil || scope == nil || len(filePaths) == 0 {
		return true
	}
	prefixes := frameworkScopePrefixes(scope)
	if len(prefixes) == 0 {
		return true
	}
	if probe(g, prefixes, filePaths) {
		f.mu.Lock()
		for _, prefix := range prefixes {
			f.present[frameworkRepoFactKey{synth, prefix}] = true
		}
		f.mu.Unlock()
		return true
	}
	for _, prefix := range prefixes {
		key := frameworkRepoFactKey{synth, prefix}
		f.mu.Lock()
		present, known := f.present[key]
		f.mu.Unlock()
		if !known {
			present = probe(g, []string{prefix}, nil)
			f.mu.Lock()
			if prior, raced := f.present[key]; raced {
				present = present || prior
			}
			f.present[key] = present
			f.mu.Unlock()
		}
		if present {
			return true
		}
	}
	return false
}

// forget drops the memo for the repositories a run without a file frontier
// covers; a nil scope covers every repository.
func (f *FrameworkRepoFacts) forget(scope map[string]bool) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.present {
		if scope == nil || scope[key.repo] {
			delete(f.present, key)
		}
	}
}

func csharpEFFactsInScope(g graph.Store, prefixes, filePaths []string) bool {
	for n := range graph.NodesInScopeSeq(g, prefixes, filePaths, graph.KindType, graph.KindField, graph.KindFile) {
		if n == nil || n.Meta == nil {
			continue
		}
		switch n.Kind {
		case graph.KindFile:
			if len(csharpEFActionsFromFile(n)) > 0 {
				return true
			}
		case graph.KindType:
			if _, ok := csharpEFConfigFactFromNode(n); ok {
				return true
			}
		case graph.KindField:
			if _, ok := csharpDbSetFactFromNode(n); ok {
				return true
			}
		}
	}
	return false
}

func mediatrFactsInScope(g graph.Store, prefixes, filePaths []string) bool {
	for n := range graph.NodesInScopeSeq(g, prefixes, filePaths, graph.KindMethod, graph.KindFunction) {
		if n == nil || n.Meta == nil {
			continue
		}
		if reqType, _ := n.Meta["mediatr_request_type"].(string); reqType != "" {
			return true
		}
	}
	for row := range graph.EdgesInScopeSeq(g, prefixes, filePaths, graph.EdgeCalls) {
		if row.Edge == nil || row.Edge.Meta == nil {
			continue
		}
		if via, _ := row.Edge.Meta["via"].(string); via == mediatrVia {
			return true
		}
	}
	return false
}

func csharpDIFactsInScope(g graph.Store, prefixes, filePaths []string) bool {
	for row := range graph.EdgesInScopeSeq(g, prefixes, filePaths, graph.EdgeProvides) {
		if row.Edge == nil || row.Edge.Meta == nil {
			continue
		}
		if binding, _ := row.Edge.Meta[graph.MetaDIBinding].(string); binding == graph.DIBindingAsImplemented {
			return true
		}
	}
	return false
}
