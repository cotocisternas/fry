package telemetry

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestExportPrivacyAndEnvironmentIsolation(t *testing.T) {
	const secret = "private-mail-address-and-receipt-sentinel"
	var mu sync.Mutex
	var paths []string
	var payloads []string
	collector := privacyCollector(t, secret, &mu, &paths, &payloads)
	defer collector.Close()
	setPrivateTelemetryEnvironment(t, secret)
	beforeTrace, beforeMetric := otel.GetTracerProvider(), otel.GetMeterProvider()
	ratio := 1.0
	r, err := New(
		context.Background(),
		"fry-test",
		Config{Enabled: true, Endpoint: collector.URL + "/collector", AllowInsecure: true, TraceSampleRatio: &ratio},
	)
	if err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() != beforeTrace || otel.GetMeterProvider() != beforeMetric {
		t.Fatal("runtime changed global providers")
	}
	ctx, finish := r.Start(context.Background(), "worker.process")
	q := queryTracer{runtime: r}
	queryCtx := q.TraceQueryStart(
		ctx,
		nil,
		pgx.TraceQueryStartData{SQL: "-- name: PrivateQuery :one\nSELECT '" + secret + "'", Args: []any{secret}},
	)
	q.TraceQueryEnd(queryCtx, nil, pgx.TraceQueryEndData{Err: errors.New(secret)})
	finish(errors.New(secret))
	finish(nil)
	exercisePrivateHTTPRequests(t, r, secret)
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	if err := r.Shutdown(shutdown); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	requirePrivateExports(t, paths, payloads, secret)
}

func setPrivateTelemetryEnvironment(t *testing.T, secret string) {
	t.Helper()
	for key, value := range map[string]string{
		"OTEL_RESOURCE_ATTRIBUTES":            "mail.secret=" + secret,
		"OTEL_EXPORTER_OTLP_HEADERS":          "Authorization=" + secret,
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS":   "X-Secret=" + secret,
		"OTEL_EXPORTER_OTLP_METRICS_HEADERS":  "X-Secret=" + secret,
		"OTEL_EXPORTER_OTLP_ENDPOINT":         "http://127.0.0.1:1/" + secret,
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":  "http://127.0.0.1:1/" + secret,
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://127.0.0.1:1/" + secret,
		"OTEL_EXPORTER_OTLP_COMPRESSION":      "gzip",
		"OTEL_TRACES_SAMPLER":                 "always_off",
		"OTEL_METRIC_EXPORT_INTERVAL":         "999999",
	} {
		t.Setenv(key, value)
	}
}

func requirePrivateExports(t *testing.T, paths, payloads []string, secret string) {
	t.Helper()
	foundTrace, foundMetric := false, false
	for i, path := range paths {
		if strings.Contains(payloads[i], secret) {
			t.Error("export contains private sentinel")
		}
		switch path {
		case "/collector/v1/traces":
			foundTrace = true
			if !strings.Contains(payloads[i], "postgresql.select") || !strings.Contains(payloads[i], "admin.request") {
				t.Error("trace export missing expected operations")
			}
		case "/collector/v1/metrics":
			foundMetric = true
		default:
			t.Errorf("unexpected export path %q", path)
		}
	}
	if !foundTrace || !foundMetric {
		t.Fatalf("missing signal: traces=%v metrics=%v", foundTrace, foundMetric)
	}
}

func privacyCollector(t *testing.T, secret string, mu *sync.Mutex, paths, payloads *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(io.LimitReader(req.Body, 2<<20))
		if err != nil {
			t.Error(err)
		}
		if strings.Contains(req.Header.Get("Authorization"), secret) ||
			strings.Contains(req.Header.Get("X-Secret"), secret) {
			t.Error("export inherited environment headers")
		}
		if req.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Errorf("content type = %q", req.Header.Get("Content-Type"))
		}
		mu.Lock()
		*paths = append(*paths, req.URL.Path)
		*payloads = append(*payloads, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
}

