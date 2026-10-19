package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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

type batchAdmissionGateWatcher struct {
	mutationTestWatcher
	entered chan struct{}
	release chan struct{}
	calls   atomic.Uint64
}

func (w *batchAdmissionGateWatcher) EnqueueFileMutation(_ context.Context, path string) (*indexer.MutationTicket, error) {
	generation := w.calls.Add(1)
	if generation == 1 {
		close(w.entered)
		<-w.release
	}
	done := make(chan indexer.MutationResult, 1)
	done <- indexer.MutationResult{RequestedGeneration: generation, AppliedGeneration: generation, Reindexed: true}
	close(done)
	return &indexer.MutationTicket{Path: path, Generation: generation, Done: done}, nil
}

func TestAtomicBatchCommittedDiskUnlocksBeforeGraphAdmission(t *testing.T) {
	t.Run("commit", func(t *testing.T) { testBatchDiskUnlocksBeforeGraphAdmission(t, false) })
	t.Run("recovery", func(t *testing.T) { testBatchDiskUnlocksBeforeGraphAdmission(t, true) })
}

func testBatchDiskUnlocksBeforeGraphAdmission(t *testing.T, recoverPrepared bool) {
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	g := graph.New()
	s := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	watcher := &batchAdmissionGateWatcher{entered: make(chan struct{}), release: make(chan struct{})}
	s.watcher = watcher
	path := writeAtomicBatchFixture(t, t.TempDir(), "Dockerfile", "before\n")
	request := func(id, before, after string) mcplib.CallToolRequest {
		req := mcplib.CallToolRequest{}
		req.Params.Name = "batch_edit"
		req.Params.Arguments = map[string]any{"transaction_id": id, "edits": []any{
			map[string]any{"op": "edit_file", "path": path, "old_string": before, "new_string": after},
		}}
		return req
	}
	type response struct {
		result *mcplib.CallToolResult
		err    error
	}
	firstDone := make(chan response, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(watcher.release) }) }
	defer unblock()
	if recoverPrepared {
		require.NoError(t, s.persistBatchManifest(batchTransactionReceipt{
			Version: batchTransactionVersion, TransactionID: "first-admission-gated",
			Status: "prepared", DiskStatus: "unchanged", GraphStatus: "not_started",
			Files: []batchTransactionFile{{
				Path: path, RelativePath: "Dockerfile", Mode: 0o600,
				BeforeSHA256: digestBatchBytes([]byte("before\n")), AfterSHA256: digestBatchBytes([]byte("middle\n")),
			}},
		}))
		require.NoError(t, os.WriteFile(path, []byte("middle\n"), 0o600))
	}
	go func() {
		if recoverPrepared {
			_, err := s.batchTransactionStatus(context.Background(), "first-admission-gated")
			firstDone <- response{err: err}
			return
		}
		r, err := s.handleAtomicBatchEdit(context.Background(), request("first-admission-gated", "before", "middle"))
		firstDone <- response{r, err}
	}()
	defer func() {
		unblock()
		select {
		case r := <-firstDone:
			assert.NoError(t, r.err)
			if r.result != nil {
				assert.False(t, r.result.IsError)
			}
		case <-time.After(5 * time.Second):
			t.Error("first batch did not stop after releasing the admission gate")
		}
	}()
	select {
	case <-watcher.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first batch did not reach graph admission")
	}
	require.Equal(t, "middle\n", readAtomicBatchFixture(t, path))
	value, found := s.batchTransactions.Load("first-admission-gated")
	require.True(t, found)
	first := value.(*batchTransactionState).snapshot()
	require.Equal(t, "committed", first.DiskStatus)
	require.Equal(t, "pending", first.GraphStatus)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := s.handleAtomicBatchEdit(ctx, request("second-admission-free", "middle", "after"))
	require.NoError(t, err)
	var second batchTransactionReceipt
	require.NoError(t, json.Unmarshal([]byte(viewResultText(t, result)), &second))
	require.Equal(t, "committed", second.DiskStatus, "a committed batch must not block later edits while admitting graph work: %s", second.Error)
	assert.Equal(t, "after\n", readAtomicBatchFixture(t, path))
	assert.Equal(t, uint64(2), watcher.calls.Load())
}
