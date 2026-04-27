package main

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

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

func markOrderFailed(ctx context.Context, msg OrderMessage, reason string) error {
	return db.WithContext(ctx).
		Model(&Order{}).
		Where("order_id = ? AND user_id = ? AND status <> ?", msg.OrderID, msg.UserID, OrderStatusSuccess).
		Updates(map[string]any{
			"status":      OrderStatusFailed,
			"fail_reason": reason,
		}).Error
}
