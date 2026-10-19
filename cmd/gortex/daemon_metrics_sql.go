package main

import (
	"context"
	"database/sql"
	"sort"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/observability"
)

type daemonMetricFiles struct {
	database, wal int64
	sampledAt     time.Time
}

// File sizing is asynchronous because filesystem metadata can block. One
// context-bound sampler is shared by all scrapes of this handler.
func sampleDaemonMetricFiles(ctx context.Context, store any, dst *atomic.Pointer[daemonMetricFiles]) {
	reader, ok := store.(interface{ DBStats() (int64, int64) })
	if !ok {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		database, wal := reader.DBStats()
		dst.Store(&daemonMetricFiles{database: database, wal: wal, sampledAt: time.Now()})
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func writeDaemonSQLMetrics(e *observability.Exposition, store any, files *daemonMetricFiles) {
	if pools, ok := store.(interface{ PoolStats() map[string]sql.DBStats }); ok {
		snapshot := pools.PoolStats()
		names := make([]string, 0, len(snapshot))
		for name := range snapshot {
			if name == "reader" || name == "writer" || name == "shared" {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			st := snapshot[name]
			e.Gauge("gortex_sqlite_connections", "SQLite database/sql connections by pool and state.", float64(st.OpenConnections), "pool", name, "state", "open")
			e.Gauge("gortex_sqlite_connections", "SQLite database/sql connections by pool and state.", float64(st.InUse), "pool", name, "state", "in_use")
			e.Gauge("gortex_sqlite_connections", "SQLite database/sql connections by pool and state.", float64(st.Idle), "pool", name, "state", "idle")
			e.Gauge("gortex_sqlite_connection_limit", "Configured maximum database/sql connections; zero means unlimited.", float64(st.MaxOpenConnections), "pool", name)
			e.Counter("gortex_sqlite_pool_waits_total", "Times a caller waited for a database/sql connection.", float64(st.WaitCount), "pool", name)
			e.Counter("gortex_sqlite_pool_wait_duration_seconds_total", "Accumulated database/sql connection wait time.", st.WaitDuration.Seconds(), "pool", name)
		}
	}
	available := float64(0)
	if files != nil {
		e.Gauge("gortex_sqlite_file_sample_timestamp_seconds", "Unix timestamp of asynchronous database file sizes.", float64(files.sampledAt.Unix()))
		// Existing DBStats returns zero for unavailable paths. An empty or
		// in-memory database is therefore unknown, not reported as an empty
		// disk database. WAL zero is valid when the database itself is known.
		if files.database > 0 {
			available = 1
			e.Gauge("gortex_sqlite_file_bytes", "SQLite files on disk, distinct from process memory and page cache.", float64(files.database), "kind", "database")
			e.Gauge("gortex_sqlite_file_bytes", "SQLite files on disk, distinct from process memory and page cache.", float64(files.wal), "kind", "wal")
		}
	}
	e.Gauge("gortex_sqlite_file_sample_available", "One when an on-disk database size was available.", available)
}
