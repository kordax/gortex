package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

func TestDeadCodeDoesNotWaitForAnalysisWriter(t *testing.T) {
	s := &Server{}
	s.analysisMu.Lock()
	locked := true
	done := make(chan struct{})
	var result *mcpgo.CallToolResult
	var err error
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		if locked {
			s.analysisMu.Unlock()
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("dead-code handler did not finish after releasing analysis writer")
		}
	})
	go func() {
		defer close(done)
		result, err = s.handleFindDeadCode(ctx, mcpgo.CallToolRequest{})
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		// Cancel before releasing the lock so a regressed implementation can
		// finish without running a graph traversal on this minimal fixture.
		cancel()
		s.analysisMu.Unlock()
		locked = false
		t.Fatal("dead-code request queued behind analysis writer")
	}
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, fmt.Sprint(result.Content), "analysis_pending")
}

func TestDeadCodeUsesCachedProcessesWithoutMaterialization(t *testing.T) {
	processes := &analysis.ProcessResult{Processes: []analysis.Process{{EntryPoint: "entry"}}}
	s := &Server{graph: graph.New(), analysisEpoch: 1, processes: processes}
	s.communitiesToken = s.currentCommunityToken()
	s.analysisMaterializeMu.Lock()
	defer s.analysisMaterializeMu.Unlock()
	got, query, _, ready := s.deadCodeProcesses()
	require.True(t, ready)
	require.Same(t, processes, got)
	require.Nil(t, query)
}

type deadCodeReachabilityStub struct {
	calls int
	fail  bool
}

func (q *deadCodeReachabilityStub) AnalysisReachableNodesContext(ctx context.Context, _ int64, ids []string) ([]string, error) {
	q.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if q.fail {
		return nil, errors.New("fixture query failure")
	}
	if len(ids) > analysisGenerationQueryMax {
		return nil, errors.New("unbounded query")
	}
	return ids[:1], nil
}

func TestDeadCodeProcessEvidenceFailsClosed(t *testing.T) {
	s := &Server{}
	entries := make([]analysis.DeadCodeEntry, analysisGenerationQueryMax+1)
	for i := range entries {
		entries[i].ID = fmt.Sprintf("node-%d", i)
	}
	query := &deadCodeReachabilityStub{fail: true}
	header := graph.AnalysisGenerationHeader{GenerationID: 1}
	rows, err := s.filterDeadCodeProcesses(t.Context(), entries, query, header)
	require.Error(t, err)
	require.Nil(t, rows, "query errors must not become false dead-code positives")
	query.fail = false
	query.calls = 0
	rows, err = s.filterDeadCodeProcesses(t.Context(), entries, query, header)
	require.Equal(t, 2, query.calls, "candidate requests are bounded")
	require.ErrorIs(t, err, errAnalysisGenerationUnavailable)
	require.Nil(t, rows, "a generation lost between pages must not publish partial results")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.filterDeadCodeProcesses(ctx, entries, query, header)
	require.ErrorIs(t, err, context.Canceled)
}
