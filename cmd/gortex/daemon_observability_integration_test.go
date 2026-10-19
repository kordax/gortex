package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/observability"
	collecttrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

// TestDaemonObservabilityIntegration reuses the private Git/XDG/store/process
// fixture. An explicit binary is required; the user's daemon is never addressed.
func TestDaemonObservabilityIntegration(t *testing.T) {
	binary := os.Getenv("GORTEX_OBSERVABILITY_TEST_BINARY")
	if binary == "" {
		t.Skip("set GORTEX_OBSERVABILITY_TEST_BINARY to an explicitly built test binary")
	}
	binary, err := filepath.Abs(binary)
	require.NoError(t, err)
	var spans atomic.Int64
	var privatePayload atomic.Bool
	f := newIssue767Fixture(t, binary)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			w.WriteHeader(400)
			return
		}
		request := new(collecttrace.ExportTraceServiceRequest)
		if err := proto.Unmarshal(data, request); err != nil {
			w.WriteHeader(400)
			return
		}
		if bytes.Contains(data, []byte(f.primary)) || bytes.Contains(data, []byte("Issue767PrimaryMarker")) {
			privatePayload.Store(true)
		}
		for _, resource := range request.ResourceSpans {
			for _, scope := range resource.ScopeSpans {
				spans.Add(int64(len(scope.Spans)))
			}
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(200)
	}))
	defer collector.Close()
	var env []string
	for _, value := range f.env {
		key, _, _ := strings.Cut(value, "=")
		if key != "HOME" && !strings.HasPrefix(key, "OTEL_") {
			env = append(env, value)
		}
	}
	f.env = append(env, "HOME="+filepath.Join(f.root, "home"), "XDG_STATE_HOME="+filepath.Join(f.root, "state"), "XDG_RUNTIME_DIR="+filepath.Join(f.root, "runtime"),
		"GORTEX_METRICS_ADDR=127.0.0.1:0", "GORTEX_DIAGNOSTICS_ADDR=127.0.0.1:0", "OTEL_TRACES_EXPORTER=otlp", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT="+collector.URL+"/v1/traces", "OTEL_TRACES_SAMPLER=always_on")
	for _, directory := range []string{"home", "state", "runtime"} {
		require.NoError(t, os.MkdirAll(filepath.Join(f.root, directory), 0o700))
	}
	f.start()
	addresses := map[string]string{}
	logs, err := os.ReadFile(f.log.Name())
	require.NoError(t, err)
	for _, line := range bytes.Split(logs, []byte("\n")) {
		var entry struct {
			Message string `json:"msg"`
			Kind    string `json:"kind"`
			Address string `json:"address"`
		}
		if json.Unmarshal(line, &entry) == nil && entry.Message == "daemon: local observability listener" {
			addresses[entry.Kind] = entry.Address
		}
	}
	require.Contains(t, addresses, "metrics")
	require.Contains(t, addresses, "profiles")
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	read := func(kind, path string) (int, string) {
		response, err := client.Get("http://" + addresses[kind] + path)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		require.NoError(t, err)
		return response.StatusCode, string(body)
	}
	f.awaitSymbol(f.primary, "Issue767PrimaryMarker")
	f.await("ready metrics and private OTLP spans", 30*time.Second, func() bool {
		_, body := read("metrics", "/metrics")
		return strings.Contains(body, "gortex_daemon_ready 1") && strings.Contains(body, "gortex_process_sample_available 1") && spans.Load() > 0
	})
	f.await("autonomous advisory metadata", 15*time.Second, func() bool {
		_, body := read("metrics", "/metrics")
		return strings.Contains(body, "gortex_daemon_aggregate_source{source=\"indexer_metadata\"} 1")
	})
	_, metrics := read("metrics", "/metrics")
	assert.NotContains(t, metrics, "gortex_repo_indexed{", "advisory metadata must not assert a queryable repository")
	assert.NotContains(t, metrics, "gortex_daemon_repos{state=\"indexed\"}", "unknown loaded state must not become zero")
	assert.NotContains(t, metrics, f.primary)
	assert.NotContains(t, metrics, "Issue767PrimaryMarker")
	assert.Contains(t, metrics, "gortex_mcp_requests_total")
	code, page := read("metrics", "/")
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, page, "Gortex")
	code, currentJSON := read("metrics", "/api/v1/current")
	assert.Equal(t, http.StatusOK, code)
	var current observability.HistorySample
	require.NoError(t, json.Unmarshal([]byte(currentJSON), &current))
	assert.Positive(t, current.At)
	assert.Contains(t, current.Values, "gortex_runtime_goroutines")
	code, historyJSON := read("metrics", "/api/v1/history?seconds=86400&limit=10&scope=charts")
	assert.Equal(t, http.StatusOK, code)
	var history struct {
		Samples   []observability.HistorySample `json:"samples"`
		Available bool                          `json:"available"`
		Retention int                           `json:"retention_seconds"`
	}
	require.NoError(t, json.Unmarshal([]byte(historyJSON), &history))
	require.True(t, history.Available)
	require.NotEmpty(t, history.Samples)
	assert.Equal(t, 86400, history.Retention)
	oldPoint := history.Samples[0].At
	code, _ = read("metrics", "/debug/pprof/heap")
	assert.Equal(t, 404, code)
	code, _ = read("profiles", "/debug/pprof/cmdline")
	assert.Equal(t, 404, code, "diagnostics must not expose daemon argv")
	monitor := f.command(10*time.Second, f.primary, "daemon", "monitor", "--once", "--json")
	assert.True(t, json.Valid(bytes.TrimSpace(monitor)), "monitor must emit one valid JSON snapshot")
	profile := filepath.Join(f.root, "heap.pprof")
	f.command(10*time.Second, f.primary, "daemon", "profile", "--kind", "heap", "--address", addresses["profiles"], "--out", profile)
	data, err := os.ReadFile(profile)
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(data, []byte{0x1f, 0x8b}))
	if os.Getenv("GORTEX_OBSERVABILITY_STACK_SMOKE") == "1" {
		cmd := exec.CommandContext(t.Context(), "python3", "deploy/observability/smoke.py")
		cmd.Dir = filepath.Join("..", "..")
		cmd.Env = append(os.Environ(), "GORTEX_OBSERVABILITY_SMOKE_METRICS_URL=http://"+addresses["metrics"]+"/metrics")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "isolated collector stack: %s", output)
		t.Logf("collector stack: %s", output)
	}
	f.stop()
	f.start()
	logs, err = os.ReadFile(f.log.Name())
	require.NoError(t, err)
	for _, line := range bytes.Split(logs, []byte("\n")) {
		var entry struct {
			Message string `json:"msg"`
			Kind    string `json:"kind"`
			Address string `json:"address"`
		}
		if json.Unmarshal(line, &entry) == nil && entry.Message == "daemon: local observability listener" {
			addresses[entry.Kind] = entry.Address
		}
	}
	code, historyJSON = read("metrics", "/api/v1/history?seconds=86400&limit=10&scope=charts")
	require.Equal(t, http.StatusOK, code)
	require.NoError(t, json.Unmarshal([]byte(historyJSON), &history))
	assert.True(t, history.Available)
	found := false
	for _, sample := range history.Samples {
		if sample.At == oldPoint {
			found = true
		}
	}
	assert.True(t, found, "daily history must survive a daemon restart")
	f.stop()
	assert.False(t, privatePayload.Load(), "trace payload must not contain fixture paths or source symbols")
	t.Logf("isolated daemon metrics, monitor, profile and %d OTLP spans verified", spans.Load())
}
