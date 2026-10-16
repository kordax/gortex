package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

func testEvidenceFixture(t *testing.T, excludeTests bool) (*Server, *graph.Node) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "api.go"), []byte("package fixture\nfunc Process() {}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "api_test.go"), []byte("package fixture\nfunc TestUnrelated() {}\n"), 0o600))
	cfg := config.Default()
	if excludeTests {
		cfg.Index.Exclude = append(cfg.Index.Exclude, "**/*_test.go")
	}
	g := graph.New()
	idx := indexer.New(g, testRegistry(), cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
	for _, n := range g.AllNodes() {
		if n.Name == "Process" {
			return srv, n
		}
	}
	t.Fatal("fixture Process symbol was not indexed")
	return nil, nil
}

func evidenceRequest(args map[string]any) mcplib.CallToolRequest {
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

func evidenceJSON(t *testing.T, result *mcplib.CallToolResult, err error) map[string]any {
	t.Helper()
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].(mcplib.TextContent).Text), &payload))
	return payload
}

func TestTestIndexEvidenceExcludedTests(t *testing.T) {
	srv, node := testEvidenceFixture(t, true)
	ctx := context.Background()
	result, err := srv.handleGetTestTargets(ctx, evidenceRequest(map[string]any{"ids": node.ID}))
	payload := evidenceJSON(t, result, err)
	assert.Equal(t, false, payload["coverage_known"])
	assert.Empty(t, payload["uncovered"])
	assert.Equal(t, []any{node.ID}, payload["coverage_unknown"])
	assert.Contains(t, payload["coverage_note"], "may be excluded or not indexed")
	assert.NotContains(t, payload["coverage_note"], "0/1")

	result, err = srv.handleGetTestTargets(ctx, evidenceRequest(map[string]any{"ids": node.ID, "compact": true}))
	require.NoError(t, err)
	text := result.Content[0].(mcplib.TextContent).Text
	assert.Contains(t, text, "test coverage unknown")
	assert.NotContains(t, text, "no test targets found")
	assert.NotContains(t, text, "uncovered:")

	result, err = srv.handleGetUntestedSymbols(ctx, evidenceRequest(nil))
	payload = evidenceJSON(t, result, err)
	assert.Equal(t, false, payload["coverage_known"])
	assert.Nil(t, payload["coverage_ratio"])
	assert.Empty(t, payload["untested"])
	assert.Equal(t, float64(0), payload["total_uncovered"])
	assert.Equal(t, float64(1), payload["total_unknown"])

	blast := srv.buildBlastRadius(ctx, []*graph.Node{node})
	assert.Equal(t, false, blast["coverage_known"])
	assert.Contains(t, blast["warning"], "test coverage unknown")
	assert.NotContains(t, blast["warning"], "no covering tests found")
}

func TestTestIndexEvidenceIndexedTestsCanReportGraphGap(t *testing.T) {
	srv, node := testEvidenceFixture(t, false)
	ctx := context.Background()
	result, err := srv.handleGetTestTargets(ctx, evidenceRequest(map[string]any{"ids": node.ID}))
	payload := evidenceJSON(t, result, err)
	assert.Equal(t, true, payload["coverage_known"])
	assert.Equal(t, []any{node.ID}, payload["uncovered"])
	assert.Empty(t, payload["coverage_unknown"])
	assert.Contains(t, payload["coverage_note"], "indexed test coverage")

	result, err = srv.handleGetUntestedSymbols(ctx, evidenceRequest(nil))
	payload = evidenceJSON(t, result, err)
	assert.Equal(t, true, payload["coverage_known"])
	assert.Equal(t, float64(0), payload["coverage_ratio"])
	assert.Equal(t, float64(1), payload["total_uncovered"])
	assert.Equal(t, float64(0), payload["total_unknown"])

	// A display filter retains its result-count semantics and cannot turn a
	// known graph gap into an apparent fully covered repository.
	result, err = srv.handleGetUntestedSymbols(ctx, evidenceRequest(map[string]any{"min_fan_in": 100}))
	payload = evidenceJSON(t, result, err)
	assert.Empty(t, payload["untested"])
	assert.Equal(t, float64(0), payload["total_uncovered"])
	assert.Equal(t, float64(0), payload["coverage_ratio"])
	assert.Equal(t, false, payload["truncated"])

	blast := srv.buildBlastRadius(ctx, []*graph.Node{node})
	assert.Equal(t, true, blast["coverage_known"])
	assert.Equal(t, "no covering tests found in the indexed graph", blast["warning"])
}

