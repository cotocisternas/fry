package telemetry

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/trace"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// This follows scripts/tests/receipts.sh's disposable PostgreSQL convention.
// It owns its pool and runs only SELECT; no application schema is needed.
func TestPostgreSQLExportsParentedPrivateQuery(t *testing.T) {
	dsn := os.Getenv("FRY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set FRY_TEST_DATABASE_URL or run scripts/tests/receipts.sh for real PostgreSQL tests")
	}
	const secret = "private-pg-query-argument-sentinel@example.invalid"
	capture := &pgTelemetryCapture{t: t, forbidden: []string{secret, dsn, "PrivateTelemetryQuery"}}
	collector := httptest.NewServer(http.HandlerFunc(capture.serve))
	defer collector.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ratio := 1.0
	r, err := New(
		ctx,
		"fry-pg-test",
		Config{Enabled: true, Endpoint: collector.URL, AllowInsecure: true, TraceSampleRatio: &ratio},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		requireNoError(t, r.Shutdown(cleanup))
	}()
	pool := telemetryPool(t, ctx, r, dsn)
	defer pool.Close()
	queryCtx, finish := r.Start(ctx, "test.postgresql")
	parent := trace.SpanFromContext(queryCtx).SpanContext()
	var got string
	err = pool.QueryRow(queryCtx, "-- name: PrivateTelemetryQuery :one\nSELECT $1::text", secret).Scan(&got)
	finish(err)
	if err != nil {
		t.Fatal(err)
	}
	if got != secret {
		t.Fatal("parameterized SELECT changed behavior")
	}
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := r.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	requirePostgresExport(t, parent, capture.spans, capture.foundMetric)
}

func telemetryPool(t *testing.T, ctx context.Context, r *Runtime, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MinConns, cfg.MaxConns = 0, 1
	if err := r.ConfigurePool(cfg); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

type pgTelemetryCapture struct {
	t           *testing.T
	mu          sync.Mutex
	forbidden   []string
	spans       []*tracepb.Span
	foundMetric bool
}

func (c *pgTelemetryCapture) serve(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(io.LimitReader(req.Body, 2<<20))
	if err != nil {
		c.t.Error(err)
		return
	}
	for _, secret := range c.forbidden {
		if bytes.Contains(body, []byte(secret)) {
			c.t.Error("telemetry leaked SQL, parameter, or DSN")
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch req.URL.Path {
	case "/v1/traces":
		c.captureTraces(body)
	case "/v1/metrics":
		c.captureMetrics(body)
	default:
		c.t.Errorf("unexpected signal path %q", req.URL.Path)
	}
	w.WriteHeader(http.StatusOK)
}

func (c *pgTelemetryCapture) captureTraces(body []byte) {
	var export collectortrace.ExportTraceServiceRequest
	if err := proto.Unmarshal(body, &export); err != nil {
		c.t.Error(err)
	}
	for _, rs := range export.ResourceSpans {
		for _, scope := range rs.ScopeSpans {
			c.spans = append(c.spans, scope.Spans...)
		}
	}
}

func (c *pgTelemetryCapture) captureMetrics(body []byte) {
	var export collectormetrics.ExportMetricsServiceRequest
	if err := proto.Unmarshal(body, &export); err != nil {
		c.t.Error(err)
	}
	for _, rm := range export.ResourceMetrics {
		for _, scope := range rm.ScopeMetrics {
			c.foundMetric = c.foundMetric || hasPostgresCount(scope.Metrics)
		}
	}
}

func hasPostgresCount(metrics []*metricspb.Metric) bool {
	for _, metric := range metrics {
		if metric.Name != "fry.operation.count" || metric.GetSum() == nil {
			continue
		}
		for _, point := range metric.GetSum().DataPoints {
			if isPostgresCount(point) {
				return true
			}
		}
	}
	return false
}

func isPostgresCount(point *metricspb.NumberDataPoint) bool {
	if point.GetAsInt() != 1 {
		return false
	}
	for _, attr := range point.Attributes {
		if attr.Key == "fry.operation" && attr.Value.GetStringValue() == "postgresql.select" {
			return true
		}
	}
	return false
}

func requirePostgresExport(t *testing.T, parent trace.SpanContext, spans []*tracepb.Span, foundMetric bool) {
	t.Helper()
	foundSpan := false
	parentID, traceID := parent.SpanID(), parent.TraceID()
	for _, span := range spans {
		if span.Name != "postgresql.select" {
			continue
		}
		foundSpan = true
		if !bytes.Equal(span.ParentSpanId, parentID[:]) || !bytes.Equal(span.TraceId, traceID[:]) ||
			span.Kind != tracepb.Span_SPAN_KIND_CLIENT {
			t.Error("database span lost caller parent or client kind")
		}
		requirePrivateDatabaseAttributes(t, span)
	}
	if !foundSpan || !foundMetric {
		t.Fatalf("missing PostgreSQL telemetry: span=%v metric=%v", foundSpan, foundMetric)
	}
}

func requirePrivateDatabaseAttributes(t *testing.T, span *tracepb.Span) {
	t.Helper()
	for _, attr := range span.Attributes {
		if attr.Key == "db.query.text" || attr.Key == "db.statement" || attr.Key == "db.connection_string" {
			t.Errorf("unexpected database attribute %q", attr.Key)
		}
	}
}

func TestExporterOutageDoesNotBlockOperations(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	collector := blockedCollector(t, entered, release)
	defer collector.Close()
	defer close(release)
	ratio := 1.0
	r, err := New(
		context.Background(),
		"fry-outage-test",
		Config{Enabled: true, Endpoint: collector.URL, AllowInsecure: true, TraceSampleRatio: &ratio},
	)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	for range 1024 {
		_, finish := r.Start(context.Background(), "worker.iteration")
		finish(nil)
	}
	if time.Since(started) > time.Second {
		t.Fatal("application operations waited for unavailable collector")
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("exporter did not attempt export")
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started = time.Now()
	if err := r.Shutdown(shutdown); err == nil {
		t.Fatal("unavailable exporter shutdown unexpectedly succeeded")
	}
	if time.Since(started) > time.Second {
		t.Fatal("shutdown exceeded caller budget")
	}
}

func blockedCollector(t *testing.T, entered chan<- struct{}, release <-chan struct{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			w.WriteHeader(http.StatusServiceUnavailable)
		case <-req.Context().Done():
		}
	}))
}
