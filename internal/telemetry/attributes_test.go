package telemetry

import (
	"context"
	"slices"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestOperationOwnsAttributesUntilFinished(t *testing.T) {
	r, sink := unitRuntime(t, sdktrace.AlwaysSample())
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { requireNoError(t, provider.Shutdown(context.Background())) })
	var err error
	r.count, err = provider.Meter("test").Int64Counter("test.count")
	requireNoError(t, err)
	backing := []attribute.KeyValue{
		attribute.String("http.request.method", "GET"),
		attribute.String("caller.operation", "untouched"),
		attribute.String("caller.outcome", "untouched"),
	}
	original := slices.Clone(backing)
	_, finish := r.start(t.Context(), "test.operation", backing[:1])
	if !slices.Equal(backing, original) {
		t.Fatal("starting an operation overwrote the caller's backing array")
	}
	// The caller may immediately reuse its slice; deferred metrics must retain GET.
	backing[0] = attribute.String("http.request.method", "POST")
	original[0] = backing[0]
	finish(context.Canceled)
	finish(nil)
	if !slices.Equal(backing, original) {
		t.Fatal("finishing an operation overwrote the caller's backing array")
	}
	if len(sink.spans) != 1 {
		t.Fatalf("span completion count = %d, want 1", len(sink.spans))
	}
	var exported metricdata.ResourceMetrics
	requireNoError(t, reader.Collect(t.Context(), &exported))
	if len(exported.ScopeMetrics) != 1 || len(exported.ScopeMetrics[0].Metrics) != 1 {
		t.Fatalf("unexpected metric groups: %+v", exported.ScopeMetrics)
	}
	sum, ok := exported.ScopeMetrics[0].Metrics[0].Data.(metricdata.Sum[int64])
	if !ok || len(sum.DataPoints) != 1 {
		t.Fatalf("unexpected operation counter: %+v", exported.ScopeMetrics[0].Metrics[0])
	}
	point := sum.DataPoints[0]
	want := attribute.NewSet(
		attribute.String("http.request.method", "GET"),
		attribute.String("fry.operation", "test.operation"),
		attribute.String("fry.outcome", "canceled"),
	)
	if point.Value != 1 || !point.Attributes.Equals(&want) {
		t.Fatalf("completed count = %d, attributes = %v; want one canceled GET", point.Value, point.Attributes)
	}
}
