package store_sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAnalysisGenerationGCWriterWaitHonorsDeadline(t *testing.T) {
	for _, phase := range []string{"chunk", "finish"} {
		t.Run(phase, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "gc.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			release, err := store.HoldWriteGate(context.Background())
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			finished := make(chan struct{})
			t.Cleanup(func() {
				release()
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("GC worker did not exit after releasing writer gate")
				}
			})
			go func() {
				defer close(finished)
				if phase == "chunk" {
					_, _, err := store.pruneAnalysisGenerationChunk(ctx, 1, analysisGenerationGCTables[0], 1)
					done <- err
				} else {
					done <- store.finishPruneAnalysisGeneration(ctx, 1)
				}
			}()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.DeadlineExceeded)
			case <-time.After(2 * time.Second):
				t.Fatal("GC ignored its deadline while another writer held the gate")
			}
			// A canceled waiter must not consume or release another writer's token.
			require.False(t, store.writeMu.TryLock())
			release()
			next, err := store.HoldWriteGate(context.Background())
			require.NoError(t, err)
			next()
		})
	}
}
