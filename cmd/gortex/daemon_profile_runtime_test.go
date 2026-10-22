package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
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

func TestDaemonProfilerLifecycleAndCaptureCancellation(t *testing.T) {
	previous := runtime.SetMutexProfileFraction(0)
	defer runtime.SetMutexProfileFraction(previous)
	d := &daemonProfiler{ctx: t.Context(), logger: zap.NewNop()}
	defer d.close()
	status, err := d.apply(t.Context(), daemon.ProfilerParams{Action: "status"})
	require.NoError(t, err)
	assert.False(t, status.Running)
	_, err = d.apply(t.Context(), daemon.ProfilerParams{Action: "start", Address: "0.0.0.0:0"})
	require.Error(t, err)
	status, err = d.apply(t.Context(), daemon.ProfilerParams{Action: "start", Address: "127.0.0.1:0"})
	require.NoError(t, err)
	address := status.Address
	require.True(t, status.Running)
	assert.Equal(t, address, daemonPProfAddr())
	again, err := d.apply(t.Context(), daemon.ProfilerParams{Action: "start"})
	require.NoError(t, err)
	assert.Equal(t, status, again)

	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://" + address + "/debug/pprof/heap")
	require.NoError(t, err)
	data, readErr := io.ReadAll(response.Body)
	require.NoError(t, response.Body.Close())
	require.NoError(t, readErr)
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.True(t, bytes.HasPrefix(data, []byte{0x1f, 0x8b}))

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		response, err := client.Get("http://" + address + "/debug/pprof/mutex?seconds=30")
		if err == nil {
			_ = response.Body.Close()
		}
	}()
	require.Eventually(t, func() bool { return runtime.SetMutexProfileFraction(-1) == 10 }, 3*time.Second, time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	status, err = d.apply(ctx, daemon.ProfilerParams{Action: "stop"})
	require.NoError(t, err)
	assert.False(t, status.Running)
	assert.False(t, status.Stopping)
	assert.Zero(t, runtime.SetMutexProfileFraction(-1), "stop must join capture cleanup before returning")
	assert.Empty(t, daemonPProfAddr())
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("capture client did not finish")
	}
	listener, err := net.Listen("tcp", address)
	require.NoError(t, err, "stop must release the port")
	require.NoError(t, listener.Close())
	status, err = d.apply(t.Context(), daemon.ProfilerParams{Action: "start", Address: address})
	require.NoError(t, err)
	assert.True(t, status.Running)
	d.close()
	_, err = d.apply(t.Context(), daemon.ProfilerParams{Action: "start"})
	require.Error(t, err)
}

func TestDaemonProfilerConcurrentControlsAndImmediateStop(t *testing.T) {
	d := &daemonProfiler{ctx: t.Context(), logger: zap.NewNop(), preferred: "127.0.0.1:0"}
	defer d.close()
	var workers sync.WaitGroup
	for range 6 {
		workers.Go(func() {
			for range 5 {
				for _, action := range []string{"start", "status", "stop"} {
					ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
					_, err := d.apply(ctx, daemon.ProfilerParams{Action: action})
					cancel()
					assert.NoError(t, err)
				}
			}
		})
	}
	workers.Wait()
	status, err := d.apply(t.Context(), daemon.ProfilerParams{Action: "stop"})
	require.NoError(t, err)
	assert.False(t, status.Running)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = d.apply(ctx, daemon.ProfilerParams{Action: "start"})
	assert.ErrorIs(t, err, context.Canceled)
	_, err = d.apply(t.Context(), daemon.ProfilerParams{Action: "stop", Address: "127.0.0.1:0"})
	assert.Error(t, err)
}

func TestDaemonProfilerWaitsForCancelledCaptureBeforeRestart(t *testing.T) {
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	d := &daemonProfiler{ctx: t.Context(), logger: zap.NewNop(), handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(cancelled)
		<-release
	})}
	var releaseOnce sync.Once
	releaseCapture := func() { releaseOnce.Do(func() { close(release) }) }
	defer func() { releaseCapture(); d.close() }()
	status, err := d.apply(t.Context(), daemon.ProfilerParams{Action: "start", Address: "127.0.0.1:0"})
	require.NoError(t, err)
	address := status.Address
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		client := &http.Client{Timeout: 3 * time.Second}
		response, err := client.Get("http://" + address + "/capture")
		if err == nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("capture did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	status, err = d.apply(ctx, daemon.ProfilerParams{Action: "stop"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.True(t, status.Stopping)
	assert.False(t, status.Running)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel the handler")
	}
	listener, err := net.Listen("tcp", address)
	require.NoError(t, err, "cleanup must not keep the port bound")
	require.NoError(t, listener.Close())
	_, err = d.apply(t.Context(), daemon.ProfilerParams{Action: "start"})
	require.ErrorContains(t, err, "still stopping")
	select {
	case <-clientDone:
	case <-time.After(time.Second):
		t.Fatal("stop did not close the HTTP connection")
	}
	releaseCapture()
	require.Eventually(t, func() bool {
		status, err := d.apply(t.Context(), daemon.ProfilerParams{Action: "status"})
		return err == nil && !status.Stopping && !status.Running
	}, time.Second, time.Millisecond)
	status, err = d.apply(t.Context(), daemon.ProfilerParams{Action: "start", Address: address})
	require.NoError(t, err)
	assert.True(t, status.Running)
}

func TestDaemonProfilerCancelledControlDoesNotWaitForLock(t *testing.T) {
	d := &daemonProfiler{ctx: t.Context(), logger: zap.NewNop()}
	d.mu.Lock()
	defer d.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := d.apply(ctx, daemon.ProfilerParams{Action: "status"})
		done <- err
	}()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("cancelled profiler control waited for the held mutex")
	}
}

func TestDaemonProfileControlCommands(t *testing.T) {
	for _, action := range []string{"start", "stop", "status"} {
		t.Run(action, func(t *testing.T) {
			var calls []daemon.ProfilerParams
			cmd := &cobra.Command{Use: "profile"}
			addDaemonProfileControls(cmd, func(_ context.Context, p daemon.ProfilerParams) (daemon.ProfilerStatus, error) {
				calls = append(calls, p)
				return daemon.ProfilerStatus{Running: p.Action == "start"}, nil
			})
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(io.Discard)
			args := []string{action, "--json"}
			if action == "start" {
				args = append(args, "--address", "127.0.0.1:0")
			}
			cmd.SetArgs(args)
			require.NoError(t, cmd.Execute())
			require.Len(t, calls, 1)
			assert.Equal(t, action, calls[0].Action)
			assert.Contains(t, output.String(), `"running":`)
		})
	}
}