func TestTestIndexEvidenceScopedByRepositoryAndLanguage(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "other/test.go::TestForeign", Name: "TestForeign", Kind: graph.KindFunction, FilePath: "other/foreign_test.go", RepoPrefix: "other"})
	g.AddNode(&graph.Node{ID: "repo/test.go::TestLocal", Name: "TestLocal", Kind: graph.KindFunction, FilePath: "repo/local_test.go", RepoPrefix: "repo"})
	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	ctx := context.Background()
	assert.False(t, srv.testEvidenceKnown(ctx, &graph.Node{FilePath: "missing/api.go", RepoPrefix: "missing"}))
	assert.False(t, srv.testEvidenceKnown(ctx, &graph.Node{FilePath: "repo/api.py", RepoPrefix: "repo"}))
	assert.True(t, srv.testEvidenceKnown(ctx, &graph.Node{FilePath: "repo/api.go", RepoPrefix: "repo"}))
	assert.False(t, srv.testEvidenceKnownForNodes(ctx, []*graph.Node{{FilePath: "repo/api.go", RepoPrefix: "repo"}, {FilePath: "repo/api.py", RepoPrefix: "repo"}}))

	result, err := srv.handleGetTestTargets(ctx, evidenceRequest(map[string]any{"ids": "unknown-symbol"}))
	payload := evidenceJSON(t, result, err)
	assert.Equal(t, false, payload["coverage_known"])
	assert.Empty(t, payload["uncovered"])
	assert.Equal(t, []any{"unknown-symbol"}, payload["coverage_unknown"])
}

func TestTestIndexEvidencePreservesPartialPositiveResults(t *testing.T) {
	g := graph.New()
	goNode := &graph.Node{ID: "repo/api.go::Process", Name: "Process", Kind: graph.KindFunction, FilePath: "repo/api.go", RepoPrefix: "repo"}
	pyNode := &graph.Node{ID: "repo/api.py::process", Name: "process", Kind: graph.KindFunction, FilePath: "repo/api.py", RepoPrefix: "repo"}
	testNode := &graph.Node{ID: "repo/api_test.go::TestProcess", Name: "TestProcess", Kind: graph.KindFunction, FilePath: "repo/api_test.go", RepoPrefix: "repo"}
	for _, node := range []*graph.Node{goNode, pyNode, testNode} {
		g.AddNode(node)
	}
	g.AddEdge(&graph.Edge{From: testNode.ID, To: goNode.ID, Kind: graph.EdgeTests})
	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	result, err := srv.handleGetTestTargets(context.Background(), evidenceRequest(map[string]any{"ids": goNode.ID + "," + pyNode.ID}))
	payload := evidenceJSON(t, result, err)
	assert.Equal(t, false, payload["coverage_known"])
	assert.Equal(t, float64(1), payload["total_files"])
	assert.Len(t, payload["run_commands"], 1)
	assert.Empty(t, payload["uncovered"])
	assert.Equal(t, []any{pyNode.ID}, payload["coverage_unknown"])
}

func TestTestIndexEvidenceImpactFormatsAndStrategy(t *testing.T) {
	srv, node := testEvidenceFixture(t, true)
	ctx := context.Background()
	for _, format := range []string{"json", "gcx", "toon"} {
		t.Run(format, func(t *testing.T) {
			result, err := srv.handleEnhancedChangeImpact(ctx, evidenceRequest(map[string]any{"ids": node.ID, "format": format}))
			require.NoError(t, err)
			require.False(t, result.IsError)
			text := result.Content[0].(mcplib.TextContent).Text
			assert.Contains(t, text, "test coverage unknown")
			if format == "json" {
				payload := evidenceJSON(t, result, err)
				assert.Equal(t, false, payload["coverage_known"])
			}
			if format == "gcx" {
				assert.Contains(t, text, "explain_change_impact.test_evidence")
			}
		})
	}

	node.EndLine = node.StartLine + 80
	strategy := srv.buildEditStrategy(ctx, &prediction{nodes: []*graph.Node{node}})
	require.NotNil(t, strategy)
	assert.Contains(t, strategy.Safety, testIndexUnavailableNote)
	assert.NotContains(t, strategy.Safety, "no covering tests found — add one before refactoring")
}
