package semantic

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

type frontierCompletionProvider struct {
	*mockProvider
	supplemental bool
	result       *EnrichResult
	err          error
	calls        int
}

func (p *frontierCompletionProvider) Supplemental() bool { return p.supplemental }

func (p *frontierCompletionProvider) EnrichFilesContext(context.Context, graph.Store, string, string, []string) (*EnrichResult, error) {
	p.calls++
	return p.result, p.err
}

func TestManagerEnrichFilesRetainsSupplementalIncompleteState(t *testing.T) {
	failed := errors.New("supplemental failed")
	for _, mode := range []string{"partial", "error", "only_supplemental_error"} {
		t.Run(mode, func(t *testing.T) {
			manager := NewManager(Config{Enabled: true}, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, manager.Close()) })
			primaryResult := &EnrichResult{Provider: "primary", Language: "go", EdgesAdded: 3, EdgesConfirmed: 5}
			primary := &frontierCompletionProvider{
				mockProvider: &mockProvider{name: "primary", languages: []string{"go"}, available: true},
				result:       primaryResult,
			}
			if mode != "only_supplemental_error" {
				manager.RegisterProvider(primary)
			}
			supplemental := &frontierCompletionProvider{
				mockProvider: &mockProvider{name: "supplemental", languages: []string{"go"}, available: true},
				supplemental: true,
				result:       &EnrichResult{Provider: "supplemental", Language: "go"},
			}
			if mode == "partial" {
				supplemental.result.Partial = true
				supplemental.result.AbortReason = "deadline reached"
			} else {
				supplemental.result = nil
				supplemental.err = failed
			}
			manager.RegisterProvider(supplemental)
			result, err := manager.EnrichFiles(graph.New(), "fixture", t.TempDir(), "go", []string{"fixture/a.go", "fixture/b.go"})
			if mode == "partial" {
				require.NoError(t, err)
				require.NotNil(t, result)
				assert.True(t, result.Partial, "a supplemental partial must keep the shared frontier incomplete")
			} else {
				assert.ErrorIs(t, err, failed, "a supplemental failure must reach the frontier owner")
			}
			if mode != "only_supplemental_error" {
				require.NotNil(t, result)
				assert.Equal(t, 3, result.EdgesAdded, "primary committed work must remain observable")
				assert.Equal(t, 5, result.EdgesConfirmed)
				assert.False(t, primaryResult.Partial, "aggregation must not mutate a provider-owned result")
			}
			assert.Equal(t, 1, supplemental.calls)

			// A successful retry clears the aggregate incomplete state; it must
			// not retain a synthetic partial on the provider's previous result.
			supplemental.err = nil
			supplemental.result = &EnrichResult{Provider: "supplemental", Language: "go"}
			result, err = manager.EnrichFiles(graph.New(), "fixture", t.TempDir(), "go", []string{"fixture/a.go", "fixture/b.go"})
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.False(t, result.Partial)
			assert.Equal(t, 2, supplemental.calls)
		})
	}
}
