package mcp

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	mcpwire "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/observability"
)

func metricOutcomeCount(outcome string) float64 {
	var text bytes.Buffer
	observability.Default.Write(observability.NewExposition(&text))
	var total float64
	for _, line := range strings.Split(text.String(), "\n") {
		if !strings.HasPrefix(line, "gortex_mcp_requests_total{") || !strings.Contains(line, "outcome=\""+outcome+"\"") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 2 {
			value, _ := strconv.ParseFloat(fields[1], 64)
			total += value
		}
	}
	return total
}

func TestBoundHandlerMetricsClassifiesCallerOutcomes(t *testing.T) {
	for _, outcome := range []string{"success", "error", "cancelled", "deadline", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			before := metricOutcomeCount(outcome)
			ctx := context.Background()
			if outcome == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			handler := func(ctx context.Context, _ int) (*mcpwire.CallToolResult, error) {
				switch outcome {
				case "error":
					return mcpwire.NewToolResultError("private details never exported"), nil
				case "deadline":
					<-ctx.Done()
					return nil, ctx.Err()
				case "panic":
					panic("private details never exported")
				default:
					return mcpwire.NewToolResultText("ok"), nil
				}
			}
			bound := boundHandler(&Server{ToolCallTimeout: 15 * time.Millisecond}, "tool", "observability_fixture", handler,
				func(int64, time.Duration) (*mcpwire.CallToolResult, error) {
					return mcpwire.NewToolResultError("busy"), nil
				},
				func(time.Duration, *mutationCommitNote) (*mcpwire.CallToolResult, error) {
					return mcpwire.NewToolResultError("deadline"), nil
				},
				func(any) (*mcpwire.CallToolResult, error) { return mcpwire.NewToolResultError("panic"), nil })
			_, err := bound(ctx, 1)
			if outcome == "cancelled" {
				require.True(t, errors.Is(err, context.Canceled))
			} else {
				require.NoError(t, err)
			}
			assert.GreaterOrEqual(t, metricOutcomeCount(outcome), before+1)
			var text bytes.Buffer
			observability.Default.Write(observability.NewExposition(&text))
			assert.NotContains(t, text.String(), "private details")
		})
	}
}
