package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"go.uber.org/zap"
)

type batchPollBarrierWatcher struct {
	mutationTestWatcher
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	attempts atomic.Int64
}

func (w *batchPollBarrierWatcher) EnqueueFileMutation(ctx context.Context, path string) (*indexer.MutationTicket, error) {
	w.attempts.Add(1)
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.mutationTestWatcher.EnqueueFileMutation(ctx, path)
}

func TestBatchStatusOnlyDoesNotJoinBlockedRefresh(t *testing.T) {
	for _, facade := range []bool{false, true} {
		name := "direct"
		if facade {
			name = "facade"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("GORTEX_TOOLS", "facade-v1")
			t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
			g := graph.New()
			s := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
			watcher := &batchPollBarrierWatcher{entered: make(chan struct{}), release: make(chan struct{})}
			s.watcher = watcher
			path := writeAtomicBatchFixture(t, t.TempDir(), "blocked.txt", "before\n")
			const id = "poll-behind-refresh"
			handler := s.boundToolHandler(s.handleAtomicBatchEdit)
			call := func(ctx context.Context, options map[string]any, edits []any) (*mcplib.CallToolResult, error) {
				req := mcplib.CallToolRequest{}
				req.Params.Name = "batch_edit"
				args := map[string]any{"transaction_id": id}
				for k, v := range options {
					args[k] = v
				}
				if edits != nil {
					args["edits"] = edits
				}
				if facade {
					req.Params.Name = "edit"
					args = map[string]any{"operation": "batch", "options": args, "output": map[string]any{"compact": false}}
					if edits != nil {
						args["changes"] = edits
						delete(args["options"].(map[string]any), "edits")
					}
				}
				req.Params.Arguments = args
				return handler(ctx, req)
			}
			if facade {
				handler = s.mcpServer.GetTool("edit").Handler
			}
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(watcher.release) }) }
			var state *batchTransactionState
			originalDone := make(chan struct{})
			t.Cleanup(func() {
				unblock()
				select {
				case <-originalDone:
				case <-time.After(5 * time.Second):
					t.Error("original waiter did not stop after cleanup release")
				}
				if state == nil {
					if value, found := s.batchTransactions.Load(id); found {
						state = value.(*batchTransactionState)
					}
				}
				if state != nil {
					waitBatchTimeoutTerminal(t, state)
				}
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			original := make(chan fallbackWriteResult, 1)
			go func() {
				result, err := call(ctx, nil, []any{map[string]any{"op": "edit_file", "path": path, "old_string": "before", "new_string": "after"}})
				original <- fallbackWriteResult{result: result, err: err}
				close(originalDone)
			}()
			select {
			case <-watcher.entered:
			case outcome := <-original:
				t.Fatalf("mutation returned before graph refresh: result=%+v err=%v", outcome.result, outcome.err)
			case <-time.After(5 * time.Second):
				t.Fatal("mutation never entered graph refresh")
			}
			value, exists := s.batchTransactions.Load(id)
			require.True(t, exists)
			state = value.(*batchTransactionState)
			require.Equal(t, "committed", state.snapshot().DiskStatus)
			cancel()
			select {
			case outcome := <-original:
				// The original waiter cancels independently of its committed owner.
				assert.ErrorIs(t, outcome.err, context.Canceled)
			case <-time.After(time.Second):
				t.Fatal("original waiter did not cancel")
			}
			cancelled, cancelPoll := context.WithCancel(t.Context())
			cancelPoll()
			_, _ = call(cancelled, map[string]any{"status_only": true}, nil)
			pollCtx, stopPoll := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer stopPoll()
			result, err := call(pollCtx, map[string]any{"status_only": true}, nil)
			require.NoError(t, err)
			require.False(t, result.IsError, "poll waited behind the refresh owner: %s", viewResultText(t, result))
			payload := decodeFileOpsResult(t, result)
			assert.Equal(t, id, payload["transaction_id"])
			assert.Equal(t, "committed", payload["disk_status"])
			assert.Equal(t, "pending", payload["graph_status"])
			assert.Equal(t, int64(1), watcher.attempts.Load())
			select {
			case <-watcher.release:
				t.Fatal("test barrier opened before the poll completed")
			default:
			}
			assert.Equal(t, "after\n", readAtomicBatchFixture(t, path))
			// Explicit recovery and identical retry may wait for the owner, but
			// their admission must cancel without a stranded mutex goroutine.
			resumeCtx, stopResume := context.WithTimeout(t.Context(), 20*time.Millisecond)
			_, resumeErr := s.batchTransactionStatus(resumeCtx, id)
			stopResume()
			assert.ErrorIs(t, resumeErr, context.DeadlineExceeded)
			retryCtx, stopRetry := context.WithTimeout(t.Context(), 20*time.Millisecond)
			_, retryErr := s.runBatchTransaction(retryCtx, []batchEditItem{atomicFileEdit(path, "before", "after")}, id)
			stopRetry()
			assert.ErrorIs(t, retryErr, context.DeadlineExceeded)
			assert.Equal(t, int64(1), watcher.attempts.Load())
			unblock()
			waitBatchTimeoutTerminal(t, state)
			result, err = call(t.Context(), map[string]any{"status_only": true}, nil)
			require.NoError(t, err)
			require.False(t, result.IsError, viewResultText(t, result))
			assert.Equal(t, "fresh", decodeFileOpsResult(t, result)["graph_status"])
			assert.Equal(t, int64(1), watcher.attempts.Load())
		})
	}
}

