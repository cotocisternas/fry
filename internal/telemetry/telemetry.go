// Package telemetry owns optional, privacy-bounded telemetry for one process.
// It never registers global providers or changes process environment variables.
package telemetry

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type Config struct {
	Enabled               bool     `json:"enabled"`
	Endpoint              string   `json:"endpoint"`
	AllowInsecure         bool     `json:"allow_insecure"`
	TraceSampleRatio      *float64 `json:"trace_sample_ratio"`
	ExportIntervalSeconds int      `json:"export_interval_seconds"`
}

// Runtime is safe to share. Its zero value disables telemetry.
type Runtime struct {
	tracer   trace.Tracer
	count    metric.Int64Counter
	duration metric.Float64Histogram
	stop     func(context.Context) error
	once     sync.Once
	stopErr  error
}

// New constructs explicit providers. Endpoint is an OTLP HTTP base URL;
// /v1/traces and /v1/metrics are appended to its path. Disabled telemetry
// constructs neither exporters nor background workers.
func New(ctx context.Context, service string, cfg Config) (*Runtime, error) {
	if !cfg.Enabled {
		return &Runtime{}, nil
	}
	u, ratio, interval, err := cfg.exportSettings(service)
	if err != nil {
		return nil, err
	}
	transport, traceExporter, metricExporter, err := newExporters(ctx, u)
	if err != nil {
		return nil, err
	}
	safeResource := resource.NewSchemaless(attribute.String("service.name", service))
	// SDK WithResource merges OTEL_RESOURCE_ATTRIBUTES even with an explicit
	// resource. Replace resources at the final export boundary as well.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(safeResource),
		sdktrace.WithoutPanicRecording(),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio),
			sdktrace.WithRemoteParentSampled(sdktrace.TraceIDRatioBased(ratio)),
			sdktrace.WithRemoteParentNotSampled(sdktrace.NeverSample()))),
		sdktrace.WithRawSpanLimits(
			sdktrace.SpanLimits{
				AttributeValueLengthLimit:   128,
				AttributeValueDepthLimit:    4,
				AttributeCountLimit:         16,
				EventCountLimit:             0,
				LinkCountLimit:              0,
				AttributePerEventCountLimit: 0,
				AttributePerLinkCountLimit:  0,
			},
		),
		sdktrace.WithBatcher(resourceTraceExporter{SpanExporter: traceExporter, resource: safeResource},
			sdktrace.WithMaxQueueSize(512), sdktrace.WithMaxExportBatchSize(128),
			sdktrace.WithBatchTimeout(time.Second), sdktrace.WithExportTimeout(5*time.Second)),
	)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(safeResource),
		sdkmetric.WithCardinalityLimit(256),
		sdkmetric.WithExemplarFilter(exemplar.AlwaysOffFilter),
		sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(resourceMetricExporter{Exporter: metricExporter, resource: safeResource},
				sdkmetric.WithInterval(time.Duration(interval)*time.Second), sdkmetric.WithTimeout(5*time.Second)),
		),
	)
	meter := mp.Meter("fry/telemetry")
	count, countErr := meter.Int64Counter("fry.operation.count")
	duration, durationErr := meter.Float64Histogram("fry.operation.duration", metric.WithUnit("s"))
	r := &Runtime{tracer: tp.Tracer("fry/telemetry"), count: count, duration: duration}
	r.stop = func(ctx context.Context) error {
		defer transport.CloseIdleConnections()
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}
	if err := errors.Join(countErr, durationErr); err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if cleanupErr := r.Shutdown(cleanup); cleanupErr != nil {
			return nil, errors.New("telemetry: construct operation instruments; provider cleanup failed")
		}
		return nil, errors.New("telemetry: construct operation instruments")
	}
	return r, nil
}

