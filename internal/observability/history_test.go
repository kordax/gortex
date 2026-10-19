package observability

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestHistory(t *testing.T, now *time.Time) (*History, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "metrics.db")
	h, err := openHistory(path, func() time.Time { return *now })
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, h.Close()) })
	return h, path
}

func historyTestSample(at time.Time) HistorySample {
	return HistorySample{At: at.UnixMilli(), Values: map[string]float64{"gortex_runtime_goroutines": 42, `gortex_mcp_requests_total{outcome="success"}`: 10}}
}

func TestHistoryRetentionReopenAndRead(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	h, path := newTestHistory(t, &now)
	first := historyTestSample(now.Add(-HistoryRetention + time.Millisecond))
	require.NoError(t, h.Append(ctx, first))
	require.NoError(t, h.Append(ctx, historyTestSample(now)))
	rows, err := h.Read(ctx, time.Time{}, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, first, rows[0])
	assert.Equal(t, now.UnixMilli(), rows[1].At)

	now = now.Add(time.Millisecond)
	rows, err = h.Read(ctx, time.Time{}, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	var count int
	require.NoError(t, h.db.QueryRow("SELECT count(*) FROM metric_history").Scan(&count))
	assert.Equal(t, 1, count, "Read physically prunes expired rows")
	require.NoError(t, h.Close())
	now = now.Add(HistoryRetention)
	reopened, err := openHistory(path, func() time.Time { return now })
	require.NoError(t, err)
	defer func() { assert.NoError(t, reopened.Close()) }()
	rows, err = reopened.Read(ctx, time.Time{}, 10)
	require.NoError(t, err)
	assert.Empty(t, rows)
	require.NoError(t, reopened.db.QueryRow("SELECT count(*) FROM metric_history").Scan(&count))
	assert.Zero(t, count)
}

func TestHistoryDownsamplesWithLatestEndpointAndSince(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	h, _ := newTestHistory(t, &now)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		require.NoError(t, h.Append(ctx, historyTestSample(now.Add(time.Duration(i-9)*time.Second))))
	}
	rows, err := h.Read(ctx, time.Time{}, 4)
	require.NoError(t, err)
	require.Len(t, rows, 4)
	for i, offset := range []int{-9, -6, -3, 0} {
		assert.Equal(t, now.Add(time.Duration(offset)*time.Second).UnixMilli(), rows[i].At)
	}
	rows, err = h.Read(ctx, time.Time{}, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, now.UnixMilli(), rows[0].At)
	rows, err = h.Read(ctx, now.Add(-2*time.Second), 99)
	require.NoError(t, err)
	assert.Len(t, rows, 3)
	rows, err = h.Read(ctx, now.Add(time.Second), 99)
	require.NoError(t, err)
	assert.Empty(t, rows)
	_, err = h.Read(ctx, time.Time{}, -1)
	assert.Error(t, err)
	// Replacement is deterministic and does not grow the history.
	sample := historyTestSample(now)
	sample.Values["gortex_runtime_goroutines"] = 7
	require.NoError(t, h.Append(ctx, sample))
	rows, err = h.Read(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, float64(7), rows[0].Values["gortex_runtime_goroutines"])
}

