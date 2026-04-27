package main

import (
	"context"
	"encoding/json"
	"time"

	"gorm.io/gorm"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

func orderCreatedEventID(orderID string) string {
	return "order.created:" + orderID
}

func marshalTraceHeaders(ctx context.Context) string {
	headers := map[string]string{}
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(headers))

	body, err := json.Marshal(headers)
	if err != nil {
		return "{}"
	}
	return string(body)
}

func createPendingOrderWithOutbox(ctx context.Context, msg OrderMessage, payload []byte, headers string) error {
	order := Order{
		OrderID:   msg.OrderID,
		UserID:    msg.UserID,
		ProductID: msg.ProductID,
		Count:     msg.Count,
		Amount:    msg.Amount,
		Status:    OrderStatusPending,
	}
	event := OutboxEvent{
		EventID:       orderCreatedEventID(msg.OrderID),
		AggregateType: OutboxAggregateOrder,
		AggregateID:   msg.OrderID,
		EventType:     OutboxEventOrderCreated,
		Payload:       string(payload),
		Headers:       headers,
		Status:        OutboxStatusPending,
		NextRetryAt:   time.Now(),
	}

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&order).Error; err != nil {
			return err
		}
		if err := tx.Create(&event).Error; err != nil {
			return err
		}

		return nil
	})
}
