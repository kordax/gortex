package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestReplaceVectorCorpusIsAtomicPerRepositoryAndReportsStats(t *testing.T) {
	store := openVectorCorpusTestStore(t, filepath.Join(t.TempDir(), "corpus.sqlite"))
	ctx := context.Background()
	addVectorCorpusNodes(t, store, "A", "A/one", "A/two", "A/new")
	addVectorCorpusNodes(t, store, "B", "B/keep")

	stats, err := store.ReplaceVectorCorpus(ctx, "A", 2, []graph.VectorCorpusItem{
		{NodeID: "A/one", Vec: []float32{1, 0}},
		{NodeID: "A/two#chunk0", ParentID: "A/two", Vec: []float32{0, 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertVectorCorpusStats(t, stats, graph.VectorCorpusStats{
		VectorCount: 2, ChunkCount: 1,
		RepositoryVectorCount: 2, RepositoryChunkCount: 1,
		Dims: 2,
	})

	stats, err = store.ReplaceVectorCorpus(ctx, "B", 2, []graph.VectorCorpusItem{
		{NodeID: "B/keep", Vec: []float32{1, 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertVectorCorpusStats(t, stats, graph.VectorCorpusStats{
		VectorCount: 3, ChunkCount: 1,
		RepositoryVectorCount: 1,
		Dims:                  2,
	})

	stats, err = store.ReplaceVectorCorpus(ctx, "A", 2, []graph.VectorCorpusItem{
		{NodeID: "A/new", Vec: []float32{1, -1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertVectorCorpusStats(t, stats, graph.VectorCorpusStats{
		VectorCount:           2,
		RepositoryVectorCount: 1,
		Dims:                  2,
	})
	assertVectorCorpusRows(t, store, []vectorCorpusTestRow{
		{nodeID: "A/new", repoPrefix: "A", dims: 2},
		{nodeID: "B/keep", repoPrefix: "B", dims: 2},
	})

	stats, err = store.ReplaceVectorCorpus(ctx, "A", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertVectorCorpusStats(t, stats, graph.VectorCorpusStats{VectorCount: 1, Dims: 2})
	assertVectorCorpusRows(t, store, []vectorCorpusTestRow{
		{nodeID: "B/keep", repoPrefix: "B", dims: 2},
	})
}

func TestVectorCorpusStatsForRepoReportsGlobalAndScopedCounts(t *testing.T) {
	store := openVectorCorpusTestStore(t, filepath.Join(t.TempDir(), "repo-stats.sqlite"))
	ctx := context.Background()
	addVectorCorpusNodes(t, store, "A", "A/one", "A/two")
	addVectorCorpusNodes(t, store, "B", "B/keep")

	if _, err := store.ReplaceVectorCorpus(ctx, "A", 2, []graph.VectorCorpusItem{
		{NodeID: "A/one", Vec: []float32{1, 0}},
		{NodeID: "A/two#chunk0", ParentID: "A/two", Vec: []float32{0, 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplaceVectorCorpus(ctx, "B", 2, []graph.VectorCorpusItem{
		{NodeID: "B/keep", Vec: []float32{1, 1}},
	}); err != nil {
		t.Fatal(err)
	}

	stats, err := store.VectorCorpusStatsForRepo(ctx, "A", 2)
	if err != nil {
		t.Fatal(err)
	}
	assertVectorCorpusStats(t, stats, graph.VectorCorpusStats{
		VectorCount: 3, ChunkCount: 1,
		RepositoryVectorCount: 2, RepositoryChunkCount: 1,
		Dims: 2,
	})

	stats, err = store.VectorCorpusStatsForRepo(ctx, "B", 0)
	if err != nil {
		t.Fatal(err)
	}
	assertVectorCorpusStats(t, stats, graph.VectorCorpusStats{
		VectorCount: 3, ChunkCount: 1,
		RepositoryVectorCount: 1,
		Dims:                  2,
	})

	stats, err = store.VectorCorpusStatsForRepo(ctx, "missing", 0)
	if err != nil {
		t.Fatal(err)
	}
	assertVectorCorpusStats(t, stats, graph.VectorCorpusStats{
		VectorCount: 3, ChunkCount: 1, Dims: 2,
	})
}

func TestVectorCorpusChunkParentsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chunks.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	addVectorCorpusNodes(t, store, "repo", "repo/symbol")
	_, err = store.ReplaceVectorCorpus(context.Background(), "repo", 2, []graph.VectorCorpusItem{
		{NodeID: "repo/symbol#chunk0", ParentID: "repo/symbol", Vec: []float32{1, 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	hits, err := store.SimilarTo([]float32{1, 0}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].NodeID != "repo/symbol#chunk0" || hits[0].ParentID != "repo/symbol" {
		t.Fatalf("durable chunk identity lost after reopen: %#v", hits)
	}
	stats, err := store.VectorCorpusStats(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	assertVectorCorpusStats(t, stats, graph.VectorCorpusStats{VectorCount: 1, ChunkCount: 1, Dims: 2})
}

func TestReplaceVectorCorpusFailuresPreserveCommittedRows(t *testing.T) {
	store := openVectorCorpusTestStore(t, filepath.Join(t.TempDir(), "rollback.sqlite"))
	ctx := context.Background()
	addVectorCorpusNodes(t, store, "A", "A/old", "A/new", "A/fail")
	addVectorCorpusNodes(t, store, "B", "B/keep", "B/fresh", "B/foreign-parent")
	if _, err := store.ReplaceVectorCorpus(ctx, "A", 2, []graph.VectorCorpusItem{
		{NodeID: "A/old", Vec: []float32{1, 0}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplaceVectorCorpus(ctx, "B", 2, []graph.VectorCorpusItem{
		{NodeID: "B/keep", Vec: []float32{0, 1}},
	}); err != nil {
		t.Fatal(err)
	}

	assertPreserved := func(label string) {
		t.Helper()
		assertVectorCorpusRows(t, store, []vectorCorpusTestRow{
			{nodeID: "A/old", repoPrefix: "A", dims: 2},
			{nodeID: "B/keep", repoPrefix: "B", dims: 2},
		})
	}

	if _, err := store.ReplaceVectorCorpus(ctx, "A", 2, []graph.VectorCorpusItem{
		{NodeID: "A/bad", Vec: []float32{1}},
	}); err == nil {
		t.Fatal("dimension mismatch unexpectedly succeeded")
	}
	assertPreserved("invalid dimensions")

	if _, err := store.ReplaceVectorCorpus(ctx, "A", 2, []graph.VectorCorpusItem{
		{NodeID: "B/fresh", Vec: []float32{1, 1}},
	}); err == nil {
		t.Fatal("fresh cross-repository node ownership conflict unexpectedly succeeded")
	}
	assertPreserved("fresh ownership conflict")

	if _, err := store.ReplaceVectorCorpus(ctx, "A", 2, []graph.VectorCorpusItem{
		{NodeID: "B/foreign-parent#chunk0", ParentID: "B/foreign-parent", Vec: []float32{1, 1}},
	}); err == nil {
		t.Fatal("foreign chunk parent unexpectedly succeeded")
	}
	assertPreserved("foreign chunk parent")

	if _, err := store.ReplaceVectorCorpus(ctx, "A", 2, []graph.VectorCorpusItem{
		{NodeID: "A/missing#chunk0", ParentID: "A/missing", Vec: []float32{1, 1}},
	}); err == nil {
		t.Fatal("missing chunk parent unexpectedly succeeded")
	}
	assertPreserved("missing chunk parent")

	if _, err := store.ReplaceVectorCorpus(ctx, "A", 2, []graph.VectorCorpusItem{
		{NodeID: "A/old#chunk01", ParentID: "A/old", Vec: []float32{1, 1}},
	}); err == nil {
		t.Fatal("malformed chunk ID unexpectedly succeeded")
	}
	assertPreserved("malformed chunk ID")

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.ReplaceVectorCorpus(cancelled, "A", 2, []graph.VectorCorpusItem{
		{NodeID: "A/cancelled", Vec: []float32{1, 1}},
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled replacement error = %v, want context.Canceled", err)
	}
	assertPreserved("cancellation")

	if _, err := store.writerDB.Exec(`
CREATE TRIGGER fail_vector_corpus_insert
BEFORE INSERT ON vectors
WHEN NEW.node_id = 'A/fail'
BEGIN
    SELECT RAISE(ABORT, 'injected vector failure');
END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplaceVectorCorpus(ctx, "A", 2, []graph.VectorCorpusItem{
		{NodeID: "A/new", Vec: []float32{1, 1}},
		{NodeID: "A/fail", Vec: []float32{-1, 1}},
	}); err == nil {
		t.Fatal("injected insertion failure unexpectedly succeeded")
	}
	assertPreserved("transaction failure")
}

func TestVectorCorpusStatsDiscoveryRejectsMixedDimensions(t *testing.T) {
	store := openVectorCorpusTestStore(t, filepath.Join(t.TempDir(), "stats.sqlite"))
	ctx := context.Background()
	addVectorCorpusNodes(t, store, "A", "A/three")
	addVectorCorpusNodes(t, store, "B", "B/two")

	stats, err := store.VectorCorpusStats(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertVectorCorpusStats(t, stats, graph.VectorCorpusStats{})

	if _, err := store.ReplaceVectorCorpus(ctx, "A", 3, []graph.VectorCorpusItem{
		{NodeID: "A/three", Vec: []float32{1, 0, 0}},
	}); err != nil {
		t.Fatal(err)
	}
	stats, err = store.VectorCorpusStats(ctx, -1)
	if err != nil {
		t.Fatal(err)
	}
	assertVectorCorpusStats(t, stats, graph.VectorCorpusStats{VectorCount: 1, Dims: 3})

	if _, err := store.ReplaceVectorCorpus(ctx, "B", 2, []graph.VectorCorpusItem{
		{NodeID: "B/two", Vec: []float32{0, 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.VectorCorpusStats(ctx, 0); !errors.Is(err, errVectorCorpusAmbiguousDims) {
		t.Fatalf("mixed-dimension discovery error = %v, want %v", err, errVectorCorpusAmbiguousDims)
	}
	stats, err = store.VectorCorpusStats(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	assertVectorCorpusStats(t, stats, graph.VectorCorpusStats{VectorCount: 1, Dims: 3})
}

func TestSimilarToObservesCompleteCorpusDuringReplacement(t *testing.T) {
	store := openVectorCorpusTestStore(t, filepath.Join(t.TempDir(), "visibility.sqlite"))
	ctx := context.Background()
	oldItems := vectorCorpusItems("repo/old-", 64)
	newItems := vectorCorpusItems("repo/new-", 64)
	nodeIDs := make([]string, 0, len(oldItems)+len(newItems))
	for _, item := range oldItems {
		nodeIDs = append(nodeIDs, item.NodeID)
	}
	for _, item := range newItems {
		nodeIDs = append(nodeIDs, item.NodeID)
	}
	addVectorCorpusNodes(t, store, "repo", nodeIDs...)
	if _, err := store.ReplaceVectorCorpus(ctx, "repo", 2, oldItems); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	observed := make(chan struct{})
	errCh := make(chan error, 1)
	var once sync.Once
	go func() {
		for {
			select {
			case <-stop:
				errCh <- nil
				return
			default:
			}
			hits, err := store.SimilarTo([]float32{1, 1}, 128)
			if err != nil {
				errCh <- err
				return
			}
			if err := classifyCompleteVectorCorpus(hits, len(oldItems)); err != nil {
				errCh <- err
				return
			}
			once.Do(func() { close(observed) })
		}
	}()
	<-observed

	for i := 0; i < 24; i++ {
		items := newItems
		if i%2 == 1 {
			items = oldItems
		}
		if _, err := store.ReplaceVectorCorpus(ctx, "repo", 2, items); err != nil {
			close(stop)
			if readErr := <-errCh; readErr != nil {
				t.Errorf("concurrent vector read: %v", readErr)
			}
			t.Fatal(err)
		}
	}
	close(stop)
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestOpenV9RebuildsOnlyVectorCorpus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v9.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.AddNode(&graph.Node{
		ID:         "repo/file.go::Kept",
		Kind:       graph.KindFunction,
		Name:       "Kept",
		FilePath:   "repo/file.go",
		RepoPrefix: "repo",
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`DROP INDEX IF EXISTS vectors_by_repo`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`DROP TABLE vectors`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE vectors (
			node_id TEXT PRIMARY KEY,
			dims INTEGER NOT NULL,
			vec BLOB NOT NULL
		) WITHOUT ROWID`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO vectors(node_id, dims, vec) VALUES (?, ?, ?)`, "repo/legacy", 2, encodeVec([]float32{1, 0})); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`PRAGMA user_version = 9`); err != nil {
			t.Fatal(err)
		}
	})

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if store.NeedsRebuild() {
		t.Fatal("v9 vector-sidecar migration unexpectedly wiped the graph store")
	}
	if got := store.GetNode("repo/file.go::Kept"); got == nil {
		t.Fatal("v9 vector migration discarded graph topology")
	}
	stats, err := store.VectorCorpusStats(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	assertVectorCorpusStats(t, stats, graph.VectorCorpusStats{})

	columns := make(map[string]bool)
	rows, err := store.db.Query(`PRAGMA table_info(vectors)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !columns["repo_prefix"] || !columns["parent_id"] {
		t.Fatalf("migrated vector columns = %v", columns)
	}
}

type vectorCorpusTestRow struct {
	nodeID     string
	repoPrefix string
	parentID   string
	dims       int
}

func openVectorCorpusTestStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func addVectorCorpusNodes(t *testing.T, store *Store, repoPrefix string, ids ...string) {
	t.Helper()
	nodes := make([]*graph.Node, 0, len(ids))
	for _, id := range ids {
		nodes = append(nodes, &graph.Node{
			ID:         id,
			Kind:       graph.KindFunction,
			Name:       id,
			FilePath:   id,
			RepoPrefix: repoPrefix,
		})
	}
	store.AddBatch(nodes, nil)
}

func assertVectorCorpusStats(t *testing.T, got, want graph.VectorCorpusStats) {
	t.Helper()
	if got != want {
		t.Fatalf("vector corpus stats = %+v, want %+v", got, want)
	}
}

func assertVectorCorpusRows(t *testing.T, store *Store, want []vectorCorpusTestRow) {
	t.Helper()
	rows, err := store.db.Query(`SELECT node_id, repo_prefix, parent_id, dims FROM vectors ORDER BY node_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []vectorCorpusTestRow
	for rows.Next() {
		var row vectorCorpusTestRow
		if err := rows.Scan(&row.nodeID, &row.repoPrefix, &row.parentID, &row.dims); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("vector rows = %#v, want %#v", got, want)
	}
}

func vectorCorpusItems(prefix string, count int) []graph.VectorCorpusItem {
	items := make([]graph.VectorCorpusItem, count)
	for i := range items {
		items[i] = graph.VectorCorpusItem{
			NodeID: fmt.Sprintf("%s%03d", prefix, i),
			Vec:    []float32{1, 1},
		}
	}
	return items
}

func classifyCompleteVectorCorpus(hits []graph.VectorHit, want int) error {
	if len(hits) != want {
		return fmt.Errorf("observed partial vector corpus: got %d hits, want %d", len(hits), want)
	}
	ids := make([]string, len(hits))
	for i, hit := range hits {
		ids[i] = hit.NodeID
	}
	sort.Strings(ids)
	old := strings.HasPrefix(ids[0], "repo/old-")
	newCorpus := strings.HasPrefix(ids[0], "repo/new-")
	if !old && !newCorpus {
		return fmt.Errorf("unexpected vector corpus ID %q", ids[0])
	}
	prefix := "repo/old-"
	if newCorpus {
		prefix = "repo/new-"
	}
	for _, id := range ids {
		if !strings.HasPrefix(id, prefix) {
			return fmt.Errorf("observed mixed vector corpus: %q and prefix %q", id, prefix)
		}
	}
	return nil
}
