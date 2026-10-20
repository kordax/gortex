package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestDaemonObservabilityDashboardRequiresExplicitConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		flag       string
		envOff     bool
		wantHTTP   bool
	}{
		{name: "default-off"},
		{name: "config-on", configured: "127.0.0.1:0", wantHTTP: true},
		{name: "config-off", configured: "off"},
		{name: "flag-on", flag: "127.0.0.1:0", wantHTTP: true},
		{name: "env-off-overrides-config", configured: "127.0.0.1:0", envOff: true},
		{name: "flag-off-overrides-config", configured: "127.0.0.1:0", flag: "off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GORTEX_METRICS_ADDR", "")
			if !tc.envOff {
				require.NoError(t, os.Unsetenv("GORTEX_METRICS_ADDR"))
			}
			t.Setenv("GORTEX_DIAGNOSTICS_ADDR", "")
			t.Setenv("GORTEX_DAEMON_PPROF_ADDR", "")
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte("daemon:\n  metrics_addr: \""+tc.configured+"\"\n"), 0o600))
			manager, err := config.NewConfigManager(path)
			require.NoError(t, err)
			assert.Equal(t, tc.configured, manager.Global().Daemon.MetricsAddr)
			previous := daemonMetricsAddr
			t.Cleanup(func() { daemonMetricsAddr = previous })
			cmd := &cobra.Command{}
			cmd.Flags().StringVar(&daemonMetricsAddr, "metrics-addr", "", "")
			if tc.flag != "" {
				require.NoError(t, cmd.Flags().Set("metrics-addr", tc.flag))
			}
			core, logs := observer.New(zap.InfoLevel)
			stop, err := startDaemonObservability(cmd, nil, &realController{configManager: manager}, zap.New(core))
			require.NoError(t, err)
			t.Cleanup(stop)
			entries := logs.FilterMessage("daemon: local observability listener").All()
			if !tc.wantHTTP {
				assert.Empty(t, entries, "disabled dashboard must not bind an HTTP port")
				return
			}
			require.Len(t, entries, 1)
			assert.Equal(t, "metrics", entries[0].ContextMap()["kind"])
			address := entries[0].ContextMap()["address"].(string)
			client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
			defer client.CloseIdleConnections()
			response, err := client.Get("http://" + address + "/healthz")
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, response.StatusCode)
			require.NoError(t, response.Body.Close())
		})
	}
}

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
