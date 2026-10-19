package store_sqlite

import "github.com/zzet/gortex/internal/graph"

var _ graph.RerankEdgeReader = (*Store)(nil)

// GetRerankOutEdgesByNodeIDs omits payloads that ranking does not consume.
// The ordinary edge readers retain their full metadata contract.
func (s *Store) GetRerankOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return s.rerankEdgesByNodeIDs(ids, "from_id", func(e *graph.Edge) string { return e.From })
}

// GetRerankInEdgesByNodeIDs is the incoming sibling of the outgoing projection.
func (s *Store) GetRerankInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return s.rerankEdgesByNodeIDs(ids, "to_id", func(e *graph.Edge) string { return e.To })
}

func (s *Store) rerankEdgesByNodeIDs(ids []string, col string, key func(*graph.Edge) string) map[string][]*graph.Edge {
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil
	}
	out := make(map[string][]*graph.Edge, len(uniq))
	for _, id := range uniq {
		out[id] = nil
	}
	for i := 0; i < len(uniq); i += lookupChunkSize {
		chunk := uniq[i:minInt(i+lookupChunkSize, len(uniq))]
		// CASE avoids transferring or decoding metadata for ordinary edges.
		// Keep file/line for structural-drop diagnostics, and preserve the
		// backend's existing traversal order instead of introducing a sort.
		q := rerankEdgesQuery(col, len(chunk))
		args := append([]any{string(graph.EdgeSimilarTo)}, toAnyArgs(chunk)...)
		args = append(args, s.viewGen)
		s.queryRerankEdges(q, args, out, key)
	}
	return out
}

func rerankEdgesQuery(col string, count int) string {
	return `SELECT from_id, to_id, kind, confidence, file_path, line,
		CASE WHEN kind = ? THEN meta ELSE NULL END
		FROM edges WHERE ` + col + ` IN (` + inPlaceholders(count) + `) AND view_gen = ?`
}

// Materialize and close each chunk before the next query: SQLite stores may
// have a single pooled connection. Errors follow the full-reader policy.
func (s *Store) queryRerankEdges(q string, args []any, out map[string][]*graph.Edge, key func(*graph.Edge) string) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		panicOnFatal(err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var e graph.Edge
		var meta []byte
		if err := rows.Scan(&e.From, &e.To, &e.Kind, &e.Confidence, &e.FilePath, &e.Line, &meta); err != nil {
			panicOnFatal(err)
			return
		}
		if graph.StructuralEdgeTargetInvalid(e.Kind, e.To) {
			s.noteStructuralReadDrop(graph.StructuralPathSQLiteLightRead, &e)
			continue
		}
		if len(meta) > 0 {
			e.Meta, err = decodeMeta(meta)
			if err != nil {
				panicOnFatal(err)
				return
			}
		}
		id := key(&e)
		out[id] = append(out[id], &e)
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
}
