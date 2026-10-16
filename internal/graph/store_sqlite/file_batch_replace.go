package store_sqlite

import (
	"database/sql"
	"errors"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.FileBatchReplacer = (*Store)(nil)

// ReplaceFiles uses the existing generation-scoped eviction transaction for
// retirement and publication, including shared-contract owner accounting.
func (s *Store) ReplaceFiles(replacement graph.FileBatchReplacement) error {
	pathsJSON, ok := nonEmptyProjectionJSON(replacement.FilePaths)
	if !ok {
		return errors.New("file replacement requires nonempty file paths")
	}
	if kept, rejected := graph.FilterStructuralEdgeViolations(replacement.Edges); len(rejected) > 0 {
		replacement.Edges = kept
		inputRepos := structuralInputNodeRepos(replacement.Nodes)
		for _, edge := range rejected {
			repo := s.structuralWriteRepo(edge, inputRepos)
			s.recordStructuralEdge(graph.StructuralDropWrite, graph.StructuralPathSQLiteAddBatch, repo, edge)
		}
	}
	// UPSERT is idempotent, so sentinel materialization needs no shared
	// seen-set state to roll back if replacement fails.
	if builtins := graph.BuiltinStubNodes(replacement.Edges); len(builtins) > 0 {
		replacement.Nodes = append(append([]*graph.Node(nil), replacement.Nodes...), builtins...)
	}
	_, _, err := s.evictByPredicateWithReplacement(evictFilesPredicate, pathsJSON, evictThisGeneration, &replacement)
	if err == nil {
		for _, edge := range replacement.Edges {
			if edge != nil && graph.IsUnresolvedTarget(edge.To) &&
				!graph.IsProxyID(edge.From) && !graph.IsProxyID(edge.To) {
				s.unresolvedInserts.Add(1)
			}
		}
	}
	return err
}

// Ordinary file documents belong to this replacement. Shared contract FTS
// follows applyContractFileEvictionTx's canonical owner decision instead.
func (s *Store) deleteReplacementSymbolFTSTx(tx *sql.Tx, scoped string, scopeArgs []any) error {
	ids := `SELECT id FROM nodes WHERE (` + scoped + `) AND kind <> ?`
	args := append([]any{s.viewGen}, scopeArgs...)
	args = append(args, string(graph.KindContract))
	if _, err := tx.Exec(`DELETE FROM symbol_fts WHERE rowid IN (
SELECT fts_rowid FROM symbol_fts_rowid WHERE view_gen = ? AND node_id IN (`+ids+`)
)`, args...); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM symbol_fts_rowid WHERE view_gen = ? AND node_id IN (`+ids+`)`, args...)
	return err
}

func (s *Store) insertFileReplacementTx(tx *sql.Tx, replacement graph.FileBatchReplacement, receipt *sqliteMutationReceiptAccumulator) (int, error) {
	nodesChanged, _, _, err := insertNodeChunksTx(tx, s.viewGen, replacement.Nodes, false)
	if err != nil {
		return 0, err
	}
	if rows := cloneCorpusRowsFromNodes(replacement.Nodes); len(rows) > 0 {
		if err := upsertCloneCorpusTx(tx, s.viewGen, "", rows); err != nil {
			return 0, err
		}
	}
	edgesAdded, _, _, err := insertEdgeChunksTx(tx, s.viewGen, replacement.Edges, false)
	if err != nil {
		return 0, err
	}
	items := dedupeSymbolFTSItems(replacement.Symbols)
	if len(items) > 0 {
		nextRowID, err := nextFTSRowIDTx(tx, "symbol_fts")
		if err != nil {
			return 0, err
		}
		var stats symbolFTSBatchStats
		for start := 0; start < len(items); start += ftsInsertChunkRows {
			end := minInt(start+ftsInsertChunkRows, len(items))
			if err := upsertSymbolFTSChunkTx(tx, s.viewGen, items[start:end], &nextRowID, &stats); err != nil {
				return 0, err
			}
		}
	}
	if receipt != nil {
		files := make(map[string]string, len(replacement.Nodes))
		for _, node := range replacement.Nodes {
			if node == nil || node.ID == "" || graph.IsProxyNode(node) {
				continue
			}
			files[node.ID] = node.FilePath
			recordSQLiteAddedNode(receipt, node)
		}
		for _, edge := range replacement.Edges {
			if edge == nil || graph.IsProxyID(edge.From) || graph.IsProxyID(edge.To) {
				continue
			}
			file := edge.FilePath
			if file == "" {
				file = files[edge.From]
			}
			recordSQLiteAddedEdge(receipt, edge, file)
		}
	}
	return nodesChanged + edgesAdded, nil
}
