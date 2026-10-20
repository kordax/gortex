package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/observability"
	"go.uber.org/zap"
)

func TestDaemonHistoryCaptureAggregatesRequestLabelsAndKeepsRepositoryMetadata(t *testing.T) {
	source := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# TYPE gortex_mcp_requests_total counter\n" +
			"gortex_mcp_requests_total{kind=\"tool\",operation=\"read_file\",outcome=\"success\"} 3\n" +
			"gortex_mcp_requests_total{operation=\"search_symbols\",outcome=\"success\",kind=\"tool\"} 4\n" +
			"gortex_mcp_requests_total{kind=\"tool\",operation=\"search_symbols\",outcome=\"error\"} 1\n" +
			"gortex_mcp_request_duration_seconds_bucket{operation=\"read_file\",le=\"0.1\",kind=\"tool\"} 2\n" +
			"gortex_repo_files{repo=\"comma, quote\\\" test\"} 12\n" +
			"gortex_runtime_goroutines 42\n"))
	})
	at := time.Now()
	sample, err := captureDaemonMetrics(t.Context(), source, at)
	require.NoError(t, err)
	assert.Equal(t, at.UnixMilli(), sample.At)
	assert.Equal(t, float64(7), sample.Values[`gortex_mcp_requests_total{outcome="success"}`])
	assert.Equal(t, float64(1), sample.Values[`gortex_mcp_requests_total{outcome="error"}`])
	assert.Equal(t, float64(2), sample.Values[`gortex_mcp_request_duration_seconds_bucket{le="0.1"}`])
	assert.Equal(t, float64(12), sample.Values[`gortex_repo_files{repo="comma, quote\" test"}`])
	assert.Equal(t, float64(42), sample.Values["gortex_runtime_goroutines"])
	for key := range sample.Values {
		assert.NotContains(t, key, "operation=")
	}
	store, err := observability.OpenHistory(filepath.Join(t.TempDir(), "history.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.NoError(t, store.Append(t.Context(), sample), "collector keys must satisfy persistent history contract")
}

func TestDaemonHistoryCaptureRejectsUnavailableOrInvalidSources(t *testing.T) {
	for name, body := range map[string]string{
		"empty": "# just metadata\n", "nonfinite": "gortex_bad NaN\n", "malformed": "gortex_bad\n",
		"duplicate": "gortex_bad 1\ngortex_bad 2\n", "labels": "gortex_bad{repo=\"unterminated} 1\n",
		"duplicate_label": "gortex_bad{repo=\"x\",repo=\"y\"} 1\n", "foreign_name": "user_query 1\n",
		"oversized": strings.Repeat("#", maxDaemonMetricSnapshot+1),
	} {
		t.Run(name, func(t *testing.T) {
			source := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
			_, err := captureDaemonMetrics(t.Context(), source, time.Now())
			require.Error(t, err)
		})
	}
	_, err := captureDaemonMetrics(t.Context(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }), time.Now())
	require.Error(t, err)
}

func TestDaemonHistoryCollectsWithoutScrapesAndStops(t *testing.T) {
	store, err := observability.OpenHistory(filepath.Join(t.TempDir(), "history.sqlite"))
	require.NoError(t, err)
	h := &daemonMetricHistory{store: store, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); h.close() })
	source := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("gortex_runtime_goroutines 42\n")) })
	go h.collect(ctx, source, zap.NewNop(), 15*time.Millisecond)
	require.Eventually(t, func() bool {
		rows, err := store.Read(t.Context(), time.Now().Add(-time.Minute), 10)
		return err == nil && len(rows) >= 2 && h.current().Values["gortex_runtime_goroutines"] == 42
	}, 2*time.Second, 10*time.Millisecond, "history must advance without an HTTP listener or browser")
	cancel()
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		t.Fatal("history collector did not stop after cancellation")
	}
}

func TestDaemonDashboardAddressRequiresOptInAndHonorsPrecedence(t *testing.T) {
	t.Setenv("GORTEX_METRICS_ADDR", "")
	require.NoError(t, os.Unsetenv("GORTEX_METRICS_ADDR"))
	cmd := &cobra.Command{}
	assert.Empty(t, daemonDashboardAddress(cmd, "unused", ""))
	assert.Empty(t, daemonDashboardAddress(cmd, "unused", "off"))
	assert.Equal(t, "127.0.0.1:9474", daemonDashboardAddress(cmd, "unused", "127.0.0.1:9474"))
	for _, value := range []string{"", "off", "127.0.0.1:0"} {
		t.Setenv("GORTEX_METRICS_ADDR", value)
		expected := value
		if value == "off" {
			expected = ""
		}
		assert.Equal(t, expected, daemonDashboardAddress(cmd, "unused", "127.0.0.1:9474"))
	}
	cmd.Flags().String("metrics-addr", "", "")
	for _, value := range []string{"", "off", "127.0.0.1:9476"} {
		require.NoError(t, cmd.Flags().Set("metrics-addr", value))
		expected := value
		if value == "off" {
			expected = ""
		}
		assert.Equal(t, expected, daemonDashboardAddress(cmd, value, "127.0.0.1:9474"))
	}
}

func BenchmarkDaemonHistoryCapture(b *testing.B) {
	var body strings.Builder
	for i := 0; i < 128; i++ {
		body.WriteString("gortex_mcp_requests_total{kind=\"tool\",operation=\"op_")
		body.WriteString(strings.Repeat("a", i+1))
		body.WriteString("\",outcome=\"success\"} 1\n")
	}
	data := []byte(body.String())
	source := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) })
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := captureDaemonMetrics(context.Background(), source, time.Now()); err != nil {
			b.Fatal(err)
		}
	}
}
