package diagnostics

import (
	"net/http"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// HTTPTracing preserves streaming, flushing and hijacking by passing the original
// writer unchanged. Incoming W3C context parents the actual MCP tool spans.
func HTTPTracing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := propagation.TraceContext{}.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		operation := "other"
		if r.URL.Path == "/mcp" || r.URL.Path == "/mcp/" {
			operation = "mcp"
		} else if len(r.URL.Path) >= 3 && r.URL.Path[:3] == "/v1" {
			operation = "api"
		}
		ctx, finish := TraceOperation(ctx, "http", operation)
		defer func() { finish(ctx.Err()) }()
		if span := trace.SpanContextFromContext(ctx); span.IsValid() && activeTracing.Load() != nil {
			w.Header().Set("X-Gortex-Trace-ID", span.TraceID().String())
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
