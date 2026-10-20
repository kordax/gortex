package indexer

import (
	"context"
	"fmt"
	"time"
)

const (
	multiWatcherMutationRouteWait = 250 * time.Millisecond
	multiWatcherMutationRoutePoll = 5 * time.Millisecond
)

// CanSkipFileMutation asks only the active owning watcher for an explicit
// exclusion proof. Snapshot routing under the lifecycle mutex, then release it
// before the watcher performs filesystem or graph lookups.
func (mw *MultiWatcher) CanSkipFileMutation(ctx context.Context, filePath string) (bool, error) {
	prefix := mw.multi.RepoForFile(filePath)
	if err := mw.lockMutationRoute(ctx); err != nil {
		return false, err
	}
	w, started := mw.watchers[prefix], mw.started[prefix]
	if w == nil && prefix == "" && len(mw.watchers) == 1 {
		for candidatePrefix, candidate := range mw.watchers {
			w, started = candidate, mw.started[candidatePrefix]
		}
	}
	mw.mu.Unlock()
	if w == nil || !started {
		return false, nil
	}
	return w.CanSkipFileMutation(ctx, filePath)
}

// lockMutationRoute bounds admission while Start or AddRepo owns the lifecycle
// mutex. Post-commit callers detach cancellation, so they also need a budget.
// No mutation generation is admitted on failure; returning an error instead of
// a nil ticket prevents the caller from entering synchronous reindex fallback.
func (mw *MultiWatcher) lockMutationRoute(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if mw.mu.TryLock() {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, multiWatcherMutationRouteWait)
	defer cancel()
	ticker := time.NewTicker(multiWatcherMutationRoutePoll)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			if err := ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("watcher lifecycle busy: %w", errMutationAdmissionDeferred)
		case <-ticker.C:
			if mw.mu.TryLock() {
				if err := waitCtx.Err(); err != nil {
					mw.mu.Unlock()
					if err := ctx.Err(); err != nil {
						return err
					}
					return fmt.Errorf("watcher lifecycle busy: %w", errMutationAdmissionDeferred)
				}
				return nil
			}
		}
	}
}
