package observability

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HistoryReader supplies bounded, chronological metric snapshots.
type HistoryReader interface {
	Read(context.Context, time.Time, int) ([]HistorySample, error)
}

// A projected reader avoids decompressing repository metadata for every chart
// refresh. Other HistoryReader implementations retain the bounded fallback.
type dashboardChartReader interface {
	ReadCharts(context.Context, time.Time, int) ([]HistorySample, error)
}

//go:embed dashboard/index.html dashboard/app.js dashboard/style.css
var dashboardAssets embed.FS

const dashboardResponseLimit = 8 << 20

// DashboardHandler serves the local UI and bounded history API. The listener must
// wrap this handler in its localhost Host/Origin guard; this is not authentication.
func DashboardHandler(history HistoryReader, current func() HistorySample) http.Handler {
	gate := make(chan struct{}, 4)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/", "/app.js", "/style.css":
			name, contentType := "index.html", "text/html; charset=utf-8"
			switch r.URL.Path {
			case "/app.js":
				name, contentType = "app.js", "text/javascript; charset=utf-8"
			case "/style.css":
				name, contentType = "style.css", "text/css; charset=utf-8"
			}
			asset, err := dashboardAssets.ReadFile("dashboard/" + name)
			if err != nil {
				http.Error(w, "dashboard unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", contentType)
			_, _ = w.Write(asset)
		case "/api/v1/current":
			if r.URL.RawQuery != "" {
				dashboardJSON(w, http.StatusBadRequest, map[string]string{"error": "unexpected query parameters"})
				return
			}
			sample := HistorySample{Values: map[string]float64{}}
			if current != nil {
				sample = dashboardSafeSample(current())
			}
			dashboardJSON(w, http.StatusOK, sample)
		case "/api/v1/history":
			seconds, limit, ok := dashboardQuery(r)
			if !ok {
				dashboardJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid history parameters"})
				return
			}
			response := dashboardHistoryResponse{Samples: []HistorySample{}, RetentionSeconds: 86400}
			if history != nil {
				ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
				defer cancel()
				read := history.Read
				projected := false
				if charts, ok := history.(dashboardChartReader); ok && r.URL.Query().Get("scope") == "charts" {
					read = charts.ReadCharts
					projected = true
				}
				samples, err := read(ctx, time.Now().Add(-time.Duration(seconds)*time.Second), limit)
				if err != nil || len(samples) > limit {
					response.Error = "history temporarily unavailable"
					dashboardJSON(w, http.StatusServiceUnavailable, response)
					return
				}
				response.Available = true
				estimated := 1024
				for _, sample := range samples {
					if !projected && r.URL.Query().Get("scope") == "charts" {
						sample = dashboardChartSample(sample)
					}
					for key := range sample.Values {
						// Six bytes per input byte bounds JSON escaping; numeric
						// values and object punctuation fit in the fixed allowance.
						estimated += 6*len(key) + 64
						if estimated > dashboardResponseLimit-1024 {
							dashboardJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "dashboard response unavailable", "available": false})
							return
						}
					}
					estimated += 128
					response.Samples = append(response.Samples, dashboardSafeSample(sample))
				}
			}
			dashboardJSON(w, http.StatusOK, response)
		default:
			http.NotFound(w, r)
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		select {
		case gate <- struct{}{}:
			// Release admission when the underlying callback ends, including after
			// an HTTP timeout. An uncooperative reader cannot create unbounded work.
			guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { <-gate }()
				handler.ServeHTTP(w, r)
			})
			http.TimeoutHandler(guarded, 3*time.Second, "dashboard request timed out\n").ServeHTTP(w, r)
		default:
			http.Error(w, "dashboard busy", http.StatusServiceUnavailable)
		}
	})
}

type dashboardHistoryResponse struct {
	Samples          []HistorySample `json:"samples"`
	RetentionSeconds int             `json:"retention_seconds"`
	Available        bool            `json:"available"`
	Error            string          `json:"error,omitempty"`
}

func dashboardQuery(r *http.Request) (int, int, bool) {
	if len(r.URL.RawQuery) > 128 {
		return 0, 0, false
	}
	// ParseQuery errors (for example a stray semicolon) must not silently use defaults.
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, 0, false
	}
	seconds, limit := 3600, 360
	for key, values := range query {
		if len(values) != 1 {
			return 0, 0, false
		}
		if key == "scope" {
			if values[0] != "charts" {
				return 0, 0, false
			}
			continue
		}
		value, parseErr := strconv.Atoi(values[0])
		if parseErr != nil {
			return 0, 0, false
		}
		switch key {
		case "seconds":
			if value < 1 || value > 86400 {
				return 0, 0, false
			}
			seconds = value
		case "limit":
			if value < 2 || value > 1440 {
				return 0, 0, false
			}
			limit = value
		default:
			return 0, 0, false
		}
	}
	return seconds, limit, true
}

// dashboardChartSample excludes high-cardinality repository snapshots from
// frequent chart refreshes; the current endpoint retains repository metadata.
func dashboardChartSample(sample HistorySample) HistorySample {
	filtered := HistorySample{At: sample.At, Values: make(map[string]float64)}
	for key, value := range sample.Values {
		name, _, _ := strings.Cut(key, "{")
		switch name {
		case "gortex_daemon_uptime_seconds", "gortex_process_cpu_seconds_total", "gortex_process_resident_memory_bytes", "gortex_process_pss_memory_bytes", "gortex_children_resident_memory_bytes", "gortex_runtime_heap_alloc_bytes", "gortex_runtime_heap_inuse_bytes", "gortex_runtime_heap_idle_bytes", "gortex_runtime_heap_released_bytes", "gortex_runtime_goroutines", "gortex_cache_bytes", "gortex_mcp_requests_total", "gortex_mcp_request_duration_seconds_bucket", "gortex_mcp_request_duration_seconds_count":
			filtered.Values[key] = value
		}
	}
	return filtered
}

func dashboardSafeSample(sample HistorySample) HistorySample {
	clean := HistorySample{At: sample.At, Values: make(map[string]float64)}
	if sample.At <= 0 || len(sample.Values) > 4096 {
		clean.At = 0
		return clean
	}
	for key, value := range sample.Values {
		if len(key) <= 512 && !math.IsInf(value, 0) && !math.IsNaN(value) {
			clean.Values[key] = value
		}
	}
	return clean
}

type dashboardBuffer struct{ bytes.Buffer }

func (b *dashboardBuffer) Write(p []byte) (int, error) {
	if len(p) > dashboardResponseLimit-b.Len() {
		return 0, errors.New("dashboard response too large")
	}
	return b.Buffer.Write(p)
}

func dashboardJSON(w http.ResponseWriter, status int, value any) {
	var buffer dashboardBuffer
	if err := json.NewEncoder(&buffer).Encode(value); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "{\"error\":\"dashboard response unavailable\",\"available\":false}\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buffer.Bytes())
}
