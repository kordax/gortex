package mcp

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Without a watcher (the daemon until warmup starts them) the reindex pass
// waited on the resolver lock behind deferred enrichment and held the edit
// for the whole 59 s request deadline. The edit now answers pending after the
// usual bounded wait, and a second edit folds into one more pass.
func TestReindexFileBoundedAnswersPendingWhileThePassWaits(t *testing.T) {
	release := make(chan struct{})
	var passes atomic.Int32
	s := &Server{mutationReindexWait: 50 * time.Millisecond}
	s.detachedReindexHook = func(string) bool {
		passes.Add(1)
		<-release
		return true
	}

	start := time.Now()
	first := s.reindexFileBounded(t.Context(), "/repo/a.go")
	assert.True(t, first.Pending)
	assert.False(t, first.Reindexed)
	assert.Less(t, time.Since(start), 2*time.Second, "the edit does not wait for the pass")

	second := s.reindexFileBounded(t.Context(), "/repo/a.go")
	assert.True(t, second.Pending, "an edit during a running pass joins it")

	close(release)
	require.Eventually(t, func() bool {
		s.detachedReindexMu.Lock()
		defer s.detachedReindexMu.Unlock()
		return len(s.detachedReindex) == 0
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, int32(2), passes.Load(), "the joined edit costs one more pass, not a second worker")
}

func TestReindexFileBoundedReportsAFastPass(t *testing.T) {
	s := &Server{}
	s.detachedReindexHook = func(string) bool { return true }
	out := s.reindexFileBounded(t.Context(), "/repo/a.go")
	assert.True(t, out.Reindexed)
	assert.False(t, out.Pending)
}

func TestEditFileWithoutWatcherDoesNotWaitForTheGraph(t *testing.T) {
	srv, dir := setupTestServer(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "slow.go"), []byte("package p\n\nfunc A() int { return 1 }\n"), 0o644))
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv.mutationReindexWait = 50 * time.Millisecond
	srv.detachedReindexHook = func(string) bool { <-release; return true }

	start := time.Now()
	m := extractTextResult(t, callTool(t, srv, "edit_file", map[string]any{
		"path": "slow.go", "old_string": "return 1", "new_string": "return 2", "format": "json",
	}))
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Equal(t, "applied", m["status"])
	assert.Equal(t, "pending", m["graph_status"])
	data, err := os.ReadFile(filepath.Join(dir, "slow.go"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "return 2", "the bytes are on disk before the graph catches up")
}
