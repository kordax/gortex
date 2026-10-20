package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

func batchTimeoutVerdict(t *testing.T, result *mcplib.CallToolResult) mutationCommitVerdict {
	t.Helper()
	require.True(t, result.IsError)
	_, payload, found := strings.Cut(viewResultText(t, result), "mutation_commit=")
	require.True(t, found, "a batch timeout must preserve its recovery identity")
	var verdict mutationCommitVerdict
	require.NoError(t, json.Unmarshal([]byte(payload), &verdict))
	require.Len(t, verdict.Batches, 1)
	return verdict
}

func waitBatchTimeoutTerminal(t *testing.T, state *batchTransactionState) {
	t.Helper()
	select {
	case <-state.done:
	case <-time.After(5 * time.Second):
		t.Fatal("batch did not finish after its test gate was released")
	}
	require.Eventually(t, func() bool { return abandonedToolCalls.Load() == 0 }, time.Second, time.Millisecond)
}

func TestAtomicBatchTimeoutPreservesCommittedReceipt(t *testing.T) {
	for _, facade := range []bool{false, true} {
		name := "atomic-handler"
		if facade {
			name = "facade"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("GORTEX_TOOLS", "facade-v1")
			t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
			done := make(chan indexer.MutationResult, 2)
			g := graph.New()
			s := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
			watcher := &batchPollBarrierWatcher{mutationTestWatcher: mutationTestWatcher{done: done, generation: 7}, entered: make(chan struct{}), release: make(chan struct{})}
			s.watcher = watcher
			s.ToolCallTimeout = time.Second
			s.mutationReindexWait = 5 * time.Second
			dir := t.TempDir()
			first := writeAtomicBatchFixture(t, dir, "first.txt", "before-a\n")
			second := writeAtomicBatchFixture(t, dir, "second.txt", "before-b\n")
			changes := []any{
				map[string]any{"op": "edit_file", "path": first, "old_string": "before-a", "new_string": "after-a"},
				map[string]any{"op": "edit_file", "path": second, "old_string": "before-b", "new_string": "after-b"},
			}
			req := mcplib.CallToolRequest{}
			req.Params.Name = "batch_edit"
			const id = "committed-before-timeout"
			req.Params.Arguments = map[string]any{"transaction_id": id, "edits": changes}
			handler := s.boundToolHandler(s.handleAtomicBatchEdit)
			if facade {
				tool := s.mcpServer.GetTool("edit")
				require.NotNil(t, tool)
				handler = tool.Handler
				req.Params.Name = "edit"
				req.Params.Arguments = map[string]any{"operation": "batch", "changes": changes, "options": map[string]any{"transaction_id": id}}
			}
			// Bootstrap the real transaction without a tool deadline. The retry's
			// real timeout starts only after durable commit and refresh admission;
			// filesystem speed must not choose which state this test exercises.
			owner := make(chan fallbackWriteResult, 1)
			ownerReq := makeReq("batch_edit", map[string]any{"transaction_id": id, "edits": changes})
			var state *batchTransactionState
			t.Cleanup(func() {
				close(watcher.release)
				for range 2 {
					done <- indexer.MutationResult{RequestedGeneration: 7, AppliedGeneration: 7, Reindexed: true}
				}
				close(done)
				select {
				case outcome := <-owner:
					assert.NoError(t, outcome.err)
					if assert.NotNil(t, outcome.result) {
						assert.False(t, outcome.result.IsError, viewResultText(t, outcome.result))
					}
				case <-time.After(5 * time.Second):
					t.Error("committed owner did not finish after release")
				}
				if state != nil {
					waitBatchTimeoutTerminal(t, state)
				}
			})
			go func() {
				result, err := s.handleAtomicBatchEdit(t.Context(), ownerReq)
				owner <- fallbackWriteResult{result: result, err: err}
			}()
			select {
			case <-watcher.entered:
			case outcome := <-owner:
				owner <- outcome
				t.Fatalf("transaction returned before refresh admission: result=%+v err=%v", outcome.result, outcome.err)
			case <-time.After(5 * time.Second):
				t.Fatal("transaction did not enter refresh admission")
			}
			value, found := s.batchTransactions.Load(id)
			require.True(t, found)
			state = value.(*batchTransactionState)
			require.Equal(t, "committed", state.snapshot().DiskStatus)
			require.Equal(t, "pending", state.snapshot().GraphStatus)
			result, err := handler(t.Context(), req)
			require.NoError(t, err)
			assert.Equal(t, int64(1), watcher.attempts.Load(), "retry must not replay the committed write")
			verdict := batchTimeoutVerdict(t, result)
			assert.Equal(t, mutationDiskCommitted, verdict.Status)
			batch := verdict.Batches[0]
			assert.Equal(t, state.snapshot().TransactionID, batch.TransactionID)
			assert.NotEmpty(t, batch.TransactionID)
			assert.Equal(t, "committed", batch.DiskStatus)
			assert.Equal(t, "pending", batch.GraphStatus)
			assert.Equal(t, "batch", batch.StatusRequest["operation"])
			assert.Equal(t, batch.TransactionID, batch.StatusRequest["options"].(map[string]any)["transaction_id"])
			assert.Equal(t, true, batch.StatusRequest["options"].(map[string]any)["status_only"])
			assert.Equal(t, "after-a\n", readAtomicBatchFixture(t, first))
			assert.Equal(t, "after-b\n", readAtomicBatchFixture(t, second))
		})
	}
}

