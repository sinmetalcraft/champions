package telemetry

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestStartAndEnd(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)

	ctx := context.Background()

	testFn := func(ctx context.Context, returnErr bool) (err error) {
		ctx, span := Start(ctx, "testOperation")
		defer End(span, &err)

		if returnErr {
			err = errors.New("something went wrong")
			return err
		}
		return nil
	}

	// 成功パターン
	if err := testFn(ctx, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 ended span, got %d", len(spans))
	}
	if spans[0].Name() != "testOperation" {
		t.Errorf("span name = %q, want testOperation", spans[0].Name())
	}
	if spans[0].Status().Code != codes.Unset {
		t.Errorf("status code = %v, want Unset", spans[0].Status().Code)
	}

	// エラーパターン
	if err := testFn(ctx, true); err == nil {
		t.Fatal("expected error, got nil")
	}

	spans = sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("expected 2 ended spans, got %d", len(spans))
	}
	if spans[1].Status().Code != codes.Error {
		t.Errorf("status code = %v, want Error", spans[1].Status().Code)
	}
	if len(spans[1].Events()) == 0 {
		t.Errorf("expected error event on span, got none")
	}
}

func TestInitDisabled(t *testing.T) {
	shutdown, err := Init(context.Background(), Config{
		Disabled: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("unexpected shutdown error: %v", err)
	}
}

func TestInitFallback(t *testing.T) {
	shutdown, err := Init(context.Background(), Config{
		ProjectID:   "test-project",
		ServiceName: "test-service",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown func should not be nil")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("unexpected shutdown error: %v", err)
	}
}

