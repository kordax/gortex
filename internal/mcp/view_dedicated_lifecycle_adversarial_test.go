package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
)

func adversarialExactFile(t *testing.T, f *realCheckoutMutationFixture, cwd, root, want, absent string) *mcplib.CallToolResult {
	t.Helper()
	result := f.facade(t, cwd, "read", map[string]any{
		"operation": "file", "target": map[string]any{"file": "repo/edit.go"},
		"view": map[string]any{"kind": "worktree", "path": root}, "require_exact": true,
	})
	require.False(t, result.IsError, "%+v", result.Content)
	text := viewResultText(t, result)
	assert.Contains(t, text, want)
	if absent != "" {
		assert.NotContains(t, text, absent)
	}
	rider := resultFreshness(t, result)
	assert.Equal(t, true, rider["exact"])
	assert.Equal(t, rider["requested_view"], rider["actual_view"])
	assert.NotContains(t, rider, "fallback_reason")
	return result
}

func adversarialAwaitExactFile(t *testing.T, f *realCheckoutMutationFixture, cwd, root, want, absent string) {
	t.Helper()
	var result *mcplib.CallToolResult
	require.Eventually(t, func() bool {
		result = f.facade(t, cwd, "read", map[string]any{
			"operation": "file", "target": map[string]any{"file": "repo/edit.go"},
			"view": map[string]any{"kind": "worktree", "path": root}, "require_exact": true,
		})
		if result.IsError {
			// A queued follow-up cycle may briefly withdraw the route after a
			// prior materialization succeeded. Only this explicit pending read
			// refusal is retryable; scope or source errors must fail the test.
			assert.Contains(t, viewResultText(t, result), graphview.CodeViewBuilding)
			return false
		}
		text := viewResultText(t, result)
		assert.Contains(t, text, want)
		assert.NotContains(t, text, absent)
		return strings.Contains(text, want) && !strings.Contains(text, absent)
	}, 20*time.Second, 20*time.Millisecond, "exact file did not become readable after publication")
	rider := resultFreshness(t, result)
	assert.Equal(t, true, rider["exact"])
	assert.Equal(t, rider["requested_view"], rider["actual_view"])
	assert.NotContains(t, rider, "fallback_reason")
}

func adversarialUnknownScopeRefuses(t *testing.T, f *realCheckoutMutationFixture, cwd string) {
	t.Helper()
	for _, tool := range []string{"read", "search"} {
		args := map[string]any{"view": map[string]any{"kind": "worktree", "checkout_id": f.checkoutID}}
		if tool == "read" {
			args["operation"] = "file"
			args["target"] = map[string]any{"file": "repo/edit.go"}
		} else {
			args["operation"] = "symbols"
			args["query"] = "Old"
			args["options"] = map[string]any{"limit": 10}
		}
		result := f.facade(t, cwd, tool, args)
		assertToolError(t, result, graphview.CodeSelectorOutOfScope)
		assert.NotContains(t, viewResultText(t, result), "fallback_reason", "denied scope cannot gain a fallback rider")
		assert.Nil(t, result.Meta, "scope refusal must not be decorated as a served view")
	}
}

func adversarialSweep(t *testing.T, f *realCheckoutMutationFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := f.srv.lifecycle.Sweep(ctx)
	require.NoError(t, err)
}

