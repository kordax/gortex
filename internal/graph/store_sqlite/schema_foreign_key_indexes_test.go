package store_sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var foreignKeyRepairIndexes = []string{
	"maintenance_analysis_steps_node_rowid",
	"maintenance_ref_view_builds_ref_view_id",
	"maintenance_dedicated_graphs_family_id",
}

func foreignKeyPlan(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	return details
}

func assertForeignKeySearch(t *testing.T, plan []string, table string) {
	t.Helper()
	found := false
	for _, detail := range plan {
		assert.NotContains(t, detail, "SCAN "+table, "unindexed child probe: %v", plan)
		found = found || strings.HasPrefix(detail, "SEARCH "+table+" ")
	}
	assert.True(t, found, "no indexed child probe for %s: %v", table, plan)
}

func foreignKeySchemaIndexes(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT name, sql FROM sqlite_schema WHERE type = 'index' AND sql IS NOT NULL`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	indexes := make(map[string]string)
	for rows.Next() {
		var name, ddl string
		require.NoError(t, rows.Scan(&name, &ddl))
		indexes[name] = ddl
	}
	require.NoError(t, rows.Err())
	return indexes
}

func assertForeignKeyIntegrity(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	require.NoError(t, err)
	assert.False(t, rows.Next(), "foreign_key_check found a dangling reference")
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	var integrity string
	require.NoError(t, db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity))
	assert.Equal(t, "ok", integrity)
}

func TestForeignKeyChildIndexesAllPredicatesSearch(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	rows, err := s.db.Query(`SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	require.NoError(t, err)
	var tables []string
	for rows.Next() {
		var table string
		require.NoError(t, rows.Scan(&table))
		tables = append(tables, table)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	predicates := 0
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	for _, table := range tables {
		fkRows, err := s.db.Query(`SELECT id, "from" FROM pragma_foreign_key_list(?) ORDER BY id, seq`, table)
		require.NoError(t, err)
		keys := make(map[int][]string)
		for fkRows.Next() {
			var id int
			var column string
			require.NoError(t, fkRows.Scan(&id, &column))
			keys[id] = append(keys[id], column)
		}
		require.NoError(t, fkRows.Err())
		require.NoError(t, fkRows.Close())
		for id, columns := range keys {
			predicates++
			t.Run(fmt.Sprintf("%s/fk%d", table, id), func(t *testing.T) {
				var where []string
				var args []any
				for _, column := range columns {
					where = append(where, quote(column)+" = ?")
					args = append(args, 1)
				}
				assertForeignKeySearch(t, foreignKeyPlan(t, s.db, "SELECT 1 FROM "+quote(table)+" WHERE "+strings.Join(where, " AND "), args...), table)
			})
		}
	}
	// Enumerate future keys too, including nullable keys already served by
	// partial IS NOT NULL indexes. No hard-coded index names or whole-plan text.
	assert.GreaterOrEqual(t, predicates, 26)
	t.Logf("checked %d foreign-key child predicates", predicates)
}

func assertForeignKeyDeletePlans(t *testing.T, db *sql.DB) {
	t.Helper()
	var enabled int
	require.NoError(t, db.QueryRow(`PRAGMA foreign_keys`).Scan(&enabled))
	require.Equal(t, 1, enabled)
	for _, test := range []struct{ parent, key, child string }{
		{"analysis_nodes", "id", "analysis_process_steps"},
		{"ref_views", "ref_view_id", "ref_view_builds"},
		{"repository_families", "family_id", "dedicated_graphs"},
	} {
		assertForeignKeySearch(t, foreignKeyPlan(t, db, "DELETE FROM "+test.parent+" WHERE "+test.key+" = ?", 1), test.child)
	}
	for _, table := range analysisGenerationGCTables {
		if table.name == "nodes" {
			assertForeignKeySearch(t, foreignKeyPlan(t, db, table.delete, 1, 1000), "analysis_process_steps")
		}
	}
}

func seedForeignKeyGenerations(t *testing.T, db *sql.DB) {
	t.Helper()
	for generation := 1; generation <= 3; generation++ {
		_, err := db.Exec(`INSERT INTO analysis_generations
			(generation_id, format_version, build_revision, created_at_unix, state, node_count, community_count, process_count, concept_count)
			VALUES (?, 1, 1, 1, 1, 1, 0, 1, 0)`, generation)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO analysis_nodes(id, generation_id, node_id) VALUES (?, ?, ?)`, generation, generation, fmt.Sprintf("node-%d", generation))
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO analysis_processes(generation_id, process_id, step_count) VALUES (?, 'process', 1)`, generation)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO analysis_process_steps(generation_id, process_id, ordinal, node_rowid, depth) VALUES (?, 'process', 0, ?, 0)`, generation, generation)
		require.NoError(t, err)
	}
	_, err := db.Exec(`INSERT INTO analysis_active_generation(slot, generation_id) VALUES (1, 3)`)
	require.NoError(t, err)
}

func TestForeignKeyChildIndexesUpgradeV21InPlace(t *testing.T) {
	for _, repaired := range []bool{false, true} {
		t.Run(fmt.Sprintf("maintenance_indexes=%t", repaired), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.sqlite")
			s, err := Open(path)
			require.NoError(t, err)
			seedForeignKeyGenerations(t, s.writerDB)
			_, err = s.writerDB.Exec(`INSERT INTO nodes(id, kind, name, file_path) VALUES ('preserved', 'function', 'Preserved', 'repo/main.go')`)
			require.NoError(t, err)
			require.NoError(t, s.Close())
			var before map[string]string
			withRawDB(t, path, func(db *sql.DB) {
				if !repaired {
					for _, name := range foreignKeyRepairIndexes {
						_, err := db.Exec("DROP INDEX " + name)
						require.NoError(t, err)
					}
					// The old generation-leading index remains but cannot seek
					// this probe. This is a regression witness, not a time limit.
					require.Contains(t, strings.Join(foreignKeyPlan(t, db, `SELECT 1 FROM analysis_process_steps WHERE node_rowid = ?`, 1), "\n"), "SCAN analysis_process_steps")
				}
				before = foreignKeySchemaIndexes(t, db)
				require.NoError(t, setUserVersion(db, 21))
			})
			upgraded, err := Open(path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
			assert.False(t, upgraded.NeedsRebuild())
			version, err := readUserVersion(upgraded.db)
			require.NoError(t, err)
			assert.Equal(t, currentSchemaVersion, version)
			assert.Equal(t, 1, nodeCount(t, upgraded.db))
			after := foreignKeySchemaIndexes(t, upgraded.db)
			if repaired {
				assert.Equal(t, before, after, "maintenance repairs must not gain duplicate indexes")
			} else {
				for _, name := range foreignKeyRepairIndexes {
					require.Contains(t, after, name)
					delete(after, name)
				}
				assert.Equal(t, before, after, "only the three missing indexes are added")
			}
			assertForeignKeyDeletePlans(t, upgraded.writerDB)
			// Open invalidates the old active analysis on a schema transition.
			var active int
			require.NoError(t, upgraded.db.QueryRow(`SELECT COUNT(*) FROM analysis_active_generation`).Scan(&active))
			assert.Zero(t, active)
			require.NoError(t, upgraded.PruneAnalysisGenerations(t.Context(), 2, 1))
			for _, table := range []string{"analysis_generations", "analysis_nodes", "analysis_processes", "analysis_process_steps"} {
				var ids []int
				rows, err := upgraded.db.Query("SELECT generation_id FROM " + table + " ORDER BY generation_id")
				require.NoError(t, err)
				for rows.Next() {
					var id int
					require.NoError(t, rows.Scan(&id))
					ids = append(ids, id)
				}
				require.NoError(t, rows.Err())
				require.NoError(t, rows.Close())
				assert.Equal(t, []int{2, 3}, ids, table)
			}
			assertForeignKeyIntegrity(t, upgraded.db)
		})
	}
}

func TestForeignKeyChildIndexesMigrationWithoutSchemaReconciliation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	withRawDB(t, path, func(db *sql.DB) {
		for _, name := range foreignKeyRepairIndexes {
			_, err := db.Exec("DROP INDEX " + name)
			require.NoError(t, err)
		}
		steps := pendingBetween(21, currentSchemaVersion, schemaMigrations)
		require.Len(t, steps, 1)
		require.NoError(t, applyInPlaceMigrations(db, steps))
		before := foreignKeySchemaIndexes(t, db)
		for _, name := range foreignKeyRepairIndexes {
			require.Contains(t, before, name)
		}
		require.NoError(t, applyInPlaceMigrations(db, steps))
		assert.Equal(t, before, foreignKeySchemaIndexes(t, db))
	})
}

func TestForeignKeyChildIndexesFreshDeleteIntegrity(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	assertForeignKeyDeletePlans(t, s.writerDB)
	seedForeignKeyGenerations(t, s.writerDB)
	_, err = s.writerDB.Exec(`DELETE FROM analysis_nodes WHERE id = 1`)
	require.ErrorContains(t, err, "FOREIGN KEY constraint failed")
	_, err = s.writerDB.Exec(`INSERT INTO repository_families(family_id, common_dir_identity, state) VALUES ('family', 'common', 'ready')`)
	require.NoError(t, err)
	_, err = s.writerDB.Exec(`INSERT INTO dedicated_graphs(graph_id, repo_prefix, family_id, state) VALUES ('graph', 'repo', 'family', 'ready')`)
	require.NoError(t, err)
	_, err = s.writerDB.Exec(`DELETE FROM repository_families WHERE family_id = 'family'`)
	require.ErrorContains(t, err, "FOREIGN KEY constraint failed")
	_, err = s.writerDB.Exec(`INSERT INTO ref_views(ref_view_id, graph_id, selector_kind, selector_value, enrichment_profile, state) VALUES ('view', 'graph', 'ref', 'main', '', 'ready')`)
	require.NoError(t, err)
	_, err = s.writerDB.Exec(`INSERT INTO ref_view_builds(build_id, ref_view_id, build_fingerprint, captured_route_epoch, state, build_token) VALUES ('build', 'view', 'fingerprint', 0, 'ready', 'token')`)
	require.NoError(t, err)
	_, err = s.writerDB.Exec(`DELETE FROM ref_views WHERE ref_view_id = 'view'`)
	require.NoError(t, err)
	var builds int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM ref_view_builds`).Scan(&builds))
	assert.Zero(t, builds, "completed builds must cascade too")
	require.NoError(t, s.PruneAnalysisGenerations(t.Context(), 1, 1))
	var active int64
	require.NoError(t, s.db.QueryRow(`SELECT generation_id FROM analysis_active_generation`).Scan(&active))
	assert.EqualValues(t, 3, active, "GC must preserve the active generation")
	assertForeignKeyIntegrity(t, s.db)
}
