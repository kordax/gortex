package mcp

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBatchStatusDoesNotObserveUnknownLinkedCheckout(t *testing.T) {
	for _, facade := range []bool{false, true} {
		for _, selection := range []string{"cwd", "path", "nested session scope"} {
			t.Run(fmt.Sprintf("facade=%t/%s", facade, selection), func(t *testing.T) {
				preset := "full"
				if facade {
					preset = "facade-v1"
				}
				t.Setenv("GORTEX_TOOLS", preset)
				t.Setenv("GORTEX_AUTOINDEX", "0")
				f := newRealCheckoutMutationFixture(t)
				unknown := filepath.Join(t.TempDir(), "unobserved")
				output, err := exec.CommandContext(t.Context(), "git", "-C", f.primary, "worktree", "add", "--detach", unknown, "HEAD").CombinedOutput()
				require.NoError(t, err, "create private linked checkout: %s", output)
				_, found, err := f.srv.registeredCheckoutForPath(t.Context(), unknown)
				require.NoError(t, err)
				require.False(t, found, "test checkout was already observed")
				before, err := f.srv.lifecycle.FamiliesOverview(t.Context(), "")
				require.NoError(t, err)
				primary, found, err := f.srv.registeredCheckoutForPath(t.Context(), f.primary)
				require.NoError(t, err)
				require.True(t, found)
				args := map[string]any{"transaction_id": "missing-unobserved-transaction", "status_only": true, "compact": false}
				cwd := unknown
				switch selection {
				case "path":
					cwd = f.primary
					args["view"] = map[string]any{"kind": "worktree", "path": unknown}
				case "nested session scope":
					args["view"] = map[string]any{"kind": "worktree", "checkout_id": primary.CheckoutID}
				}
				req := mcplib.CallToolRequest{}
				req.Params.Name = "batch_edit"
				handler := f.srv.wrapToolHandler(f.srv.handleAtomicBatchEdit)
				if facade {
					req.Params.Name = "edit"
					args["operation"] = "batch"
					handler = f.srv.mcpServer.GetTool("edit").Handler
				}
				req.Params.Arguments = args
				ctx := WithSessionCWD(WithSessionID(t.Context(), "unobserved-status-"+selection), cwd)
				result, err := handler(ctx, req)
				require.NoError(t, err)
				require.True(t, result.IsError, "missing transaction cannot succeed")
				after, err := f.srv.lifecycle.FamiliesOverview(t.Context(), "")
				require.NoError(t, err)
				assert.Equal(t, before, after, "poll changed catalog/routes or coordinator activation state")
				_, found, err = f.srv.registeredCheckoutForPath(t.Context(), unknown)
				require.NoError(t, err)
				assert.False(t, found, "poll registered an unknown linked checkout")
			})
		}
	}
}
