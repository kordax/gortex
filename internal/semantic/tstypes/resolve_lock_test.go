package tstypes

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

type observedResolveLockStore struct {
	graph.Store
	mu      sync.Mutex
	reached chan struct{}
	once    sync.Once
}

func (g *observedResolveLockStore) ResolveMutex() *sync.Mutex {
	g.once.Do(func() { close(g.reached) })
	return &g.mu
}

type resolveLockOutcome struct {
	result *semantic.EnrichResult
	err    error
}

func TestEnrichRepoContextCancelsWhileResolveMutexHeld(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "cancel"
		if deadline {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			base, dir := buildFixture(t, map[string]string{
				"src/svc.ts": tsSvc,
				"src/app.ts": `import { Svc } from "./svc";
export function main(): void { const s = new Svc(); s.run(); }
`,
			})
			g := &observedResolveLockStore{Store: base, reached: make(chan struct{})}
			p := NewProvider(TypeScriptSpec(), zap.NewNop())
			var spoolPath string
			p.observeSpool = func(path string) { spoolPath = path }
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), time.Second)
			}
			defer cancel()
			before := semanticSnapshot(g)
			g.mu.Lock()
			locked := true
			done := make(chan resolveLockOutcome, 1)
			go func() {
				result, err := p.EnrichRepoContext(ctx, g, "", dir, nil)
				done <- resolveLockOutcome{result: result, err: err}
			}()
			defer func() {
				if locked {
					g.mu.Unlock()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("provider did not stop after test cleanup released resolve mutex")
					}
				}
			}()
			select {
			case <-g.reached:
			case <-time.After(5 * time.Second):
				t.Fatal("provider did not reach resolve mutex")
			}
			require.NoError(t, ctx.Err(), "cancellation must occur after staging, at lock admission")
			if !deadline {
				cancel()
			}
			<-ctx.Done()
			var outcome resolveLockOutcome
			select {
			case outcome = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled provider remained blocked while resolve mutex was held")
			}
			require.NoError(t, outcome.err)
			require.NotNil(t, outcome.result)
			assert.True(t, outcome.result.Partial)
			assert.Equal(t, ctx.Err().Error(), outcome.result.AbortReason)
			assert.Equal(t, semantic.EnrichBoundBudget, outcome.result.BoundReason)
			assert.Zero(t, outcome.result.EdgesAdded)
			assert.Zero(t, outcome.result.EdgesConfirmed)
			assert.Zero(t, outcome.result.NodesEnriched)
			assert.Equal(t, before, semanticSnapshot(g))
			require.NotEmpty(t, spoolPath)
			_, statErr := os.Stat(spoolPath)
			assert.ErrorIs(t, statErr, os.ErrNotExist)
			g.mu.Unlock()
			locked = false
			time.Sleep(25 * time.Millisecond)
			assert.Equal(t, before, semanticSnapshot(g), "cancelled lock waiter must not apply after return")
			require.True(t, g.mu.TryLock(), "cancelled waiter must not retain resolve mutex")
			g.mu.Unlock()
		})
	}
}

func TestEnrichRepoContextResolveMutexLiveWaiterCompletes(t *testing.T) {
	base, dir := buildFixture(t, map[string]string{"src/svc.ts": tsSvc})
	g := &observedResolveLockStore{Store: base, reached: make(chan struct{})}
	p := NewProvider(TypeScriptSpec(), zap.NewNop())
	g.mu.Lock()
	done := make(chan resolveLockOutcome, 1)
	go func() {
		result, err := p.EnrichRepoContext(context.Background(), g, "", dir, nil)
		done <- resolveLockOutcome{result: result, err: err}
	}()
	select {
	case <-g.reached:
	case <-time.After(5 * time.Second):
		g.mu.Unlock()
		t.Fatal("provider did not reach resolve mutex")
	}
	select {
	case <-done:
		g.mu.Unlock()
		t.Fatal("provider applied without acquiring resolve mutex")
	default:
	}
	g.mu.Unlock()
	select {
	case outcome := <-done:
		require.NoError(t, outcome.err)
		require.NotNil(t, outcome.result)
		assert.False(t, outcome.result.Partial)
		assert.Positive(t, outcome.result.SymbolsCovered)
		require.True(t, g.mu.TryLock(), "completed waiter must release resolve mutex")
		g.mu.Unlock()
	case <-time.After(5 * time.Second):
		t.Fatal("live waiter did not complete after resolve mutex release")
	}
}
