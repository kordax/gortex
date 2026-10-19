package store_sqlite

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPoolStatsSharedPoolsWithoutMutationGate(t *testing.T) {
	for _, memory := range []bool{true, false} {
		t.Run(map[bool]string{true: "memory", false: "disk"}[memory], func(t *testing.T) {
			path := ":memory:"
			if !memory {
				path = filepath.Join(t.TempDir(), "store.db")
			}
			store, err := Open(path)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, store.Close()) })
			store.writeMu.Lock()
			done := make(chan struct{})
			go func() {
				defer close(done)
				stats := store.PoolStats()
				if memory {
					assert.Contains(t, stats, "shared")
					assert.Len(t, stats, 1)
				} else {
					assert.Contains(t, stats, "reader")
					assert.Contains(t, stats, "writer")
					assert.Len(t, stats, 2)
				}
			}()
			select {
			case <-done:
				store.writeMu.Unlock()
			case <-time.After(time.Second):
				store.writeMu.Unlock()
				t.Fatal("pool counter snapshot waited behind the mutation gate")
			}
		})
	}
	var missing *Store
	assert.Nil(t, missing.PoolStats())
	assert.Nil(t, (&Store{}).PoolStats())
}
