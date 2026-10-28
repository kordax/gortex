package indexer

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// A backend that misses its readiness window (a 42k-directory checkout while
// the whole fleet started) used to fail Start and leave the repository
// unwatched until a restart. It now degrades to the adaptive poller.
func TestWatcherReadyTimeoutFallsBackToPoller(t *testing.T) {
	oldTimeout, oldSignal := watcherReadyTimeout, watcherReadySignal
	watcherReadyTimeout = 20 * time.Millisecond
	watcherReadySignal = func(chan struct{}) <-chan struct{} { return nil }
	t.Cleanup(func() { watcherReadyTimeout, watcherReadySignal = oldTimeout, oldSignal })

	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc Main() {}\n")
	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRootPath(dir)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, w.Start([]string{dir}), "a slow backend is not a start failure")
	t.Cleanup(func() { _ = w.Stop() })

	assert.True(t, w.degradedNoFsnotify)
	assert.Nil(t, w.fsw, "the abandoned backend is closed")
	assert.NotNil(t, w.poller, "the adaptive poller covers the repository")
	assert.True(t, strings.Contains(w.DegradedReason(), "adaptive poller"), w.DegradedReason())
}
