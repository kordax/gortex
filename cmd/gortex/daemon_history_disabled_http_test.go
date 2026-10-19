package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/observability"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/testenv"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestDaemonHistoryPersistsWhenAllHTTPListenersAreOff(t *testing.T) {
	testenv.Sandbox(t)
	t.Setenv("GORTEX_METRICS_ADDR", "off")
	t.Setenv("GORTEX_DIAGNOSTICS_ADDR", "")
	t.Setenv("GORTEX_DAEMON_PPROF_ADDR", "")
	core, logs := observer.New(zap.InfoLevel)
	stop, err := startDaemonObservability(&cobra.Command{}, nil, nil, zap.New(core))
	require.NoError(t, err)
	defer stop()
	history, err := observability.OpenHistory(filepath.Join(platform.CacheDir(), "observability", "history.sqlite"))
	require.NoError(t, err)
	defer func() { require.NoError(t, history.Close()) }()
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		rows, err := history.ReadCharts(ctx, time.Now().Add(-time.Minute), 10)
		return err == nil && len(rows) > 0 && rows[0].Values["gortex_runtime_goroutines"] > 0
	}, 3*time.Second, 10*time.Millisecond)
	assert.Empty(t, logs.FilterMessage("daemon: local observability listener").All())
}