func TestBatchStatusRequestValidation(t *testing.T) {
	for _, facade := range []bool{false, true} {
		for _, args := range []map[string]any{
			{"transaction_id": "validation", "status_only": "true"},
			{"transaction_id": "validation", "status_only": nil},
			{"transaction_id": "validation", "resume": 1},
			{"transaction_id": "validation", "status_only": true, "resume": true},
			{"status_only": true},
			{"resume": true},
			{"transaction_id": 123, "status_only": true},
			{"transaction_id": "validation", "status_only": true, "edits": []any{}},
			{"transaction_id": "validation", "resume": true, "edits": []any{}},
		} {
			t.Run(fmt.Sprintf("facade=%t/%v", facade, args), func(t *testing.T) {
				t.Setenv("GORTEX_TOOLS", "facade-v1")
				g := graph.New()
				s := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
				req := mcplib.CallToolRequest{}
				req.Params.Name, req.Params.Arguments = "batch_edit", args
				handler := s.boundToolHandler(s.handleAtomicBatchEdit)
				if facade {
					req.Params.Name = "edit"
					req.Params.Arguments = map[string]any{"operation": "batch", "options": args}
					handler = s.mcpServer.GetTool("edit").Handler
				}
				result, err := handler(t.Context(), req)
				require.NoError(t, err)
				require.True(t, result.IsError, viewResultText(t, result))
				_, exists := s.batchTransactions.Load("validation")
				assert.False(t, exists, "invalid request admitted a mutation")
			})
		}
	}
	for _, malformed := range []any{nil, true, "status_only", []any{}} {
		t.Run(fmt.Sprintf("options=%v", malformed), func(t *testing.T) {
			t.Setenv("GORTEX_TOOLS", "facade-v1")
			g := graph.New()
			s := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
			req := mcplib.CallToolRequest{}
			req.Params.Name, req.Params.Arguments = "edit", map[string]any{"operation": "batch", "options": malformed}
			result, err := s.mcpServer.GetTool("edit").Handler(t.Context(), req)
			require.NoError(t, err)
			require.True(t, result.IsError)
			assert.Contains(t, viewResultText(t, result), "options must be an object")
		})
	}
}

