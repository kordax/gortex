package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

var errAnalysisPending = errors.New("graph analysis is running; community and process evidence is pending")

// analysisMembershipsForNodes reuses current full views, but never hydrates them.
// Returned partial views are local to this request and must not be installed.
func (s *Server) analysisMembershipsForNodes(ctx context.Context, nodeIDs []string) (*analysis.CommunityResult, *analysis.ProcessResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if !s.analysisMu.TryRLock() {
		return nil, nil, errAnalysisPending
	}
	var communities *analysis.CommunityResult
	var processes *analysis.ProcessResult
	if s.analysisSnapshotCurrentLocked() {
		communities, processes = s.communities, s.processes
	}
	s.analysisMu.RUnlock()
	if communities != nil && processes != nil {
		return communities, processes, nil
	}
	query, header, ok := s.activeAnalysisQuery()
	if !ok {
		return communities, processes, errAnalysisGenerationUnavailable
	}
	needCommunities, needProcesses := communities == nil, processes == nil
	if needCommunities {
		communities = &analysis.CommunityResult{NodeToComm: make(map[string]string)}
	}
	if needProcesses {
		processes = &analysis.ProcessResult{NodeToProcs: make(map[string][]string)}
	}
	ids := dedupeStrings(nodeIDs)
	for start := 0; start < len(ids); start += analysisGenerationQueryMax {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		end := min(start+analysisGenerationQueryMax, len(ids))
		rows, err := query.AnalysisMembershipsContext(ctx, header.GenerationID, ids[start:end])
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			s.logAnalysisMaterializationError("memberships", err)
			// Optional enrichments retain the old unavailable-on-read-error path.
			if needCommunities {
				communities = nil
			}
			if needProcesses {
				processes = nil
			}
			return communities, processes, fmt.Errorf("%w: %w", errAnalysisGenerationUnavailable, err)
		}
		for _, row := range rows {
			if needCommunities && row.CommunityID != "" {
				communities.NodeToComm[row.NodeID] = row.CommunityID
			}
			if needProcesses && row.ProcessID != "" {
				processes.NodeToProcs[row.NodeID] = append(processes.NodeToProcs[row.NodeID], row.ProcessID)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	// Do not return rows from a generation invalidated during the query.
	_, current, ok := s.activeAnalysisQuery()
	if !ok || current.GenerationID != header.GenerationID {
		return nil, nil, errAnalysisGenerationUnavailable
	}
	return communities, processes, nil
}

// Keep the legacy traversal's three-second budget, inside the request lifetime.
func diffImpactContext(ctx context.Context, reader graph.Reader, ids []string, communities *analysis.CommunityResult, processes *analysis.ProcessResult) *analysis.ImpactResult {
	walkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return analysis.AnalyzeImpactContext(walkCtx, reader, ids, communities, processes)
}

func (s *Server) analyzeDiffImpact(ctx context.Context, symbolIDs []string) (*analysis.ImpactResult, error) {
	result := diffImpactContext(ctx, s.readerFor(ctx), symbolIDs, nil, nil)
	return result, s.enrichImpactMemberships(ctx, result, symbolIDs)
}

// enrichImpactMemberships adds only bounded optional rows, never full-generation
// compatibility views. Missing evidence remains explicit rather than becoming a
// claim that no community or process is affected.
func (s *Server) enrichImpactMemberships(ctx context.Context, result *analysis.ImpactResult, symbolIDs []string) error {
	ids := append([]string(nil), symbolIDs...)
	for depth := 1; depth <= 3; depth++ {
		for _, entry := range result.ByDepth[depth] {
			ids = append(ids, entry.ID)
		}
	}
	communities, processes, err := s.analysisMembershipsForNodes(ctx, ids)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		markImpactAnalysisUnavailable(result, err)
	}
	commSet, procSet := make(map[string]bool), make(map[string]bool)
	for _, id := range ids {
		if communities != nil {
			if cid, ok := communities.NodeToComm[id]; ok {
				commSet[cid] = true
			}
		}
		if processes != nil {
			for _, pid := range processes.NodeToProcs[id] {
				procSet[pid] = true
			}
		}
	}
	for id := range commSet {
		result.AffectedCommunities = append(result.AffectedCommunities, id)
	}
	for id := range procSet {
		result.AffectedProcesses = append(result.AffectedProcesses, id)
	}
	sort.Strings(result.AffectedCommunities)
	sort.Strings(result.AffectedProcesses)
	return ctx.Err()
}

func markImpactAnalysisUnavailable(result *analysis.ImpactResult, err error) {
	previousDetail := result.AnalysisDetail
	if result.StructuralComplete == nil {
		structuralComplete := !result.LowerBound && !result.Truncated
		result.StructuralComplete = &structuralComplete
	}
	result.LowerBound = true
	result.AnalysisStatus = "unavailable"
	if errors.Is(err, errAnalysisPending) {
		result.AnalysisStatus = "pending"
	}
	result.AnalysisDetail = "Community and process evidence is " + result.AnalysisStatus + "; structural impact remains available"
	if err != nil {
		result.AnalysisDetail += ": " + err.Error()
	}
	if previousDetail != "" {
		result.Summary = strings.TrimSuffix(result.Summary, "; "+previousDetail)
	}
	result.Summary += "; " + result.AnalysisDetail
}
