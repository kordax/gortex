package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// The fixture is a real cold publication followed by a fresh Indexer loading
// persisted receipts. Generated noise is excluded either per file or as a
// whole subtree. Fixture writes, parsing and reopen are outside the timer.
func benchmarkCensusWalkFixture(b *testing.B, noiseFiles int, pruneTree bool) (*Indexer, string) {
	b.Helper()
	root := b.TempDir()
	const sourceFiles = 519
	for i := range sourceFiles {
		body := fmt.Sprintf("package fixture\nfunc Symbol%d() int { return %d }\n", i, i)
		require.NoError(b, os.WriteFile(filepath.Join(root, fmt.Sprintf("source%04d.go", i)), []byte(body), 0o600))
	}
	for i := range noiseFiles {
		dir := filepath.Join(root, "generated", fmt.Sprintf("package%03d", i%64))
		require.NoError(b, os.MkdirAll(dir, 0o700))
		require.NoError(b, os.WriteFile(filepath.Join(dir, fmt.Sprintf("ignored%05d.blob", i)), []byte("opaque generated cache bytes\n"), 0o600))
	}
	cfg := config.Default().Index
	cfg.Exclude = append(cfg.Exclude, "**/*.blob")
	if pruneTree {
		cfg.Exclude = append(cfg.Exclude, "generated/")
	}
	store, err := store_sqlite.Open(filepath.Join(b.TempDir(), "graph.sqlite"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })
	newIndexer := func() *Indexer {
		registry := parser.NewRegistry()
		registry.Register(languages.NewGoExtractor())
		idx := New(store, registry, cfg, zap.NewNop())
		idx.SetRepoPrefix("fixture")
		idx.SetDeferResolve(true)
		idx.SetDeferGlobalPasses(true)
		idx.storeRootPath(root)
		return idx
	}
	cold := newIndexer()
	_, err = cold.IndexCtx(context.Background(), root)
	require.NoError(b, err)
	cold.Close()
	warm := newIndexer()
	warm.SetFileMtimes(store.LoadFileMtimes("fixture"))
	b.Cleanup(warm.Close)
	require.Len(b, warm.publishFileMtimes(), sourceFiles)
	return warm, root
}

func BenchmarkWarmCensusIgnoredFilesSQLite(b *testing.B) {
	for _, tc := range []struct {
		name  string
		noise int
		prune bool
	}{
		{name: "source_only"},
		{name: "ignored_files_16384", noise: 16384},
		{name: "pruned_tree_16384", noise: 16384, prune: true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.StopTimer()
			idx, root := benchmarkCensusWalkFixture(b, tc.noise, tc.prune)
			check := func() {
				changed, deleted, detected, err := idx.changedSinceMtimesCensus(root)
				require.NoError(b, err)
				require.Empty(b, changed)
				require.Empty(b, deleted)
				require.Equal(b, 519, detected)
			}
			check() // Warm filesystem and directory-ignore caches equally.
			if prefix := os.Getenv("GORTEX_CENSUS_CPU_PROFILE"); prefix != "" {
				file, err := os.OpenFile(fmt.Sprintf("%s.%s.n%d.pprof", prefix, tc.name, b.N), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
				require.NoError(b, err)
				require.NoError(b, pprof.StartCPUProfile(file))
				defer func() { pprof.StopCPUProfile(); require.NoError(b, file.Close()) }()
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for range b.N {
				check()
			}
			b.StopTimer()
		})
	}
}
