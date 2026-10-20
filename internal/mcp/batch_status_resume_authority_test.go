package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBatchResumeRejectsForeignPrimaryOrigin(t *testing.T) {
	for _, facade := range []bool{false, true} {
		t.Run(fmt.Sprintf("facade=%t", facade), func(t *testing.T) {
			preset := "full"
			if facade {
				preset = "facade-v1"
			}
			t.Setenv("GORTEX_TOOLS", preset)
			t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
			origin := newRealCheckoutMutationFixture(t)
			foreign := newRealCheckoutMutationFixture(t)
			originalCheckout, found, err := origin.srv.registeredCheckoutForPath(t.Context(), origin.primary)
			require.NoError(t, err)
			require.True(t, found)
			foreignCheckout, found, err := foreign.srv.registeredCheckoutForPath(t.Context(), foreign.primary)
			require.NoError(t, err)
			require.True(t, found)
			path := filepath.Join(origin.primary, "edit.go")
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			const id = "resume-recorded-origin"
			resumeFlag := true
			call := func(s *Server, cwd, checkoutID string, execute bool) *mcplib.CallToolResult {
				t.Helper()
				args := map[string]any{"transaction_id": id, "compact": false}
				if checkoutID != "" {
					args["view"] = map[string]any{"kind": "worktree", "checkout_id": checkoutID}
				}
				if execute {
					args["edits"] = []any{map[string]any{"op": "edit_file", "path": path, "old_string": string(before), "new_string": string(before) + "\n// resume origin\n"}}
				} else if resumeFlag {
					args["resume"] = true
				}
				req := mcplib.CallToolRequest{}
				req.Params.Name = "batch_edit"
				handler := s.wrapToolHandler(s.handleAtomicBatchEdit)
				if facade {
					req.Params.Name = "edit"
					args["operation"] = "batch"
					if edits, ok := args["edits"]; ok {
						args["changes"] = edits
						delete(args, "edits")
					}
					handler = s.mcpServer.GetTool("edit").Handler
				}
				req.Params.Arguments = args
				ctx := WithSessionCWD(WithSessionID(t.Context(), "resume-authority"), cwd)
				result, err := handler(ctx, req)
				require.NoError(t, err)
				return result
			}
			applied := call(origin.srv, origin.primary, originalCheckout.CheckoutID, true)
			require.False(t, applied.IsError, viewResultText(t, applied))
			for _, flag := range []bool{true, false} {
				resumeFlag = flag
				foreignResult := call(foreign.srv, foreign.primary, foreignCheckout.CheckoutID, false)
				assert.True(t, foreignResult.IsError, "another dedicated primary resumed the original transaction (resume=%t): %s", flag, viewResultText(t, foreignResult))
				_, cached := foreign.srv.batchTransactions.Load(id)
				assert.False(t, cached, "foreign request cached a recovery owner")
			}
			value, exists := origin.srv.batchTransactions.Load(id)
			require.True(t, exists)
			state := value.(*batchTransactionState)
			receipt := state.snapshot()
			bad := cloneBatchReceipt(receipt)
			bad.OriginCheckoutIncarnation = "old-incarnation-at-reused-root"
			state.publish(bad, false)
			for _, flag := range []bool{true, false} {
				resumeFlag = flag
				stale := call(origin.srv, origin.primary, originalCheckout.CheckoutID, false)
				assert.True(t, stale.IsError, "recreated checkout resumed an older incarnation (resume=%t)", flag)
			}
			legacy := cloneBatchReceipt(receipt)
			legacy.OriginCheckoutID, legacy.OriginCheckoutIncarnation, legacy.OriginCheckoutRoot = "", "", ""
			state.publish(legacy, false)
			for _, flag := range []bool{true, false} {
				resumeFlag = flag
				unscoped := call(origin.srv, origin.primary, "", false)
				require.False(t, unscoped.IsError, "legacy unscoped recovery lost compatibility (resume=%t): %s", flag, viewResultText(t, unscoped))
			}
		})
	}
}
