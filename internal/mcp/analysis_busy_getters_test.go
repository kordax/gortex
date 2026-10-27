package mcp

import (
	"context"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The analysis getters and the search concept expansion are optional request
// readers too: a natural-language search_symbols used to block in
// ensureAutoConceptsMaterialized behind RunAnalysis until its tool deadline.
func TestAnalysisBusyGettersDoNotWait(t *testing.T) {
	s, _, _ := busyAnalysisFixture(t)
	getters := map[string]func() bool{
		"auto_concepts": func() bool { return s.getAutoConcepts() != nil },
		"communities":   func() bool { return s.getCommunities() != nil },
		"processes":     func() bool { return s.getProcesses() != nil },
		"pagerank":      func() bool { return s.getPageRank() != nil },
		"hits":          func() bool { return s.getHITS() != nil },
		"adjacency":     func() bool { return s.getAdjacency() != nil },
		"hotspots":      func() bool { return s.getHotspots() != nil },
	}
	for name, get := range getters {
		t.Run(name, func(t *testing.T) {
			got := whileAnalysisWriterHeld(t, s, func() any { return get() }).(bool)
			assert.False(t, got, "a reader behind the writer answers without analysis data")
		})
	}
	assert.True(t, s.getCommunities() != nil, "the snapshot is served again once the writer is gone")
	assert.True(t, s.getProcesses() != nil)
}

func TestAnalysisBusyNaturalLanguageSearchDoesNotWait(t *testing.T) {
	s, _, _ := busyAnalysisFixture(t)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "function that calls alpha", "format": "json"}
	result := whileAnalysisWriterHeld(t, s, func() any {
		out, _ := s.handleSearchSymbols(context.Background(), req)
		return out
	}).(*mcplib.CallToolResult)
	require.NotNil(t, result)
	assert.False(t, result.IsError, toolText(result))
}

func TestAnalysisRecomputeMarkSkipsWithoutWaiting(t *testing.T) {
	s, _, _ := busyAnalysisFixture(t)
	recomputed := s.beginAnalysisRecompute()
	s.analysisMu.Lock()
	started := time.Now()
	got := s.getCommunities()
	waited := time.Since(started)
	s.analysisMu.Unlock()
	recomputed()

	assert.Nil(t, got)
	assert.Less(t, waited, analysisRequestLockWait, "a marked recompute is skipped at once, not waited out")
	assert.NotNil(t, s.getCommunities())
}

func TestAnalysisShortWriterIsWaitedOut(t *testing.T) {
	s, _, _ := busyAnalysisFixture(t)
	require.NotNil(t, s.getCommunities())
	s.analysisMu.Lock()
	released := make(chan struct{})
	go func() {
		time.Sleep(analysisRequestLockWait / 5)
		s.analysisMu.Unlock()
		close(released)
	}()
	assert.NotNil(t, s.getCommunities(), "an unmarked short writer is waited out, not mistaken for a recompute")
	<-released
}
