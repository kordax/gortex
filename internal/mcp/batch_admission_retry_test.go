package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

type batchDeferredAdmissionWatcher struct {
	mutationTestWatcher
	ready    atomic.Bool
	attempts atomic.Uint64
}

func (w *batchDeferredAdmissionWatcher) EnqueueFileMutation(_ context.Context, path string) (*indexer.MutationTicket, error) {
	generation := w.attempts.Add(1)
	if !w.ready.Load() {
		return nil, errors.New("watcher lifecycle busy: mutation admission deferred")
	}
	done := make(chan indexer.MutationResult, 1)
	done <- indexer.MutationResult{RequestedGeneration: generation, AppliedGeneration: generation, Reindexed: true}
	close(done)
	return &indexer.MutationTicket{Path: path, Generation: generation, Done: done}, nil
}

func TestBatchDeferredAdmissionStatusRetriesOnlyGraph(t *testing.T) {
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	g := graph.New()
	s := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	watcher := &batchDeferredAdmissionWatcher{}
	s.watcher = watcher
	path := writeAtomicBatchFixture(t, t.TempDir(), "main.go", "package main\nfunc Before() {}\n")
	var writes atomic.Int32
	s.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
		writes.Add(1)
		return durableAtomicWriteFile(path, content, mode)
	}
	req := mcplib.CallToolRequest{}
	req.Params.Name = "batch_edit"
	req.Params.Arguments = map[string]any{"transaction_id": "deferred-admission", "edits": []any{
		map[string]any{"op": "edit_file", "path": path, "old_string": "Before", "new_string": "After"},
	}}
	result, err := s.handleAtomicBatchEdit(context.Background(), req)
	require.NoError(t, err)
	var receipt batchTransactionReceipt
	require.NoError(t, json.Unmarshal([]byte(viewResultText(t, result)), &receipt))
	assert.Equal(t, "committed", receipt.Status)
	assert.Equal(t, "committed", receipt.DiskStatus)
	assert.Equal(t, "failed", receipt.GraphStatus, "an unadmitted refresh must never claim graph freshness")
	require.Len(t, receipt.Results, 1)
	assert.Contains(t, receipt.Results[0].ReindexError, "mutation admission deferred")
	assert.Equal(t, "package main\nfunc After() {}\n", readAtomicBatchFixture(t, path))
	assert.Equal(t, int32(1), writes.Load())

	watcher.ready.Store(true)
	receipt, err = s.batchTransactionStatus(context.Background(), "deferred-admission")
	require.NoError(t, err)
	assert.Equal(t, "committed", receipt.DiskStatus)
	assert.Equal(t, "fresh", receipt.GraphStatus)
	assert.Equal(t, uint64(2), watcher.attempts.Load())
	assert.Equal(t, int32(1), writes.Load(), "checking status must not repeat the disk mutation")
	assert.Equal(t, "package main\nfunc After() {}\n", readAtomicBatchFixture(t, path))
}
