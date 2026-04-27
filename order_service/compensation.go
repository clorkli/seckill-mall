package main

import (
	"context"
	"log"
	"time"
)

func markPendingOrderFailed(orderID string, userID int64, reason string) {
	updateCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := markOrderFailed(updateCtx, orderID, userID, reason); err != nil {
		log.Printf("order mark failed status failed order_id=%s err=%v", orderID, err)
	}
}

func rollbackStockAndMarkFailed(orderID string, productID, userID int64, count int32, reason string) {
	if err := rollbackStock(productID, userID, count); err != nil {
		log.Printf("order compensation rollback failed order_id=%s product_id=%d user_id=%d count=%d reason=%q err=%v", orderID, productID, userID, count, reason, err)
		markPendingOrderFailed(orderID, userID, reason+"，Redis回滚失败")
		return
	}

	log.Printf("order compensation rollback succeeded order_id=%s product_id=%d user_id=%d count=%d", orderID, productID, userID, count)
	markPendingOrderFailed(orderID, userID, reason)
}