func TestDedicatedSelectorSessionSurvivesPrimaryBranchAndHEADSwitch(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	f := newRealCheckoutMutationFixture(t)
	ctx := context.Background()
	owned, found, err := f.store.Catalog().GetDedicatedGraph(ctx, indexer.GraphIDFor("repo"))
	require.NoError(t, err)
	require.True(t, found)
	primaryBefore, found, err := f.store.Catalog().GetCheckout(ctx, owned.OwnerCheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	automaticBefore, found, err := f.store.Catalog().GetCheckout(ctx, f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	adversarialExactFile(t, f, f.worktree, f.primary, "func Old()", "func New()")
	adversarialExactFile(t, f, f.primary, f.worktree, "func New()", "func Old()")

	// A real Git branch and commit change while source bytes stay identical.
	// That isolates checkout metadata refresh from unrelated parser changes.
	checkoutMutationGit(t, f.primary, "switch", "-c", "release/adversarial")
	checkoutMutationGit(t, f.primary, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "new HEAD same source")
	newHEAD := checkoutMutationGit(t, f.primary, "rev-parse", "HEAD")
	require.NotEqual(t, primaryBefore.HeadCommit, newHEAD)
	adversarialSweep(t, f)
	primaryAfter, found, err := f.store.Catalog().GetCheckout(ctx, owned.OwnerCheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, primaryBefore.Incarnation, primaryAfter.Incarnation)
	assert.Equal(t, "refs/heads/release/adversarial", primaryAfter.HeadRef)
	assert.Equal(t, newHEAD, primaryAfter.HeadCommit)
	assert.Equal(t, primaryBefore.HeadTree, primaryAfter.HeadTree)
	automaticAfter, found, err := f.store.Catalog().GetCheckout(ctx, f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, automaticBefore.HeadRef, automaticAfter.HeadRef)
	assert.Equal(t, automaticBefore.HeadCommit, automaticAfter.HeadCommit)
	for range 3 {
		result := adversarialExactFile(t, f, f.worktree, f.primary, "func Old()", "func New()")
		assert.Equal(t, owned.GraphID, resultFreshness(t, result)["graph_id"])
		assert.Equal(t, owned.OwnerCheckoutID, resultFreshness(t, result)["checkout_id"])
		adversarialExactFile(t, f, f.primary, f.worktree, "func New()", "func Old()")
	}
	_, routed, err := f.store.Catalog().GetCheckoutRoute(ctx, owned.OwnerCheckoutID)
	require.NoError(t, err)
	assert.False(t, routed, "dedicated checkout must not acquire an automatic route")

	checkoutMutationGit(t, f.primary, "switch", "main")
	adversarialSweep(t, f)
	restored, found, err := f.store.Catalog().GetCheckout(ctx, owned.OwnerCheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, primaryBefore.HeadRef, restored.HeadRef)
	assert.Equal(t, primaryBefore.HeadCommit, restored.HeadCommit)
	adversarialExactFile(t, f, f.worktree, f.primary, "func Old()", "func New()")
}

func TestDedicatedSelectorStaysExactDuringAutomaticBranchPublication(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	f, blocker := newBlockedLifecycleFixture(t)
	ctx := context.Background()
	before, found, err := f.store.Catalog().GetCheckoutRoute(ctx, f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	checkoutBefore, found, err := f.store.Catalog().GetCheckout(ctx, f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	adversarialExactFile(t, f, f.primary, f.worktree, "func New()", "func Old()")

	checkoutMutationGit(t, f.worktree, "switch", "-c", "release/automatic")
	path := filepath.Join(f.worktree, "edit.go")
	const source = "package repo\n\nfunc LifecycleBlocked() {}\n"
	require.NoError(t, os.WriteFile(path, []byte(source), 0o644))
	checkoutMutationGit(t, f.worktree, "add", "edit.go")
	checkoutMutationGit(t, f.worktree, "-c", "commit.gpgsign=false", "commit", "-m", "automatic branch source")
	branchHEAD := checkoutMutationGit(t, f.worktree, "rev-parse", "HEAD")
	adversarialSweep(t, f)
	select {
	case <-blocker.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("automatic branch rebuild never reached the held parser")
	}

	for range 3 {
		// These calls reuse the same agent session, alternating exact dedicated
		// reads with pending automatic reads. A previous rider cannot leak.
		adversarialExactFile(t, f, f.worktree, f.primary, "func Old()", "LifecycleBlocked")
		// Exact identifies the checkout; it does not promise that its graph
		// has caught up to filesystem bytes. The previously published graph
		// stays readable while this new generation is under construction.
		published := f.facade(t, f.worktree, "search", map[string]any{
			"operation": "symbols", "query": "New", "require_exact": true,
			"options": map[string]any{"limit": 10},
		})
		require.False(t, published.IsError, "%+v", published.Content)
		rider := resultFreshness(t, published)
		assert.Equal(t, true, rider["exact"])
		assert.Equal(t, f.checkoutID, rider["checkout_id"])
		assert.NotContains(t, rider, "fallback_reason")
		assert.Contains(t, viewResultText(t, published), "repo/edit.go::New")
		adversarialExactFile(t, f, f.primary, f.worktree, "func LifecycleBlocked()", "func New()")
		mutation := f.facade(t, f.primary, "edit", map[string]any{
			"operation": "file", "target": map[string]any{"file": "repo/edit.go"},
			"match": "LifecycleBlocked", "replacement": "MustNotLand",
			"view": map[string]any{"kind": "worktree", "path": f.worktree}, "require_exact": true,
		})
		assertToolError(t, mutation, graphview.CodeViewReadOnly)
		assert.Contains(t, viewResultText(t, mutation), "checkout mutation was not admitted")
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, source, string(body))
	}

	blocker.unblock()
	require.Eventually(t, func() bool {
		view, err := f.srv.selectRequestView(ctx,
			graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: f.checkoutID}, requestViewPolicy{})
		if err != nil {
			return false
		}
		defer view.close()
		return hasNode(view.reader, "repo/edit.go::LifecycleBlocked") && !hasNode(view.reader, "repo/edit.go::New")
	}, 20*time.Second, 20*time.Millisecond, "new branch must become readable after parser release")
	for range 3 {
		adversarialAwaitExactFile(t, f, f.primary, f.worktree, "func LifecycleBlocked()", "func New()")
		adversarialExactFile(t, f, f.worktree, f.primary, "func Old()", "LifecycleBlocked")
	}
	checkoutAfter, found, err := f.store.Catalog().GetCheckout(ctx, f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, checkoutBefore.Incarnation, checkoutAfter.Incarnation)
	assert.Equal(t, "refs/heads/release/automatic", checkoutAfter.HeadRef)
	assert.Equal(t, branchHEAD, checkoutAfter.HeadCommit)
	after, found, err := f.store.Catalog().GetCheckoutRoute(ctx, f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Greater(t, after.RouteEpoch, before.RouteEpoch)
	assert.NotEqual(t, before.CommitGenerationID, after.CommitGenerationID)
}

func TestDedicatedSelectorSessionSurvivesCheckoutDisappearanceAndReturn(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	f := newRealCheckoutMutationFixture(t)
	ctx := context.Background()
	unknownCWD := t.TempDir()
	adversarialUnknownScopeRefuses(t, f, unknownCWD)
	adversarialExactFile(t, f, f.primary, f.worktree, "func New()", "func Old()")
	away := f.worktree + "-unavailable"
	require.NoError(t, os.Rename(f.worktree, away))
	t.Cleanup(func() { _ = os.Rename(away, f.worktree) })
	adversarialSweep(t, f)
	missing, found, err := f.store.Catalog().GetCheckout(ctx, f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.NotEqual(t, store_sqlite.CheckoutStateReady, missing.State)
	adversarialUnknownScopeRefuses(t, f, unknownCWD)
	for range 3 {
		adversarialExactFile(t, f, f.primary, f.primary, "func Old()", "func New()")
		refused := f.facade(t, f.primary, "read", map[string]any{
			"operation": "file", "target": map[string]any{"file": "repo/edit.go"},
			"view": map[string]any{"kind": "worktree", "checkout_id": f.checkoutID}, "require_exact": true,
		})
		assertToolError(t, refused, graphview.CodeCheckoutInaccessible)
		fallback := f.facade(t, f.primary, "search", map[string]any{
			"operation": "symbols", "query": "Old", "options": map[string]any{"limit": 10},
			"view": map[string]any{"kind": "worktree", "checkout_id": f.checkoutID},
		})
		require.False(t, fallback.IsError, "%+v", fallback.Content)
		rider := resultFreshness(t, fallback)
		assert.Equal(t, false, rider["exact"])
		assert.Equal(t, string(missing.State), rider["fallback_reason"])
		assert.Equal(t, f.checkoutID, rider["checkout_id"])
		assert.Equal(t, "base:"+indexer.GraphIDFor("repo"), rider["actual_view"])
	}

	require.NoError(t, os.Rename(away, f.worktree))
	adversarialSweep(t, f)
	require.Eventually(t, func() bool {
		view, err := f.srv.selectRequestView(ctx,
			graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: f.checkoutID}, requestViewPolicy{})
		if err != nil {
			return false
		}
		defer view.close()
		return hasNode(view.reader, "repo/edit.go::New")
	}, 20*time.Second, 20*time.Millisecond, "returned checkout must regain its exact view")
	adversarialUnknownScopeRefuses(t, f, unknownCWD)
	for range 3 {
		adversarialAwaitExactFile(t, f, f.primary, f.worktree, "func New()", "func Old()")
		adversarialExactFile(t, f, f.worktree, f.primary, "func Old()", "func New()")
	}
}

func TestDedicatedSelectorSessionSurvivesRapidBranchSwitch(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	intermediate := &lifecycleBlockingExtractor{
		marker: []byte("IntermediateBranch"), entered: make(chan struct{}), release: make(chan struct{}),
	}
	latest := &lifecycleBlockingExtractor{
		marker: []byte("LatestBranch"), entered: make(chan struct{}), release: make(chan struct{}),
	}
	f := newRealCheckoutMutationFixtureWithRegistry(t, func(registry *parser.Registry) {
		var found bool
		latest.Extractor, found = registry.GetByLanguage("go")
		require.True(t, found)
		intermediate.Extractor = latest
		registry.Register(intermediate)
	})
	path := filepath.Join(f.worktree, "edit.go")
	intermediate.diskPath.Store(&path)
	latest.diskPath.Store(&path)
	t.Cleanup(func() { intermediate.unblock(); latest.unblock() })
	// The shared fixture calls its initial branch "feature", which would
	// conflict with Git's refs/heads/feature/CORE-* namespace in this scenario.
	checkoutMutationGit(t, f.worktree, "branch", "-m", "fixture-initial")
	ctx := context.Background()
	before, found, err := f.store.Catalog().GetCheckout(ctx, f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	primaryBefore, err := os.ReadFile(filepath.Join(f.primary, "edit.go"))
	require.NoError(t, err)
	commitBranch := func(branch, name string) string {
		checkoutMutationGit(t, f.worktree, "switch", "-c", branch)
		require.NoError(t, os.WriteFile(path, []byte("package repo\n\nfunc "+name+"() {}\n"), 0o644))
		checkoutMutationGit(t, f.worktree, "add", "edit.go")
		checkoutMutationGit(t, f.worktree, "-c", "commit.gpgsign=false", "commit", "-m", branch)
		return checkoutMutationGit(t, f.worktree, "rev-parse", "HEAD")
	}
	waitParser := func(entered <-chan struct{}, stage string) {
		select {
		case <-entered:
		case <-time.After(20 * time.Second):
			t.Fatalf("%s branch never reached its parser barrier", stage)
		}
	}

	intermediateHEAD := commitBranch("feature/CORE-18585", "IntermediateBranch")
	adversarialSweep(t, f)
	waitParser(intermediate.entered, "intermediate")
	latestHEAD := commitBranch("feature/CORE-18596", "LatestBranch")
	latestTree := checkoutMutationGit(t, f.worktree, "rev-parse", "HEAD^{tree}")
	require.NotEqual(t, intermediateHEAD, latestHEAD)
	adversarialSweep(t, f)
	current, found, err := f.store.Catalog().GetCheckout(ctx, f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "refs/heads/feature/CORE-18596", current.HeadRef)
	assert.Equal(t, latestHEAD, current.HeadCommit)

	// A cancelled real MCP read must not cancel the lifecycle-owned rebuild.
	// This is pre-dispatch cancellation, not an imitation of an analysis job.
	cancelled, cancel := context.WithCancel(WithSessionCWD(WithSessionID(ctx, "real-checkout-lifecycle"), f.worktree))
	cancel()
	read := f.srv.mcpServer.GetTool("read")
	require.NotNil(t, read)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "read"
	req.Params.Arguments = map[string]any{
		"operation": "file", "target": map[string]any{"file": "repo/edit.go"},
		"view": map[string]any{"kind": "worktree", "path": f.worktree},
	}
	_, err = read.Handler(cancelled, req)
	require.ErrorIs(t, err, context.Canceled, "cancelled read must preserve transport cancellation")

	intermediate.unblock()
	waitParser(latest.entered, "latest")
	// Completion of the older branch must not roll back the accepted HEAD.
	current, found, err = f.store.Catalog().GetCheckout(ctx, f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, before.Incarnation, current.Incarnation)
	assert.Equal(t, "refs/heads/feature/CORE-18596", current.HeadRef)
	assert.Equal(t, latestHEAD, current.HeadCommit)
	adversarialExactFile(t, f, f.worktree, f.primary, "func Old()", "LatestBranch")
	refused := f.facade(t, f.primary, "edit", map[string]any{
		"operation": "file", "target": map[string]any{"file": "repo/edit.go"},
		"match": "func LatestBranch() {}", "replacement": "func MustNotLand() {}",
		"view": map[string]any{"kind": "worktree", "path": f.worktree},
	})
	require.True(t, refused.IsError)
	assert.Regexp(t, `^(view_building: checkout .+ is not fully routed yet|view_read_only: checkout mutation was not admitted; no files were changed:)`,
		viewResultText(t, refused), "only route readiness or explicit pre-write lease refusal is allowed")
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(body), "func LatestBranch()")
	assert.NotContains(t, string(body), "MustNotLand")

	latest.unblock()
	require.Eventually(t, func() bool {
		view, err := f.srv.selectRequestView(ctx,
			graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: f.checkoutID}, requestViewPolicy{})
		if err != nil {
			return false
		}
		defer view.close()
		return hasNode(view.reader, "repo/edit.go::LatestBranch") &&
			!hasNode(view.reader, "repo/edit.go::IntermediateBranch") && !hasNode(view.reader, "repo/edit.go::New")
	}, 20*time.Second, 20*time.Millisecond, "latest branch graph was stranded behind an older rebuild")
	adversarialAwaitExactFile(t, f, f.primary, f.worktree, "func LatestBranch()", "IntermediateBranch")
	route, found, err := f.store.Catalog().GetCheckoutRoute(ctx, f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	generation, found, err := f.store.Catalog().GetViewGeneration(ctx, route.CommitGenerationID)
	require.NoError(t, err)
	require.True(t, found)
	// Automatic commit generations identify their payload by tree, while the
	// checkout row carries HEAD (provenance_commit_oid is optional here).
	assert.Equal(t, latestTree, generation.TreeOID)

	// Retry only an explicit pre-write admission refusal during a follow-up
	// cycle. Once a write returns a receipt, poll that receipt, never reapply.
	var written *mcplib.CallToolResult
	require.Eventually(t, func() bool {
		written = f.facade(t, f.primary, "edit", map[string]any{
			"operation": "file", "target": map[string]any{"file": "repo/edit.go"},
			"match": "func LatestBranch() {}", "replacement": "func LatestEdited() {}",
			"view": map[string]any{"kind": "worktree", "path": f.worktree},
		})
		if !written.IsError {
			return true
		}
		text := viewResultText(t, written)
		assert.True(t, strings.Contains(text, graphview.CodeViewBuilding) ||
			(strings.Contains(text, graphview.CodeViewReadOnly) && strings.Contains(text, "no files were changed")), text)
		return false
	}, 20*time.Second, 20*time.Millisecond, "current checkout never regained mutation admission")
	f.awaitMutation(t, f.primary, written)
	adversarialAwaitExactFile(t, f, f.primary, f.worktree, "func LatestEdited()", "IntermediateBranch")
	primaryAfter, err := os.ReadFile(filepath.Join(f.primary, "edit.go"))
	require.NoError(t, err)
	assert.Equal(t, primaryBefore, primaryAfter)
}