func newExporters(ctx context.Context, u *url.URL) (*http.Transport, sdktrace.SpanExporter, sdkmetric.Exporter, error) {
	transport := &http.Transport{
		DialContext:       (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, MaxIdleConns: 2, MaxIdleConnsPerHost: 2,
		IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: 3 * time.Second,
		ResponseHeaderTimeout: 3 * time.Second,
	}
	client := &http.Client{
		Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	signalURL := func(signal string) string {
		v := *u
		v.Path = strings.TrimRight(v.Path, "/") + "/v1/" + signal
		v.RawPath = ""
		return v.String()
	}
	traceExporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(signalURL("traces")),
		otlptracehttp.WithHeaders(map[string]string{}),
		otlptracehttp.WithCompression(otlptracehttp.NoCompression),
		otlptracehttp.WithEncoding(otlptracehttp.EncodingProtobuf),
		otlptracehttp.WithTLSClientConfig(nil), otlptracehttp.WithHTTPClient(client),
		otlptracehttp.WithTimeout(5*time.Second),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}),
		otlptracehttp.WithMaxRequestSize(1<<20), otlptracehttp.WithMaxResponseSize(64<<10))
	if err != nil {
		transport.CloseIdleConnections()
		return nil, nil, nil, errors.New("telemetry: construct trace exporter")
	}
	metricExporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(signalURL("metrics")),
		otlpmetrichttp.WithHeaders(map[string]string{}),
		otlpmetrichttp.WithCompression(otlpmetrichttp.NoCompression),
		otlpmetrichttp.WithTLSClientConfig(nil), otlpmetrichttp.WithHTTPClient(client),
		otlpmetrichttp.WithTimeout(5*time.Second),
		otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}),
		otlpmetrichttp.WithTemporalitySelector(sdkmetric.DefaultTemporalitySelector),
		otlpmetrichttp.WithAggregationSelector(sdkmetric.DefaultAggregationSelector),
		otlpmetrichttp.WithMaxRequestSize(1<<20), otlpmetrichttp.WithMaxResponseSize(64<<10))
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		cleanupErr := traceExporter.Shutdown(cleanup)
		transport.CloseIdleConnections()
		if cleanupErr != nil {
			return nil, nil, nil, errors.New("telemetry: construct metric exporter; trace cleanup failed")
		}
		return nil, nil, nil, errors.New("telemetry: construct metric exporter")
	}
	return transport, traceExporter, metricExporter, nil
}

func (cfg Config) endpointURL() (*url.URL, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || !validExportURL(u) {
		return nil, errors.New(
			"telemetry: endpoint must be an absolute HTTP(S) base URL without credentials, " +
				"query, or fragment",
		)
	}
	if u.Scheme == "http" && !cfg.AllowInsecure {
		return nil, errors.New("telemetry: HTTP export requires allow_insecure")
	}
	return u, nil
}

func validExportURL(u *url.URL) bool {
	return u != nil && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Opaque == "" &&
		(u.Scheme == "https" || u.Scheme == "http")
}

func (cfg Config) exportSettings(service string) (*url.URL, float64, int, error) {
	u, err := cfg.endpointURL()
	if err != nil {
		return nil, 0, 0, err
	}
	if !fixedName(service) {
		return nil, 0, 0, errors.New("telemetry: invalid service name")
	}
	ratio := 0.1
	if cfg.TraceSampleRatio != nil {
		ratio = *cfg.TraceSampleRatio
	}
	if !(ratio >= 0 && ratio <= 1) {
		return nil, 0, 0, errors.New("telemetry: trace sample ratio must be between zero and one")
	}
	interval := cfg.ExportIntervalSeconds
	if interval == 0 {
		interval = 30
	}
	if interval < 1 || interval > 300 {
		return nil, 0, 0, errors.New("telemetry: export interval must be between 1 and 300 seconds")
	}
	return u, ratio, interval, nil
}

// Shutdown releases the owned providers once; callers supply the shutdown budget.
func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		if r.stop != nil {
			r.stopErr = r.stop(ctx)
		}
	})
	return r.stopErr
}

