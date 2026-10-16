package mcp

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

const testIndexUnavailableNote = "test coverage unknown: test symbols for the requested repository or language are unavailable in the index; test files may be excluded or not indexed"

// testEvidenceKnown determines whether a negative graph test lookup can be
// interpreted. Indexed tests establish graph evidence, not runtime coverage or
// proof that every test file was indexed. Reuse the review probe and its epoch
// invalidation rather than treating an empty test-edge set as zero coverage.
func (s *Server) testEvidenceKnown(ctx context.Context, node *graph.Node) bool {
	if node == nil || s.graph == nil {
		return false
	}
	if nodeIsTestSymbol(node) {
		return true
	}
	lang, ok := testLangByExt[strings.ToLower(filepath.Ext(node.FilePath))]
	if !ok {
		return false
	}
	annotateBaseScoped(ctx, graphview.CapSyntaxGraph)
	return s.testLangsIndexed(node.RepoPrefix)[lang]
}

func (s *Server) testEvidenceKnownForNodes(ctx context.Context, nodes []*graph.Node) bool {
	if len(nodes) == 0 {
		return false
	}
	for _, node := range nodes {
		if !s.testEvidenceKnown(ctx, node) {
			return false
		}
	}
	return true
}
