package indexer

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMultiWatcherMutationAdmissionDoesNotWaitForLifecycle(t *testing.T) {
	for _, name := range []string{"background", "deadline"} {
		t.Run(name, func(t *testing.T) {
			mw, mi, dir, _ := setupMultiWatcherTest(t)
			defer func() { assert.NoError(t, mw.Stop()) }()
			ctx := context.Background()
			if name == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
				defer cancel()
			}
			// Start and AddRepo hold this mutex while starting filesystem
			// watchers. Keep it held to model a startup which cannot finish.
			mw.mu.Lock()
			var once sync.Once
			unlock := func() { once.Do(mw.mu.Unlock) }
			defer unlock()
			type outcome struct {
				ticket *MutationTicket
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				ticket, err := mw.EnqueueFileMutation(ctx, filepath.Join(dir, "main.go"))
				done <- outcome{ticket, err}
			}()
			joined := false
			defer func() {
				unlock()
				if !joined {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("admission goroutine did not stop after releasing the lifecycle gate")
					}
				}
			}()
			select {
			case result := <-done:
				joined = true
				assert.Nil(t, result.ticket, "a refusal must not admit a mutation generation")
				if name == "deadline" {
					assert.ErrorIs(t, result.err, context.DeadlineExceeded)
				} else {
					assert.ErrorIs(t, result.err, errMutationAdmissionDeferred)
				}
			case <-time.After(time.Second):
				t.Fatal("mutation admission remained blocked by watcher lifecycle")
			}
			unlock()

			// The refusal must not poison routing or admission after startup.
			require.NoError(t, mw.Start())
			writeFile(t, filepath.Join(dir, "main.go"), "package main\nfunc RecoveredAfterAdmission() {}\n")
			ticket, err := mw.EnqueueFileMutation(context.Background(), filepath.Join(dir, "main.go"))
			require.NoError(t, err)
			require.NotNil(t, ticket)
			select {
			case result := <-ticket.Done:
				require.NoError(t, result.Err)
				assert.GreaterOrEqual(t, result.AppliedGeneration, ticket.Generation)
			case <-time.After(5 * time.Second):
				t.Fatal("mutation did not complete after startup")
			}
			assert.NotEmpty(t, mi.Graph().FindNodesByName("RecoveredAfterAdmission"))
		})
	}
}

func TestMultiWatcherMutationAdmissionWaitsForBriefLifecycle(t *testing.T) {
	mw, _, dir, _ := setupMultiWatcherTest(t)
	require.NoError(t, mw.Start())
	defer func() { assert.NoError(t, mw.Stop()) }()
	mw.mu.Lock()
	var once sync.Once
	unlock := func() { once.Do(mw.mu.Unlock) }
	defer unlock()
	// Brief lifecycle activity should wait and recover, rather than reject
	// an otherwise healthy watcher merely because TryLock initially fails.
	timer := time.AfterFunc(20*time.Millisecond, unlock)
	defer timer.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ticket, err := mw.EnqueueFileMutation(ctx, filepath.Join(dir, "main.go"))
	require.NoError(t, err)
	require.NotNil(t, ticket)
	select {
	case result := <-ticket.Done:
		assert.NoError(t, result.Err)
	case <-time.After(5 * time.Second):
		t.Fatal("mutation did not complete after brief lifecycle contention")
	}
}

func TestMultiWatcherMutationAdmissionRejectsCancelledContext(t *testing.T) {
	mw, _, dir, _ := setupMultiWatcherTest(t)
	defer func() { assert.NoError(t, mw.Stop()) }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mw.mu.Lock()
	defer mw.mu.Unlock()
	ticket, err := mw.EnqueueFileMutation(ctx, filepath.Join(dir, "main.go"))
	assert.Nil(t, ticket)
	assert.ErrorIs(t, err, context.Canceled)
}
