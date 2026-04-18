package tracer

import (
	"context"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
)

type AMQPHeaderCarrier amqp.Table

func (c AMQPHeaderCarrier) Get(key string) string {
	value, ok := c[key]
	if !ok {
		return ""
	}

	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}

func (c AMQPHeaderCarrier) Set(key string, value string) {
	c[key] = value
}

func (c AMQPHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for key := range c {
		keys = append(keys, key)
	}
	return keys
}

func InjectAMQPHeaders(ctx context.Context, headers amqp.Table) amqp.Table {
	if headers == nil {
		headers = amqp.Table{}
	}
	otel.GetTextMapPropagator().Inject(ctx, AMQPHeaderCarrier(headers))
	return headers
}

func ExtractAMQPHeaders(ctx context.Context, headers amqp.Table) context.Context {
	if headers == nil {
		headers = amqp.Table{}
	}
	return otel.GetTextMapPropagator().Extract(ctx, AMQPHeaderCarrier(headers))
}
