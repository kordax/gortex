package mcp

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/savings"
)

func TestSavingsPersistenceDoesNotWaitForExternalWriter(t *testing.T) {
	for _, mode := range []string{"budget", "cancellation", "pending_batch"} {
		t.Run(mode, func(t *testing.T) {
			canceled := mode == "cancellation"
			srv, _, dir := newRetrievalSavingsServer(t, 1)
			path := filepath.Join(t.TempDir(), "savings.sqlite")
			store, err := savings.Open(path)
			require.NoError(t, err)
			defer store.Close()
			srv.InitSavings(store, "")
			db, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			defer db.Close()
			conn, err := db.Conn(context.Background())
			require.NoError(t, err)
			defer conn.Close()
			_, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE")
			require.NoError(t, err)
			done := make(chan struct{})
			defer func() {
				_, rollbackErr := conn.ExecContext(context.Background(), "ROLLBACK")
				require.NoError(t, rollbackErr)
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("accounting worker failed to terminate after writer release")
				}
			}()
			ctx, cancel := context.WithCancel(WithSessionID(context.Background(), "contention"))
			defer cancel()
			if canceled {
				cancel()
			}
			expectedDrops := int64(1)
			if mode == "pending_batch" {
				expectedDrops = 16
			}
			go func() {
				if mode == "pending_batch" {
					pending := make([]pendingSavings, expectedDrops)
					for i := range pending {
						pending[i] = pendingSavings{absPath: filepath.Join(dir, "mod0.go"), tool: "get_symbol_source", returned: 1, fullFile: 100}
					}
					srv.recordPendingSavings(ctx, pending)
				} else {
					srv.recordFileSetBaselineSavings(ctx, "search_symbols", []string{"myrepo/mod0.go"}, "ready response")
				}
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("savings blocked completed response behind unrelated writer")
			}
			snap, err := store.Snapshot()
			require.NoError(t, err)
			require.Equal(t, expectedDrops, snap.DroppedObservations)
			require.Zero(t, snap.Totals.CallsCounted)
		})
	}
}
