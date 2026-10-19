package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func TestBatchCommittedDiskUnlocksBeforeSessionBookkeeping(t *testing.T) {
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	g := graph.New()
	s := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	watcher := &batchDeferredAdmissionWatcher{}
	watcher.ready.Store(true)
	s.watcher = watcher
	path := writeAtomicBatchFixture(t, t.TempDir(), "shared.go", "package main\nfunc Before() {}\n")
	request := func(id, before, after string) mcplib.CallToolRequest {
		r := mcplib.CallToolRequest{}
		r.Params.Name = "batch_edit"
		r.Params.Arguments = map[string]any{"transaction_id": id, "edits": []any{
			map[string]any{"op": "edit_file", "path": path, "old_string": before, "new_string": after},
		}}
		return r
	}
	firstCtx := WithSessionID(context.Background(), "blocked-bookkeeping")
	firstSession := s.sessionFor(firstCtx)
	firstSession.mu.Lock()
	var once sync.Once
	unlock := func() { once.Do(firstSession.mu.Unlock) }
	defer unlock()
	firstDone := make(chan error, 1)
	go func() {
		_, err := s.handleAtomicBatchEdit(firstCtx, request("first-bookkeeping", "Before", "Middle"))
		firstDone <- err
	}()
	defer func() {
		unlock()
		select {
		case err := <-firstDone:
			assert.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("first batch did not stop after session bookkeeping was released")
		}
	}()
	require.Eventually(t, func() bool {
		r, found, err := readBatchManifest("first-bookkeeping")
		return err == nil && found && r.DiskStatus == "committed"
	}, 3*time.Second, time.Millisecond, "first batch must durably commit before session bookkeeping")

	ctx, cancel := context.WithTimeout(WithSessionID(context.Background(), "independent-session"), time.Second)
	defer cancel()
	result, err := s.handleAtomicBatchEdit(ctx, request("second-bookkeeping", "Middle", "After"))
	require.NoError(t, err)
	var receipt batchTransactionReceipt
	require.NoError(t, json.Unmarshal([]byte(viewResultText(t, result)), &receipt))
	require.Equal(t, "committed", receipt.DiskStatus, "session bookkeeping must not retain committed file locks: %s", receipt.Error)
	assert.Equal(t, "package main\nfunc After() {}\n", readAtomicBatchFixture(t, path))
}