func TestAtomicBatchTimeoutRemainsUnknownUntilTerminalDiskState(t *testing.T) {
	for _, beforeCommit := range []bool{false, true} {
		name := "partial-write-then-rollback"
		if beforeCommit {
			name = "cancel-before-first-write"
		}
		t.Run(name, func(t *testing.T) {
			s := newAtomicBatchTestServer(t, mutationTestWatcher{})
			s.ToolCallTimeout = time.Second
			dir := t.TempDir()
			first := writeAtomicBatchFixture(t, dir, "a-first.txt", "before-a\n")
			last := writeAtomicBatchFixture(t, dir, "z-last.txt", "before-b\n")
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			targetWrites := 0
			s.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
				targetWrites++
				if path == last && !beforeCommit {
					<-release
					return errors.New("injected late target failure")
				}
				return durableAtomicWriteFile(path, content, mode)
			}
			if beforeCommit {
				var gate sync.Once
				s.batchDurabilityOverride = &batchDurabilityOps{writeFile: func(path string, content []byte, mode os.FileMode) error {
					if filepath.Base(path) == "manifest.json" {
						var receipt batchTransactionReceipt
						if err := json.Unmarshal(content, &receipt); err != nil {
							return err
						}
						if receipt.Status == "prepared" {
							gate.Do(func() { <-release })
						}
					}
					return durableAtomicWriteFile(path, content, mode)
				}}
			}
			req := mcplib.CallToolRequest{}
			req.Params.Name = "batch_edit"
			req.Params.Arguments = map[string]any{"transaction_id": name, "edits": []any{
				map[string]any{"op": "edit_file", "path": first, "old_string": "before-a", "new_string": "after-a"},
				map[string]any{"op": "edit_file", "path": last, "old_string": "before-b", "new_string": "after-b"},
			}}
			result, err := s.boundToolHandler(s.handleAtomicBatchEdit)(context.Background(), req)
			require.NoError(t, err)
			value, ok := s.batchTransactions.Load(name)
			require.True(t, ok)
			state := value.(*batchTransactionState)
			defer func() { unblock(); waitBatchTimeoutTerminal(t, state) }()
			verdict := batchTimeoutVerdict(t, result)
			assert.Equal(t, mutationDiskInFlight, verdict.Status)
			assert.Equal(t, name, verdict.Batches[0].TransactionID)
			unblock()
			waitBatchTimeoutTerminal(t, state)
			assert.Equal(t, "aborted", state.snapshot().Status)
			if beforeCommit {
				assert.Zero(t, targetWrites)
				assert.Equal(t, "unchanged", state.snapshot().DiskStatus)
			} else {
				assert.Equal(t, "rolled_back", state.snapshot().DiskStatus)
			}
			assert.Equal(t, "before-a\n", readAtomicBatchFixture(t, first))
			assert.Equal(t, "before-b\n", readAtomicBatchFixture(t, last))
		})
	}
}

func TestBatchMutationNoteTracksTerminalStatesAndCheckout(t *testing.T) {
	var empty *mutationCommitNote
	empty.observeBatch(nil)
	for _, tc := range []struct{ status, disk, want string }{
		{"preparing", "unchanged", mutationDiskInFlight},
		{"prepared", "unchanged", mutationDiskInFlight},
		{"committed", "committed", mutationDiskCommitted},
		{"aborted", "unchanged", mutationDiskNotApplied},
		{"aborted", "rolled_back", mutationDiskNotApplied},
		{"recovery_conflict", "conflict", mutationDiskInFlight},
	} {
		t.Run(tc.status+"/"+tc.disk, func(t *testing.T) {
			state := &batchTransactionState{receipt: batchTransactionReceipt{
				TransactionID: "batch-id", Status: tc.status, DiskStatus: tc.disk,
				CheckoutID: "checkout-id", CheckoutIncarnation: "incarnation-id",
			}}
			note := &mutationCommitNote{}
			note.observeBatch(state)
			note.observeBatch(state)
			verdict := note.verdict()
			assert.Equal(t, tc.want, verdict.Status)
			require.Len(t, verdict.Batches, 1)
			assert.Equal(t, "incarnation-id", verdict.Batches[0].CheckoutIncarnation)
			assert.Equal(t, map[string]any{"kind": "worktree", "checkout_id": "checkout-id"}, verdict.Batches[0].StatusRequest["view"])
		})
	}
}
