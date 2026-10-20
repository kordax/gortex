package tstypes

import (
	"context"
	"sync"
	"time"
)

// lockResolveContext parks without a detached lock goroutine, so cancellation
// cannot leave a waiter that later acquires the mutex and applies stale facts.
// Contended callers poll at 10ms intervals using one timer; admission is not FIFO.
func lockResolveContext(ctx context.Context, mu *sync.Mutex) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if mu.TryLock() {
		if err := ctx.Err(); err != nil {
			mu.Unlock()
			return err
		}
		return nil
	}

	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			if err := ctx.Err(); err != nil {
				return err
			}
			if mu.TryLock() {
				if err := ctx.Err(); err != nil {
					mu.Unlock()
					return err
				}
				return nil
			}
			timer.Reset(10 * time.Millisecond)
		}
	}
}
