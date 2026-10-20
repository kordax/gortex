package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

func busyAnalysisFixture(t *testing.T) (*Server, string, string) {
	t.Helper()
	dir, a, b, c := siblingDiffGitRepo(t)
	t.Chdir(dir)
	store, err := store_sqlite.Open(t.TempDir() + "/analysis.sqlite")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{
		{ID: "Alpha", Name: "Alpha", Kind: graph.KindFunction, FilePath: a, StartLine: 3, EndLine: 7, Language: "go"},
		{ID: "Beta", Name: "Beta", Kind: graph.KindFunction, FilePath: b, StartLine: 3, EndLine: 7, Language: "go"},
		{ID: "Gamma", Name: "Gamma", Kind: graph.KindFunction, FilePath: c, StartLine: 3, EndLine: 7, Language: "go"},
		{ID: "Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: "caller.go", StartLine: 1, EndLine: 3, Language: "go"},
	}, []*graph.Edge{{From: "Caller", To: "Alpha", Kind: graph.EdgeCalls, FilePath: "caller.go", Line: 2}})
	s, metrics := populateAnalysisForTest(store)
	require.NoError(t, metrics.cacheSaveErr)
	community := s.communities.NodeToComm["Alpha"]
	require.NotEmpty(t, community)
	require.NotEmpty(t, s.processes.NodeToProcs["Alpha"])
	process := s.processes.NodeToProcs["Alpha"][0]
	s.engine = query.NewEngine(store)
	s.indexer = indexer.New(store, nil, config.IndexConfig{}, zap.NewNop())
	s.indexer.SetRootPath(dir)
	s.session = newSessionState()
	require.True(t, s.releaseTransientAnalysisIfIdle())
	return s, community, process
}

// Keep the writer held until the reader returns: a deadlock guard is not a
// latency SLO, and releasing the lock cannot make this test pass accidentally.
func whileAnalysisWriterHeld(t *testing.T, s *Server, call func() any) any {
	t.Helper()
	s.analysisMu.Lock()
	done := make(chan any, 1)
	go func() { done <- call() }()
	select {
	case got := <-done:
		s.analysisMu.Unlock()
		return got
	case <-time.After(5 * time.Second):
		s.analysisMu.Unlock()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("analysis reader did not stop after cleanup unlocked writer")
		}
		t.Fatal("optional analysis reader waited behind whole-graph analysis writer")
		return nil
	}
}

func TestAnalysisBusyCoreImpactRetainsCaller(t *testing.T) {
	for _, name := range []string{"detect", "contract", "review"} {
		t.Run(name, func(t *testing.T) {
			s, community, process := busyAnalysisFixture(t)
			call := func() *analysis.ImpactResult {
				switch name {
				case "detect":
					result, err := s.analyzeDiffImpact(t.Context(), []string{"Alpha"})
					if err != nil {
						return nil
					}
					return result
				case "contract":
					return s.analyzeImpactLazy(t.Context(), []string{"Alpha"})
				default:
					return s.reviewImpact(t.Context(), []analysis.ChangedSymbol{{ID: "Alpha"}})["Alpha"]
				}
			}
			got := whileAnalysisWriterHeld(t, s, func() any { return call() }).(*analysis.ImpactResult)
			require.NotNil(t, got)
			assertImpactContainsCaller(t, got)
			assert.True(t, got.LowerBound)
			wire, err := json.Marshal(got)
			require.NoError(t, err)
			assert.Contains(t, string(wire), `"analysis_status":"pending"`)
			assert.NotContains(t, got.Summary, "no impact")
			ready := call()
			require.NotNil(t, ready)
			assertImpactContainsCaller(t, ready)
			assert.Contains(t, ready.AffectedCommunities, community)
			assert.Contains(t, ready.AffectedProcesses, process)
			wire, err = json.Marshal(ready)
			require.NoError(t, err)
			assert.NotContains(t, string(wire), "analysis_status", "available result keeps its existing shape")
			assertNoMaterializedAnalysis(t, s)
		})
	}
}

func assertImpactContainsCaller(t *testing.T, result *analysis.ImpactResult) {
	t.Helper()
	for _, entry := range result.ByDepth[1] {
		if entry.ID == "Caller" {
			return
		}
	}
	t.Error("core graph caller disappeared while optional analysis was unavailable")
}

func TestAnalysisBusyRerankMetricsDoNotWait(t *testing.T) {
	s, _, _ := busyAnalysisFixture(t)
	got := whileAnalysisWriterHeld(t, s, func() any { return s.rerankAnalysisMetrics([]string{"Alpha"}) })
	assert.Nil(t, got)
	assert.Contains(t, s.rerankAnalysisMetrics([]string{"Alpha"}), "Alpha")
}

func TestAnalysisBusyDetectAndContractWireCompleteness(t *testing.T) {
	for _, name := range []string{"detect", "contract"} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := busyAnalysisFixture(t)
			args := map[string]any{"format": "json", "scope": "compare", "base_ref": "base-ref"}
			handler := s.handleDetectChanges
			if name == "contract" {
				args["source"], args["symbols"] = "symbols", "Alpha"
				handler = s.handleChangeContract
			}
			req := mcplib.CallToolRequest{}
			req.Params.Arguments = args
			result := whileAnalysisWriterHeld(t, s, func() any {
				out, _ := handler(context.Background(), req)
				return out
			}).(*mcplib.CallToolResult)
			require.NotNil(t, result)
			require.False(t, result.IsError, toolText(result))
			text := toolText(result)
			assert.Contains(t, text, `"analysis_status":"pending"`)
			assert.Contains(t, text, `"complete":false`)
			assert.Contains(t, text, `"lower_bound":true`)
			if name == "detect" {
				assert.Contains(t, text, "Caller")
			}
		})
	}
}
