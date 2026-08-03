// Package telemetry initialises slog + the OpenTelemetry SDK for pwrapd.
//
// Tracing  : OTLP/HTTP exporter (configured via PWRAP_OTLP_ENDPOINT). Sampling is
//
//	controlled by PWRAP_TRACE_SAMPLE_RATE (0.0–1.0, default 1.0).
//
// Metrics  : same OTLP target if set, plus an optional Prometheus exporter
//
//	served from PWRAP_METRICS_ADDR (e.g. ":9464"). Useful as a fallback
//	when there's no OTLP collector in the deployment.
//
// All exporters are optional. When PWRAP_OTLP_ENDPOINT is empty and
// PWRAP_METRICS_ADDR is empty, this package returns a no-op providers — the
// SDK is still initialised so instrumented code paths don't have to nil-check.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// Options configures the OTel SDK. All fields are optional.
type Options struct {
	// ServiceName tags every span/metric. Defaults to "pwrapd".
	ServiceName string
	// ServiceVersion (optional). Defaults to "dev".
	ServiceVersion string
	// Environment (e.g. "development", "production"). Defaults to "development".
	Environment string

	// OTLPEndpoint enables OTLP/HTTP export. Example: "localhost:4318" or
	// "https://otel.acme.com". Scheme prefix optional; OTLPInsecure controls TLS.
	OTLPEndpoint string
	// OTLPInsecure skips TLS verification — useful for localhost collectors.
	OTLPInsecure bool

	// TraceSampleRate in [0,1]. 0 = drop everything, 1 = sample everything.
	TraceSampleRate float64

	// PrometheusAddr, when non-empty, starts an HTTP server exposing /metrics
	// in Prometheus format. Example: ":9464".
	PrometheusAddr string

	// Logger used for setup messages.
	Logger *slog.Logger
}

// Providers is the bundle of OTel SDK objects pwrapd needs to keep alive for
// the process lifetime. Call Shutdown on shutdown to flush in-flight spans
// and metric exports.
type Providers struct {
	Tracer        *trace.TracerProvider
	Meter         *metric.MeterProvider
	promServer    *http.Server
	shutdownFuncs []func(context.Context) error
}