func TestDisabledAndConfigValidation(t *testing.T) {
	ctx := context.Background()
	r, err := New(ctx, "", Config{Endpoint: ":invalid"})
	if err != nil {
		t.Fatal(err)
	}
	if got, finish := r.Start(ctx, "operation"); got != ctx {
		t.Error("disabled context changed")
	} else {
		finish(errors.New("private"))
	}
	if err := r.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	var absent *Runtime
	_, finish := absent.Start(ctx, "operation")
	finish(nil)
	if FromContext(WithRuntime(ctx, nil)) == nil || FromContext(ctx) == nil {
		t.Fatal("missing no-op runtime")
	}
	nan := math.NaN()
	for _, cfg := range []Config{
		{Enabled: true},
		{Enabled: true, Endpoint: "ftp://collector"},
		// G101: synthetic URL tests rejection of credentials; owner FRY maintainers, review by 2027-01-18.
		//nolint:gosec // Synthetic credentials exercise URL validation.
		{Enabled: true, Endpoint: "https://user:secret@collector"},
		{Enabled: true, Endpoint: "https://collector?secret=x"},
		{Enabled: true, Endpoint: "http://collector"},
		{Enabled: true, Endpoint: "https://collector", TraceSampleRatio: &nan},
		{Enabled: true, Endpoint: "https://collector", ExportIntervalSeconds: 301},
	} {
		if runtime, err := New(ctx, "fry-test", cfg); err == nil {
			requireNoError(t, runtime.Shutdown(ctx))
			t.Errorf("accepted invalid config: %+v", cfg)
		}
	}
}

func TestSQLVerb(t *testing.T) {
	for sql, want := range map[string]string{
		"-- name: Lookup :one\n SELECT secret":   "SELECT",
		"/* outer /* nested */ end */\nupdate x": "UPDATE",
		"\n\tWITH x AS (SELECT 1) SELECT 1":      "WITH",
		"/* unfinished":                          "OTHER",
		"-- comment":                             "OTHER",
		"private_customer_identifier":            "OTHER",
		"":                                       "OTHER",
	} {
		if got := sqlVerb(sql); got != want {
			t.Errorf("sqlVerb(%q)=%q want %q", sql, got, want)
		}
	}
}

func TestPoolPreservesExistingTracer(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://localhost/example")
	if err != nil {
		t.Fatal(err)
	}
	r := &Runtime{tracer: tracenoop.NewTracerProvider().Tracer("test")}
	if err := r.ConfigurePool(cfg); err != nil {
		t.Fatal(err)
	}
	existing := cfg.ConnConfig.Tracer
	if err := r.ConfigurePool(cfg); err == nil || cfg.ConnConfig.Tracer != existing {
		t.Fatal("existing tracer was replaced")
	}
}

func TestBodyEndsExactlyOnce(t *testing.T) {
	var ended int
	body := &tracedBody{ReadCloser: io.NopCloser(strings.NewReader("stream")), finish: func(err error) {
		if err != nil {
			t.Errorf("finish error: %v", err)
		}
		ended++
	}}
	if ended != 0 {
		t.Fatal("ended before consumption")
	}
	data, err := io.ReadAll(body)
	if err != nil || string(data) != "stream" {
		t.Fatalf("read = %q, %v", data, err)
	}
	requireNoError(t, body.Close())
	requireNoError(t, body.Close())
	if ended != 1 {
		t.Fatalf("finish calls = %d", ended)
	}
}

type spanSink struct{ spans []sdktrace.ReadOnlySpan }

func (s *spanSink) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	s.spans = append(s.spans, spans...)
	return nil
}
func (*spanSink) Shutdown(context.Context) error { return nil }

func unitRuntime(t *testing.T, sampler sdktrace.Sampler) (*Runtime, *spanSink) {
	t.Helper()
	sink := &spanSink{}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sampler),
		sdktrace.WithSyncer(sink),
		sdktrace.WithoutPanicRecording(),
	)
	t.Cleanup(func() { requireNoError(t, tp.Shutdown(context.Background())) })
	m := metricnoop.NewMeterProvider().Meter("test")
	count, err := m.Int64Counter("test.count")
	requireNoError(t, err)
	duration, err := m.Float64Histogram("test.duration")
	requireNoError(t, err)
	return &Runtime{tracer: tp.Tracer("test"), count: count, duration: duration}, sink
}

