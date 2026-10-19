package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type dashboardTestHistory func(context.Context, time.Time, int) ([]HistorySample, error)

func (read dashboardTestHistory) Read(ctx context.Context, since time.Time, limit int) ([]HistorySample, error) {
	return read(ctx, since, limit)
}

func dashboardRequest(handler http.Handler, method, path string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, path, nil))
	return response
}

func TestDashboardUnavailableAndFiniteCurrent(t *testing.T) {
	handler := DashboardHandler(nil, nil)
	response := dashboardRequest(handler, http.MethodGet, "/api/v1/history")
	require.Equal(t, http.StatusOK, response.Code)
	var history dashboardHistoryResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &history))
	assert.False(t, history.Available)
	assert.Equal(t, 86400, history.RetentionSeconds)
	assert.NotNil(t, history.Samples)
	assert.Empty(t, history.Samples)

	response = dashboardRequest(handler, http.MethodGet, "/api/v1/current")
	var current HistorySample
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &current))
	assert.Zero(t, current.At)
	assert.NotNil(t, current.Values)

	handler = DashboardHandler(nil, func() HistorySample {
		return HistorySample{At: time.Now().UnixMilli(), Values: map[string]float64{"finite": 12, "nan": math.NaN(), "inf": math.Inf(1)}}
	})
	response = dashboardRequest(handler, http.MethodGet, "/api/v1/current")
	require.Equal(t, http.StatusOK, response.Code)
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &current))
	assert.Equal(t, map[string]float64{"finite": 12}, current.Values)
}

func TestDashboardHistoryBoundsAndChartScope(t *testing.T) {
	calls := 0
	handler := DashboardHandler(dashboardTestHistory(func(ctx context.Context, since time.Time, limit int) ([]HistorySample, error) {
		calls++
		deadline, ok := ctx.Deadline()
		assert.True(t, ok)
		assert.WithinDuration(t, time.Now().Add(2*time.Second), deadline, time.Second)
		assert.WithinDuration(t, time.Now().Add(-24*time.Hour), since, time.Second)
		assert.Equal(t, 1440, limit)
		return []HistorySample{{At: time.Now().UnixMilli(), Values: map[string]float64{
			"gortex_process_cpu_seconds_total":                       20,
			`gortex_mcp_requests_total{outcome="success"}`:           3,
			`gortex_repo_nodes{repo="<img src=x onerror=alert(1)>"}`: 99,
		}}}, nil
	}), nil)
	response := dashboardRequest(handler, http.MethodGet, "/api/v1/history?seconds=86400&limit=1440&scope=charts")
	require.Equal(t, http.StatusOK, response.Code)
	var history dashboardHistoryResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &history))
	assert.True(t, history.Available)
	require.Len(t, history.Samples, 1)
	assert.Len(t, history.Samples[0].Values, 2)
	assert.NotContains(t, response.Body.String(), "gortex_repo")
	assert.Equal(t, 1, calls)

	for _, query := range []string{"seconds=0", "seconds=86401", "seconds=-1", "limit=1", "limit=1441", "limit=abc", "seconds=", "seconds=1&seconds=2", "other=5", "scope=raw", "scope=charts&scope=charts", "seconds=1;limit=2", strings.Repeat("x", 129)} {
		t.Run(query, func(t *testing.T) {
			response := dashboardRequest(handler, http.MethodGet, "/api/v1/history?"+query)
			assert.Equal(t, http.StatusBadRequest, response.Code)
		})
	}
	assert.Equal(t, 1, calls, "invalid requests must not touch storage")
}

func TestDashboardHistoryDefaultAndErrors(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		handler := DashboardHandler(dashboardTestHistory(func(_ context.Context, since time.Time, limit int) ([]HistorySample, error) {
			assert.WithinDuration(t, time.Now().Add(-time.Hour), since, time.Second)
			assert.Equal(t, 360, limit)
			return nil, nil
		}), nil)
		response := dashboardRequest(handler, http.MethodGet, "/api/v1/history")
		assert.Equal(t, http.StatusOK, response.Code)
		assert.Contains(t, response.Body.String(), `"samples":[]`)
		assert.Contains(t, response.Body.String(), `"available":true`)
	})
	t.Run("private error", func(t *testing.T) {
		handler := DashboardHandler(dashboardTestHistory(func(context.Context, time.Time, int) ([]HistorySample, error) {
			return nil, errors.New("secret database path and query")
		}), nil)
		response := dashboardRequest(handler, http.MethodGet, "/api/v1/history")
		assert.Equal(t, http.StatusServiceUnavailable, response.Code)
		assert.NotContains(t, response.Body.String(), "secret")
		assert.Contains(t, response.Body.String(), `"available":false`)
	})
	t.Run("backend row bound", func(t *testing.T) {
		handler := DashboardHandler(dashboardTestHistory(func(context.Context, time.Time, int) ([]HistorySample, error) {
			return make([]HistorySample, 3), nil
		}), nil)
		assert.Equal(t, http.StatusServiceUnavailable, dashboardRequest(handler, http.MethodGet, "/api/v1/history?limit=2").Code)
	})
}

