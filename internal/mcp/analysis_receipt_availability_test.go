package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
)

func TestAnalysisBusyReceiptPreservesHardGatePolicy(t *testing.T) {
	for _, breaking := range []bool{false, true} {
		name := "no_hard_gate"
		if breaking {
			name = "breaking_change"
		}
		t.Run(name, func(t *testing.T) {
			s, _, _ := busyAnalysisFixture(t)
			contracts := &contractImpact{}
			if breaking {
				contracts.Breaking = 1
			}
			ready := s.reviewReceipt(t.Context(), []string{"Alpha"}, nil, contracts, nil, true)
			require.Empty(t, ready.AnalysisStatus)
			require.Nil(t, ready.Complete)
			require.Equal(t, breaking, ready.MergeBlocker)
			pending := whileAnalysisWriterHeld(t, s, func() any {
				return s.reviewReceipt(t.Context(), []string{"Alpha"}, nil, contracts, nil, true)
			}).(analysis.ReviewReceipt)
			assert.Equal(t, "pending", pending.AnalysisStatus)
			require.NotNil(t, pending.Complete)
			assert.False(t, *pending.Complete)
			assert.True(t, pending.LowerBound)
			assert.Equal(t, ready.MergeBlocker, pending.MergeBlocker, "optional analysis cannot create or remove a hard gate")
			assert.Equal(t, ready.BlockerReason, pending.BlockerReason)
			assert.Equal(t, ready.NextSafeAction, pending.NextSafeAction, "the existing add-tests or blocker action retains priority")
			assertNoMaterializedAnalysis(t, s)
		})
	}
}
