package indexer

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/testenv"
	"go.uber.org/zap"
)

// BenchmarkIncrementalPublicOneFileSQLite measures the actual public point
// mutation, including repository admission, parse, resolver and derived tails.
func BenchmarkIncrementalPublicOneFileSQLite(b *testing.B) {
	testenv.Sandbox(b)
	previous := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(previous) })
	for _, files := range []int{64, 512} {
		for _, shape := range []bool{false, true} {
			kind := "body"
			if shape {
				kind = "shape"
			}
			b.Run(fmt.Sprintf("files=%d/%s", files, kind), func(b *testing.B) {
				b.StopTimer()
				f := newIncrementalPerformanceFixture(b, files)
				f.idx.deferGlobalPasses.Store(false)
				for revision := 1; revision <= 2; revision++ {
					f.write(b, revision, true)
					require.NoError(b, f.idx.IndexFile("file_000.go"))
				}
				// Unlike go test -cpuprofile, this optional profile starts after
				// initial indexing/warmup. Distinct names avoid overwriting Go's
				// calibration run or another selected workload.
				var profile *os.File
				defer func() {
					if profile != nil {
						pprof.StopCPUProfile()
						require.NoError(b, profile.Close())
					}
				}()
				if prefix := os.Getenv("GORTEX_INCREMENTAL_CPU_PROFILE"); prefix != "" {
					path := fmt.Sprintf("%s.files%d.%s.n%d.pprof", prefix, files, kind, b.N)
					var err error
					profile, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
					require.NoError(b, err)
					require.NoError(b, pprof.StartCPUProfile(profile))
				}
				b.ReportAllocs()
				for iteration := 0; iteration < b.N; iteration++ {
					f.write(b, iteration+1, shape)
					b.StartTimer()
					err := f.idx.IndexFile("file_000.go")
					b.StopTimer()
					require.NoError(b, err)
				}
				if profile != nil {
					pprof.StopCPUProfile()
					require.NoError(b, profile.Close())
					profile = nil
				}
				b.ReportMetric(float64(files), "fixture-files")
				b.ReportMetric(float64(f.store.NodeCount()), "fixture-nodes")
				b.ReportMetric(float64(f.store.EdgeCount()), "fixture-edges")
				require.Len(b, f.store.FindNodesByName("Call000"), 1)
				implementations := 0
				for range f.store.EdgesByKind(graph.EdgeImplements) {
					implementations++
				}
				require.Equal(b, files, implementations, "point mutation lost inferred interface links")
			})
		}
	}
}

// This workload uses real parser, resolver, derived passes and SQLite writes.
// No daemon, external model service or tracked repository is involved.
type incrementalPerformanceFixture struct {
	root  string
	store *store_sqlite.Store
	idx   *Indexer
	multi *MultiIndexer
}

func incrementalPerformanceSource(file, revision int, shape bool) string {
	field := 0
	if shape {
		field = revision % 2
	}
	target := fmt.Sprintf("Helper%03d", file)
	if !shape && revision%2 == 1 {
		target = fmt.Sprintf("Alternate%03d", file)
	}
	return fmt.Sprintf(`package workload

type Runner%03d interface { Run%03d(int) int }
type Worker%03d struct { Revision%d int }
func (w Worker%03d) Run%03d(value int) int { return Helper%03d(value) }
func Helper%03d(value int) int { return value + %d }
func Alternate%03d(value int) int { return value - %d }
func Call%03d(value int) int { return %s(value) }
func Consume%03d(r Runner%03d, value int) int { return r.Run%03d(value) }
func Incoming%03d(value int) int { return Call000(value) }
`, file, file, file, field, file, file, file,
		file, file, file, file, file, target, file, file, file, file)
}

func newIncrementalPerformanceFixture(tb testing.TB, files int) *incrementalPerformanceFixture {
	tb.Helper()
	root := tb.TempDir()
	require.NoError(tb, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/incrementalworkload\n\ngo 1.27\n"), 0o600))
	for file := 0; file < files; file++ {
		name := filepath.Join(root, fmt.Sprintf("file_%03d.go", file))
		require.NoError(tb, os.WriteFile(name, []byte(incrementalPerformanceSource(file, 0, true)), 0o600))
	}
	return openIncrementalPerformanceFixture(tb, root)
}

func openIncrementalPerformanceFixture(tb testing.TB, root string) *incrementalPerformanceFixture {
	tb.Helper()
	store, err := store_sqlite.Open(filepath.Join(tb.TempDir(), "graph.sqlite"))
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, store.Close()) })
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	idx := New(store, registry, (config.Config{}).Index, zap.NewNop())
	result, err := idx.Index(root)
	require.NoError(tb, err)
	require.Empty(tb, result.Errors)
	require.Empty(tb, result.FailedFiles)
	multi := NewMultiIndexer(store, registry, idx.search, nil, zap.NewNop())
	multi.indexers[""] = idx
	multi.repos[""] = &RepoMetadata{RepoPrefix: "", RootPath: root}
	// Keep the real bounded parse/resolver catch-up, but measure its derived
	// tail separately through the public MultiIndexer coordinator/report.
	idx.deferGlobalPasses.Store(true)
	return &incrementalPerformanceFixture{root: root, store: store, idx: idx, multi: multi}
}

