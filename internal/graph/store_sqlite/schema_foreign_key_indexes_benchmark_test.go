package store_sqlite

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkForeignKeyGenerationPrune compares the repaired schema with the
// historical node_rowid FK scan, using private stores only. Setup and integrity
// checks are excluded from ns/op, but appear in whole-process CPU profiles.
func BenchmarkForeignKeyGenerationPrune(b *testing.B) {
	for _, size := range []int{100, 1000} {
		for _, indexed := range []bool{false, true} {
			b.Run(fmt.Sprintf("steps=%d/indexed=%t", size, indexed), func(b *testing.B) {
				b.StopTimer()
				for iteration := 0; iteration < b.N; iteration++ {
					s, err := Open(filepath.Join(b.TempDir(), "store.sqlite"))
					require.NoError(b, err)
					if !indexed {
						_, err = s.writerDB.Exec(`DROP INDEX maintenance_analysis_steps_node_rowid`)
						require.NoError(b, err)
					}
					tx, err := s.writerDB.Begin()
					require.NoError(b, err)
					nodes, err := tx.Prepare(`INSERT INTO analysis_nodes(id, generation_id, node_id) VALUES (?, ?, ?)`)
					require.NoError(b, err)
					steps, err := tx.Prepare(`INSERT INTO analysis_process_steps(generation_id, process_id, ordinal, node_rowid, depth) VALUES (?, 'process', ?, ?, 0)`)
					require.NoError(b, err)
					for generation := 1; generation <= 3; generation++ {
						_, err = tx.Exec(`INSERT INTO analysis_generations
							(generation_id, format_version, build_revision, created_at_unix, state, node_count, community_count, process_count, concept_count)
							VALUES (?, 1, 1, 1, 1, ?, 0, 1, 0)`, generation, size)
						require.NoError(b, err)
						_, err = tx.Exec(`INSERT INTO analysis_processes(generation_id, process_id, step_count) VALUES (?, 'process', ?)`, generation, size)
						require.NoError(b, err)
						for ordinal := 0; ordinal < size; ordinal++ {
							id := (generation-1)*size + ordinal + 1
							_, err = nodes.Exec(id, generation, fmt.Sprintf("node-%d", id))
							require.NoError(b, err)
							_, err = steps.Exec(generation, ordinal, id)
							require.NoError(b, err)
						}
					}
					require.NoError(b, nodes.Close())
					require.NoError(b, steps.Close())
					require.NoError(b, tx.Commit())
					b.StartTimer()
					err = s.PruneAnalysisGenerations(b.Context(), 2, 1000)
					b.StopTimer()
					require.NoError(b, err)
					var remaining int
					require.NoError(b, s.db.QueryRow(`SELECT COUNT(*) FROM analysis_nodes`).Scan(&remaining))
					require.Equal(b, size*2, remaining)
					require.NoError(b, s.Close())
				}
			})
		}
	}
}
