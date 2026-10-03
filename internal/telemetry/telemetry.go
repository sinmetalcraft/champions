// Package telemetry は OpenTelemetry と Google Cloud Trace の統合を提供する。
package telemetry

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/GoogleCloudPlatform/opentelemetry-operations-go/propagator"
	"go.opentelemetry.io/contrib/detectors/gcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/oauth"
)

const instrumentationName = "github.com/sinmetalcraft/champions"

var tracer = otel.Tracer(instrumentationName)

// Config はテレメトリの初期化設定。
type Config struct {
	ProjectID   string
	ServiceName string
	Disabled    bool
}

// Init は OpenTelemetry の TracerProvider と Propagator を初期化する。
// Cloud Trace にトレースを出力する OTLP エクスポーターを設定する。
// ADC が取得できない環境（ローカルなど）ではインメモリのフォールバックを行い、エラーで終了しない。
func Init(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	// Propagator は W3C Trace Context, Baggage, および Cloud Trace (X-Cloud-Trace-Context) をサポート
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
		propagator.CloudTraceOneWayPropagator{},
	))

	if cfg.Disabled {
		return func(context.Context) error { return nil }, nil
	}

	creds, err := oauth.NewApplicationDefault(ctx, "https://www.googleapis.com/auth/trace.append")
	if err != nil {
		slog.WarnContext(ctx, "failed to get Application Default Credentials for Cloud Trace. tracing will be in-memory only", "error", err)
		tp := sdktrace.NewTracerProvider()
		otel.SetTracerProvider(tp)
		return tp.Shutdown, nil
	}

	res, err := resource.New(
		ctx,
		resource.WithDetectors(gcp.NewDetector()),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.ServiceName),
			attribute.String("gcp.project_id", cfg.ProjectID),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: failed to create resource: %w", err)
	}

	exporter, err := otlptracegrpc.New(
		ctx,
		otlptracegrpc.WithEndpoint("telemetry.googleapis.com:443"),
		otlptracegrpc.WithDialOption(grpc.WithPerRPCCredentials(creds)),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: failed to create OTLP trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)
	otel.SetTracerProvider(tp)

	return tp.Shutdown, nil
}

// Start は指定された名前で新しい Span を開始する。
// ctx から親 Span を引き継ぎ、新しい ctx と Span を返す。
func Start(ctx context.Context, spanName string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return tracer.Start(ctx, spanName, opts...)
}

// End は Span を終了し、err が非 nil の場合は Span にエラーを記録してステータスを Error に設定する。
// 使い方:
//
//	func Foo(ctx context.Context) (err error) {
//	    ctx, span := telemetry.Start(ctx, "Foo")
//	    defer telemetry.End(span, &err)
//	    ...
//	}
func End(span trace.Span, err *error) {
	if err != nil && *err != nil {
		span.RecordError(*err)
		span.SetStatus(codes.Error, (*err).Error())
	}
	span.End()
}
