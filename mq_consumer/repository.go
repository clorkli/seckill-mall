package main

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func persistOrder(ctx context.Context, msg OrderMessage) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var order Order
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("order_id = ?", msg.OrderID).
			First(&order).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return persistLegacyOrder(tx, msg)
		}
		if err != nil {
			return err
		}

		if order.Status == OrderStatusSuccess || order.Status == OrderStatusFailed {
			return errOrderAlreadyFinished
		}
		if order.Status != OrderStatusPending {
			return fmt.Errorf("unknown order status: order_id=%s status=%d", msg.OrderID, order.Status)
		}

		if err := decrementMySQLStock(tx, msg); err != nil {
			return err
		}

		result := tx.Model(&Order{}).
			Where("order_id = ? AND status = ?", msg.OrderID, OrderStatusPending).
			Updates(map[string]any{
				"product_id":  msg.ProductID,
				"count":       msg.Count,
				"amount":      msg.Amount,
				"status":      OrderStatusSuccess,
				"fail_reason": "",
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errOrderAlreadyFinished
		}

		return nil
	})
}

func decrementMySQLStock(tx *gorm.DB, msg OrderMessage) error {
	result := tx.Exec(
		"UPDATE product SET stock = stock - ? WHERE id = ? AND stock >= ?",
		msg.Count,
		msg.ProductID,
		msg.Count,
	)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errMySQLStockNotEnough
	}

	return nil
}

func persistLegacyOrder(tx *gorm.DB, msg OrderMessage) error {
	if err := decrementMySQLStock(tx, msg); err != nil {
		return err
	}

	order := Order{
		OrderID:   msg.OrderID,
		UserID:    msg.UserID,
		ProductID: msg.ProductID,
		Count:     msg.Count,
		Amount:    msg.Amount,
		Status:    OrderStatusSuccess,
	}

	if err := tx.Create(&order).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return errOrderAlreadyFinished
		}
		return err
	}

	return nil
}
