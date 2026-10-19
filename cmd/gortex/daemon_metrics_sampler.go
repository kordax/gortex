package main

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

// sampleDaemonMetricMetadata refreshes advisory indexer counters without a
// status command, SQL recount, graph scan or wait behind track/reload. It has
// the diagnostics listener's context lifetime and never owns the write gate.
func sampleDaemonMetricMetadata(ctx context.Context, c *realController, dst *atomic.Pointer[statusAggregate]) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if c.mu.TryLock() {
			if c.multiIndexer != nil {
				rows := make([]daemon.TrackedRepoStatus, 0)
				for prefix, meta := range c.multiIndexer.AllMetadata() {
					if meta == nil {
						continue
					}
					rows = append(rows, daemon.TrackedRepoStatus{Prefix: prefix, Files: meta.FileCount, Nodes: meta.NodeCount, Edges: meta.EdgeCount})
				}
				dst.Store(&statusAggregate{takenAt: time.Now(), tracked: rows})
			}
			c.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
