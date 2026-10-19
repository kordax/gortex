package observability

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistryConcurrentOutcomesAndBoundedLabels(t *testing.T) {
	var registry Registry
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := registry.Begin("tool", "search")
			done("deadline")
			done("success")
		}()
	}
	wg.Wait()
	for i := 0; i < 500; i++ {
		registry.Begin("tool", fmt.Sprintf("operation_%d", i))("success")
	}
	registry.Begin("resource", "/private/repository/token-secret")("error")
	var out bytes.Buffer
	registry.Write(NewExposition(&out))
	text := out.String()
	assert.Contains(t, text, "gortex_mcp_requests_total{kind=\"tool\",operation=\"search\",outcome=\"deadline\"} 100")
	assert.Contains(t, text, "gortex_mcp_requests_in_flight{kind=\"tool\",operation=\"search\"} 0")
	assert.Contains(t, text, "gortex_mcp_request_duration_seconds_bucket{kind=\"tool\",operation=\"search\",le=\"+Inf\"} 100")
	assert.NotContains(t, text, "private")
	require.LessOrEqual(t, len(registry.calls), 129)
	assert.Equal(t, 1, strings.Count(text, "# TYPE gortex_mcp_request_duration_seconds histogram\n"))
}

func TestExpositionEscapesPrometheusLabels(t *testing.T) {
	var out bytes.Buffer
	NewExposition(&out).Gauge("gortex_test", "Example.", 1, "repo", "quoted\"\\\nname")
	assert.Contains(t, out.String(), "repo=\"quoted\\\"\\\\\\nname\"")
}

func fakeProc(t *testing.T, root string, pid, parent int, start, user, system, rss uint64) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(dir, 0700))
	fields := make([]string, 22)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[1] = strconv.Itoa(parent)
	fields[11] = strconv.FormatUint(user, 10)
	fields[12] = strconv.FormatUint(system, 10)
	fields[19] = strconv.FormatUint(start, 10)
	fields[21] = strconv.FormatUint(rss, 10)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"), []byte(fmt.Sprintf("%d (name with ) spaces) %s", pid, strings.Join(fields, " "))), 0600))
}

func TestProcessAttributionAndUnknownOptionalCounters(t *testing.T) {
	root := t.TempDir()
	fakeProc(t, root, 10, 1, 777, 120, 30, 4)
	fakeProc(t, root, 11, 10, 888, 20, 10, 2)
	fakeProc(t, root, 12, 11, 999, 5, 5, 1)
	fakeProc(t, root, 50, 1, 222, 999, 999, 100)
	require.NoError(t, os.WriteFile(filepath.Join(root, "10", "smaps_rollup"), []byte("Pss: 7 kB\nPss_Anon: 6 kB\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "10", "io"), []byte("rchar: 999\nread_bytes: 100\nwrite_bytes: 200\n"), 0600))
	p := collectProcess(root, 10, "linux")
	require.True(t, p.Available)
	assert.Equal(t, uint64(777), p.StartTicks)
	assert.Equal(t, 1.5, p.CPUSeconds)
	assert.Equal(t, uint64(4*os.Getpagesize()), p.RSSBytes)
	assert.True(t, p.PSSAvailable)
	assert.Equal(t, uint64(7168), p.PSSBytes)
	assert.True(t, p.IOAvailable)
	assert.Equal(t, uint64(100), p.ReadBytes)
	assert.Equal(t, uint64(200), p.WriteBytes)
	assert.Equal(t, 2, p.Children)
	assert.InDelta(t, 0.4, p.ChildrenCPUSeconds, 0.0001)
	assert.Equal(t, uint64(3*os.Getpagesize()), p.ChildrenRSSBytes)
	assert.True(t, p.ChildrenComplete)
	require.NoError(t, os.Remove(filepath.Join(root, "10", "io")))
	require.NoError(t, os.Remove(filepath.Join(root, "10", "smaps_rollup")))
	p = collectProcess(root, 10, "linux")
	assert.True(t, p.Available)
	assert.False(t, p.IOAvailable)
	assert.False(t, p.PSSAvailable)
	assert.False(t, collectProcess(root, 10, "windows").Available)
	assert.False(t, collectProcess(root, 999, "linux").Available)
}
