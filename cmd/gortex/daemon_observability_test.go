package main

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestDaemonObservabilityListenersSeparateProfilesAndStopCleanly(t *testing.T) {
	t.Setenv("GORTEX_METRICS_ADDR", "127.0.0.1:0")
	t.Setenv("GORTEX_DIAGNOSTICS_ADDR", "127.0.0.1:0")
	core, logs := observer.New(zap.InfoLevel)
	server := daemon.New("", "test", zap.NewNop())
	stop, err := startDaemonObservability(&cobra.Command{}, server, nil, zap.New(core))
	require.NoError(t, err)
	defer stop()
	entries := logs.FilterMessage("daemon: local observability listener").All()
	require.Len(t, entries, 2)
	addresses := map[string]string{}
	for _, entry := range entries {
		fields := entry.ContextMap()
		addresses[fields["kind"].(string)] = fields["address"].(string)
	}
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	request := func(kind, path string) (int, string) {
		response, err := client.Get("http://" + addresses[kind] + path)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		return response.StatusCode, string(data)
	}
	code, body := request("metrics", "/metrics")
	assert.Equal(t, 200, code)
	assert.Contains(t, body, "gortex_runtime_heap_alloc_bytes")
	code, _ = request("metrics", "/debug/pprof/heap")
	assert.Equal(t, 404, code, "scraping permissions must not expose profiling")
	code, _ = request("profiles", "/metrics")
	assert.Equal(t, 404, code)
	code, _ = request("profiles", "/debug/pprof/goroutine")
	assert.Equal(t, 200, code)
	stop()
	stop()
	for _, address := range addresses {
		listener, err := net.Listen("tcp", address)
		require.NoError(t, err, "listener was not released")
		require.NoError(t, listener.Close())
	}
}

func TestDaemonObservabilityLegacyPProfAlias(t *testing.T) {
	t.Setenv("GORTEX_METRICS_ADDR", "")
	t.Setenv("GORTEX_DIAGNOSTICS_ADDR", "")
	t.Setenv("GORTEX_DAEMON_PPROF_ADDR", "127.0.0.1:0")
	stop, err := startDaemonObservability(&cobra.Command{}, nil, nil, zap.NewNop())
	require.NoError(t, err)
	defer stop()
	require.NotEmpty(t, daemonPProfAddr())
	stop()
	assert.Empty(t, daemonPProfAddr())
}

func TestDaemonObservabilityRefusesPublicBindAndCleansPartialStartup(t *testing.T) {
	t.Setenv("GORTEX_METRICS_ADDR", "127.0.0.1:0")
	t.Setenv("GORTEX_DIAGNOSTICS_ADDR", "0.0.0.0:9475")
	core, logs := observer.New(zap.InfoLevel)
	stop, err := startDaemonObservability(&cobra.Command{}, nil, nil, zap.New(core))
	require.Error(t, err)
	assert.Nil(t, stop)
	entries := logs.FilterMessage("daemon: local observability listener").All()
	require.Len(t, entries, 1)
	address := entries[0].ContextMap()["address"].(string)
	assert.True(t, strings.HasPrefix(address, "127.0.0.1:"))
	listener, err := net.Listen("tcp", address)
	require.NoError(t, err, "partially started metrics listener must close on failure")
	require.NoError(t, listener.Close())
}