func (f *incrementalPerformanceFixture) write(tb testing.TB, revision int, shape bool) {
	tb.Helper()
	require.NoError(tb, os.WriteFile(filepath.Join(f.root, "file_000.go"), []byte(incrementalPerformanceSource(0, revision, shape)), 0o600))
}

func (f *incrementalPerformanceFixture) update() (*IndexResult, IncrementalDerivedReport, error) {
	result, err := f.idx.incrementalReindexWatcherPaths(f.root, []string{"file_000.go"})
	if err != nil {
		return result, IncrementalDerivedReport{}, err
	}
	report := f.multi.RunIncrementalDerivedPasses(context.Background(), map[string]DerivedInvalidationPlan{
		"": result.DerivedInvalidation,
	})
	return result, report, nil
}

// Compare semantic relationship identities, rather than counts: a lost edge
// and a wrong replacement edge must not cancel one another out.
func incrementalPerformanceRelations(store graph.Store) []string {
	var identities []string
	for _, edge := range store.AllEdges() {
		switch edge.Kind {
		case graph.EdgeCalls, graph.EdgeImplements, graph.EdgeOverrides, graph.EdgeMemberOf:
			from, to := store.GetNode(edge.From), store.GetNode(edge.To)
			if from == nil || to == nil || from.Kind == graph.KindField {
				continue
			}
			identities = append(identities, fmt.Sprintf("%s|%s|%s|%s|%d", edge.Kind, edge.From, edge.To, edge.FilePath, edge.Line))
		}
	}
	sort.Strings(identities)
	return identities
}

func TestIncrementalPerformanceCapabilityFamiliesMatchColdIndex(t *testing.T) {
	testenv.Sandbox(t)
	root := t.TempDir()
	write := func(field, literal int, comment string) {
		source := fmt.Sprintf(`package workload
import "os"
import "os/exec"
type Holder struct { Revision%d int }
func Capability(value int) int {
 _ = exec.Command("true")
 if os.Getenv("TOKEN_NAME") != "" { return value + %d }
 return value
}
%s
`, field, literal, comment)
		require.NoError(t, os.WriteFile(filepath.Join(root, "file_000.go"), []byte(source), 0o600))
	}
	identities := func(store graph.Store) []string {
		var values []string
		for _, edge := range store.AllEdges() {
			values = append(values, fmt.Sprintf("%s|%s|%s|%s|%d", edge.Kind, edge.From, edge.To, edge.FilePath, edge.Line))
		}
		sort.Strings(values)
		return values
	}
	write(0, 1, "")
	f := openIncrementalPerformanceFixture(t, root)
	f.idx.deferGlobalPasses.Store(false)
	initial := identities(f.store)
	for _, kind := range []string{"consumes|", "executes_process|"} {
		found := false
		for _, identity := range initial {
			found = found || strings.HasPrefix(identity, kind)
		}
		require.True(t, found, "fixture must exercise %s synthesis", kind)
	}
	steps := []struct {
		name    string
		field   int
		literal int
		comment string
	}{
		{"first-shape", 1, 1, ""},
		{"warm-shape", 0, 1, ""},
		{"literal", 0, 2, ""},
		{"documentation", 0, 2, "// Documentation only."},
		{"no-op", 0, 2, "// Documentation only."},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			write(step.field, step.literal, step.comment)
			require.NoError(t, f.idx.IndexFile("file_000.go"))
			cold := openIncrementalPerformanceFixture(t, root)
			require.Equal(t, identities(cold.store), identities(f.store), "incremental publication must preserve every derived family")
			require.NoError(t, f.store.VerifyEdgeIdentities())
		})
	}
}

func TestIncrementalPerformancePublicBodyMatchesColdIndex(t *testing.T) {
	testenv.Sandbox(t)
	f := newIncrementalPerformanceFixture(t, 4)
	f.idx.deferGlobalPasses.Store(false)
	// Establish persisted incremental fingerprints before the body-only edit;
	// the first mutation otherwise takes a conservative legacy fallback.
	for revision := 1; revision <= 2; revision++ {
		f.write(t, revision, true)
		require.NoError(t, f.idx.IndexFile("file_000.go"))
	}
	f.write(t, 1, false)
	require.NoError(t, f.idx.IndexFile("file_000.go"))
	cold := openIncrementalPerformanceFixture(t, f.root)
	require.Equal(t, incrementalPerformanceRelations(cold.store), incrementalPerformanceRelations(f.store))
}

