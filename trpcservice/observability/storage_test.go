package observability

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestStorageSpanUsesOnlyRedactedLowCardinalityAttributes(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := trace.NewTracerProvider(trace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	ctx, finish := StartStorage(context.Background(), "session.commit", "postgres", "tenant-secret-value", "revision-1")
	if ctx == nil {
		t.Fatal("storage span did not return context")
	}
	finish(errors.New("provider response contains secret-value"))
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	for _, attr := range spans[0].Attributes() {
		if strings.Contains(attr.Value.AsString(), "tenant-secret-value") || strings.Contains(attr.Value.AsString(), "provider response") {
			t.Fatalf("sensitive value appeared in span attribute %s=%s", attr.Key, attr.Value.AsString())
		}
	}
}