func TestBatchStatusOnlyBypassesHeldCheckoutLease(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	f := newRealCheckoutMutationFixture(t)
	checkout, found, err := f.store.Catalog().GetCheckout(t.Context(), f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	route, found, err := f.store.Catalog().GetCheckoutRoute(t.Context(), f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	lease, err := f.srv.lifecycle.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, route.RouteEpoch)
	require.NoError(t, err)
	defer lease.Close()
	const id = "poll-held-checkout-lease"
	receipt := batchTransactionReceipt{Version: batchTransactionVersion, TransactionID: id, Status: "committed", DiskStatus: "committed", GraphStatus: "pending", CheckoutID: f.checkoutID, CheckoutIncarnation: checkout.Incarnation, CheckoutRoot: f.worktree, Files: []batchTransactionFile{{Path: filepath.Join(f.worktree, "edit.go"), RelativePath: "repo/edit.go"}}}
	state := &batchTransactionState{done: make(chan struct{}), receipt: receipt}
	f.srv.batchTransactions.Store(id, state)
	for _, facade := range []bool{false, true} {
		t.Run(fmt.Sprintf("facade=%t", facade), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			ctx = WithSessionCWD(WithSessionID(ctx, "held-lease-poll"), f.primary)
			req := mcplib.CallToolRequest{}
			req.Params.Name = "batch_edit"
			req.Params.Arguments = map[string]any{"transaction_id": id, "status_only": true, "view": map[string]any{"kind": "worktree", "checkout_id": f.checkoutID}}
			handler := f.srv.boundToolHandler(f.srv.handleAtomicBatchEdit)
			if !facade {
				ctx = withCheckoutControl(ctx, &checkoutControlScope{Checkout: checkout, CheckoutScoped: true})
			}
			if facade {
				req.Params.Name = "edit"
				req.Params.Arguments = map[string]any{"operation": "batch", "options": map[string]any{"transaction_id": id, "status_only": true}, "view": map[string]any{"kind": "worktree", "checkout_id": f.checkoutID}, "output": map[string]any{"compact": false}}
				handler = f.srv.mcpServer.GetTool("edit").Handler
			}
			invoke := func() (*mcplib.CallToolResult, error) {
				// View middleware consumes envelope fields from its argument map.
				// Each poll is an independent decoded transport request.
				payload, marshalErr := json.Marshal(req.Params.Arguments)
				require.NoError(t, marshalErr)
				var args map[string]any
				require.NoError(t, json.Unmarshal(payload, &args))
				copyReq := req
				copyReq.Params.Arguments = args
				return handler(ctx, copyReq)
			}
			result, err := invoke()
			require.NoError(t, err)
			require.NotNil(t, result)
			require.False(t, result.IsError, viewResultText(t, result))
			payload := decodeFileOpsResult(t, result)
			assert.Equal(t, "committed", payload["disk_status"])
			assert.Equal(t, "pending", payload["graph_status"])
			assert.Equal(t, receipt, state.snapshot(), "poll must not publish into owner state")
			for _, bad := range []batchTransactionReceipt{
				{TransactionID: id, CheckoutID: "another", CheckoutIncarnation: checkout.Incarnation, CheckoutRoot: f.worktree},
				{TransactionID: id, CheckoutID: f.checkoutID, CheckoutIncarnation: "recreated", CheckoutRoot: f.worktree},
				{TransactionID: id, CheckoutID: f.checkoutID, CheckoutIncarnation: checkout.Incarnation, CheckoutRoot: f.primary},
				{TransactionID: id},
			} {
				state.publish(bad, false)
				result, err = invoke()
				require.NoError(t, err)
				require.True(t, result.IsError, "wrong checkout identity accepted: %+v", bad)
			}
			state.publish(receipt, false)
			if facade {
				args := req.GetArguments()
				args["view"] = map[string]any{"kind": "worktree", "checkout_id": "removed-or-unknown"}
				result, err = invoke()
				require.NoError(t, err)
				require.True(t, result.IsError, "unregistered checkout must fail closed")
			}
		})
	}
}

