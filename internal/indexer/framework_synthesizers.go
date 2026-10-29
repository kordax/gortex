package indexer

import (
	"sort"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/resolver"
)

func (idx *Indexer) frameworkSynthesizerSelection() resolver.FrameworkSynthesizerSelection {
	if idx == nil || idx.config.FrameworkSynthesizers == nil {
		return resolver.AllFrameworkSynthesizers()
	}
	selection, err := resolver.NewFrameworkSynthesizerSelection(*idx.config.FrameworkSynthesizers)
	if err == nil {
		return selection
	}
	if idx.logger != nil {
		idx.logger.Error(
			"invalid index.framework_synthesizers; using the complete registry",
			zap.Error(err),
		)
	}
	return resolver.AllFrameworkSynthesizers()
}

// frameworkSynthesizerSelection combines the effective configuration of the
// repositories participating in one shared-graph pass. An omitted selection in
// any participating repository preserves the legacy all-enabled behavior;
// otherwise the configured allow-lists are unioned. The pipeline is disabled
// only when every participating repository explicitly configured an empty list.
func (mi *MultiIndexer) frameworkSynthesizerSelection(
	scope map[string]bool,
) resolver.FrameworkSynthesizerSelection {
	mi.mu.RLock()
	selected := make(map[string]struct{})
	participants := 0
	all := false
	for prefix, idx := range mi.indexers {
		if len(scope) > 0 && !scope[prefix] {
			continue
		}
		if idx == nil {
			continue
		}
		participants++
		if idx.config.FrameworkSynthesizers == nil {
			all = true
			break
		}
		for _, name := range *idx.config.FrameworkSynthesizers {
			selected[name] = struct{}{}
		}
	}
	mi.mu.RUnlock()

	if all || participants == 0 {
		return resolver.AllFrameworkSynthesizers()
	}
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	selection, err := resolver.NewFrameworkSynthesizerSelection(names)
	if err == nil {
		return selection
	}
	if mi.logger != nil {
		mi.logger.Error(
			"invalid index.framework_synthesizers; using the complete registry",
			zap.Error(err),
		)
	}
	return resolver.AllFrameworkSynthesizers()
}

// frameworkRepoFacts returns the store's framework fact memo, created on first
// use so indexers assembled without the constructor share the behavior.
func (mi *MultiIndexer) frameworkRepoFacts() *resolver.FrameworkRepoFacts {
	mi.frameworkFactsOnce.Do(func() {
		mi.frameworkFacts = resolver.NewFrameworkRepoFacts()
	})
	return mi.frameworkFacts
}