func TestHistoryValidation(t *testing.T) {
	now := time.Now()
	h, _ := newTestHistory(t, &now)
	for _, tc := range []struct {
		name   string
		sample HistorySample
	}{
		{"expired", historyTestSample(now.Add(-HistoryRetention))},
		{"future", historyTestSample(now.Add(time.Millisecond))},
		{"empty", HistorySample{At: now.UnixMilli(), Values: map[string]float64{}}},
		{"nan", HistorySample{At: now.UnixMilli(), Values: map[string]float64{"gortex_test": math.NaN()}}},
		{"infinity", HistorySample{At: now.UnixMilli(), Values: map[string]float64{"gortex_test": math.Inf(1)}}},
		{"malformed", HistorySample{At: now.UnixMilli(), Values: map[string]float64{"gortex_test{request=secret}": 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Error(t, h.Append(context.Background(), tc.sample)) })
	}
	values := make(map[string]float64)
	for i := 0; i <= historyMaxSeries; i++ {
		values[fmt.Sprintf("gortex_value_%d", i)] = 1
	}
	assert.ErrorContains(t, h.Append(context.Background(), HistorySample{At: now.UnixMilli(), Values: values}), "series count")
	values = make(map[string]float64)
	for i := 0; i < historyMaxSeries; i++ {
		values[fmt.Sprintf("gortex_value_%d_%s", i, strings.Repeat("a", 180))] = 1
	}
	assert.ErrorContains(t, h.Append(context.Background(), HistorySample{At: now.UnixMilli(), Values: values}), "byte limit")
	var count int
	require.NoError(t, h.db.QueryRow("SELECT count(*) FROM metric_history").Scan(&count))
	assert.Zero(t, count)
}

func TestHistorySeriesGrammar(t *testing.T) {
	for _, key := range []string{"gortex_runtime_goroutines", "recording:rule_total", `gortex_value{a="one",b="quote\" slash\\ newline\n"}`, `gortex_value{repo="репозиторий"}`} {
		assert.True(t, validHistorySeries(key), "%s", key)
	}
	for _, key := range []string{"", "3metric", "a-b", "metric{}", `metric{a="x",a="y"}`, `metric{b="x",a="y"}`, `metric{a="x",}`, `metric{a="x"}junk`, `metric{a="bad\t"}`, "metric{a=\"line\nfeed\"}", `metric{a="unfinished}`, `metric{3a="x"}`, "metric{a=\"\xff\"}", strings.Repeat("a", 2049)} {
		assert.False(t, validHistorySeries(key), "%s", key)
	}
}

func TestHistoryRowAndByteCeilings(t *testing.T) {
	now := time.Now()
	h, _ := newTestHistory(t, &now)
	h.maxRows = 3
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		require.NoError(t, h.Append(ctx, historyTestSample(now.Add(time.Duration(i-6)*time.Second))))
	}
	rows, err := h.Read(ctx, time.Time{}, 99)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	assert.Equal(t, now.Add(-2*time.Second).UnixMilli(), rows[0].At)
	var size int64
	require.NoError(t, h.db.QueryRow("SELECT length(payload)+length(chart_payload) FROM metric_history LIMIT 1").Scan(&size))
	h.maxBytes = 2 * size
	require.NoError(t, h.Prune(ctx, now))
	rows, err = h.Read(ctx, time.Time{}, 99)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, now.UnixMilli(), rows[1].At)
	var total int64
	require.NoError(t, h.db.QueryRow("SELECT coalesce(sum(length(payload)+length(chart_payload)),0) FROM metric_history").Scan(&total))
	assert.LessOrEqual(t, total, h.maxBytes)
	h.maxBytes = 1
	assert.ErrorContains(t, h.Append(ctx, historyTestSample(now)), "storage limit")
}

func TestHistoryDecodedReadBudget(t *testing.T) {
	now := time.Now()
	h, _ := newTestHistory(t, &now)
	values := make(map[string]float64)
	for i := 0; i < historyMaxSeries; i++ {
		values[fmt.Sprintf("gortex_long_measurement_%d_%s", i, strings.Repeat("x", 70))] = 1
	}
	raw, err := json.Marshal(values)
	require.NoError(t, err)
	require.Less(t, len(raw), historyMaxSampleBytes)
	var payload bytes.Buffer
	writer := gzip.NewWriter(&payload)
	_, err = writer.Write(raw)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	emptyChart, _ := historyTestPayload(t, map[string]float64{})
	ctx := context.Background()
	tx, err := h.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	for i := 0; i < 100; i++ {
		_, err = tx.ExecContext(ctx, "INSERT INTO metric_history(at,payload,raw_bytes,chart_payload,chart_raw_bytes) VALUES(?,?,?,?,?)", now.Add(time.Duration(i-99)*time.Second).UnixMilli(), payload.Bytes(), len(raw), emptyChart, 2)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	rows, err := h.Read(ctx, time.Time{}, 100)
	require.NoError(t, err)
	assert.Len(t, rows, historyReadBytes/len(raw))
	assert.Equal(t, now.UnixMilli(), rows[len(rows)-1].At)
	assert.LessOrEqual(t, len(rows)*len(raw), historyReadBytes)
}

func TestHistoryPrunesFutureRecordsAndReclaimsPages(t *testing.T) {
	now := time.Now()
	h, path := newTestHistory(t, &now)
	values := make(map[string]float64)
	for i := 0; i < 1000; i++ {
		values[fmt.Sprintf("gortex_value_%d", i)] = float64(i * i)
	}
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		require.NoError(t, h.Append(ctx, HistorySample{At: now.Add(time.Duration(i-19) * time.Second).UnixMilli(), Values: values}))
	}
	before, err := os.Stat(path)
	require.NoError(t, err)
	var mode string
	var secureDelete, autoVacuum, pageSize, maxPages int
	require.NoError(t, h.db.QueryRow("PRAGMA journal_mode").Scan(&mode))
	require.NoError(t, h.db.QueryRow("PRAGMA secure_delete").Scan(&secureDelete))
	require.NoError(t, h.db.QueryRow("PRAGMA auto_vacuum").Scan(&autoVacuum))
	require.NoError(t, h.db.QueryRow("PRAGMA page_size").Scan(&pageSize))
	require.NoError(t, h.db.QueryRow("PRAGMA max_page_count").Scan(&maxPages))
	assert.Equal(t, "delete", mode)
	assert.Equal(t, 1, secureDelete)
	assert.Equal(t, 1, autoVacuum)
	assert.LessOrEqual(t, pageSize*maxPages, 72<<20)
	require.NoError(t, h.Prune(ctx, now.Add(-time.Hour)))
	rows, err := h.Read(ctx, time.Time{}, 10)
	require.NoError(t, err)
	assert.Empty(t, rows, "records from a future clock epoch must not appear")
	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.Less(t, after.Size(), before.Size())
	assert.NoFileExists(t, path+"-wal")
	assert.NoFileExists(t, path+"-journal")
}

func TestHistoryCorruptRecordsAreErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload []byte
		size    int
	}{
		{"gzip", []byte("not gzip"), 10},
		{"size", nil, 1},
		{"oversized", nil, historyMaxSampleBytes + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			h, _ := newTestHistory(t, &now)
			require.NoError(t, h.Append(context.Background(), historyTestSample(now)))
			switch tc.name {
			case "size":
				_, err := h.db.Exec("UPDATE metric_history SET raw_bytes=1")
				require.NoError(t, err)
			case "oversized":
				_, err := decodeHistoryValues([]byte("bad"), tc.size)
				assert.Error(t, err)
				return
			default:
				_, err := h.db.Exec("UPDATE metric_history SET payload=?", tc.payload)
				require.NoError(t, err)
			}
			rows, err := h.Read(context.Background(), time.Time{}, 10)
			assert.Error(t, err)
			assert.Nil(t, rows)
		})
	}
	var payload bytes.Buffer
	writer := gzip.NewWriter(&payload)
	_, err := writer.Write([]byte(`{"not a metric":1}`))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	_, err = decodeHistoryValues(payload.Bytes(), len(`{"not a metric":1}`))
	assert.ErrorContains(t, err, "malformed")
}

func TestHistoryPrivatePathsAndForeignDatabase(t *testing.T) {
	now := time.Now()
	h, path := newTestHistory(t, &now)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
		info, err = os.Stat(filepath.Dir(path))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0700), info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "metrics.db", entries[0].Name())
	require.NoError(t, h.Close())
	foreign := filepath.Join(t.TempDir(), "graph.db")
	db, err := sql.Open("sqlite", foreign)
	require.NoError(t, err)
	_, err = db.Exec("CREATE TABLE graph_nodes(id TEXT)")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.NoError(t, os.Chmod(foreign, 0640))
	opened, err := OpenHistory(foreign)
	assert.ErrorContains(t, err, "another application")
	assert.Nil(t, opened)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(foreign)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0640), info.Mode().Perm(), "a rejected foreign database must remain untouched")
	}
	_, err = OpenHistory(filepath.Dir(path))
	assert.Error(t, err)
	corrupt := filepath.Join(t.TempDir(), "corrupt.db")
	require.NoError(t, os.WriteFile(corrupt, []byte("not SQLite"), 0600))
	_, err = OpenHistory(corrupt)
	assert.Error(t, err)
}

