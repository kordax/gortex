package mcp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mutationDirectoryAlias(t *testing.T, root string) (string, string) {
	t.Helper()
	realDir := filepath.Join(root, "real")
	require.NoError(t, os.Mkdir(realDir, 0o755))
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("directory symlinks unavailable: %v", err)
		}
		require.NoError(t, err)
	}
	return realDir, alias
}

func TestConcurrentBatchDirectoryAliasesPreserveBothEdits(t *testing.T) {
	srv := newAtomicBatchTestServer(t, mutationTestWatcher{})
	realDir, aliasDir := mutationDirectoryAlias(t, t.TempDir())
	path := writeAtomicBatchFixture(t, realDir, "source.txt", "left=old\nright=old\n")
	alias := filepath.Join(aliasDir, "source.txt")
	ready, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	srv.batchWriteOverride = func(p string, data []byte, mode os.FileMode) error {
		if p == path {
			close(ready)
			<-release
		}
		return durableAtomicWriteFile(p, data, mode)
	}
	type outcome struct {
		receipt batchTransactionReceipt
		err     error
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, second := make(chan outcome, 1), make(chan outcome, 1)
	go func() {
		receipt, err := srv.runBatchTransaction(ctx, []batchEditItem{atomicFileEdit(path, "left=old", "left=A")}, "directory-alias-first")
		first <- outcome{receipt: receipt, err: err}
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("first transaction did not reach its write seam")
	}
	go func() {
		receipt, err := srv.runBatchTransaction(ctx, []batchEditItem{atomicFileEdit(alias, "right=old", "right=B")}, "directory-alias-second")
		second <- outcome{receipt: receipt, err: err}
	}()
	var secondResult outcome
	secondFinished := false
	select {
	case secondResult = <-second:
		secondFinished = true
	case <-time.After(500 * time.Millisecond):
	}
	unblock()
	select {
	case result := <-first:
		require.NoError(t, result.err)
		require.Equal(t, "committed", result.receipt.Status)
	case <-ctx.Done():
		t.Fatal("first transaction did not finish")
	}
	if !secondFinished {
		select {
		case secondResult = <-second:
		case <-ctx.Done():
			t.Fatal("second transaction did not finish")
		}
	}
	require.NoError(t, secondResult.err)
	require.Equal(t, "committed", secondResult.receipt.Status)
	assert.Equal(t, "left=A\nright=B\n", readAtomicBatchFixture(t, path), "both committed edits must survive")
}

func TestMutationPathSetsDeduplicateDirectoryAliases(t *testing.T) {
	realDir, aliasDir := mutationDirectoryAlias(t, t.TempDir())
	for _, relative := range []string{"source.txt", filepath.Join("not-created", "source.txt")} {
		t.Run(relative, func(t *testing.T) {
			path, alias := filepath.Join(realDir, relative), filepath.Join(aliasDir, relative)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			release, err := acquireMutationPaths(ctx, []string{path, alias})
			require.NoError(t, err, "a batch must not wait on its own alias lock")
			defer release()
			probeCtx, probeCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer probeCancel()
			otherRelease, err := acquireMutationPath(probeCtx, alias)
			if otherRelease != nil {
				otherRelease()
			}
			assert.ErrorIs(t, err, context.DeadlineExceeded, "another caller must share the same parent-alias lock")
		})
	}
}

func TestMutationLocksPreserveLeafSymlinkEntryIdentity(t *testing.T) {
	root := t.TempDir()
	target := writeAtomicBatchFixture(t, root, "target.txt", "target\n")
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink("target.txt", link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("file symlinks unavailable: %v", err)
		}
		require.NoError(t, err)
	}
	release, err := acquireMutationPath(context.Background(), target)
	require.NoError(t, err)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	linkRelease, err := acquireMutationPath(ctx, link)
	require.NoError(t, err, "replacing a symlink entry must not lock its separate target entry")
	linkRelease()
}
