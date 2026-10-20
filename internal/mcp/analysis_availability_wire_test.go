package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestAnalysisBusyAvailabilitySurvivesWireFormatsAndBudget(t *testing.T) {
	s, _, _ := busyAnalysisFixture(t)
	for _, operation := range []string{"detect", "contract", "review_pack", "diff_context", "pr_context"} {
		for _, format := range []string{"json", "gcx", "toon", "compact"} {
			for _, budget := range []int{0, 600} {
				t.Run(fmt.Sprintf("%s/%s/budget%d", operation, format, budget), func(t *testing.T) {
					args := map[string]any{"format": format, "scope": "compare", "base_ref": "base-ref", "base": "base-ref", "max_tokens": budget, "use_llm": false}
					handler := s.handleDetectChanges
					switch operation {
					case "contract":
						handler = s.handleChangeContract
						args["source"], args["symbols"] = "symbols", "Alpha"
					case "review_pack":
						handler = s.handleReviewPack
						args["include_pack"] = true
					case "diff_context":
						handler = s.handleDiffContext
					case "pr_context":
						handler = s.handlePRReviewContext
						args["ids"], args["sections"], args["verify"], args["audit_config"] = "Alpha,Beta,Gamma", "diff_context", false, false
					}
					if format == "compact" {
						args["compact"] = true
					}
					req := mcplib.CallToolRequest{}
					req.Params.Name = map[string]string{"detect": "detect_changes", "contract": "change_contract", "review_pack": "review_pack", "diff_context": "diff_context", "pr_context": "get_pr_review_context"}[operation]
					req.Params.Arguments = args
					result := whileAnalysisWriterHeld(t, s, func() any {
						out, _ := handler(t.Context(), req)
						return out
					}).(*mcplib.CallToolResult)
					require.NotNil(t, result)
					require.False(t, result.IsError, toolText(result))
					text := toolText(result)
					assert.Contains(t, text, "analysis_status")
					assert.Contains(t, text, "pending")
					assert.Contains(t, text, "complete")
					assert.Contains(t, text, "lower_bound")
					assert.Contains(t, text, "false")
					assert.NotRegexp(t, `(?:^|[\n{, ])"?complete"?[:= ]+true`, text, "unavailable enrichment cannot be declared complete")
					callerIDsVisible := operation != "contract"
					switch operation {
					case "review_pack", "pr_context":
						callerIDsVisible = format != "gcx"
					case "diff_context":
						callerIDsVisible = format != "compact"
					}
					if budget == 0 && callerIDsVisible {
						assert.Contains(t, text, "Caller", "core caller information should remain available")
					}
				})
			}
		}
	}
}

type unavailableMembershipStore struct {
	*store_sqlite.Store
	cause  error
	cancel context.CancelFunc
}

func (s *unavailableMembershipStore) AnalysisMembershipsContext(context.Context, int64, []string) ([]graph.AnalysisMembership, error) {
	if s.cancel != nil {
		s.cancel()
	}
	return nil, s.cause
}

func TestAnalysisEvidenceFailureIsUnavailableAndCancellationIsError(t *testing.T) {
	s, _, _ := busyAnalysisFixture(t)
	cause := errors.New("test membership query failed")
	// Select the wrapper as the backend, rather than the fixture indexer's
	// original store, so real membership queries exercise the injected error.
	s.indexer = nil
	s.graph = &unavailableMembershipStore{Store: s.graph.(*store_sqlite.Store), cause: cause}
	_, _, err := s.analysisMembershipsForNodes(t.Context(), []string{"Alpha"})
	require.ErrorIs(t, err, cause)
	result, err := s.analyzeDiffImpact(t.Context(), []string{"Alpha"})
	require.NoError(t, err)
	assert.Equal(t, "unavailable", result.AnalysisStatus)
	assert.Contains(t, result.AnalysisDetail, cause.Error())
	assertImpactContainsCaller(t, result)
	assert.True(t, result.LowerBound)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	gotErr := whileAnalysisWriterHeld(t, s, func() any {
		_, err := s.analyzeDiffImpact(ctx, []string{"Alpha"})
		return err
	})
	require.ErrorIs(t, gotErr.(error), context.Canceled)
	_, err = s.analyzeDiffImpact(ctx, []string{"Alpha"})
	require.ErrorIs(t, err, context.Canceled)
}

func TestAnalysisPublicHandlersPreserveCancellationDuringEnrichment(t *testing.T) {
	for _, operation := range []string{"contract", "pre_commit", "safe_to_change"} {
		t.Run(operation, func(t *testing.T) {
			s, _, _ := busyAnalysisFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store := &unavailableMembershipStore{Store: s.graph.(*store_sqlite.Store), cancel: cancel}
			s.indexer = nil
			s.graph = store
			if operation == "contract" {
				req := mcplib.CallToolRequest{}
				req.Params.Arguments = map[string]any{"source": "symbols", "symbols": "Alpha", "format": "json"}
				result, err := s.handleChangeContract(ctx, req)
				require.ErrorIs(t, err, context.Canceled)
				assert.Nil(t, result)
				return
			}
			if operation == "pre_commit" {
				// The fixture HEAD is clean. Change an indexed function's body
				// in this private repository so pre-commit reaches enrichment.
				path := "internal/alpha/a.go"
				content, err := os.ReadFile(path)
				require.NoError(t, err)
				lines := strings.Split(string(content), "\n")
				require.Greater(t, len(lines), 4)
				lines[3] += " // pending change"
				require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644))
			}
			req := mcplib.GetPromptRequest{}
			req.Params.Arguments = map[string]string{"ids": "Alpha", "scope": "unstaged"}
			handler := s.handlePromptSafeToChange
			if operation == "pre_commit" {
				handler = s.handlePromptPreCommit
			}
			result, err := handler(ctx, req)
			require.ErrorIs(t, err, context.Canceled)
			assert.Nil(t, result)
		})
	}
}

func TestImpactAvailabilityPreservesStructuralCompletenessWhenRepeated(t *testing.T) {
	s, _, _ := busyAnalysisFixture(t)
	result := diffImpactContext(t.Context(), s.graph, []string{"Alpha"}, nil, nil)
	require.False(t, result.LowerBound)
	markImpactAnalysisUnavailable(result, errAnalysisPending)
	require.NotNil(t, result.StructuralComplete)
	require.True(t, *result.StructuralComplete)
	firstSummary := result.Summary
	markImpactAnalysisUnavailable(result, errAnalysisPending)
	assert.True(t, *result.StructuralComplete)
	assert.Equal(t, firstSummary, result.Summary)
}
