package mcp

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAtomicBatchCancelledRetriesDoNotCancelOwner(t *testing.T) {
	var scheduled, writes atomic.Int64
	s := newAtomicBatchTestServer(t, mutationTestWatcher{scheduled: &scheduled})
	dir := t.TempDir()
	first := writeAtomicBatchFixture(t, dir, "a-first.txt", "before-a\n")
	last := writeAtomicBatchFixture(t, dir, "z-last.txt", "before-b\n")
	independent := writeAtomicBatchFixture(t, dir, "independent.txt", "independent-before\n")
	edits := []batchEditItem{atomicFileEdit(first, "before-a", "after-a"), atomicFileEdit(last, "before-b", "after-b")}
	entered, release := make(chan struct{}), make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(release) }) }
	s.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
		if err := durableAtomicWriteFile(path, content, mode); err != nil {
			return err
		}
		writes.Add(1)
		if path == first {
			close(entered)
			<-release
		}
		return nil
	}
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()
	ownerDone := make(chan struct{})
	var ownerReceipt batchTransactionReceipt
	var ownerErr error
	go func() {
		defer close(ownerDone)
		ownerReceipt, ownerErr = s.runBatchTransaction(ownerCtx, edits, "interleaved-owner")
	}()
	t.Cleanup(func() {
		unblock()
		select {
		case <-ownerDone:
		case <-time.After(5 * time.Second):
			t.Error("owner did not finish after releasing the write gate")
		}
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("owner did not reach first committed write")
	}
	// Once a commit has begun, cancellation must not leave its second file
	// unapplied. Each retry still owns its own cancellation and must return.
	cancelOwner()
	const retryCount = 12
	errors := make(chan error, retryCount)
	receipts := make(chan batchTransactionReceipt, retryCount)
	var retries sync.WaitGroup
	for range retryCount {
		retries.Add(1)
		go func() {
			defer retries.Done()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			receipt, err := s.runBatchTransaction(ctx, edits, "interleaved-owner")
			errors <- err
			receipts <- receipt
		}()
	}
	retriesDone := make(chan struct{})
	go func() { retries.Wait(); close(retriesDone) }()
	select {
	case <-retriesDone:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled retries blocked behind the owner's write gate")
	}
	close(errors)
	close(receipts)
	for err := range errors {
		require.NoError(t, err)
	}
	for receipt := range receipts {
		assert.Equal(t, "interleaved-owner", receipt.TransactionID)
		assert.Equal(t, "prepared", receipt.Status)
		assert.Contains(t, receipt.Error, "transaction continues independently")
	}
	assert.Equal(t, int64(1), writes.Load(), "retry must not replay a partially committed payload")
	type probeResult struct {
		receipt batchTransactionReceipt
		err     error
	}
	probe := func(call func() (batchTransactionReceipt, error)) (batchTransactionReceipt, error) {
		result := make(chan probeResult, 1)
		go func() {
			receipt, err := call()
			result <- probeResult{receipt, err}
		}()
		select {
		case outcome := <-result:
			return outcome.receipt, outcome.err
		case <-time.After(5 * time.Second):
			// Release the deliberately held owner before reporting a broken
			// cancellation/status contract, so cleanup can join both callers.
			unblock()
			select {
			case <-result:
			case <-time.After(5 * time.Second):
				t.Error("probe did not finish even after releasing the owner")
			}
			t.Fatal("probe blocked behind a partially committed owner")
			return batchTransactionReceipt{}, context.DeadlineExceeded
		}
	}
	status, err := probe(func() (batchTransactionReceipt, error) {
		return s.batchTransactionStatus(context.Background(), "interleaved-owner")
	})
	require.NoError(t, err)
	assert.Equal(t, "prepared", status.Status, "status lookup must not recover an active transaction")
	_, err = probe(func() (batchTransactionReceipt, error) {
		return s.runBatchTransaction(context.Background(), []batchEditItem{atomicFileEdit(first, "after-a", "conflicting")}, "interleaved-owner")
	})
	require.ErrorContains(t, err, "different edit payload")

	// A different ID waiting on this file must honor its own deadline, while
	// an unrelated path stays usable during the owner's non-cancellable write.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	blocked, err := probe(func() (batchTransactionReceipt, error) {
		return s.runBatchTransaction(ctx, []batchEditItem{atomicFileEdit(first, "after-a", "blocked")}, "interleaved-blocked")
	})
	require.NoError(t, err)
	assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	assert.Equal(t, "aborted", blocked.Status)
	assert.Equal(t, "unchanged", blocked.DiskStatus)
	independentCtx, cancelIndependent := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelIndependent()
	other, err := probe(func() (batchTransactionReceipt, error) {
		return s.runBatchTransaction(independentCtx, []batchEditItem{atomicFileEdit(independent, "independent-before", "independent-after")}, "interleaved-independent")
	})
	require.NoError(t, err)
	assert.Equal(t, "committed", other.Status)
	assert.Equal(t, "before-b\n", readAtomicBatchFixture(t, last))
	unblock()
	select {
	case <-ownerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled owner did not complete its atomic commit")
	}
	require.NoError(t, ownerErr)
	assert.Equal(t, "committed", ownerReceipt.Status)
	assert.Equal(t, "fresh", ownerReceipt.GraphStatus)
	assert.Equal(t, "after-a\n", readAtomicBatchFixture(t, first))
	assert.Equal(t, "after-b\n", readAtomicBatchFixture(t, last))
	assert.Equal(t, "independent-after\n", readAtomicBatchFixture(t, independent))
	assert.Equal(t, int64(3), writes.Load())
	assert.Equal(t, int64(3), scheduled.Load())
	// A cancelled admission cannot poison path locks for the next transaction.
	final, err := s.runBatchTransaction(context.Background(), []batchEditItem{atomicFileEdit(last, "after-b", "final-b")}, "interleaved-final")
	require.NoError(t, err)
	assert.Equal(t, "committed", final.Status)
	replayed, err := s.runBatchTransaction(context.Background(), edits, "interleaved-owner")
	require.NoError(t, err)
	assert.Equal(t, ownerReceipt, replayed)
	assert.Equal(t, "final-b\n", readAtomicBatchFixture(t, last), "replay must not restore the owner's stale after-image")
	assert.Equal(t, int64(4), writes.Load())
	assert.Equal(t, int64(4), scheduled.Load())
}
