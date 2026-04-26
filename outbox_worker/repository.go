package main

import (
	"context"
	"errors"
	"log"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func claimPendingEvents(ctx context.Context, limit int) ([]OutboxEvent, error) {
	var events []OutboxEvent
	now := time.Now()
	claimUntil := now.Add(ClaimVisibility)

	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ? AND next_retry_at <= ?", OutboxStatusPending, now).
			Order("next_retry_at ASC, id ASC").
			Limit(limit).
			Find(&events).Error; err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}

		ids := make([]uint64, 0, len(events))
		for _, event := range events {
			ids = append(ids, event.ID)
		}

		return tx.Model(&OutboxEvent{}).
			Where("id IN ? AND status = ?", ids, OutboxStatusPending).
			Update("next_retry_at", claimUntil).Error
	})
	if err != nil {
		return nil, err
	}

	return events, nil
}

func updateOutboxPendingGauge(ctx context.Context) {
	var count int64
	if err := db.WithContext(ctx).
		Model(&OutboxEvent{}).
		Where("status = ?", OutboxStatusPending).
		Count(&count).Error; err != nil {
		log.Printf("统计Outbox待投递事件失败: %v", err)
		return
	}

	outboxPendingGauge.Set(float64(count))
}

func scheduleRetry(ctx context.Context, event OutboxEvent, err error) {
	outboxRetryTotal.WithLabelValues("process").Inc()
	if errRetry := scheduleRetryWithCount(ctx, event, event.RetryCount+1, err); errRetry != nil {
		log.Printf("安排Outbox重试失败: event_id=%s err=%v", event.EventID, errRetry)
	}
}

func scheduleRetryWithCount(ctx context.Context, event OutboxEvent, retryCount int, err error) error {
	return db.WithContext(ctx).
		Model(&OutboxEvent{}).
		Where("event_id = ? AND status = ?", event.EventID, OutboxStatusPending).
		Updates(map[string]any{
			"retry_count":   retryCount,
			"next_retry_at": time.Now().Add(retryDelay(retryCount)),
			"last_error":    truncateText(err.Error(), 255),
		}).Error
}

func retryDelay(retryCount int) time.Duration {
	if retryCount <= 0 {
		return ScanInterval
	}
	delay := time.Duration(1<<uint(min(retryCount-1, 5))) * 5 * time.Second
	if delay > MaxRetryDelay {
		return MaxRetryDelay
	}
	return delay
}

func markEventSent(ctx context.Context, eventID string) error {
	return db.WithContext(ctx).
		Model(&OutboxEvent{}).
		Where("event_id = ? AND status = ?", eventID, OutboxStatusPending).
		Updates(map[string]any{
			"status":     OutboxStatusSent,
			"last_error": "",
		}).Error
}

func markEventFailed(ctx context.Context, event OutboxEvent, reason string) {
	if err := db.WithContext(ctx).
		Model(&OutboxEvent{}).
		Where("event_id = ? AND status = ?", event.EventID, OutboxStatusPending).
		Updates(map[string]any{
			"status":     OutboxStatusFailed,
			"last_error": truncateText(reason, 255),
		}).Error; err != nil {
		log.Printf("标记Outbox事件失败状态失败: event_id=%s err=%v", event.EventID, err)
	}
}

func markEventAndOrderFailed(ctx context.Context, event OutboxEvent, reason string) error {
	reason = truncateText(reason, 255)
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&OutboxEvent{}).
			Where("event_id = ? AND status = ?", event.EventID, OutboxStatusPending).
			Updates(map[string]any{
				"status":     OutboxStatusFailed,
				"last_error": reason,
			}).Error; err != nil {
			return err
		}

		return tx.Model(&Order{}).
			Where("order_id = ? AND status <> ?", event.AggregateID, OrderStatusSuccess).
			Updates(map[string]any{
				"status":      OrderStatusFailed,
				"fail_reason": reason,
			}).Error
	})
}

func getOrder(ctx context.Context, orderID string) (*Order, bool, error) {
	var order Order
	err := db.WithContext(ctx).Where("order_id = ?", orderID).First(&order).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &order, true, nil
}
