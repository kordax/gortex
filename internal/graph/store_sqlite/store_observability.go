package store_sqlite

import "database/sql"

// PoolStats snapshots database/sql's maintained counters without issuing SQL,
// waiting for a connection or acquiring the store's mutation gate. Handles at
// other graph generations share these pools. In-memory stores have one shared
// pool, which is reported once rather than counted as both reader and writer.
func (s *Store) PoolStats() map[string]sql.DBStats {
	if s.coreless() || s.db == nil {
		return nil
	}
	if s.db == s.writerDB {
		return map[string]sql.DBStats{"shared": s.db.Stats()}
	}
	out := map[string]sql.DBStats{"reader": s.db.Stats()}
	if s.writerDB != nil {
		out["writer"] = s.writerDB.Stats()
	}
	return out
}
