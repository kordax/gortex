package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
	"go.uber.org/zap"
)

func TestDaemonDashboardCallerCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("real Unix socket fixture; Windows transport needs platform-specific verification")
	}
	for _, phase := range []string{"handshake", "control-response"} {
		t.Run(phase, func(t *testing.T) {
			// Keep the Unix socket below platform sun_path limits even when the
			// test name and the caller's TMPDIR are long.
			dir, err := os.MkdirTemp("", "gx-dash-")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
			socket := filepath.Join(dir, "s")
			t.Setenv("GORTEX_DAEMON_SOCKET", socket)
			listener, err := net.Listen("unix", socket)
			require.NoError(t, err)
			defer listener.Close()
			require.NoError(t, listener.(*net.UnixListener).SetDeadline(time.Now().Add(3*time.Second)))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				_, err := callDaemonDashboard(ctx, daemon.DashboardParams{Action: "status"})
				result <- err
			}()
			conn, err := listener.Accept()
			require.NoError(t, err)
			defer func() {
				_ = conn.Close()
				select {
				case <-finished:
				case <-time.After(time.Second):
					t.Error("dashboard caller leaked after fixture teardown")
				}
			}()
			require.NoError(t, conn.SetDeadline(time.Now().Add(3*time.Second)))
			decoder := json.NewDecoder(conn)
			var handshake daemon.Handshake
			require.NoError(t, decoder.Decode(&handshake))
			assert.Equal(t, daemon.ModeControl, handshake.Mode)
			if phase == "control-response" {
				require.NoError(t, daemon.WriteJSONLine(conn, daemon.HandshakeAck{OK: true}))
				var request daemon.ControlRequest
				require.NoError(t, decoder.Decode(&request))
				assert.Equal(t, daemon.ControlDashboard, request.Kind)
			}
			cancel()
			select {
			case err := <-result:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(time.Second):
				t.Fatal("cancellation waited for the daemon's handshake/control timeout")
			}
			var unexpected json.RawMessage
			require.ErrorIs(t, decoder.Decode(&unexpected), io.EOF, "cancellation must close its private control connection")
		})
	}
}

func TestDaemonDashboardStopDoesNotWaitForBlockedHandler(t *testing.T) {
	d := testDashboard(t)
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handlerExited := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	d.handler = http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		defer close(handlerExited)
		close(entered)
		<-r.Context().Done()
		close(cancelled)
		<-release
	})
	status, err := d.apply(t.Context(), daemon.DashboardParams{Action: "start"})
	require.NoError(t, err)
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, err := client.Get(status.URL)
		if err == nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	stopped := make(chan error, 1)
	go func() { _, err := d.apply(t.Context(), daemon.DashboardParams{Action: "stop"}); stopped <- err }()
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("stop waited for a blocked handler")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel the old HTTP request")
	}
	assert.NoError(t, d.ctx.Err(), "HTTP stop must retain collection context")
	// A new server may start while an old handler is still unwinding.
	restarted, err := d.apply(t.Context(), daemon.DashboardParams{Action: "start"})
	require.NoError(t, err)
	assert.True(t, restarted.Running)
	d.close()
	releaseOnce.Do(func() { close(release) })
	select {
	case <-handlerExited:
	case <-time.After(time.Second):
		t.Fatal("old HTTP handler did not exit")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("HTTP client did not exit after stop")
	}
}

func TestDaemonDashboardShutdownDuringConcurrentControls(t *testing.T) {
	d := testDashboard(t)
	started := make(chan struct{})
	var startedOnce sync.Once
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 30 {
				_, err := d.apply(t.Context(), daemon.DashboardParams{Action: "start"})
				if err != nil {
					assert.ErrorContains(t, err, "shutting down")
				} else {
					startedOnce.Do(func() { close(started) })
				}
				_, err = d.apply(t.Context(), daemon.DashboardParams{Action: "status"})
				assert.NoError(t, err)
				_, err = d.apply(t.Context(), daemon.DashboardParams{Action: "stop"})
				assert.NoError(t, err)
			}
		})
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("dashboard did not start")
	}
	d.close()
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown deadlocked concurrent controls")
	}
	status, err := d.apply(t.Context(), daemon.DashboardParams{Action: "status"})
	require.NoError(t, err)
	assert.False(t, status.Running)
}

func TestDaemonObservabilityImmediateStopReleasesProfilePort(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	t.Setenv("GORTEX_METRICS_ADDR", "off")
	t.Setenv("GORTEX_DIAGNOSTICS_ADDR", "127.0.0.1:0")
	t.Setenv("GORTEX_DAEMON_PPROF_ADDR", "")
	for range 10 {
		stop, err := startDaemonObservability(&cobra.Command{}, nil, nil, zap.NewNop())
		require.NoError(t, err)
		t.Cleanup(stop)
		address := daemonPProfAddr()
		stop()
		listener, err := net.Listen("tcp", address)
		require.NoError(t, err, "stop must release a profile listener even before Serve registers it")
		require.NoError(t, listener.Close())
	}
}
