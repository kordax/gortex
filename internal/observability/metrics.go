// Package observability contains bounded, process-local diagnostic measurements.
package observability

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var durationBuckets = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

// Registry has a hard series ceiling. Request arguments, paths, IDs and error
// strings never become labels; resource and prompt names are collapsed.
type Registry struct {
	mu    sync.Mutex
	calls map[string]*callStats
}

type callStats struct {
	kind, operation string
	inFlight        int64
	outcomes        [6]uint64
	buckets         [len(durationBuckets)]uint64
	count           uint64
	sum             float64
}

var outcomes = [...]string{"success", "error", "cancelled", "deadline", "busy", "panic"}
var Default = &Registry{}

func operationLabels(kind, name string) (string, string) {
	switch kind {
	case "tool":
	case "resource", "prompt":
		return kind, kind
	default:
		return "other", "other"
	}
	if len(name) == 0 || len(name) > 80 {
		return kind, "other"
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return kind, "other"
		}
	}
	return kind, name
}

// Begin records caller-visible latency, including admission refusals. A
// detached timed-out handler is measured separately by the MCP abandoned gauge.
func (r *Registry) Begin(kind, name string) func(string) {
	kind, name = operationLabels(kind, name)
	key := kind + ":" + name
	r.mu.Lock()
	if r.calls == nil {
		r.calls = make(map[string]*callStats)
	}
	st := r.calls[key]
	if st == nil {
		if len(r.calls) >= 128 {
			key, kind, name = "other:other", "other", "other"
			st = r.calls[key]
		}
		if st == nil {
			st = &callStats{kind: kind, operation: name}
			r.calls[key] = st
		}
	}
	st.inFlight++
	r.mu.Unlock()
	started := time.Now()
	var once sync.Once
	return func(outcome string) {
		once.Do(func() {
			idx := 1
			for i, candidate := range outcomes {
				if candidate == outcome {
					idx = i
					break
				}
			}
			seconds := time.Since(started).Seconds()
			r.mu.Lock()
			st.inFlight--
			st.outcomes[idx]++
			st.count++
			st.sum += seconds
			for i, bound := range durationBuckets {
				if seconds <= bound {
					st.buckets[i]++
				}
			}
			r.mu.Unlock()
		})
	}
}

// Exposition writes Prometheus 0.0.4 text. Metadata is emitted once per family.
type Exposition struct {
	w    io.Writer
	seen map[string]bool
}

func NewExposition(w io.Writer) *Exposition {
	return &Exposition{w: w, seen: make(map[string]bool)}
}

func (e *Exposition) family(name, help, typ string) {
	if !e.seen[name] {
		_, _ = fmt.Fprintf(e.w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		e.seen[name] = true
	}
}

func (e *Exposition) Gauge(name, help string, value float64, labels ...string) {
	e.family(name, help, "gauge")
	e.sample(name, value, labels...)
}

func (e *Exposition) Counter(name, help string, value float64, labels ...string) {
	e.family(name, help, "counter")
	e.sample(name, value, labels...)
}

func (e *Exposition) sample(name string, value float64, labels ...string) {
	_, _ = io.WriteString(e.w, name)
	if len(labels) > 0 {
		_, _ = io.WriteString(e.w, "{")
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				_, _ = io.WriteString(e.w, ",")
			}
			_, _ = fmt.Fprintf(e.w, "%s=\"%s\"", labels[i], escapeLabel(labels[i+1]))
		}
		_, _ = io.WriteString(e.w, "}")
	}
	_, _ = fmt.Fprintf(e.w, " %s\n", strconv.FormatFloat(value, 'g', -1, 64))
}

var labelEscaper = strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"")

func escapeLabel(s string) string {
	return labelEscaper.Replace(s)
}

func (r *Registry) Write(e *Exposition) {
	r.mu.Lock()
	rows := make([]callStats, 0, len(r.calls))
	for _, st := range r.calls {
		rows = append(rows, *st)
	}
	r.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].kind+rows[i].operation < rows[j].kind+rows[j].operation })
	for _, st := range rows {
		labels := []string{"kind", st.kind, "operation", st.operation}
		e.Gauge("gortex_mcp_requests_in_flight", "Caller-visible MCP requests currently waiting.", float64(st.inFlight), labels...)
		for i, outcome := range outcomes {
			e.Counter("gortex_mcp_requests_total", "Completed caller-visible MCP requests by outcome.", float64(st.outcomes[i]), append(labels, "outcome", outcome)...)
		}
		e.family("gortex_mcp_request_duration_seconds", "Caller-visible MCP request latency, including failures.", "histogram")
		for i, bound := range durationBuckets {
			e.sample("gortex_mcp_request_duration_seconds_bucket", float64(st.buckets[i]), append(labels, "le", strconv.FormatFloat(bound, 'g', -1, 64))...)
		}
		e.sample("gortex_mcp_request_duration_seconds_bucket", float64(st.count), append(labels, "le", "+Inf")...)
		e.sample("gortex_mcp_request_duration_seconds_sum", st.sum, labels...)
		e.sample("gortex_mcp_request_duration_seconds_count", float64(st.count), labels...)
	}
}
