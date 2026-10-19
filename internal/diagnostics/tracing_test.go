package diagnostics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	collecttrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestTraceConfigurationIsExplicitAndValidated(t *testing.T) {
	read := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	cfg, err := TraceConfigFromEnv(read(nil), "test")
	require.NoError(t, err)
	assert.Empty(t, cfg.Endpoint)
	cfg, err = TraceConfigFromEnv(read(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"}), "test")
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:4318/v1/traces", cfg.Endpoint)
	cfg, err = TraceConfigFromEnv(read(map[string]string{"OTEL_TRACES_EXPORTER": "none", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"}), "test")
	require.NoError(t, err)
	assert.Empty(t, cfg.Endpoint)
	for _, endpoint := range []string{"file:///tmp/private", "http://user:secret@localhost:4318", "http://localhost:4318?secret=value", "http://localhost:4318#secret", "not a URL"} {
		_, err := TraceConfigFromEnv(read(map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": endpoint}), "test")
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "secret")
	}
	for _, ratio := range []string{"NaN", "+Inf", "-1", "1.1", "invalid"} {
		_, err := TraceConfigFromEnv(read(map[string]string{"OTEL_TRACES_EXPORTER": "otlp", "OTEL_TRACES_SAMPLER_ARG": ratio}), "test")
		require.Error(t, err, ratio)
	}
}

func TestOTLPTraceExportAndHTTPParentingWithoutPayloads(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "")
	requests := make(chan *collecttrace.ExportTraceServiceRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			w.WriteHeader(400)
			return
		}
		request := new(collecttrace.ExportTraceServiceRequest)
		if err := proto.Unmarshal(data, request); err != nil {
			w.WriteHeader(400)
			return
		}
		requests <- request
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(200)
	}))
	defer server.Close()
	shutdown, err := StartTracing(context.Background(), TraceConfig{Endpoint: server.URL + "/v1/traces", Sampler: "always_on", Version: "test"})
	require.NoError(t, err)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, shutdown(ctx))
	}()
	_, finish := TraceOperation(context.Background(), "mcp", "read")
	finish(fmt.Errorf("secret source code and SQL parameters"))
	request := httptest.NewRequest("GET", "http://127.0.0.1/mcp?private=user-token", nil)
	request.Header.Set("Traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
	recorder := httptest.NewRecorder()
	HTTPTracing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, done := TraceOperation(r.Context(), "mcp", "search")
		done(context.DeadlineExceeded)
		flusher, ok := w.(http.Flusher)
		require.True(t, ok, "tracing must preserve streaming writer interfaces")
		flusher.Flush()
	})).ServeHTTP(recorder, request)
	assert.Equal(t, "11111111111111111111111111111111", recorder.Header().Get("X-Gortex-Trace-ID"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, activeTracing.Load().ForceFlush(ctx))
	var data *collecttrace.ExportTraceServiceRequest
	select {
	case data = <-requests:
	case <-ctx.Done():
		t.Fatal("no local OTLP request")
	}
	assert.NotContains(t, data.String(), "secret")
	assert.NotContains(t, data.String(), "user-token")
	spans := map[string]*tracepb.Span{}
	for _, resource := range data.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			for _, span := range scope.Spans {
				operation := ""
				for _, attribute := range span.Attributes {
					if attribute.Key == "gortex.operation" {
						operation = attribute.Value.GetStringValue()
					}
				}
				spans[operation] = span
			}
		}
	}
	require.Contains(t, spans, "mcp")
	require.Contains(t, spans, "search")
	assert.Equal(t, spans["mcp"].SpanId, spans["search"].ParentSpanId)
	assert.Equal(t, []byte(strings.Repeat("\x22", 8)), spans["mcp"].ParentSpanId)
	assert.Equal(t, "deadline", spans["search"].Status.Message)
}
