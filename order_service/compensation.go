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
		log.Printf("X! 标记订单失败状态失败，请人工介入: order_id=%s err=%v", orderID, err)
	}
}

func rollbackStockAndMarkFailed(orderID string, productID, userID int64, count int32, reason string) {
	if err := rollbackStock(productID, userID, count); err != nil {
		log.Printf("X! %s 且回滚库存失败，请人工介入，CRITICAL ERROR: %v", reason, err)
		markPendingOrderFailed(orderID, userID, reason+"，Redis回滚失败")
		return
	}

	log.Printf("库存回滚成功: order_id=%s", orderID)
	markPendingOrderFailed(orderID, userID, reason)
}