func TestHistoryRefusesLeafSymlinkAndAcceptsParentAlias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privilege is not available in every Windows test environment")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	require.NoError(t, os.WriteFile(target, []byte("unchanged"), 0600))
	leaf := filepath.Join(root, "leaf.db")
	require.NoError(t, os.Symlink(target, leaf))
	_, err := OpenHistory(leaf)
	assert.Error(t, err)
	content, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "unchanged", string(content))
	alias := filepath.Join(root, "alias")
	require.NoError(t, os.Symlink(root, alias))
	h, err := OpenHistory(filepath.Join(alias, "private", "metrics.db"))
	require.NoError(t, err)
	require.NoError(t, h.Close())
	journalPath := filepath.Join(root, "journal.db")
	require.NoError(t, os.Symlink(target, journalPath+"-journal"))
	_, err = OpenHistory(journalPath)
	assert.Error(t, err)
	assert.NoFileExists(t, journalPath)
}

func historyTestPayload(t testing.TB, values map[string]float64) ([]byte, int) {
	t.Helper()
	raw, err := json.Marshal(values)
	require.NoError(t, err)
	var payload bytes.Buffer
	writer, err := gzip.NewWriterLevel(&payload, gzip.BestSpeed)
	require.NoError(t, err)
	_, err = writer.Write(raw)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return payload.Bytes(), len(raw)
}

func BenchmarkHistory105Repositories(b *testing.B) {
	for _, operation := range []string{"append", "read720", "charts360"} {
		b.Run(operation, func(b *testing.B) {
			now := time.Now().UTC()
			h, err := openHistory(filepath.Join(b.TempDir(), "history.db"), func() time.Time { return now })
			require.NoError(b, err)
			b.Cleanup(func() { assert.NoError(b, h.Close()) })
			values := make(map[string]float64)
			for i := 0; i < 105; i++ {
				for _, metric := range []string{"nodes", "edges", "generation", "index_duration_seconds"} {
					values[fmt.Sprintf("gortex_repository_%s{repo=\"fixture-%03d\"}", metric, i)] = float64(i + 1)
				}
			}
			for i := 0; i < 80; i++ {
				values[fmt.Sprintf("gortex_runtime_measurement_%d", i)] = float64(i)
			}
			values["gortex_runtime_goroutines"] = 42
			values["gortex_runtime_heap_alloc_bytes"] = 1 << 20
			values["gortex_process_cpu_seconds_total"] = 10
			values[`gortex_mcp_requests_total{outcome="success"}`] = 100
			for i, upper := range []string{"0.005", "0.01", "0.025", "0.05", "0.1", "0.25", "0.5", "1", "2.5", "5", "10", "30", "60", "120", "+Inf"} {
				values[fmt.Sprintf("gortex_mcp_request_duration_seconds_bucket{le=\"%s\"}", upper)] = float64(i + 1)
			}
			payload, rawBytes := historyTestPayload(b, values)
			chartValues := dashboardChartSample(HistorySample{Values: values}).Values
			require.NotEmpty(b, chartValues)
			chartPayload, chartRawBytes := historyTestPayload(b, chartValues)
			tx, err := h.db.Begin()
			require.NoError(b, err)
			statement, err := tx.Prepare("INSERT INTO metric_history(at,payload,raw_bytes,chart_payload,chart_raw_bytes) VALUES(?,?,?,?,?)")
			require.NoError(b, err)
			for i := 0; i < historyMaxRows; i++ {
				_, err = statement.Exec(now.Add(time.Duration(i-historyMaxRows)*10*time.Second).UnixMilli(), payload, rawBytes, chartPayload, chartRawBytes)
				require.NoError(b, err)
			}
			require.NoError(b, statement.Close())
			require.NoError(b, tx.Commit())
			require.NoError(b, h.Prune(context.Background(), now))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				switch operation {
				case "append":
					now = now.Add(time.Millisecond)
					require.NoError(b, h.Append(context.Background(), HistorySample{At: now.UnixMilli(), Values: values}))
				case "read720":
					rows, err := h.Read(context.Background(), now.Add(-HistoryRetention), 720)
					require.NoError(b, err)
					require.Len(b, rows, 720)
				default:
					rows, err := h.ReadCharts(context.Background(), now.Add(-HistoryRetention), 360)
					require.NoError(b, err)
					require.Len(b, rows, 360)
				}
			}
		})
	}
}

