package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/reconcile"
)

func TestDedicatedWorktreeSelectorPrimaryNeedsNoRoute(t *testing.T) {
	for _, byPath := range []bool{false, true} {
		t.Run(map[bool]string{false: "checkout ID", true: "path"}[byPath], func(t *testing.T) {
			v := newViewStack(t)
			selector := map[string]any{"kind": "worktree", "checkout_id": viewTestPrimary}
			if byPath {
				selector = map[string]any{"kind": "worktree", "path": v.repoRoot}
			}
			_, found, err := v.store.Catalog().GetCheckoutRoute(context.Background(), viewTestPrimary)
			require.NoError(t, err)
			require.False(t, found)
			var reader graph.Reader
			res, err := v.callWithView(t, v.repoRoot, "get_symbol", map[string]any{"view": selector},
				func(ctx context.Context) (*mcplib.CallToolResult, error) {
					view := requestViewFromContext(ctx)
					require.NotNil(t, view)
					assert.False(t, view.routed())
					assert.Nil(t, view.materialized)
					assert.Equal(t, v.repoRoot, view.viewRoot)
					assert.Equal(t, "repo", viewRepoPrefix(view))
					reader = v.srv.readerFor(ctx)
					return mcplib.NewToolResultText(`{"ok":true}`), nil
				})
			require.NoError(t, err)
			require.False(t, res.IsError, "%s", viewResultText(t, res))
			assert.True(t, hasNode(reader, "repo/edit.go::Old"))
			assert.False(t, hasNode(reader, "repo/edit.go::New"))
			rider := resultFreshness(t, res)
			assert.Equal(t, true, rider["exact"])
			assert.Equal(t, rider["requested_view"], rider["actual_view"])
			assert.Equal(t, viewTestPrimary, rider["checkout_id"])
			assert.Equal(t, v.graphID, rider["graph_id"])
			assert.NotContains(t, rider, "fallback_reason")
		})
	}
}

func dedicateViewWorktree(t *testing.T, v *viewStack, ownState string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, v.store.Catalog().DeleteCheckoutRoute(ctx, viewTestWorktree))
	require.NoError(t, v.store.Catalog().UpdateCheckoutState(ctx, store_sqlite.UpdateCheckoutStateRequest{
		CheckoutID: viewTestWorktree, Incarnation: "inc-worktree", State: store_sqlite.CheckoutStateReady,
		DesiredMode: store_sqlite.CheckoutModeDedicated, EffectiveMode: store_sqlite.CheckoutModeDedicated,
	}))
	if ownState != "" {
		require.NoError(t, v.store.Catalog().UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
			GraphID: "graph-dedicated-worktree", OwnerCheckoutID: viewTestWorktree,
			RepoPrefix: "repo-dedicated", FamilyID: viewTestFamily, State: ownState,
		}))
	}
}

func TestDedicatedWorktreeSelectorUsesOwnedGraph(t *testing.T) {
	v := newViewStack(t)
	dedicateViewWorktree(t, v, reconcile.GraphStateReady)
	selector := graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: viewTestWorktree}
	view, err := v.srv.viewForWorktreeSelector(context.Background(), selector, requestViewPolicy{})
	require.NoError(t, err)
	require.NotNil(t, view)
	defer view.close()
	assert.Equal(t, "graph-dedicated-worktree", view.rider.GraphID)
	assert.NotEqual(t, v.graphID, view.rider.GraphID)
	assert.Equal(t, viewTestWorktree, view.rider.CheckoutID)
	assert.Equal(t, selector.String(), view.rider.RequestedView)
	assert.Equal(t, selector.String(), view.rider.ActualView)
	assert.True(t, view.rider.Exact)
	assert.Equal(t, v.worktreeRoot, view.viewRoot)
	assert.Equal(t, "repo-dedicated", viewRepoPrefix(view))
	assert.False(t, view.routed())
}

func TestDedicatedWorktreeSelectorNeverSubstitutesPrimary(t *testing.T) {
	for _, tc := range []struct {
		name, state, code string
	}{
		{name: "missing owned graph", code: graphview.CodeCheckoutInaccessible},
		{name: "building owned graph", state: "graph_building", code: graphview.CodeViewBuilding},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newViewStack(t)
			dedicateViewWorktree(t, v, tc.state)
			view, err := v.srv.viewForWorktreeSelector(context.Background(),
				graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: viewTestWorktree}, requestViewPolicy{})
			require.Error(t, err)
			assert.Nil(t, view)
			assert.Equal(t, tc.code, graphview.CodeOf(err))
		})
	}
}

