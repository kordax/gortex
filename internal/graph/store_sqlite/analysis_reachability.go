package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.AnalysisProcessReachability = (*Store)(nil)

// AnalysisReachableNodesContext avoids materializing every process and step
// merely to exclude a small set of dead-code candidates.
func (s *Store) AnalysisReachableNodesContext(ctx context.Context, generationID int64, nodeIDs []string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(nodeIDs) > 1000 {
		return nil, fmt.Errorf("analysis reachability exceeds 1000 candidate IDs")
	}
	ids, err := sortedUniqueAnalysisStrings(nodeIDs, "node id")
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM analysis_active_generation a
		JOIN analysis_generations g ON g.generation_id = a.generation_id
		WHERE a.slot = 1 AND a.generation_id = ? AND g.state = ?`, generationID, analysisGenerationReady).Scan(&one)
	if err == sql.ErrNoRows {
		return nil, graph.ErrAnalysisGenerationInactive
	}
	if err != nil {
		return nil, err
	}
	args := make([]any, 0, 2*(len(ids)+1))
	for range 2 {
		args = append(args, generationID)
		for _, id := range ids {
			args = append(args, id)
		}
	}
	placeholders := analysisPlaceholders(len(ids))
	rows, err := tx.QueryContext(ctx, `SELECT n.node_id FROM analysis_nodes n
		WHERE n.generation_id = ? AND n.node_id IN (`+placeholders+`)
		AND EXISTS (SELECT 1 FROM analysis_process_steps s
		WHERE s.generation_id = n.generation_id AND s.node_rowid = n.id)
		UNION SELECT entry_point FROM analysis_processes
		WHERE generation_id = ? AND entry_point IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reachable []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		reachable = append(reachable, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return reachable, nil
}