// Init wires up the global OpenTelemetry SDK using opts and returns the provider
// bundle. Failures in any single exporter degrade gracefully (logged, no
// process exit) so missing infra doesn't take pwrapd down. Always returns a
// non-nil *Providers — Shutdown is safe to call.
func Init(ctx context.Context, opts Options) (*Providers, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if opts.ServiceName == "" {
		opts.ServiceName = "pwrapd"
	}
	if opts.ServiceVersion == "" {
		opts.ServiceVersion = "dev"
	}
	if opts.Environment == "" {
		opts.Environment = "development"
	}
	if opts.TraceSampleRate < 0 {
		opts.TraceSampleRate = 0
	}
	if opts.TraceSampleRate > 1 {
		opts.TraceSampleRate = 1
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(opts.ServiceName),
		semconv.ServiceVersion(opts.ServiceVersion),
		semconv.DeploymentEnvironmentName(opts.Environment),
	))
	if err != nil {
		// Even on resource-merge failure, return a non-nil Providers so Shutdown
		// is always callable; the caller already logs the warning.
		return &Providers{}, fmt.Errorf("otel resource: %w", err)
	}

	p := &Providers{}

	// --- Tracer ---------------------------------------------------------------
	traceOpts := []trace.TracerProviderOption{
		trace.WithResource(res),
		trace.WithSampler(trace.ParentBased(trace.TraceIDRatioBased(opts.TraceSampleRate))),
	}
	if opts.OTLPEndpoint != "" {
		exp, err := newTraceExporter(ctx, opts)
		if err != nil {
			logger.Warn("otel trace exporter init failed; spans will be dropped", "err", err)
		} else {
			traceOpts = append(traceOpts, trace.WithBatcher(exp,
				trace.WithBatchTimeout(5*time.Second),
			))
			p.shutdownFuncs = append(p.shutdownFuncs, exp.Shutdown)
		}
	}
	p.Tracer = trace.NewTracerProvider(traceOpts...)
	otel.SetTracerProvider(p.Tracer)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	p.shutdownFuncs = append(p.shutdownFuncs, p.Tracer.Shutdown)

	// --- Meter ----------------------------------------------------------------
	meterOpts := []metric.Option{metric.WithResource(res)}
	if opts.OTLPEndpoint != "" {
		exp, err := newMetricExporter(ctx, opts)
		if err != nil {
			logger.Warn("otel metric exporter init failed; OTLP metrics disabled", "err", err)
		} else {
			meterOpts = append(meterOpts, metric.WithReader(metric.NewPeriodicReader(exp,
				metric.WithInterval(30*time.Second),
			)))
			p.shutdownFuncs = append(p.shutdownFuncs, exp.Shutdown)
		}
	}
	if opts.PrometheusAddr != "" {
		promExp, err := prometheus.New()
		if err != nil {
			logger.Warn("prometheus exporter init failed", "err", err)
		} else {
			meterOpts = append(meterOpts, metric.WithReader(promExp))
			mux := http.NewServeMux()
			mux.Handle("/metrics", promhttp.Handler())
			p.promServer = &http.Server{
				Addr:              opts.PrometheusAddr,
				Handler:           mux,
				ReadHeaderTimeout: 5 * time.Second,
			}
			go func() {
				logger.Info("prometheus metrics endpoint listening", "addr", opts.PrometheusAddr, "path", "/metrics")
				if err := p.promServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Warn("prometheus server stopped", "err", err)
				}
			}()
		}
	}
	p.Meter = metric.NewMeterProvider(meterOpts...)
	otel.SetMeterProvider(p.Meter)
	p.shutdownFuncs = append(p.shutdownFuncs, p.Meter.Shutdown)

	if opts.OTLPEndpoint == "" && opts.PrometheusAddr == "" {
		logger.Info("telemetry exporters disabled — set PWRAP_OTLP_ENDPOINT or PWRAP_METRICS_ADDR to enable")
	} else {
		logger.Info("telemetry initialised",
			"service", opts.ServiceName,
			"otlp", opts.OTLPEndpoint,
			"prometheus", opts.PrometheusAddr,
			"trace_sample_rate", opts.TraceSampleRate,
		)
	}
	return p, nil
}

// Shutdown flushes pending spans and metric exports, then stops the Prometheus
// HTTP server. Idempotent; safe to call on a Providers returned even after a
// partial Init failure.
func (p *Providers) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var errs []error
	for i := len(p.shutdownFuncs) - 1; i >= 0; i-- {
		if err := p.shutdownFuncs[i](ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if p.promServer != nil {
		if err := p.promServer.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func newTraceExporter(ctx context.Context, opts Options) (*otlptrace.Exporter, error) {
	tOpts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(stripScheme(opts.OTLPEndpoint))}
	if opts.OTLPInsecure {
		tOpts = append(tOpts, otlptracehttp.WithInsecure())
	}
	return otlptracehttp.New(ctx, tOpts...)
}

func newMetricExporter(ctx context.Context, opts Options) (*otlpmetrichttp.Exporter, error) {
	mOpts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(stripScheme(opts.OTLPEndpoint))}
	if opts.OTLPInsecure {
		mOpts = append(mOpts, otlpmetrichttp.WithInsecure())
	}
	return otlpmetrichttp.New(ctx, mOpts...)
}

// stripScheme accepts "http://host:port" or "host:port" forms and returns
// "host:port" — the OTLP HTTP exporter wants just the authority.
func stripScheme(s string) string {
	for _, p := range []string{"https://", "http://"} {
		if len(s) > len(p) && s[:len(p)] == p {
			return s[len(p):]
		}
	}
	return s
}
