package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newBatchSymlinkFixture(t *testing.T) (*Server, string, string, string, []batchEditItem) {
	t.Helper()
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	dir := t.TempDir()
	target := writeAtomicBatchFixture(t, dir, "target.txt", "before\n")
	link := filepath.Join(dir, "a-link.txt")
	if err := os.Symlink("target.txt", link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		require.NoError(t, err)
	}
	last := writeAtomicBatchFixture(t, dir, "z-last.txt", "last-before\n")
	return s, link, target, last, []batchEditItem{
		atomicFileEdit(link, "before", "after"),
		atomicFileEdit(last, "last-before", "last-after"),
	}
}

func requireBatchSymlinkRestored(t *testing.T, link, target, last string) {
	t.Helper()
	assert.Equal(t, "before\n", readAtomicBatchFixture(t, target))
	assert.Equal(t, "before\n", readAtomicBatchFixture(t, link))
	assert.Equal(t, "last-before\n", readAtomicBatchFixture(t, last))
	spelling, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, "target.txt", spelling)
}

func TestAtomicBatchRollbackRestoresSymlinkEntry(t *testing.T) {
	s, link, target, last, edits := newBatchSymlinkFixture(t)
	s.batchWriteOverride = func(path string, data []byte, mode os.FileMode) error {
		if path == last {
			return errors.New("injected last target failure")
		}
		return durableAtomicWriteFile(path, data, mode)
	}
	receipt, err := s.runBatchTransaction(context.Background(), edits, "symlink-abort")
	require.NoError(t, err)
	require.Equal(t, "aborted", receipt.Status)
	require.Equal(t, "rolled_back", receipt.DiskStatus)
	requireBatchSymlinkRestored(t, link, target, last)
}

func TestAtomicBatchRecoveryRestoresSymlinkEntry(t *testing.T) {
	s, link, target, last, edits := newBatchSymlinkFixture(t)
	ctx := context.Background()
	plans := s.planBatchTransaction(ctx, edits, true)
	for _, plan := range plans {
		require.Empty(t, plan.err)
	}
	buffers, paths, err := s.readBatchBuffers(ctx, plans)
	require.NoError(t, err)
	results, _, err := applyBatchPlans(plans, buffers)
	require.NoError(t, err)
	receipt := batchTransactionReceipt{
		Version: batchTransactionVersion, TransactionID: "symlink-recovery",
		Fingerprint: batchEditFingerprint(edits), StartedAt: time.Now().UTC(),
		Results: results, Summary: batchSummary(results),
	}
	require.NoError(t, s.prepareBatchJournal(&receipt, buffers, paths))
	// A crash after the first rename leaves one after-image and one before-image.
	require.NoError(t, durableAtomicWriteFile(link, buffers[link].content, buffers[link].mode))
	restarted := &Server{watcher: mutationTestWatcher{}, session: newSessionState()}
	recovered, err := restarted.batchTransactionStatus(ctx, receipt.TransactionID)
	require.NoError(t, err)
	assert.True(t, recovered.Recovered)
	require.Equal(t, "aborted", recovered.Status)
	require.Equal(t, "rolled_back", recovered.DiskStatus)
	requireBatchSymlinkRestored(t, link, target, last)
}

func TestBatchRecoveryClassifiesDirectoryEntryIdentity(t *testing.T) {
	t.Run("unchanged bytes still restore the original link", func(t *testing.T) {
		s, link, _, _, _ := newBatchSymlinkFixture(t)
		before := []byte("before\n")
		receipt := batchTransactionReceipt{Version: batchTransactionVersion, TransactionID: "same-bytes-link"}
		buffer := &batchFileBuffer{
			absPath: link, relPath: "a-link.txt", mode: 0o644,
			original: before, content: before, beforeSymlinkTarget: "target.txt",
			existsBefore: true, existsAfter: true, existenceSet: true,
		}
		require.NoError(t, s.prepareBatchJournal(&receipt, map[string]*batchFileBuffer{link: buffer}, []string{link}))
		require.NoError(t, durableAtomicWriteFile(link, before, 0o644))
		status, err := s.rollbackBatchReceipt(receipt)
		require.NoError(t, err)
		assert.Equal(t, "aborted", status)
		spelling, err := os.Readlink(link)
		require.NoError(t, err)
		assert.Equal(t, "target.txt", spelling)
	})
	t.Run("a different symlink with the same bytes is a conflict", func(t *testing.T) {
		s, link, _, _, _ := newBatchSymlinkFixture(t)
		other := writeAtomicBatchFixture(t, filepath.Dir(link), "other.txt", "before\n")
		require.NoError(t, os.Remove(link))
		require.NoError(t, os.Symlink(other, link))
		receipt := batchTransactionReceipt{Files: []batchTransactionFile{{
			Path: link, RelativePath: "a-link.txt", BeforeSymlinkTarget: "target.txt",
			BeforeSHA256: digestBatchBytes([]byte("before\n")), AfterSHA256: digestBatchBytes([]byte("after\n")),
		}}}
		status, err := s.rollbackBatchReceipt(receipt)
		require.ErrorContains(t, err, "unknown disk state")
		assert.Equal(t, "recovery_conflict", status)
		spelling, err := os.Readlink(link)
		require.NoError(t, err)
		assert.Equal(t, other, spelling)
		assert.Equal(t, "before\n", readAtomicBatchFixture(t, other))
	})
}
