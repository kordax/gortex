package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
	"go.uber.org/zap"
)

func testDashboard(t *testing.T) *daemonDashboard {
	t.Helper()
	d := &daemonDashboard{ctx: t.Context(), handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }), logger: zap.NewNop(), preferred: "127.0.0.1:0"}
	t.Cleanup(d.close)
	return d
}

func TestDaemonDashboardRuntimeStartStopAndPortRelease(t *testing.T) {
	d := testDashboard(t)
	st, err := d.apply(t.Context(), daemon.DashboardParams{Action: "status"})
	require.NoError(t, err)
	assert.False(t, st.Running)
	for range 10 {
		st, err = d.apply(t.Context(), daemon.DashboardParams{Action: "start"})
		require.NoError(t, err)
		require.True(t, st.Running)
		again, err := d.apply(t.Context(), daemon.DashboardParams{Action: "start"})
		require.NoError(t, err)
		assert.Equal(t, st, again)
		client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
		response, err := client.Get(st.URL)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, response.StatusCode)
		require.NoError(t, response.Body.Close())
		client.CloseIdleConnections()
		_, err = d.apply(t.Context(), daemon.DashboardParams{Action: "stop"})
		require.NoError(t, err)
		_, err = d.apply(t.Context(), daemon.DashboardParams{Action: "stop"})
		require.NoError(t, err)
		listener, err := net.Listen("tcp", st.Address)
		require.NoError(t, err, "stop must release the port before acknowledging")
		require.NoError(t, listener.Close())
	}
}

func TestDaemonDashboardImmediateStopReleasesPort(t *testing.T) {
	d := testDashboard(t)
	for range 30 {
		st, err := d.apply(t.Context(), daemon.DashboardParams{Action: "start"})
		require.NoError(t, err)
		_, err = d.apply(t.Context(), daemon.DashboardParams{Action: "stop"})
		require.NoError(t, err)
		listener, err := net.Listen("tcp", st.Address)
		require.NoError(t, err, "immediate stop must release even a not-yet-served listener")
		require.NoError(t, listener.Close())
	}
}

func TestDaemonDashboardRuntimeFailuresPreserveState(t *testing.T) {
	d := testDashboard(t)
	_, err := d.apply(t.Context(), daemon.DashboardParams{Action: "start", Address: "0.0.0.0:0"})
	require.Error(t, err)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer busy.Close()
	_, err = d.apply(t.Context(), daemon.DashboardParams{Action: "start", Address: busy.Addr().String()})
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = d.apply(ctx, daemon.DashboardParams{Action: "start"})
	require.ErrorIs(t, err, context.Canceled)
	before, err := d.apply(t.Context(), daemon.DashboardParams{Action: "start"})
	require.NoError(t, err)
	for _, p := range []daemon.DashboardParams{{Action: "invalid"}, {Action: "start", Address: "127.0.0.1:9479"}, {Action: "stop", Address: "127.0.0.1:0"}} {
		_, err := d.apply(t.Context(), p)
		require.Error(t, err)
		after, err := d.apply(t.Context(), daemon.DashboardParams{Action: "status"})
		require.NoError(t, err)
		assert.Equal(t, before, after)
	}
	d.close()
	_, err = d.apply(t.Context(), daemon.DashboardParams{Action: "start"})
	require.ErrorContains(t, err, "shutting down")
}

func TestDaemonDashboardImmediateConcurrentToggleAndShutdown(t *testing.T) {
	d := testDashboard(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				_, err := d.apply(t.Context(), daemon.DashboardParams{Action: "start"})
				assert.NoError(t, err)
				_, err = d.apply(t.Context(), daemon.DashboardParams{Action: "status"})
				assert.NoError(t, err)
				_, err = d.apply(t.Context(), daemon.DashboardParams{Action: "stop"})
				assert.NoError(t, err)
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dashboard control deadlocked")
	}
	d.close()
	st, err := d.apply(t.Context(), daemon.DashboardParams{Action: "status"})
	require.NoError(t, err)
	assert.False(t, st.Running)
}

func TestDaemonDashboardCLICommands(t *testing.T) {
	d := testDashboard(t)
	var started daemon.DashboardStatus
	for i, action := range []string{"start", "status", "stop", "status"} {
		cmd := newDaemonDashboardCommand(d.apply)
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetArgs([]string{action, "--json"})
		require.NoError(t, cmd.Execute())
		var st daemon.DashboardStatus
		require.NoError(t, json.Unmarshal(output.Bytes(), &st))
		assert.Equal(t, i < 2, st.Running)
		if action == "start" {
			started = st
			assert.True(t, st.Running)
		} else if action == "stop" {
			assert.False(t, st.Running)
		} else if st.Running {
			assert.Equal(t, started, st)
		}
	}
	cmd := newDaemonDashboardCommand(func(context.Context, daemon.DashboardParams) (daemon.DashboardStatus, error) {
		t.Fatal("invalid CLI arguments must not reach the daemon")
		return daemon.DashboardStatus{}, nil
	})
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"stop", "--address", "127.0.0.1:0"})
	require.Error(t, cmd.Execute())
}

func TestDaemonDashboardStopLeavesMetricHistoryAlive(t *testing.T) {
	t.Setenv("GORTEX_METRICS_ADDR", "off")
	t.Setenv("GORTEX_DIAGNOSTICS_ADDR", "")
	t.Setenv("GORTEX_DAEMON_PPROF_ADDR", "")
	controller := &realController{}
	stop, err := startDaemonObservability(newDaemonDashboardCommand(nil), nil, controller, zap.NewNop())
	require.NoError(t, err)
	defer stop()
	for range 2 {
		_, err := controller.Dashboard(t.Context(), daemon.DashboardParams{Action: "start", Address: "127.0.0.1:0"})
		require.NoError(t, err)
		_, err = controller.Dashboard(t.Context(), daemon.DashboardParams{Action: "stop"})
		require.NoError(t, err)
		// The retained handler still serves the same live collector/history
		// after HTTP is stopped, and can be reopened on the next iteration.
		assert.NoError(t, controller.dashboard.ctx.Err(), "stopping HTTP must not cancel collection")
		require.Eventually(t, func() bool {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/current", nil)
			request.RemoteAddr = "127.0.0.1:12345"
			controller.dashboard.handler.ServeHTTP(response, request)
			return response.Code == http.StatusOK
		}, time.Second, time.Millisecond)
	}
}
