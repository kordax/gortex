package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TraceConfig enables export only when an OTLP endpoint or exporter is explicitly
// configured. It never reads source, tool arguments, SQL, tokens or environment dumps.
type TraceConfig struct {
	Endpoint string
	Sampler  string
	Ratio    float64
	Version  string
}

func TraceConfigFromEnv(getenv func(string) string, version string) (TraceConfig, error) {
	cfg := TraceConfig{Version: version, Ratio: 0.1, Sampler: "parentbased_traceidratio"}
	exporter := getenv("OTEL_TRACES_EXPORTER")
	if exporter == "none" {
		return cfg, nil
	}
	if exporter != "" && exporter != "otlp" {
		return cfg, fmt.Errorf("OTEL_TRACES_EXPORTER must be otlp or none")
	}
	cfg.Endpoint = getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if cfg.Endpoint == "" {
		if base := getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); base != "" {
			cfg.Endpoint = strings.TrimRight(base, "/") + "/v1/traces"
		} else if exporter == "otlp" {
			cfg.Endpoint = "http://127.0.0.1:4318/v1/traces"
		}
	}
	if cfg.Endpoint == "" {
		return cfg, nil
	}
	protocol := getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
	if protocol == "" {
		protocol = getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	if protocol != "" && protocol != "http/protobuf" {
		return cfg, fmt.Errorf("trace export supports OTLP http/protobuf only")
	}
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return cfg, fmt.Errorf("OTLP trace endpoint must be an HTTP(S) URL without credentials, query or fragment")
	}
	if sampler := getenv("OTEL_TRACES_SAMPLER"); sampler != "" {
		cfg.Sampler = sampler
	}
	if raw := getenv("OTEL_TRACES_SAMPLER_ARG"); raw != "" {
		cfg.Ratio, err = strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(cfg.Ratio) || cfg.Ratio < 0 || cfg.Ratio > 1 {
			return cfg, fmt.Errorf("OTEL_TRACES_SAMPLER_ARG must be a ratio from 0 to 1")
		}
	}
	if _, err := traceSampler(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func traceSampler(cfg TraceConfig) (sdktrace.Sampler, error) {
	switch cfg.Sampler {
	case "always_on":
		return sdktrace.AlwaysSample(), nil
	case "always_off":
		return sdktrace.NeverSample(), nil
	case "parentbased_always_on":
		return sdktrace.ParentBased(sdktrace.AlwaysSample()), nil
	case "parentbased_always_off":
		return sdktrace.ParentBased(sdktrace.NeverSample()), nil
	case "traceidratio":
		return sdktrace.TraceIDRatioBased(cfg.Ratio), nil
	case "", "parentbased_traceidratio":
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.Ratio)), nil
	default:
		return nil, fmt.Errorf("unsupported OTEL_TRACES_SAMPLER")
	}
}

var activeTracing atomic.Pointer[sdktrace.TracerProvider]

// StartTracing installs a private provider, leaving unrelated global OTel users
// alone. The queue drops spans rather than blocking daemon work if Tempo is down.
func StartTracing(ctx context.Context, cfg TraceConfig) (func(context.Context) error, error) {
	if cfg.Endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	sampler, err := traceSampler(cfg)
	if err != nil {
		return nil, err
	}
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(cfg.Endpoint), otlptracehttp.WithTimeout(3*time.Second), otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}))
	if err != nil {
		return nil, fmt.Errorf("initialize OTLP HTTP trace exporter")
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "gortex"), attribute.String("service.version", cfg.Version))),
		sdktrace.WithSampler(sampler),
		sdktrace.WithBatcher(exporter, sdktrace.WithMaxQueueSize(1024), sdktrace.WithMaxExportBatchSize(128), sdktrace.WithBatchTimeout(2*time.Second), sdktrace.WithExportTimeout(3*time.Second)),
	)
	if !activeTracing.CompareAndSwap(nil, provider) {
		_ = provider.Shutdown(ctx)
		return nil, fmt.Errorf("tracing already started")
	}
	return func(shutdownCtx context.Context) error {
		activeTracing.CompareAndSwap(provider, nil)
		return provider.Shutdown(shutdownCtx)
	}, nil
}

// TraceOperation propagates the request context and records categories only.
// Never pass raw arguments, paths, SQL or an error message as an attribute.
func TraceOperation(ctx context.Context, component, operation string) (context.Context, func(error)) {
	provider := activeTracing.Load()
	if provider == nil {
		return ctx, func(error) {}
	}
	ctx, span := provider.Tracer("github.com/zzet/gortex").Start(ctx, "gortex.operation", trace.WithAttributes(attribute.String("gortex.component", component), attribute.String("gortex.operation", operation)))
	return ctx, func(err error) {
		if err != nil {
			category := "error"
			if errors.Is(err, context.Canceled) {
				category = "cancelled"
			} else if errors.Is(err, context.DeadlineExceeded) {
				category = "deadline"
			}
			span.SetStatus(codes.Error, category)
			span.SetAttributes(attribute.String("error.type", category))
		}
		span.End()
	}
}