// Start records a caller-owned fixed operation name. Never pass user input or
// identifiers as operation. Errors are classified; their strings are discarded.
func (r *Runtime) Start(ctx context.Context, operation string) (context.Context, func(error)) {
	return r.start(ctx, operation, nil)
}

func (r *Runtime) start(
	ctx context.Context,
	operation string,
	attrs []attribute.KeyValue,
	opts ...trace.SpanStartOption,
) (context.Context, func(error)) {
	if r == nil || r.tracer == nil {
		return ctx, func(error) {}
	}
	// Vendor tracestate is opaque header data and may contain private values.
	// Preserve trace identity and sampling flags while excluding that data from
	// every span and subsequent internal HTTP propagation.
	if parent := trace.SpanContextFromContext(ctx); parent.TraceState().Len() != 0 {
		ctx = trace.ContextWithSpanContext(ctx, parent.WithTraceState(trace.TraceState{}))
	}
	if !fixedName(operation) {
		operation = "operation"
	}
	// The finish callback retains these attributes. Own the backing array so
	// callers can reuse their input after start returns.
	attrs = slices.Clone(attrs)
	attrs = append(attrs, attribute.String("fry.operation", operation))
	opts = append(opts, trace.WithAttributes(attrs...))
	// Ownership transfers to the returned once-only finish callback, exercised by lifecycle tests.
	//nolint:spancheck // The caller owns completion through the returned callback.
	ctx, span := r.tracer.Start(ctx, operation, opts...)
	started := time.Now()
	var once sync.Once
	//nolint:spancheck // The caller owns completion; ending here would truncate the operation span.
	return ctx, func(err error) {
		once.Do(func() {
			outcome := "ok"
			if err != nil {
				outcome = "error"
				if errors.Is(err, context.Canceled) {
					outcome = "canceled"
				} else if errors.Is(err, context.DeadlineExceeded) {
					outcome = "timeout"
				}
				span.SetStatus(codes.Error, "")
			}
			attrs = append(attrs, attribute.String("fry.outcome", outcome))
			span.SetAttributes(attribute.String("fry.outcome", outcome))
			r.count.Add(ctx, 1, metric.WithAttributes(attrs...))
			r.duration.Record(ctx, time.Since(started).Seconds(), metric.WithAttributes(attrs...))
			span.End()
		})
	}
}

func fixedName(s string) bool {
	if s == "" || len(s) > 96 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.ContainsRune("._/-", c):
		default:
			return false
		}
	}
	return true
}

type runtimeKey struct{}

func WithRuntime(ctx context.Context, r *Runtime) context.Context {
	if r == nil {
		r = &Runtime{}
	}
	return context.WithValue(ctx, runtimeKey{}, r)
}

func FromContext(ctx context.Context) *Runtime {
	if r, ok := ctx.Value(runtimeKey{}).(*Runtime); ok && r != nil {
		return r
	}
	return &Runtime{}
}

type resourceTraceExporter struct {
	sdktrace.SpanExporter
	resource *resource.Resource
}

type resourceSpan struct {
	sdktrace.ReadOnlySpan
	resource *resource.Resource
}

func (s resourceSpan) Resource() *resource.Resource { return s.resource }

func (e resourceTraceExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	safe := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, span := range spans {
		safe[i] = resourceSpan{ReadOnlySpan: span, resource: e.resource}
	}
	return e.SpanExporter.ExportSpans(ctx, safe)
}

type resourceMetricExporter struct {
	sdkmetric.Exporter
	resource *resource.Resource
}

func (e resourceMetricExporter) Export(ctx context.Context, data *metricdata.ResourceMetrics) error {
	safe := *data
	safe.Resource = e.resource
	return e.Exporter.Export(ctx, &safe)
}

var errHTTP = errors.New("HTTP request failed")
