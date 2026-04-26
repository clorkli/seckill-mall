package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"

	"seckill-mall/common/tracer"
)

func processEvent(ctx context.Context, publisher *MQPublisher, event OutboxEvent) {
	start := time.Now()
	processResult := "unknown"
	defer func() {
		outboxProcessDuration.WithLabelValues(processResult).Observe(time.Since(start).Seconds())
	}()

	order, exists, err := getOrder(ctx, event.AggregateID)
	if err != nil {
		log.Printf("查询订单失败，延迟重试Outbox事件: event_id=%s err=%v", event.EventID, err)
		processResult = "query_order_failed"
		scheduleRetry(ctx, event, err)
		return
	}
	if !exists {
		processResult = "order_missing"
		markEventFailed(ctx, event, "订单不存在，Outbox事件终止")
		return
	}
	if order.Status == OrderStatusSuccess {
		processResult = "order_success"
		if err := markEventSent(ctx, event.EventID); err != nil {
			log.Printf("订单已成功但标记Outbox已发送失败: event_id=%s err=%v", event.EventID, err)
			processResult = "mark_sent_failed"
		}
		return
	}
	if order.Status == OrderStatusFailed {
		processResult = "order_failed"
		markEventFailed(ctx, event, "订单已失败，Outbox事件终止")
		return
	}

	msg, err := parseOrderMessage(event.Payload)
	if err != nil {
		log.Printf("Outbox事件payload非法，终止事件: event_id=%s err=%v", event.EventID, err)
		processResult = "invalid_payload"
		markEventAndOrderFailed(ctx, event, "Outbox事件payload非法: "+err.Error())
		return
	}

	headers, err := parseHeaders(event.Headers)
	if err != nil {
		log.Printf("Outbox事件headers非法，将不带父trace继续发布: event_id=%s err=%v", event.EventID, err)
		headers = amqp.Table{}
	}

	traceCtx := tracer.ExtractAMQPHeaders(ctx, headers)
	traceCtx, span := otel.Tracer("outbox-worker").Start(traceCtx, "outbox.publish_order")
	defer span.End()
	headers = tracer.InjectAMQPHeaders(traceCtx, headers)

	publishCtx, cancel := context.WithTimeout(traceCtx, 5*time.Second)
	defer cancel()

	publishStart := time.Now()
	if err := publisher.Publish(publishCtx, []byte(event.Payload), headers); err != nil {
		log.Printf("Outbox事件发布失败: event_id=%s retry_count=%d err=%v", event.EventID, event.RetryCount, err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "outbox publish failed")
		processResult = "publish_failed"
		outboxPublishTotal.WithLabelValues("failed").Inc()
		outboxPublishDuration.WithLabelValues("failed").Observe(time.Since(publishStart).Seconds())
		handlePublishFailure(ctx, event, msg, err)
		return
	}
	outboxPublishTotal.WithLabelValues("success").Inc()
	outboxPublishDuration.WithLabelValues("success").Observe(time.Since(publishStart).Seconds())

	if err := markEventSent(ctx, event.EventID); err != nil {
		log.Printf("Outbox事件已发布但标记Sent失败，后续可能重复投递: event_id=%s err=%v", event.EventID, err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "mark outbox sent failed")
		processResult = "mark_sent_failed"
		return
	}

	span.SetStatus(codes.Ok, "outbox event published")
	processResult = "published"
	log.Printf("Outbox事件发布成功: event_id=%s order_id=%s", event.EventID, msg.OrderID)
}

func parseOrderMessage(payload string) (OrderMessage, error) {
	var msg OrderMessage
	if err := json.Unmarshal([]byte(payload), &msg); err != nil {
		return msg, err
	}
	if msg.OrderID == "" {
		return msg, errors.New("order_id为空")
	}
	if msg.UserID <= 0 {
		return msg, fmt.Errorf("user_id非法: %d", msg.UserID)
	}
	if msg.ProductID <= 0 {
		return msg, fmt.Errorf("product_id非法: %d", msg.ProductID)
	}
	if msg.Count <= 0 {
		return msg, fmt.Errorf("count非法: %d", msg.Count)
	}
	return msg, nil
}

func parseHeaders(raw string) (amqp.Table, error) {
	if raw == "" {
		return amqp.Table{}, nil
	}

	values := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, err
	}

	headers := amqp.Table{}
	for key, value := range values {
		headers[key] = value
	}
	return headers, nil
}

func handlePublishFailure(ctx context.Context, event OutboxEvent, msg OrderMessage, publishErr error) {
	nextRetryCount := event.RetryCount + 1
	if nextRetryCount < MaxRetryCount {
		outboxRetryTotal.WithLabelValues("publish").Inc()
		if err := scheduleRetryWithCount(ctx, event, nextRetryCount, publishErr); err != nil {
			log.Printf("安排Outbox重试失败: event_id=%s err=%v", event.EventID, err)
		}
		return
	}

	reason, err := compensateRedis(ctx, msg)
	if err != nil {
		outboxCompensationTotal.WithLabelValues("failed").Inc()
		outboxRetryTotal.WithLabelValues("compensation").Inc()
		combinedErr := fmt.Errorf("MQ发布达到最大重试且Redis补偿失败: publish_err=%v compensation_err=%w", publishErr, err)
		if err := scheduleRetryWithCount(ctx, event, nextRetryCount, combinedErr); err != nil {
			log.Printf("安排Outbox补偿重试失败: event_id=%s err=%v", event.EventID, err)
		}
		return
	}
	outboxCompensationTotal.WithLabelValues("success").Inc()

	if err := markEventAndOrderFailed(ctx, event, reason); err != nil {
		log.Printf("标记Outbox和订单失败状态失败，稍后重试: event_id=%s err=%v", event.EventID, err)
		outboxRetryTotal.WithLabelValues("mark_failed").Inc()
		if errRetry := scheduleRetryWithCount(ctx, event, nextRetryCount, err); errRetry != nil {
			log.Printf("安排Outbox状态重试失败: event_id=%s err=%v", event.EventID, errRetry)
		}
		return
	}

	log.Printf("Outbox事件超过最大重试，已补偿Redis并标记订单失败: event_id=%s order_id=%s", event.EventID, msg.OrderID)
}

func runWorker(ctx context.Context, publisher *MQPublisher) {
	ticker := time.NewTicker(ScanInterval)
	defer ticker.Stop()

	for {
		events, err := claimPendingEvents(ctx, BatchSize)
		if err != nil {
			log.Printf("扫描Outbox事件失败: %v", err)
			outboxScanTotal.WithLabelValues("failed").Inc()
		} else {
			outboxScanTotal.WithLabelValues("success").Inc()
			outboxClaimedTotal.Add(float64(len(events)))
		}
		updateOutboxPendingGauge(ctx)
		for _, event := range events {
			processEvent(ctx, publisher, event)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
