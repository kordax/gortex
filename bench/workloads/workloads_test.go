// Package workloads measures fixed synthetic source against the real indexer and
// SQLite backend. It does not start a daemon or use tracked repositories.
package workloads

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/testenv"
	"go.uber.org/zap"
)

const functionsPerFile = 8

var fixtureSizes = []int{32, 128}

type fixture struct {
	root  string
	names []string
	bytes int64
}

func writeFixture(tb testing.TB, files int) fixture {
	tb.Helper()
	f := fixture{root: tb.TempDir()}
	require.NoError(tb, os.WriteFile(filepath.Join(f.root, "go.mod"), []byte("module example.com/gortexworkload\n\ngo 1.27\n"), 0o600))
	for file := 0; file < files; file++ {
		var source strings.Builder
		source.WriteString("package workload\n\n")
		for fn := 0; fn < functionsPerFile; fn++ {
			name := fmt.Sprintf("WorkloadFile%03dFunction%02d", file, fn)
			f.names = append(f.names, name)
			if fn == 0 {
				fmt.Fprintf(&source, "func %s(value int) int { return value + %d }\n", name, file)
			} else {
				fmt.Fprintf(&source, "func %s(value int) int { return WorkloadFile%03dFunction%02d(value) + %d }\n", name, file, fn-1, fn)
			}
		}
		body := []byte(source.String())
		f.bytes += int64(len(body))
		require.NoError(tb, os.WriteFile(filepath.Join(f.root, fmt.Sprintf("file_%03d.go", file)), body, 0o600))
	}
	return f
}

func newRegistry() *parser.Registry {
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	return reg
}

func openIndexer(tb testing.TB, reg *parser.Registry) (*store_sqlite.Store, *indexer.Indexer) {
	tb.Helper()
	store, err := store_sqlite.Open(filepath.Join(tb.TempDir(), "graph.sqlite"))
	require.NoError(tb, err)
	idx := indexer.New(store, reg, (config.Config{}).Index, zap.NewNop())
	return store, idx
}

func indexedEngine(tb testing.TB, f fixture, reg *parser.Registry) (*store_sqlite.Store, *query.Engine) {
	tb.Helper()
	store, idx := openIndexer(tb, reg)
	tb.Cleanup(func() { require.NoError(tb, store.Close()) })
	result, err := idx.Index(f.root)
	require.NoError(tb, err)
	require.Empty(tb, result.Errors)
	require.Empty(tb, result.FailedFiles)
	eng := query.NewEngine(store)
	eng.SetSearch(idx.Search())
	validateIndex(tb, f, store, eng)
	return store, eng
}

func hasExactHit(eng *query.Engine, name string) bool {
	for _, node := range eng.SearchSymbols(name, 20) {
		if node.Name == name {
			return true
		}
	}
	return false
}

func validateIndex(tb testing.TB, f fixture, store *store_sqlite.Store, eng *query.Engine) {
	tb.Helper()
	require.Equal(tb, len(f.names), store.Stats().ByKind["function"])
	for _, name := range f.names {
		require.True(tb, hasExactHit(eng, name), "missing indexed symbol %s", name)
	}
	// Search may intentionally return decomposed fallback matches. A miss
	// must never invent the exact symbol, even when related hits are returned.
	require.False(tb, hasExactHit(eng, "DefinitelyAbsentWorkloadSymbol"))
}

func TestFixtureDeterministic(t *testing.T) {
	testenv.Sandbox(t)
	first := writeFixture(t, 2)
	second := writeFixture(t, 2)
	require.Equal(t, first.names, second.names)
	require.Equal(t, first.bytes, second.bytes)
	for _, name := range []string{"go.mod", "file_000.go", "file_001.go"} {
		left, err := os.ReadFile(filepath.Join(first.root, name))
		require.NoError(t, err)
		right, err := os.ReadFile(filepath.Join(second.root, name))
		require.NoError(t, err)
		require.Equal(t, left, right, name)
	}
}

func TestSQLiteWorkload(t *testing.T) {
	testenv.Sandbox(t)
	f := writeFixture(t, 4)
	_, eng := indexedEngine(t, f, newRegistry())
	require.True(t, hasExactHit(eng, "WorkloadFile003Function07"))
}

func benchmarkEnvironment(b *testing.B) {
	b.Helper()
	testenv.Sandbox(b)
	// SQLite startup diagnostics would otherwise split Go's benchmark result
	// lines. Assertions still fail normally; restore logging for other packages.
	previous := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(previous) })
}

func BenchmarkColdIndex(b *testing.B) {
	benchmarkEnvironment(b)
	reg := newRegistry()
	for _, files := range fixtureSizes {
		b.Run(fmt.Sprintf("files=%d", files), func(b *testing.B) {
			b.StopTimer()
			f := writeFixture(b, files)
			b.ReportAllocs()
			b.SetBytes(f.bytes)
			for i := 0; i < b.N; i++ {
				func() {
					// Every iteration gets an empty SQLite database. Opening it,
					// schema creation, fixture I/O and validation are not timed.
					store, idx := openIndexer(b, reg)
					defer func() { require.NoError(b, store.Close()) }()
					b.StartTimer()
					result, err := idx.Index(f.root)
					b.StopTimer()
					require.NoError(b, err)
					require.Empty(b, result.Errors)
					require.Empty(b, result.FailedFiles)
					eng := query.NewEngine(store)
					eng.SetSearch(idx.Search())
					validateIndex(b, f, store, eng)
				}()
			}
			b.ReportMetric(float64(files), "files/op")
			b.ReportMetric(float64(len(f.names)), "functions/op")
		})
	}
}

func BenchmarkWarmSearch(b *testing.B) {
	benchmarkEnvironment(b)
	reg := newRegistry()
	for _, files := range fixtureSizes {
		b.Run(fmt.Sprintf("files=%d", files), func(b *testing.B) {
			f := writeFixture(b, files)
			_, eng := indexedEngine(b, f, reg)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !hasExactHit(eng, f.names[i%len(f.names)]) {
					b.Fatal("search lost an indexed symbol")
				}
			}
		})
	}
}

func BenchmarkParallelSearch(b *testing.B) {
	benchmarkEnvironment(b)
	reg := newRegistry()
	for _, files := range fixtureSizes {
		b.Run(fmt.Sprintf("files=%d", files), func(b *testing.B) {
			f := writeFixture(b, files)
			_, eng := indexedEngine(b, f, reg)
			var failed atomic.Bool
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					if !hasExactHit(eng, f.names[i%len(f.names)]) {
						failed.Store(true)
					}
					i++
				}
			})
			b.StopTimer()
			require.False(b, failed.Load(), "parallel search lost an indexed symbol")
		})
	}
}