func TestIncrementalPerformanceFixtureMatchesColdIndex(t *testing.T) {
	testenv.Sandbox(t)
	f := newIncrementalPerformanceFixture(t, 4)
	initial := incrementalPerformanceRelations(f.store)
	var hasCall, hasImplementation bool
	for _, identity := range initial {
		hasCall = hasCall || strings.HasPrefix(identity, string(graph.EdgeCalls)+"|")
		hasImplementation = hasImplementation || strings.HasPrefix(identity, string(graph.EdgeImplements)+"|")
	}
	require.True(t, hasCall, "fixture must exercise resolved calls")
	require.True(t, hasImplementation, "fixture must exercise interface inference")
	for _, shape := range []bool{true, false} {
		for revision := 1; revision <= 2; revision++ {
			f.write(t, revision, shape)
			result, report, err := f.update()
			require.NoError(t, err)
			require.Empty(t, result.Errors)
			require.Empty(t, result.FailedFiles)
			require.Equal(t, 1, report.Files, "shape=%v revision=%d plan=%+v", shape, revision, result.DerivedInvalidation)
			require.Equal(t, []string{"file_000.go"}, result.DerivedInvalidation.ReplacedFiles)
			cold := openIncrementalPerformanceFixture(t, f.root)
			require.Equal(t, incrementalPerformanceRelations(cold.store), incrementalPerformanceRelations(f.store), "shape=%v revision=%d", shape, revision)
		}
	}
	// An identical save keeps graph entries and cannot advertise a structural
	// replacement frontier. Metadata-only plans have a separate unit contract.
	source := incrementalPerformanceSource(0, 2, false)
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "file_000.go"), []byte(source), 0o600))
	result, _, err := f.update()
	require.NoError(t, err)
	require.Empty(t, result.DerivedInvalidation.ReplacedFiles, "no-op")
	cold := openIncrementalPerformanceFixture(t, f.root)
	require.Equal(t, incrementalPerformanceRelations(cold.store), incrementalPerformanceRelations(f.store), "no-op")
}

// BenchmarkIncrementalCoordinatorOneFileSQLite measures the bounded coordinator
// with deferred global tails. Use the public benchmark for end-to-end latency.
func BenchmarkIncrementalCoordinatorOneFileSQLite(b *testing.B) {
	testenv.Sandbox(b)
	previous := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(previous) })
	for _, files := range []int{64, 512} {
		for _, shape := range []bool{false, true} {
			kind := "body"
			if shape {
				kind = "shape"
			}
			b.Run(fmt.Sprintf("files=%d/%s", files, kind), func(b *testing.B) {
				b.StopTimer()
				f := newIncrementalPerformanceFixture(b, files)
				// A cold Index currently lacks persisted incremental fingerprints.
				// Warm two real shape mutations so measurements exercise exact
				// invalidation, not the conservative first-mutation fallback.
				for revision := 1; revision <= 2; revision++ {
					f.write(b, revision, true)
					result, report, err := f.update()
					require.NoError(b, err)
					require.Empty(b, result.Errors)
					if revision == 2 {
						require.False(b, report.LegacyFallback)
					}
				}
				var totals IncrementalDerivedReport
				var catchup time.Duration
				b.ReportAllocs()
				for iteration := 0; iteration < b.N; iteration++ {
					f.write(b, iteration+1, shape)
					b.StartTimer()
					started := time.Now()
					result, report, err := f.update()
					elapsed := time.Since(started)
					b.StopTimer()
					require.NoError(b, err)
					require.Empty(b, result.Errors)
					require.Empty(b, result.FailedFiles)
					require.Equal(b, 1, report.Files)
					catchup += elapsed
					totals.ImplementsDuration += report.ImplementsDuration
					totals.OverridesDuration += report.OverridesDuration
					totals.TestsDuration += report.TestsDuration
					totals.EntryPointsDuration += report.EntryPointsDuration
					totals.CapabilityDuration += report.CapabilityDuration
					totals.FrameworkDuration += report.FrameworkDuration
					totals.ExternalCallsDuration += report.ExternalCallsDuration
					totals.CrossRepoDuration += report.CrossRepoDuration
					totals.ContractsDuration += report.ContractsDuration
				}
				b.ReportMetric(float64(files), "fixture-files")
				b.ReportMetric(float64(f.store.NodeCount()), "fixture-nodes")
				b.ReportMetric(float64(f.store.EdgeCount()), "fixture-edges")
				for _, stage := range []struct {
					name     string
					duration time.Duration
				}{
					{"coordinator-update", catchup},
					{"implements", totals.ImplementsDuration},
					{"overrides", totals.OverridesDuration},
					{"tests", totals.TestsDuration},
					{"entrypoints", totals.EntryPointsDuration},
					{"capability", totals.CapabilityDuration},
					{"framework", totals.FrameworkDuration},
					{"external", totals.ExternalCallsDuration},
					{"crossrepo", totals.CrossRepoDuration},
					{"contracts", totals.ContractsDuration},
				} {
					b.ReportMetric(float64(stage.duration.Nanoseconds())/float64(b.N), stage.name+"-ns/op")
				}
				// Assertions are outside the timer. Preserve both the changed
				// file's calls and incoming callers from untouched files.
				relations := incrementalPerformanceRelations(f.store)
				require.NotEmpty(b, relations)
				calls := 0
				for _, identity := range relations {
					if strings.HasPrefix(identity, string(graph.EdgeCalls)+"|") {
						calls++
					}
				}
				require.GreaterOrEqual(b, calls, files*2)
			})
		}
	}
}