func TestHistoryChartsProjectionAndCorruptionIsolation(t *testing.T) {
	now := time.Now()
	h, _ := newTestHistory(t, &now)
	sample := historyTestSample(now)
	sample.Values[`gortex_repository_nodes{repo="fixture"}`] = 120
	require.NoError(t, h.Append(context.Background(), sample))
	rows, err := h.ReadCharts(context.Background(), time.Time{}, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, dashboardChartSample(sample), rows[0])
	assert.NotContains(t, rows[0].Values, `gortex_repository_nodes{repo="fixture"}`)
	full, err := h.Read(context.Background(), time.Time{}, 10)
	require.NoError(t, err)
	require.Len(t, full, 1)
	assert.Equal(t, sample, full[0])
	// The chart path reads the saved small projection, not the full blob.
	_, err = h.db.Exec("UPDATE metric_history SET payload=?", []byte("broken full payload"))
	require.NoError(t, err)
	rows, err = h.ReadCharts(context.Background(), time.Time{}, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, dashboardChartSample(sample), rows[0])
	_, err = h.Read(context.Background(), time.Time{}, 10)
	assert.Error(t, err)
	_, err = h.db.Exec("UPDATE metric_history SET chart_payload=?", []byte("broken chart payload"))
	require.NoError(t, err)
	_, err = h.ReadCharts(context.Background(), time.Time{}, 10)
	assert.Error(t, err)
}

func TestHistoryOutOfOrderAppendKeepsNewest(t *testing.T) {
	now := time.Now()
	h, _ := newTestHistory(t, &now)
	h.maxRows = 2
	ctx := context.Background()
	for _, offset := range []time.Duration{-2 * time.Second, -time.Second, 0, -3 * time.Second} {
		require.NoError(t, h.Append(ctx, historyTestSample(now.Add(offset))))
	}
	rows, err := h.Read(ctx, time.Time{}, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, now.Add(-time.Second).UnixMilli(), rows[0].At)
	assert.Equal(t, now.UnixMilli(), rows[1].At)
	require.NoError(t, h.Append(ctx, historyTestSample(now.Add(-500*time.Millisecond))))
	rows, err = h.Read(ctx, time.Time{}, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, now.Add(-500*time.Millisecond).UnixMilli(), rows[0].At)
	assert.Equal(t, now.UnixMilli(), rows[1].At)
}

func TestHistoryRejectsTruncatedGzipAndOldSchema(t *testing.T) {
	payload, size := historyTestPayload(t, map[string]float64{"gortex_test": 1})
	_, err := decodeHistoryValues(payload[:len(payload)-1], size)
	assert.Error(t, err)
	payload[len(payload)-1] ^= 1
	_, err = decodeHistoryValues(payload, size)
	assert.Error(t, err)
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec("CREATE TABLE metric_history(at INTEGER PRIMARY KEY,payload BLOB,raw_bytes INTEGER)")
	require.NoError(t, err)
	_, err = db.Exec("PRAGMA application_id=1196705864")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = OpenHistory(path)
	assert.ErrorContains(t, err, "unsupported schema")
}

func TestHistoryContextsAndConcurrentClose(t *testing.T) {
	now := time.Now()
	h, _ := newTestHistory(t, &now)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, h.Append(cancelled, historyTestSample(now)), context.Canceled)
	_, err := h.Read(cancelled, time.Time{}, 10)
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorIs(t, h.Prune(cancelled, now), context.Canceled)
	conn, err := h.db.Conn(context.Background())
	require.NoError(t, err)
	bounded, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	started := time.Now()
	assert.ErrorIs(t, h.Append(bounded, historyTestSample(now)), context.DeadlineExceeded)
	assert.Less(t, time.Since(started), time.Second)
	require.NoError(t, conn.Close())
	var workers sync.WaitGroup
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 5; j++ {
				ctx, stop := context.WithTimeout(context.Background(), time.Second)
				_ = h.Append(ctx, historyTestSample(now))
				_, _ = h.Read(ctx, time.Time{}, 2)
				stop()
			}
		}()
	}
	require.NoError(t, h.Close())
	workers.Wait()
	require.NoError(t, h.Close())
	assert.ErrorContains(t, h.Append(context.Background(), historyTestSample(now)), "closed")
	_, err = h.Read(context.Background(), time.Time{}, 10)
	assert.ErrorContains(t, err, "closed")
	assert.ErrorContains(t, h.Prune(context.Background(), now), "closed")
}