func TestDedicatedWorktreeSelectorChecksScopeBeforeReadiness(t *testing.T) {
	for _, state := range []string{reconcile.GraphStateReady, "graph_building"} {
		t.Run(state, func(t *testing.T) {
			v := newViewStack(t)
			v.setPrimaryGraphState(t, state)
			ran := false
			res, err := v.callWithView(t, v.otherRoot, "get_symbol",
				map[string]any{"view": map[string]any{"kind": "worktree", "checkout_id": viewTestPrimary}},
				func(context.Context) (*mcplib.CallToolResult, error) {
					ran = true
					return mcplib.NewToolResultText(`{"ok":true}`), nil
				})
			require.NoError(t, err)
			assertToolError(t, res, graphview.CodeSelectorOutOfScope)
			assert.False(t, ran)
		})
	}
}

func TestDedicatedWorktreeSelectorPreservesAutomaticRouteRequirement(t *testing.T) {
	v := newViewStack(t)
	require.NoError(t, v.store.Catalog().DeleteCheckoutRoute(context.Background(), viewTestWorktree))
	view, err := v.srv.viewForWorktreeSelector(context.Background(),
		graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: viewTestWorktree}, requestViewPolicy{})
	require.Error(t, err)
	assert.Nil(t, view)
	assert.Equal(t, graphview.CodeViewBuilding, graphview.CodeOf(err))
}

func TestDedicatedWorktreeSelectorKeepsMutationAndForeignPathsInTheirRoots(t *testing.T) {
	v := newViewStack(t)
	otherPath := filepath.Join(v.otherRoot, "other.go")
	otherBefore, err := os.ReadFile(otherPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(v.worktreeRoot, "edit.go"), []byte("package repo\n\nfunc WorktreeOnly() {}\n"), 0o644))
	args := map[string]any{"view": map[string]any{"kind": "worktree", "checkout_id": viewTestPrimary}}
	res, err := v.callWithView(t, v.worktreeRoot, "edit_file", args,
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			root := requestViewPathRoot(ctx)
			assert.Equal(t, v.repoRoot, root.root)
			assert.True(t, root.serves("repo"))
			assert.False(t, root.serves("other"))
			foreign, pathErr := v.srv.checkoutRootedPath(ctx, otherPath, v.otherRoot, "other", true)
			require.NoError(t, pathErr)
			assert.Equal(t, otherPath, foreign)
			req := mcplib.CallToolRequest{}
			req.Params.Name = "edit_file"
			req.Params.Arguments = map[string]any{
				"path": "repo/edit.go", "old_string": "func Old()", "new_string": "func EditedPrimary()",
			}
			return v.srv.handleEditFile(ctx, req)
		})
	require.NoError(t, err)
	require.False(t, res.IsError, "%s", viewResultText(t, res))
	primary, err := os.ReadFile(filepath.Join(v.repoRoot, "edit.go"))
	require.NoError(t, err)
	assert.Contains(t, string(primary), "func EditedPrimary()")
	worktree, err := os.ReadFile(filepath.Join(v.worktreeRoot, "edit.go"))
	require.NoError(t, err)
	assert.Contains(t, string(worktree), "func WorktreeOnly()")
	otherAfter, err := os.ReadFile(otherPath)
	require.NoError(t, err)
	assert.Equal(t, otherBefore, otherAfter)
}

func TestDedicatedWorktreeSelectorNamedRepoSearchUsesBaseCorpus(t *testing.T) {
	v := newSearchViewStack(t)
	require.NoError(t, v.store.UpsertSymbolFTS("other/other.go::Other", "other zephyr scheduler"))
	res, err := v.callWithView(t, v.repoRoot, "search_symbols",
		map[string]any{"view": map[string]any{"kind": "worktree", "checkout_id": viewTestPrimary}},
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			req := searchToolRequest(searchProseQuery)
			req.Params.Arguments.(map[string]any)["repo"] = "repo"
			return v.srv.handleSearchSymbols(ctx, req)
		})
	require.NoError(t, err)
	require.False(t, res.IsError, "%s", viewResultText(t, res))
	text := viewResultText(t, res)
	assert.Contains(t, text, searchOldID)
	assert.NotContains(t, text, searchNewID)
	assert.NotContains(t, text, "other/other.go::Other")
}
