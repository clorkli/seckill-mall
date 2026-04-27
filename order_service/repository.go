package main

import "context"

func markOrderFailed(ctx context.Context, orderID string, userID int64, reason string) error {
	return db.WithContext(ctx).
		Model(&Order{}).
		Where("order_id = ? AND user_id = ? AND status = ?", orderID, userID, OrderStatusPending).
		Updates(map[string]any{
			"status":      OrderStatusFailed,
			"fail_reason": truncateText(reason, 255),
		}).Error
}
