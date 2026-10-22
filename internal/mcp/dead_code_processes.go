package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// deadCodeProcesses snapshots immutable process evidence without joining an
// analysis writer or reconstructing the entire persisted process corpus.
func (s *Server) deadCodeProcesses() (*analysis.ProcessResult, graph.AnalysisProcessReachability, graph.AnalysisGenerationHeader, bool) {
	if !s.analysisMu.TryRLock() {
		return nil, nil, graph.AnalysisGenerationHeader{}, false
	}
	defer s.analysisMu.RUnlock()
	if !s.analysisSnapshotCurrentLocked() {
		// Preserve graph-only operation before any analysis has been requested.
		// Once a pass exists, stale evidence must not turn live code into dead code.
		return nil, nil, graph.AnalysisGenerationHeader{}, s.analysisEpoch == 0 && !s.analysisRun.running.Load()
	}
	if s.processes != nil {
		return s.processes, nil, graph.AnalysisGenerationHeader{}, true
	}
	if !s.analysisGenerationReady || s.analysisGeneration.GenerationID <= 0 ||
		s.analysisGeneration.FormatVersion != analysisGenerationFormatVersion ||
		s.analysisGeneration.GraphRevision != s.communitiesToken.analysisRevision {
		return nil, nil, graph.AnalysisGenerationHeader{}, false
	}
	_, backend := s.analysisGenerationBackends()
	query, ok := backend.(graph.AnalysisProcessReachability)
	return nil, query, s.analysisGeneration, ok
}

func (s *Server) filterDeadCodeProcesses(ctx context.Context, entries []analysis.DeadCodeEntry, query graph.AnalysisProcessReachability, header graph.AnalysisGenerationHeader) ([]analysis.DeadCodeEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if query == nil {
		return entries, nil
	}
	filtered := make([]analysis.DeadCodeEntry, 0, len(entries))
	for start := 0; start < len(entries); start += analysisGenerationQueryMax {
		end := min(start+analysisGenerationQueryMax, len(entries))
		ids := make([]string, 0, end-start)
		for _, entry := range entries[start:end] {
			ids = append(ids, entry.ID)
		}
		reachable, err := query.AnalysisReachableNodesContext(ctx, header.GenerationID, ids)
		if err != nil {
			return nil, err
		}
		alive := make(map[string]bool, len(reachable))
		for _, id := range reachable {
			alive[id] = true
		}
		for _, entry := range entries[start:end] {
			if !alive[entry.ID] {
				filtered = append(filtered, entry)
			}
		}
	}
	// All batches must still describe the same generation. Never publish a
	// partial answer if invalidation raced the request.
	_, current, ok := s.activeAnalysisQuery()
	if !ok || current.GenerationID != header.GenerationID {
		return nil, errAnalysisGenerationUnavailable
	}
	return filtered, nil
}
