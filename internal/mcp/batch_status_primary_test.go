package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graphview"
)

func TestBatchStatusDedicatedPrimarySelector(t *testing.T) {
	for _, facade := range []bool{false, true} {
		for _, implicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("facade=%t/implicit=%t", facade, implicit), func(t *testing.T) {
				preset := "full"
				if facade {
					preset = "facade-v1"
				}
				t.Setenv("GORTEX_TOOLS", preset)
				t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
				f := newRealCheckoutMutationFixture(t)
				primary, found, err := f.srv.checkoutForRequestPath(t.Context(), f.primary)
				require.NoError(t, err)
				require.True(t, found)
				require.NotEmpty(t, primary.CheckoutID)
				require.NotEqual(t, f.checkoutID, primary.CheckoutID)
				path := filepath.Join(f.primary, "edit.go")
				before, err := os.ReadFile(path)
				require.NoError(t, err)
				const id = "dedicated-primary-status"
				ctx := WithSessionCWD(WithSessionID(t.Context(), "primary-poll"), f.primary)
				dedicated, present, err := f.store.Catalog().GetDedicatedGraphByOwner(ctx, primary.CheckoutID)
				require.NoError(t, err)
				require.True(t, present)
				rider := graphview.NewViewRider(graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: primary.CheckoutID})
				rider.CheckoutID, rider.GraphID, rider.Exact = primary.CheckoutID, dedicated.GraphID, false
				var inexact batchTransactionReceipt
				require.NoError(t, f.srv.stampBatchCanonicalOrigin(withRequestView(ctx, &requestView{rider: rider, viewRoot: f.primary}), &inexact))
				assert.Empty(t, inexact.OriginCheckoutID, "a fallback cannot claim original checkout authority")
				handler := f.srv.wrapToolHandler(f.srv.handleAtomicBatchEdit)
				if facade {
					handler = f.srv.mcpServer.GetTool("edit").Handler
				}
				call := func(checkoutID string, poll bool) *mcplib.CallToolResult {
					t.Helper()
					req := mcplib.CallToolRequest{}
					req.Params.Name = "batch_edit"
					args := map[string]any{"transaction_id": id, "compact": false}
					if checkoutID != "" {
						args["view"] = map[string]any{"kind": "worktree", "checkout_id": checkoutID}
					}
					if poll {
						args["status_only"] = true
					} else {
						args["edits"] = []any{map[string]any{"op": "edit_file", "path": path, "old_string": string(before), "new_string": string(before) + "\n// primary batch\n"}}
					}
					if facade {
						req.Params.Name = "edit"
						args["operation"] = "batch"
						if edits, ok := args["edits"]; ok {
							args["changes"] = edits
							delete(args, "edits")
						}
					}
					req.Params.Arguments = args
					result, err := handler(ctx, req)
					require.NoError(t, err)
					return result
				}
				executeCheckout := primary.CheckoutID
				if implicit {
					executeCheckout = ""
				}
				applied := call(executeCheckout, false)
				require.False(t, applied.IsError, viewResultText(t, applied))
				assert.Equal(t, "committed", decodeFileOpsResult(t, applied)["disk_status"])
				polled := call(primary.CheckoutID, true)
				require.False(t, polled.IsError, "same primary selector cannot read its own batch: %s", viewResultText(t, polled))
				assert.Equal(t, id, decodeFileOpsResult(t, polled)["transaction_id"])
				assert.Equal(t, "committed", decodeFileOpsResult(t, polled)["disk_status"])
				value, cached := f.srv.batchTransactions.Load(id)
				require.True(t, cached)
				state := value.(*batchTransactionState)
				receipt := state.snapshot()
				assert.Empty(t, receipt.CheckoutID, "canonical publication must keep its existing refresh path")
				assert.Equal(t, primary.CheckoutID, receipt.OriginCheckoutID)
				assert.Equal(t, primary.Incarnation, receipt.OriginCheckoutIncarnation)
				assert.Equal(t, primary.RootPath, receipt.OriginCheckoutRoot)
				f.srv.batchTransactions.Delete(id)
				reloaded := call(primary.CheckoutID, true)
				require.False(t, reloaded.IsError, "persisted origin was lost: %s", viewResultText(t, reloaded))
				for _, invalid := range []string{"checkout", "incarnation", "root", "file boundary", "legacy"} {
					bad := cloneBatchReceipt(receipt)
					switch invalid {
					case "checkout":
						bad.OriginCheckoutID = f.checkoutID
					case "incarnation":
						bad.OriginCheckoutIncarnation = "previous-incarnation-at-same-root"
					case "root":
						bad.OriginCheckoutRoot = f.worktree
					case "file boundary":
						bad.Files[0].Path = filepath.Join(f.worktree, "edit.go")
					case "legacy":
						bad.OriginCheckoutID, bad.OriginCheckoutIncarnation, bad.OriginCheckoutRoot = "", "", ""
					}
					f.srv.batchTransactions.Store(id, &batchTransactionState{done: make(chan struct{}), receipt: bad})
					refused := call(primary.CheckoutID, true)
					require.True(t, refused.IsError, "accepted invalid primary origin: %s", invalid)
					if invalid == "incarnation" {
						require.NoError(t, f.srv.persistBatchManifest(bad))
						f.srv.batchTransactions.Delete(id)
						refused = call(primary.CheckoutID, true)
						require.True(t, refused.IsError, "durable receipt survived root reuse with another incarnation")
					}
					if invalid == "legacy" {
						compatible := call("", true)
						require.False(t, compatible.IsError, "legacy unscoped snapshot compatibility was lost: %s", viewResultText(t, compatible))
					}
				}
				f.srv.batchTransactions.Store(id, state)
				wrong := call(f.checkoutID, true)
				require.True(t, wrong.IsError, "another checkout read the primary receipt")
			})
		}
	}
}
