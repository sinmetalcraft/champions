package logx

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestParseTrace(t *testing.T) {
	tests := []struct {
		name        string
		headers     map[string]string
		wantTraceID string
		wantSpanID  string
		wantSampled bool
		wantNil     bool
	}{
		{
			name: "X-Cloud-Trace-Context with span and sampled",
			headers: map[string]string{
				"X-Cloud-Trace-Context": "105445aa7843bc8bf206b120001000/1;o=1",
			},
			wantTraceID: "105445aa7843bc8bf206b120001000",
			wantSpanID:  "0000000000000001",
			wantSampled: true,
		},
		{
			name: "X-Cloud-Trace-Context without span and sampled false",
			headers: map[string]string{
				"X-Cloud-Trace-Context": "105445aa7843bc8bf206b120001000;o=0",
			},
			wantTraceID: "105445aa7843bc8bf206b120001000",
			wantSpanID:  "",
			wantSampled: false,
		},
		{
			name: "traceparent W3C",
			headers: map[string]string{
				"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			},
			wantTraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
			wantSpanID:  "00f067aa0ba902b7",
			wantSampled: true,
		},
		{
			name:    "no trace header",
			headers: map[string]string{},
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/test", nil)
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			got := ParseTrace(r)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil, got nil")
			}
			if got.TraceID != tt.wantTraceID {
				t.Errorf("TraceID = %q, want %q", got.TraceID, tt.wantTraceID)
			}
			if got.SpanID != tt.wantSpanID {
				t.Errorf("SpanID = %q, want %q", got.SpanID, tt.wantSpanID)
			}
			if got.Sampled != tt.wantSampled {
				t.Errorf("Sampled = %v, want %v", got.Sampled, tt.wantSampled)
			}
		})
	}
}

func TestTraceMiddlewareAndHandler(t *testing.T) {
	buf := new(bytes.Buffer)
	jsonHandler := slog.NewJSONHandler(buf, nil)
	logger := slog.New(NewHandler(jsonHandler, "my-gcp-project"))

	var capturedTrace *TraceInfo
	handler := TraceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedTrace = FromContext(r.Context())
		logger.InfoContext(r.Context(), "hello world", "key", "value")
	}))

	r := httptest.NewRequest(http.MethodGet, "/test", nil)
	r.Header.Set("X-Cloud-Trace-Context", "105445aa7843bc8bf206b120001000/12345;o=1")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, r)

	if capturedTrace == nil {
		t.Fatal("expected trace info in context, got nil")
	}
	if capturedTrace.TraceID != "105445aa7843bc8bf206b120001000" {
		t.Errorf("TraceID = %q, want 105445aa7843bc8bf206b120001000", capturedTrace.TraceID)
	}

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to unmarshal log: %v\noutput: %s", err, buf.String())
	}

	wantTrace := "projects/my-gcp-project/traces/105445aa7843bc8bf206b120001000"
	if gotTrace, ok := entry["logging.googleapis.com/trace"].(string); !ok || gotTrace != wantTrace {
		t.Errorf("logging.googleapis.com/trace = %v, want %q", entry["logging.googleapis.com/trace"], wantTrace)
	}

	wantSpan := "0000000000003039" // 12345 in hex is 0x3039
	if gotSpan, ok := entry["logging.googleapis.com/spanId"].(string); !ok || gotSpan != wantSpan {
		t.Errorf("logging.googleapis.com/spanId = %v, want %q", entry["logging.googleapis.com/spanId"], wantSpan)
	}

	if gotSampled, ok := entry["logging.googleapis.com/trace_sampled"].(bool); !ok || !gotSampled {
		t.Errorf("logging.googleapis.com/trace_sampled = %v, want true", entry["logging.googleapis.com/trace_sampled"])
	}
}

func TestHandlerWithoutTrace(t *testing.T) {
	buf := new(bytes.Buffer)
	jsonHandler := slog.NewJSONHandler(buf, nil)
	logger := slog.New(NewHandler(jsonHandler, "my-gcp-project"))

	logger.InfoContext(context.Background(), "no trace message")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to unmarshal log: %v\noutput: %s", err, buf.String())
	}

	if _, ok := entry["logging.googleapis.com/trace"]; ok {
		t.Errorf("logging.googleapis.com/trace should not exist, got %v", entry["logging.googleapis.com/trace"])
	}
}

func TestHandlerWithOTelSpan(t *testing.T) {
	buf := new(bytes.Buffer)
	jsonHandler := slog.NewJSONHandler(buf, nil)
	logger := slog.New(NewHandler(jsonHandler, "my-gcp-project"))

	// OTel TracerProvider を作成
	tp := sdktrace.NewTracerProvider()
	tracer := tp.Tracer("test")

	ctx, span := tracer.Start(context.Background(), "my-operation")
	defer span.End()

	logger.InfoContext(ctx, "otel trace message")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to unmarshal log: %v\noutput: %s", err, buf.String())
	}

	sc := span.SpanContext()
	wantTrace := "projects/my-gcp-project/traces/" + sc.TraceID().String()
	if gotTrace, ok := entry["logging.googleapis.com/trace"].(string); !ok || gotTrace != wantTrace {
		t.Errorf("logging.googleapis.com/trace = %v, want %q", entry["logging.googleapis.com/trace"], wantTrace)
	}

	wantSpan := sc.SpanID().String()
	if gotSpan, ok := entry["logging.googleapis.com/spanId"].(string); !ok || gotSpan != wantSpan {
		t.Errorf("logging.googleapis.com/spanId = %v, want %q", entry["logging.googleapis.com/spanId"], wantSpan)
	}
}

