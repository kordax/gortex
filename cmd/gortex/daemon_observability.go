package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/diagnostics"
	"github.com/zzet/gortex/internal/observability"
	"go.uber.org/zap"
)

var daemonMetricsAddr string
var daemonDiagnosticsAddr string

func init() {
	daemonStartCmd.Flags().StringVar(&daemonMetricsAddr, "metrics-addr", "", "enable local dashboard and Prometheus at this address (or GORTEX_METRICS_ADDR / daemon.metrics_addr; disabled by default, collection continues)")
	daemonStartCmd.Flags().StringVar(&daemonDiagnosticsAddr, "diagnostics-addr", "", "bounded Go profiles on a separate loopback address, e.g. 127.0.0.1:9475 (or GORTEX_DIAGNOSTICS_ADDR; disabled by default)")
}

func setupDaemonTracing(logger *zap.Logger) (func(), error) {
	cfg, err := diagnostics.TraceConfigFromEnv(os.Getenv, canonicalVersion())
	if err != nil {
		return nil, err
	}
	shutdown, err := diagnostics.StartTracing(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	if cfg.Endpoint != "" {
		logger.Info("daemon: OTLP HTTP tracing enabled", zap.String("sampler", cfg.Sampler))
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			logger.Warn("daemon: trace exporter shutdown incomplete")
		}
	}, nil
}

func startDaemonObservability(cmd *cobra.Command, srv *daemon.Server, controller *realController, logger *zap.Logger) (func(), error) {
	ctx, cancel := context.WithCancel(context.Background())
	var servers []*http.Server
	var listeners []net.Listener
	var history *daemonMetricHistory
	var dashboard *daemonDashboard
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if dashboard != nil {
				dashboard.close()
			}
			pprofAddr.Store("")
			shutdownCtx, done := context.WithTimeout(context.Background(), 2*time.Second)
			defer done()
			for i, server := range servers {
				if err := server.Shutdown(shutdownCtx); err != nil {
					_ = server.Close()
				}
				// Shutdown cannot see a listener until Serve registers it.
				// Release the original even when startup is immediately stopped.
				_ = listeners[i].Close()
			}
			if history != nil {
				history.close()
			}
		})
	}
	address := func(flag, value, env string) string {
		if cmd.Flags().Changed(flag) {
			return value
		}
		return os.Getenv(env)
	}
	start := func(kind, addr string, handler http.Handler) error {
		if addr == "" {
			return nil
		}
		listener, err := diagnostics.ListenLocal(addr)
		if err != nil {
			return fmt.Errorf("%s listener: %w", kind, err)
		}
		server := &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       5 * time.Second,
			IdleTimeout:       30 * time.Second,
			WriteTimeout:      70 * time.Second,
			MaxHeaderBytes:    16 << 10,
			BaseContext:       func(_ net.Listener) context.Context { return ctx },
		}
		servers = append(servers, server)
		listeners = append(listeners, listener)
		if kind == "profiles" {
			pprofAddr.Store(listener.Addr().String())
		}
		go func() {
			if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
				logger.Warn("daemon: observability listener stopped", zap.String("kind", kind))
			}
		}()
		logger.Info("daemon: local observability listener", zap.String("kind", kind), zap.String("address", listener.Addr().String()))
		return nil
	}
	metricSource := daemonMetricsHandler(ctx, srv, controller)
	history = startDaemonMetricHistory(ctx, metricSource, logger)
	var reader observability.HistoryReader
	if history.store != nil {
		reader = history.store
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", metricSource)
	metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	metricsMux.Handle("/", observability.DashboardHandler(reader, history.current))
	var configuredMetricsAddr string
	if controller != nil && controller.configManager != nil {
		if global := controller.configManager.Global(); global != nil {
			configuredMetricsAddr = global.Daemon.MetricsAddr
		}
	}
	metricsAddress := daemonDashboardAddress(cmd, daemonMetricsAddr, configuredMetricsAddr)
	dashboard = &daemonDashboard{ctx: ctx, handler: diagnostics.LocalOnly(metricsMux), logger: logger, preferred: metricsAddress}
	if controller != nil {
		controller.dashboard = dashboard
	}
	if metricsAddress != "" {
		if _, err := dashboard.apply(ctx, daemon.DashboardParams{Action: "start", Address: metricsAddress}); err != nil {
			stop()
			return nil, err
		}
	}
	profileAddress := address("diagnostics-addr", daemonDiagnosticsAddr, "GORTEX_DIAGNOSTICS_ADDR")
	if profileAddress == "" && !cmd.Flags().Changed("diagnostics-addr") && os.Getenv("GORTEX_DIAGNOSTICS_ADDR") == "" {
		profileAddress = os.Getenv("GORTEX_DAEMON_PPROF_ADDR")
		if profileAddress != "" && !isLocalhostBind(profileAddress) {
			logger.Warn("daemon: refusing non-loopback legacy pprof bind")
			profileAddress = ""
		}
	}
	if err := start("profiles", profileAddress, diagnostics.ProfileHandler()); err != nil {
		stop()
		return nil, err
	}
	return stop, nil
}

func daemonDashboardAddress(cmd *cobra.Command, value, configured string) string {
	if cmd.Flags().Changed("metrics-addr") {
		configured = value
	} else if env, exists := os.LookupEnv("GORTEX_METRICS_ADDR"); exists {
		configured = env
	}
	if configured == "off" {
		return ""
	}
	return configured
}