func TestBatchStatusOnlyDoesNotStartAutoIndex(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	t.Setenv("GORTEX_AUTOINDEX", "0")
	private := t.TempDir()
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, filepath.Join(private, key))
	}
	t.Setenv("GORTEX_QUERY_LOG", filepath.Join(private, "query.jsonl"))
	f := newRealCheckoutMutationFixture(t)
	// The background implementation reads process cwd, not session cwd. Even
	// a regression can therefore only inspect this private, already tracked repo.
	t.Chdir(f.primary)
	t.Setenv("GORTEX_AUTOINDEX", "1")
	require.NotNil(t, f.srv.multiIndexer)
	checkout, found, err := f.store.Catalog().GetCheckout(t.Context(), f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	const id = "poll-no-autoindex"
	f.srv.batchTransactions.Store(id, &batchTransactionState{done: make(chan struct{}), receipt: batchTransactionReceipt{TransactionID: id, Status: "committed", DiskStatus: "committed", GraphStatus: "pending", CheckoutID: f.checkoutID, CheckoutIncarnation: checkout.Incarnation, CheckoutRoot: f.worktree}})
	req := mcplib.CallToolRequest{}
	req.Params.Name, req.Params.Arguments = "edit", map[string]any{"operation": "batch", "options": map[string]any{"transaction_id": id, "status_only": true}, "view": map[string]any{"kind": "worktree", "checkout_id": f.checkoutID}, "output": map[string]any{"compact": false}}
	ctx := WithSessionCWD(WithSessionID(t.Context(), "poll-no-autoindex"), f.primary)
	result, err := f.srv.mcpServer.GetTool("edit").Handler(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError, viewResultText(t, result))
	unused := false
	f.srv.autoIndexOnce.Do(func() { unused = true })
	assert.True(t, unused, "status poll consumed the one-shot auto-index admission")
}

func TestBatchStatusOnlyDoesNotRecoverPreparedJournal(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	path := writeAtomicBatchFixture(t, t.TempDir(), "prepared.txt", "before\n")
	const id = "prepared-poll"
	prepareAtomicRecoveryFixture(t, s, id, []string{path}, []string{"before\n"}, []string{"after\n"})
	require.NoError(t, os.WriteFile(path, []byte("after\n"), 0o644))
	req := mcplib.CallToolRequest{}
	req.Params.Name, req.Params.Arguments = "batch_edit", map[string]any{"transaction_id": id, "status_only": true}
	before, err := os.ReadFile(filepath.Join(batchTransactionDir(id), "manifest.json"))
	require.NoError(t, err)
	result, err := s.handleAtomicBatchEdit(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.IsError, viewResultText(t, result))
	payload := decodeFileOpsResult(t, result)
	assert.Equal(t, "prepared", payload["status"])
	assert.Equal(t, true, payload["recovery_required"])
	assert.Equal(t, "in_flight", payload["disk_status"], "an interrupted prepared manifest cannot certify unchanged disk")
	after, err := os.ReadFile(filepath.Join(batchTransactionDir(id), "manifest.json"))
	require.NoError(t, err)
	assert.Equal(t, before, after, "poll must not rewrite its durable journal")
	assert.Equal(t, "after\n", readAtomicBatchFixture(t, path))
	req.Params.Arguments = map[string]any{"transaction_id": id, "resume": true}
	result, err = s.handleAtomicBatchEdit(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.IsError, viewResultText(t, result))
	var receipt batchTransactionReceipt
	require.NoError(t, json.Unmarshal([]byte(viewResultText(t, result)), &receipt))
	assert.True(t, receipt.Recovered)
	assert.Equal(t, "committed", receipt.DiskStatus)
	assert.Equal(t, "fresh", receipt.GraphStatus)
}
