package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/observability"
	"github.com/zzet/gortex/internal/platform"
	"go.uber.org/zap"
)

const daemonHistoryInterval = 10 * time.Second
const maxDaemonMetricSnapshot = 1 << 20

// A single collector runs even when no browser or Prometheus is connected.
// It consumes the existing cheap metric source, never Status or a graph scan.
type daemonMetricHistory struct {
	latest atomic.Pointer[observability.HistorySample]
	store  *observability.History
	done   chan struct{}
}

func startDaemonMetricHistory(ctx context.Context, source http.Handler, logger *zap.Logger) *daemonMetricHistory {
	h := &daemonMetricHistory{done: make(chan struct{})}
	path := filepath.Join(platform.CacheDir(), "observability", "history.sqlite")
	store, err := observability.OpenHistory(path)
	if err != nil {
		logger.Warn("daemon: local metric history unavailable; live metrics remain enabled")
	} else {
		h.store = store
	}
	go h.collect(ctx, source, logger, daemonHistoryInterval)
	return h
}

func (h *daemonMetricHistory) current() observability.HistorySample {
	if sample := h.latest.Load(); sample != nil {
		return *sample // Immutable after publication; the HTTP encoder only reads it.
	}
	return observability.HistorySample{Values: map[string]float64{}}
}

func (h *daemonMetricHistory) close() {
	<-h.done // The owner cancels the context before closing.
	if h.store != nil {
		_ = h.store.Close()
	}
}

func (h *daemonMetricHistory) collect(ctx context.Context, source http.Handler, logger *zap.Logger, interval time.Duration) {
	defer close(h.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failed := false
	for {
		if ctx.Err() != nil {
			return
		}
		sample, err := captureDaemonMetrics(ctx, source, time.Now())
		if err == nil {
			h.latest.Store(&sample)
		}
		if h.store != nil {
			writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			// Expiry keeps running even when a bad metric prevents new samples.
			pruneErr := h.store.Prune(writeCtx, time.Now())
			if err == nil {
				if pruneErr != nil {
					err = pruneErr
				} else {
					err = h.store.Append(writeCtx, sample)
				}
			}
			cancel()
		}
		if err != nil && ctx.Err() == nil && !failed {
			logger.Warn("daemon: local metric sample unavailable")
		}
		failed = err != nil
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// This bounded in-process writer reuses the exposition source without opening
// a listener, a connection, a second sampler, or a test-only HTTP server.
type daemonMetricBuffer struct {
	header http.Header
	body   bytes.Buffer
	status int
	full   bool
}

func (w *daemonMetricBuffer) Header() http.Header    { return w.header }
func (w *daemonMetricBuffer) WriteHeader(status int) { w.status = status }
func (w *daemonMetricBuffer) Write(p []byte) (int, error) {
	if w.body.Len()+len(p) > maxDaemonMetricSnapshot {
		w.full = true
		return 0, fmt.Errorf("metric snapshot exceeds limit")
	}
	return w.body.Write(p)
}

func captureDaemonMetrics(ctx context.Context, source http.Handler, at time.Time) (observability.HistorySample, error) {
	sample := observability.HistorySample{At: at.UnixMilli(), Values: make(map[string]float64)}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1/metrics", nil)
	if err != nil {
		return sample, err
	}
	buffer := &daemonMetricBuffer{header: make(http.Header), status: http.StatusOK}
	source.ServeHTTP(buffer, request)
	if buffer.full || buffer.status != http.StatusOK || ctx.Err() != nil {
		return sample, fmt.Errorf("metric source unavailable")
	}
	for _, line := range strings.Split(buffer.body.String(), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sep := strings.LastIndexByte(line, ' ')
		if sep <= 0 {
			return sample, fmt.Errorf("invalid metric sample")
		}
		value, err := strconv.ParseFloat(line[sep+1:], 64)
		if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
			return sample, fmt.Errorf("invalid metric value")
		}
		name, labels, err := daemonMetricSeries(line[:sep])
		if err != nil {
			return sample, err
		}
		// Daily charts keep overall request outcomes and histogram buckets.
		// Per-operation breakdown stays available from the Prometheus endpoint.
		aggregate := strings.HasPrefix(name, "gortex_mcp_")
		if aggregate {
			delete(labels, "kind")
			delete(labels, "operation")
		}
		key := daemonMetricKey(name, labels)
		if aggregate {
			sample.Values[key] += value
			if math.IsInf(sample.Values[key], 0) {
				return sample, fmt.Errorf("metric aggregate overflow")
			}
		} else {
			if _, duplicate := sample.Values[key]; duplicate {
				return sample, fmt.Errorf("duplicate metric sample")
			}
			sample.Values[key] = value
		}
		if len(sample.Values) > 4096 {
			return sample, fmt.Errorf("metric series limit exceeded")
		}
	}
	if len(sample.Values) == 0 {
		return sample, fmt.Errorf("empty metric source")
	}
	return sample, nil
}

func daemonMetricSeries(raw string) (string, map[string]string, error) {
	name, tail, hasLabels := strings.Cut(raw, "{")
	validName := func(s string) bool {
		if s == "" {
			return false
		}
		for i, c := range s {
			switch {
			case c == '_' || c == ':':
			case c >= 'a' && c <= 'z':
			case c >= 'A' && c <= 'Z':
			case i > 0 && c >= '0' && c <= '9':
			default:
				return false
			}
		}
		return true
	}
	if !validName(name) || !strings.HasPrefix(name, "gortex_") {
		return "", nil, fmt.Errorf("invalid metric name")
	}
	labels := make(map[string]string)
	if !hasLabels {
		return name, labels, nil
	}
	if !strings.HasSuffix(tail, "}") {
		return "", nil, fmt.Errorf("invalid metric labels")
	}
	tail = strings.TrimSuffix(tail, "}")
	for tail != "" {
		key, value, ok := strings.Cut(tail, "=")
		if !ok || !validName(key) || strings.Contains(key, ":") || !strings.HasPrefix(value, "\"") {
			return "", nil, fmt.Errorf("invalid metric label")
		}
		end := 1
		for end < len(value) {
			if value[end] == '\\' {
				end += 2
				continue
			}
			if value[end] == '"' {
				break
			}
			end++
		}
		if end >= len(value) {
			return "", nil, fmt.Errorf("unterminated metric label")
		}
		decoded, err := strconv.Unquote(value[:end+1])
		if err != nil {
			return "", nil, fmt.Errorf("invalid metric label escape")
		}
		if _, duplicate := labels[key]; duplicate {
			return "", nil, fmt.Errorf("duplicate metric label")
		}
		labels[key] = decoded
		tail = value[end+1:]
		if tail == "" {
			break
		}
		if !strings.HasPrefix(tail, ",") || len(tail) == 1 {
			return "", nil, fmt.Errorf("invalid metric label separator")
		}
		tail = tail[1:]
	}
	return name, labels, nil
}

var daemonHistoryLabelEscaper = strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"")

func daemonMetricKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var key strings.Builder
	key.WriteString(name)
	key.WriteByte('{')
	for i, label := range keys {
		if i > 0 {
			key.WriteByte(',')
		}
		key.WriteString(label)
		key.WriteByte('=')
		value := daemonHistoryLabelEscaper.Replace(labels[label])
		key.WriteByte('"')
		key.WriteString(value)
		key.WriteByte('"')
	}
	key.WriteByte('}')
	return key.String()
}
