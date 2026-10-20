package mcp

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

func TestBatchStatusSnapshotRequiresMatchingReceipts(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.go")
	local := &mutationReceipt{id: "known", path: path, generation: 7, done: make(chan struct{}), completed: true, result: indexer.MutationResult{RequestedGeneration: 7, AppliedGeneration: 8, Reindexed: true}}
	close(local.done)
	s := &Server{}
	s.mutationReceipts.Store(local.id, local)
	file := batchTransactionFile{Path: path, RelativePath: "file.go", ReindexReceipt: local.id, ReindexGeneration: 7}
	for _, tc := range []struct {
		name         string
		files        []batchTransactionFile
		status, want string
	}{
		{"known", []batchTransactionFile{file}, "pending", "fresh"},
		{"no files", nil, "pending", "pending"},
		{"missing ticket", []batchTransactionFile{{Path: path, ReindexGeneration: 7}}, "pending", "pending"},
		{"missing generation", []batchTransactionFile{{Path: path, ReindexReceipt: local.id}}, "pending", "pending"},
		{"wrong generation", []batchTransactionFile{{Path: path, ReindexReceipt: local.id, ReindexGeneration: 9}}, "pending", "pending"},
		{"wrong path", []batchTransactionFile{{Path: filepath.Join(root, "other.go"), ReindexReceipt: local.id, ReindexGeneration: 7}}, "pending", "pending"},
		{"duplicate path", []batchTransactionFile{file, file}, "pending", "pending"},
		{"incomplete set", []batchTransactionFile{file, {Path: filepath.Join(root, "other.go")}}, "pending", "pending"},
		{"failed evidence retained", []batchTransactionFile{{Path: path}}, "failed", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receipt := batchTransactionReceipt{Status: "committed", DiskStatus: "committed", GraphStatus: tc.status, Files: tc.files, Results: []batchEditResult{{FilePath: "file.go"}}}
			s.projectBatchGraphSnapshot(&receipt)
			assert.Equal(t, tc.want, receipt.GraphStatus)
		})
	}
}

func TestBatchStatusSnapshotDoesNotTrustRestartedReceiptIDs(t *testing.T) {
	var scheduled atomic.Int64
	s := newAtomicBatchTestServer(t, mutationTestWatcher{scheduled: &scheduled})
	path := writeAtomicBatchFixture(t, t.TempDir(), "restart.go", "package fixture\n")
	const id = "poll-restarted-primary"
	receipt := batchTransactionReceipt{Version: batchTransactionVersion, TransactionID: id, Status: "committed", DiskStatus: "committed", GraphStatus: "pending", Files: []batchTransactionFile{{Path: path, RelativePath: path, ReindexReceipt: "reused", ReindexGeneration: 7}}}
	require.NoError(t, s.persistBatchManifest(receipt))
	local := &mutationReceipt{id: "reused", path: path, generation: 7, done: make(chan struct{}), completed: true, result: indexer.MutationResult{RequestedGeneration: 7, AppliedGeneration: 8, Reindexed: true}}
	close(local.done)
	s.mutationReceipts.Store(local.id, local)
	polled, err := s.pollBatchTransaction(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "pending", polled.GraphStatus, "a previous daemon's numeric ID cannot certify this transaction")
	assert.True(t, polled.ResumeAvailable)
	_, cached := s.batchTransactions.Load(id)
	assert.False(t, cached, "poll must not create recovery state")
	assert.Zero(t, scheduled.Load())
	resumed, err := s.batchTransactionStatus(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "fresh", resumed.GraphStatus)
	assert.Equal(t, int64(1), scheduled.Load(), "explicit recovery must acquire a current publication ticket")
}

func TestBatchStatusUncachedResumePreservesCancellation(t *testing.T) {
	watcher := &batchPollBarrierWatcher{entered: make(chan struct{}), release: make(chan struct{})}
	s := newAtomicBatchTestServer(t, watcher)
	path := writeAtomicBatchFixture(t, t.TempDir(), "uncached.go", "package fixture\n")
	const id = "uncached-canceled-resume"
	receipt := batchTransactionReceipt{Version: batchTransactionVersion, TransactionID: id, Status: "committed", DiskStatus: "committed", GraphStatus: "pending", Files: []batchTransactionFile{{Path: path, RelativePath: path}}}
	require.NoError(t, s.persistBatchManifest(receipt))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	var resumeErr error
	go func() { defer close(done); _, resumeErr = s.batchTransactionStatus(ctx, id) }()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(watcher.release) }) }
	t.Cleanup(func() {
		unblock()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("resume owner leaked")
		}
	})
	select {
	case <-watcher.entered:
	case <-time.After(time.Second):
		t.Fatal("resume did not enter refresh")
	}
	cancel()
	unblock()
	select {
	case <-done:
		assert.ErrorIs(t, resumeErr, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("resume did not finish")
	}
	polled, err := s.pollBatchTransaction(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "committed", polled.DiskStatus)
	assert.Equal(t, "fresh", polled.GraphStatus)
}

func TestBatchStatusLegacyEnvelopeCannotHideMutationIntent(t *testing.T) {
	for _, args := range []map[string]any{
		{"transaction_id": "id", "edits": []any{}, "options": map[string]any{"status_only": true}},
		{"transaction_id": "id", "options": nil},
		{"transaction_id": "id", "arguments": map[string]any{"resume": true}},
	} {
		_, err := classifyBatchArguments(args)
		require.Error(t, err)
	}
	s := &Server{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.pollBatchTransaction(ctx, "unknown")
	assert.ErrorIs(t, err, context.Canceled)
}
