package tracer

import (
	"context"
	"log"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
)

func InitTracer(serviceName string, jaegerEndpoint string) func(context.Context) error {
	ctx := context.Background()

	// 1. 创建 Exporter (使用最简单的配置)
	// 注意：Insecure 是必须的，因为本地 Jaeger 没配 TLS
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(jaegerEndpoint),
		otlptracehttp.WithInsecure(),
		otlptracehttp.WithTimeout(5*time.Second), // 加个超时防止卡死
	)
	if err != nil {
		log.Fatalf("tracer exporter create failed: %v", err)
	}

	// 2. 简化资源定义 (只保留服务名，防止版本冲突)
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		// 如果资源创建失败，就用默认的，不要 Panic
		log.Printf("tracer resource create failed, using default resource: %v", err)
		res = resource.Default()
	}

	// 3. 创建 TracerProvider
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)

	// 4. 设置上下文传播 (必不可少)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	log.Printf("tracer ready service=%s endpoint=%s", serviceName, jaegerEndpoint)

	return tp.Shutdown
}