func TestDashboardChartMemoryAndCounterFamilies(t *testing.T) {
	values := map[string]float64{}
	for _, name := range []string{
		"gortex_daemon_uptime_seconds", "gortex_process_cpu_seconds_total",
		"gortex_process_resident_memory_bytes", "gortex_process_pss_memory_bytes",
		"gortex_children_resident_memory_bytes", "gortex_runtime_heap_alloc_bytes",
		"gortex_runtime_heap_inuse_bytes", "gortex_runtime_heap_idle_bytes",
		"gortex_runtime_heap_released_bytes", "gortex_runtime_goroutines",
		`gortex_cache_bytes{cache="trigram"}`, `gortex_mcp_requests_total{outcome="success"}`,
		`gortex_mcp_request_duration_seconds_bucket{le="0.1"}`, "gortex_mcp_request_duration_seconds_count",
	} {
		values[name] = 42
	}
	original := HistorySample{At: 123, Values: values}
	filtered := dashboardChartSample(original)
	assert.Equal(t, original, filtered, "resource and counter families needed by the UI survive chart filtering")
	values[`gortex_repo_files{repo="sample"}`] = 99
	assert.NotContains(t, dashboardChartSample(original).Values, `gortex_repo_files{repo="sample"}`)
}

func TestDashboardResponseSizeBound(t *testing.T) {
	values := make(map[string]float64)
	for i := range 4096 {
		values[fmt.Sprintf("metric_%04d_%s", i, strings.Repeat("a", 490))] = 1
	}
	handler := DashboardHandler(dashboardTestHistory(func(context.Context, time.Time, int) ([]HistorySample, error) {
		return []HistorySample{{At: 1, Values: values}, {At: 2, Values: values}}, nil
	}), nil)
	response := dashboardRequest(handler, http.MethodGet, "/api/v1/history")
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.Less(t, response.Body.Len(), 256)
	assert.True(t, json.Valid(response.Body.Bytes()))
}

func TestDashboardAssetsAndSecurity(t *testing.T) {
	handler := DashboardHandler(nil, nil)
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		t.Run(path, func(t *testing.T) {
			response := dashboardRequest(handler, http.MethodGet, path)
			assert.Equal(t, http.StatusOK, response.Code)
			assert.NotEmpty(t, response.Body.String())
			assert.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			assert.Contains(t, response.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'")
			assert.NotContains(t, response.Body.String(), "https://")
		})
	}
	js := dashboardRequest(handler, http.MethodGet, "/app.js").Body.String()
	assert.NotContains(t, js, "innerHTML")
	assert.NotContains(t, js, "insertAdjacentHTML")
	assert.Contains(t, js, "textContent")
	assert.Contains(t, js, "scope=charts")
	assert.Contains(t, js, "collection continues")
	assert.Equal(t, http.StatusNotFound, dashboardRequest(handler, http.MethodGet, "/dashboard/index.html").Code)
	assert.Equal(t, http.StatusNotFound, dashboardRequest(handler, http.MethodGet, "/api/v1/other").Code)
	assert.Equal(t, http.StatusBadRequest, dashboardRequest(handler, http.MethodGet, "/api/v1/current?limit=1").Code)
	response := dashboardRequest(handler, http.MethodPost, "/api/v1/history")
	assert.Equal(t, http.StatusMethodNotAllowed, response.Code)
	assert.Equal(t, http.MethodGet, response.Header().Get("Allow"))
}

func TestDashboardAdmissionRemainsBoundedAfterCancellation(t *testing.T) {
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	handler := DashboardHandler(dashboardTestHistory(func(context.Context, time.Time, int) ([]HistorySample, error) {
		entered <- struct{}{}
		<-release // deliberately ignores context, exercising bounded admission
		return nil, nil
	}), nil)
	done := make(chan struct{}, 4)
	for range 4 {
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			request := httptest.NewRequest(http.MethodGet, "/api/v1/history", nil).WithContext(ctx)
			cancel()
			handler.ServeHTTP(httptest.NewRecorder(), request)
			done <- struct{}{}
		}()
	}
	defer close(release)
	for range 4 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("reader did not start")
		}
	}
	for range 4 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("cancelled request remained blocked")
		}
	}
	response := dashboardRequest(handler, http.MethodGet, "/api/v1/history")
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.Contains(t, response.Body.String(), "busy")
}