func TestTransportClonesAndEndsAfterBody(t *testing.T) {
	r, sink := unitRuntime(t, sdktrace.AlwaysSample())
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.invalid/private", nil)
	traceState, err := trace.ParseTraceState("vendor=private-sentinel")
	if err != nil {
		t.Fatal(err)
	}
	traceID, err := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	requireNoError(t, err)
	spanID, err := trace.SpanIDFromHex("0123456789abcdef")
	requireNoError(t, err)
	request = request.WithContext(
		trace.ContextWithSpanContext(request.Context(), trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled, TraceState: traceState,
		})),
	)
	request.Header.Set("Baggage", "private=value")
	request.Header.Set("Traceparent", "original")
	base := transportFunc(func(got *http.Request) (*http.Response, error) {
		requireTraceOnlyClone(t, request, got)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("stream"))}, nil
	})
	response, err := r.HTTPTransport("replica.fetch", base).RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.spans) != 0 {
		t.Fatal("span ended before stream consumption")
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	requireNoError(t, response.Body.Close())
	if len(sink.spans) != 1 {
		t.Fatalf("exported spans = %d", len(sink.spans))
	}
	if sink.spans[0].SpanContext().TraceState().Len() != 0 || sink.spans[0].Parent().TraceState().Len() != 0 {
		t.Fatal("ambient vendor tracestate was retained")
	}
	if request.Header.Get("Baggage") != "private=value" || request.Header.Get("Traceparent") != "original" {
		t.Fatal("original request headers mutated")
	}
}

func requireTraceOnlyClone(t *testing.T, request, got *http.Request) {
	t.Helper()
	if got == request || got.Header.Get("Baggage") != "" || got.Header.Get("Tracestate") != "" ||
		got.Header.Get("Traceparent") == "" ||
		got.Header.Get("Traceparent") == "original" {
		t.Fatal("request not independently cloned with trace-only propagation")
	}
}

func TestHTTPPublicRootAndInternalParent(t *testing.T) {
	r, sink := unitRuntime(t, sdktrace.AlwaysSample())
	parentID := "0123456789abcdef0123456789abcdef"
	parentSpan := "0123456789abcdef"
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.invalid/private", nil)
	request.Header.Set("Traceparent", "00-"+parentID+"-"+parentSpan+"-01")
	request.Header.Set("Tracestate", "vendor=private-sentinel")
	request.Header.Set("Baggage", "private=value")
	next := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !trace.SpanFromContext(req.Context()).SpanContext().IsValid() {
			t.Error("missing server span")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	r.HTTPHandler("public.request", next).ServeHTTP(httptest.NewRecorder(), request)
	r.InternalHTTPHandler("internal.request", next).ServeHTTP(httptest.NewRecorder(), request)
	if len(sink.spans) != 2 || sink.spans[0].Parent().IsValid() ||
		sink.spans[1].Parent().TraceID().String() != parentID ||
		sink.spans[1].Parent().SpanID().String() != parentSpan {
		t.Fatal("public root or internal parent handling failed")
	}
	for _, span := range sink.spans {
		if span.SpanContext().TraceState().Len() != 0 || span.Parent().TraceState().Len() != 0 {
			t.Fatal("incoming vendor tracestate was retained")
		}
	}
}

func TestHTTPPanicRetainsPanicAndClosesSpan(t *testing.T) {
	r, sink := unitRuntime(t, sdktrace.AlwaysSample())
	func() {
		defer func() {
			if got := recover(); got != "private-panic" {
				t.Errorf("panic = %v", got)
			}
		}()
		r.HTTPHandler(
			"public.request",
			http.HandlerFunc(func(
				http.ResponseWriter,
				*http.Request,
			) {
				panic("private-panic")
			}),
		).
			ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	}()
	if len(sink.spans) != 1 || len(sink.spans[0].Events()) != 0 {
		t.Fatal("panic span missing or panic details recorded")
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Error(err)
	}
}

func exercisePrivateHTTPRequests(t *testing.T, r *Runtime, secret string) {
	t.Helper()
	handler := r.HTTPHandler("admin.request", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if FromContext(req.Context()) != r {
			t.Error("runtime missing from handler context")
		}
		if _, ok := w.(http.Flusher); !ok {
			t.Error("Flusher lost")
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	request := httptest.NewRequestWithContext(
		t.Context(),
		"SECRET-METHOD-"+secret,
		"http://example.invalid/"+secret+"?token="+secret,
		nil,
	)
	request.Header.Set("Authorization", secret)
	request.Header.Set("Traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
	request.Header.Set("Tracestate", "vendor="+secret)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	r.InternalHTTPHandler("replica.request", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), request)
}
