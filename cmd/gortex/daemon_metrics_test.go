package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
)

func TestDaemonMetricsDoesNotWaitForControllerOrScanStore(t *testing.T) {
	controller := &realController{}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.ready.Store(true)
	controller.lastAggregate.Store(&statusAggregate{takenAt: time.Unix(12345, 0), tracked: []daemon.TrackedRepoStatus{{Prefix: "fixture", Nodes: 7, Files: 2}, {Prefix: "/private/hidden", Nodes: 99}}})
	srv := daemon.New("unused", "test", zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := daemonMetricsHandler(ctx, srv, controller)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		done <- w
	}()
	select {
	case response := <-done:
		require.Equal(t, http.StatusOK, response.Code)
		text := response.Body.String()
		assert.Contains(t, text, "gortex_daemon_ready 1\n")
		assert.Contains(t, text, "gortex_daemon_aggregate_snapshot_timestamp_seconds 12345\n")
		assert.Contains(t, text, "gortex_repo_nodes{repo=\"fixture\"} 7\n")
		assert.NotContains(t, text, "/private")
		assert.Contains(t, text, "gortex_daemon_sessions{state=\"retained\"} 0\n")
	case <-time.After(2 * time.Second):
		t.Fatal("metrics scrape blocked on the controller lock")
	}
}

func TestDaemonMetricsUnknownAggregateAndHTTPContract(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := daemonMetricsHandler(ctx, nil, &realController{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Contains(t, response.Body.String(), "gortex_daemon_aggregate_available 0\n")
	assert.NotContains(t, response.Body.String(), "gortex_repo_nodes{")
	assert.Equal(t, "text/plain; version=0.0.4; charset=utf-8", response.Header().Get("Content-Type"))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, response.Code)
	cancel()
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
}

func TestDaemonMetricSamplerStopsWithoutControllerLock(t *testing.T) {
	controller := &realController{}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		var dst atomic.Pointer[statusAggregate]
		sampleDaemonMetricMetadata(ctx, controller, &dst)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled sampler waited for controller lock")
	}
}
