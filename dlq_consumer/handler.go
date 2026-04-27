package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"

	"seckill-mall/common/tracer"
)

func validateMessage(msg OrderMessage) error {
	if msg.OrderID == "" {
		return fmt.Errorf("order_id is empty")
	}
	if msg.UserID <= 0 {
		return fmt.Errorf("user_id is invalid: %d", msg.UserID)
	}
	if msg.ProductID <= 0 {
		return fmt.Errorf("product_id is invalid: %d", msg.ProductID)
	}
	if msg.Count <= 0 {
		return fmt.Errorf("count is invalid: %d", msg.Count)
	}
	return nil
}

func handleMessage(d amqp.Delivery) {
	start := time.Now()
	defer func() {
		dlqCompensationDuration.Observe(time.Since(start).Seconds())
	}()

	traceCtx := tracer.ExtractAMQPHeaders(context.Background(), d.Headers)
	traceCtx, span := otel.Tracer("dlq-consumer").Start(traceCtx, "rabbitmq.consume_dlq")
	defer span.End()

	var msg OrderMessage
	if err := json.Unmarshal(d.Body, &msg); err != nil {
		log.Printf("dlq message invalid_json err=%v", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid dlq message json")
		dlqConsumeTotal.WithLabelValues("invalid").Inc()
		mqConsumerAckTotal.WithLabelValues(DeadQueue).Inc()
		d.Ack(false)
		return
	}

	if err := validateMessage(msg); err != nil {
		log.Printf("dlq message invalid_fields err=%v", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid dlq message fields")
		dlqConsumeTotal.WithLabelValues("invalid").Inc()
		mqConsumerAckTotal.WithLabelValues(DeadQueue).Inc()
		d.Ack(false)
		return
	}

	ctx, cancel := context.WithTimeout(traceCtx, 5*time.Second)
	defer cancel()

	order, exists, err := getOrder(ctx, msg.OrderID)
	if err != nil {
		log.Printf("dlq order query_failed order_id=%s err=%v", msg.OrderID, err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "query order failed")
		dlqConsumeTotal.WithLabelValues("retry").Inc()
		mqConsumerNackTotal.WithLabelValues(DeadQueue, "true").Inc()
		time.Sleep(3 * time.Second)
		d.Nack(false, true)
		return
	}

	if exists && order.Status == OrderStatusSuccess {
		log.Printf("dlq order already_success order_id=%s", msg.OrderID)
		span.SetStatus(codes.Ok, "order already exists")
		dlqConsumeTotal.WithLabelValues("order_exists").Inc()
		mqConsumerAckTotal.WithLabelValues(DeadQueue).Inc()
		d.Ack(false)
		return
	}
	if exists && order.Status == OrderStatusFailed {
		log.Printf("dlq order already_failed order_id=%s", msg.OrderID)
		span.SetStatus(codes.Ok, "order already failed")
		dlqConsumeTotal.WithLabelValues("already_failed").Inc()
		mqConsumerAckTotal.WithLabelValues(DeadQueue).Inc()
		d.Ack(false)
		return
	}
	if exists && order.Status != OrderStatusPending {
		log.Printf("dlq order unknown_status order_id=%s status=%d", msg.OrderID, order.Status)
		span.SetStatus(codes.Error, "unknown order status")
		dlqConsumeTotal.WithLabelValues("manual_check").Inc()
		mqConsumerAckTotal.WithLabelValues(DeadQueue).Inc()
		d.Ack(false)
		return
	}

	code, err := rollbackRedis(ctx, msg)
	if err != nil {
		log.Printf("dlq redis rollback_failed order_id=%s err=%v", msg.OrderID, err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "redis rollback failed")
		dlqConsumeTotal.WithLabelValues("retry").Inc()
		dlqCompensationTotal.WithLabelValues("retry").Inc()
		mqConsumerNackTotal.WithLabelValues(DeadQueue, "true").Inc()
		time.Sleep(3 * time.Second)
		d.Nack(false, true)
		return
	}

	handleRollbackResult(ctx, code, msg, d, span)
}
