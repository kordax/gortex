package mcp

import (
	"errors"

	"github.com/zzet/gortex/internal/analysis"
)

func isAnalysisEvidenceUnavailable(err error) bool {
	return errors.Is(err, errAnalysisPending) || errors.Is(err, errAnalysisGenerationUnavailable)
}

// applyImpactAnalysisAvailability preserves the normal response shape while
// distinguishing unavailable enrichment from a verified empty membership set.
func applyImpactAnalysisAvailability(payload map[string]any, impact *analysis.ImpactResult) {
	if impact == nil || impact.AnalysisStatus == "" {
		return
	}
	payload["analysis_status"] = impact.AnalysisStatus
	payload["analysis_detail"] = impact.AnalysisDetail
	payload["complete"] = false
	payload["lower_bound"] = true
	if impact.StructuralComplete != nil {
		payload["structural_complete"] = *impact.StructuralComplete
	}
}

func applyReviewAnalysisAvailability(payload map[string]any, impacts map[string]*analysis.ImpactResult) {
	status, detail := "", ""
	structuralComplete := true
	for _, impact := range impacts {
		if impact == nil {
			continue
		}
		if impact.StructuralComplete != nil {
			structuralComplete = structuralComplete && *impact.StructuralComplete
		} else {
			structuralComplete = structuralComplete && !impact.LowerBound && !impact.Truncated
		}
		if impact.AnalysisStatus == "" {
			continue
		}
		// Unavailable is stronger than temporarily pending, independent of map
		// iteration order. Pick a stable detail when query failures differ.
		if status == "" || (impact.AnalysisStatus == "unavailable" && status != "unavailable") ||
			(impact.AnalysisStatus == status && impact.AnalysisDetail < detail) {
			status, detail = impact.AnalysisStatus, impact.AnalysisDetail
		}
	}
	if status == "" {
		return
	}
	impact := &analysis.ImpactResult{AnalysisStatus: status, AnalysisDetail: detail, StructuralComplete: &structuralComplete}
	applyImpactAnalysisAvailability(payload, impact)
	if summary, ok := payload["summary"].(string); ok {
		payload["summary"] = summary + "; " + detail
	}
}
