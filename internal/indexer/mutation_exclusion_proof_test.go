package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
)

func exclusionProofFixture(t *testing.T, store graph.Store) (*Watcher, *Indexer, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	idx := New(store, parser.NewRegistry(), config.IndexConfig{Exclude: []string{"**/*_test.go"}}, zap.NewNop())
	idx.rootPath = root
	idx.repoPrefix = "sample"
	w, err := NewWatcher(idx, config.WatchConfig{Exclude: []string{"**/*_test.go"}}, zap.NewNop())
	require.NoError(t, err)
	path := filepath.Join(root, "new_test.go")
	require.NoError(t, os.WriteFile(path, []byte("package sample\n"), 0o644))
	return w, idx, path
}

func TestWatcherExclusionProofIsConservative(t *testing.T) {
	store := graph.New()
	w, _, path := exclusionProofFixture(t, store)
	skip, err := w.CanSkipFileMutation(t.Context(), path)
	require.NoError(t, err)
	require.True(t, skip)

	store.AddNode(&graph.Node{ID: "old", Kind: graph.KindFunction, Name: "Old", FilePath: "sample/new_test.go"})
	skip, err = w.CanSkipFileMutation(t.Context(), path)
	require.NoError(t, err)
	assert.False(t, skip, "previously indexed nodes at the prefixed canonical key require reconciliation")
	w, idx, path := exclusionProofFixture(t, graph.New())

	idx.SetExcludePatterns([]string{"**/*.tmp"})
	skip, err = w.CanSkipFileMutation(t.Context(), path)
	require.NoError(t, err)
	assert.False(t, skip, "watcher exclusion alone cannot suppress indexer work")
	idx.SetExcludePatterns([]string{"**/*_test.go"})
	w.excludes = excludes.New([]string{"**/*.tmp"})
	skip, err = w.CanSkipFileMutation(t.Context(), path)
	require.NoError(t, err)
	assert.False(t, skip, "indexer exclusion alone cannot explain nil watcher admission")
	w.excludes = excludes.New([]string{"**/*_test.go"})

	for _, candidate := range []string{filepath.Join(t.TempDir(), "outside_test.go"), filepath.Dir(path), filepath.Join(filepath.Dir(path), "missing_test.go")} {
		skip, _ := w.CanSkipFileMutation(t.Context(), candidate)
		assert.False(t, skip, candidate)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	skip, err = w.CanSkipFileMutation(ctx, path)
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, skip)
}

func TestWatcherExclusionProofRejectsSymlinkAliases(t *testing.T) {
	w, _, path := exclusionProofFixture(t, graph.New())
	alias := filepath.Join(filepath.Dir(path), "alias_test.go")
	if err := os.Symlink(path, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	skip, err := w.CanSkipFileMutation(t.Context(), alias)
	require.NoError(t, err)
	assert.False(t, skip)
	parentAlias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(filepath.Dir(path), parentAlias))
	skip, err = w.CanSkipFileMutation(t.Context(), filepath.Join(parentAlias, filepath.Base(path)))
	require.NoError(t, err)
	assert.False(t, skip)
}

type unknownExclusionStore struct{ graph.Store }

type errorExclusionStore struct {
	graph.Store
	err error
}

func (s errorExclusionStore) FindFileNodesBounded(context.Context, string, graph.LocalizationNodeScope, int) (graph.BoundedNodeProjection, error) {
	return graph.BoundedNodeProjection{}, s.err
}

func TestWatcherExclusionProofRequiresSuccessfulGraphRead(t *testing.T) {
	lookupErr := errors.New("file lookup failed")
	for _, tc := range []struct {
		name  string
		store graph.Store
		err   error
	}{
		{name: "unsupported", store: unknownExclusionStore{graph.New()}},
		{name: "query_error", store: errorExclusionStore{Store: graph.New(), err: lookupErr}, err: lookupErr},
		{name: "query_canceled", store: errorExclusionStore{Store: graph.New(), err: context.Canceled}, err: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _, path := exclusionProofFixture(t, tc.store)
			skip, err := w.CanSkipFileMutation(t.Context(), path)
			assert.False(t, skip)
			assert.ErrorIs(t, err, tc.err)
		})
	}
}

func TestWatcherExclusionProofRejectsClosedSQLite(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
	require.NoError(t, err)
	w, _, path := exclusionProofFixture(t, store)
	require.NoError(t, store.Close())
	skip, err := w.CanSkipFileMutation(t.Context(), path)
	assert.Error(t, err, "a closed database cannot prove the absence of old nodes")
	assert.False(t, skip)
}

type blockedExclusionStore struct {
	graph.Store
	entered chan struct{}
	release chan struct{}
}

func (s blockedExclusionStore) FindFileNodesBounded(ctx context.Context, _ string, _ graph.LocalizationNodeScope, _ int) (graph.BoundedNodeProjection, error) {
	close(s.entered)
	select {
	case <-s.release:
		return graph.BoundedNodeProjection{}, nil
	case <-ctx.Done():
		return graph.BoundedNodeProjection{}, ctx.Err()
	}
}

func TestMultiWatcherExclusionProofReleasesStateBeforeGraphRead(t *testing.T) {
	mw, _, root, _ := setupMultiWatcherTest(t)
	defer func() { assert.NoError(t, mw.Stop()) }()
	w := mw.watchers["repo-a"]
	idx := w.currentMutationIndexer()
	idx.SetExcludePatterns([]string{"**/*_test.go"})
	w.excludes = excludes.New([]string{"**/*_test.go"})
	path := filepath.Join(root, "new_test.go")
	require.NoError(t, os.WriteFile(path, []byte("package sample\n"), 0o644))
	skip, err := mw.CanSkipFileMutation(t.Context(), path)
	require.NoError(t, err)
	assert.False(t, skip, "unstarted watcher cannot prove admission policy")
	// Model only the route's started state, without launching filesystem work.
	mw.started["repo-a"] = true
	defer func() { mw.started["repo-a"] = false }()
	blocked := blockedExclusionStore{Store: idx.graph, entered: make(chan struct{}), release: make(chan struct{})}
	idx.graph = blocked
	var once sync.Once
	release := func() { once.Do(func() { close(blocked.release) }) }
	defer release()
	done := make(chan error, 1)
	go func() {
		skip, err := mw.CanSkipFileMutation(t.Context(), path)
		if err == nil && !skip {
			err = errors.New("expected proven exclusion after releasing lookup")
		}
		done <- err
	}()
	select {
	case <-blocked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("proof did not reach graph lookup")
	}
	locked := mw.mu.TryLock()
	if locked {
		mw.mu.Unlock()
	}
	assert.True(t, locked, "lifecycle mutex must not be held across graph I/O")
	updated := make(chan struct{})
	go func() {
		idx.SetExcludePatterns([]string{"**/*_test.go"})
		close(updated)
	}()
	select {
	case <-updated:
	case <-time.After(5 * time.Second):
		t.Error("exclude cache mutex remained held across graph I/O")
	}
	release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("proof did not stop after releasing graph lookup")
	}
}
