package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"time"

	"gorm.io/gorm"

	"seckill-mall/common/pb"

	"go.opentelemetry.io/otel"
)

type server struct {
	pb.UnimplementedOrderServiceServer
}

func orderStatusText(status int32) string {
	switch status {
	case OrderStatusPending:
		return "排队中"
	case OrderStatusSuccess:
		return "已成功"
	case OrderStatusFailed:
		return "已失败"
	default:
		return "未知状态"
	}
}

func orderStatusMessage(status int32) string {
	switch status {
	case OrderStatusPending:
		return "订单排队处理中，请稍后查询"
	case OrderStatusSuccess:
		return "订单已成功创建"
	case OrderStatusFailed:
		return "订单处理失败"
	default:
		return "订单状态未知，请联系管理员核查"
	}
}

// CreateOrder 下单逻辑 (异步版)
func (s *server) CreateOrder(ctx context.Context, req *pb.CreateOrderRequest) (*pb.CreateOrderResponse, error) {
	log.Printf("order create requested user_id=%d product_id=%d count=%d", req.UserId, req.ProductId, req.Count)

	if req.Count <= 0 {
		return &pb.CreateOrderResponse{
			Success: false,
			Message: "购买数量必须大于0",
		}, nil
	}

	//扣减 Redis 库存作为防超卖第一道防线
	deductResp, err := productClient.DeductStock(ctx, &pb.DeductStockRequest{
		ProductId: req.ProductId,
		Count:     req.Count,
		UserId:    req.UserId, //新增用户ID字段防止重复购买
	})
	if err != nil {
		return nil, fmt.Errorf("调用商品服务失败: %v", err)
	}

	if !deductResp.Success {
		log.Printf("order create rejected user_id=%d product_id=%d reason=%q", req.UserId, req.ProductId, deductResp.Message)
		return &pb.CreateOrderResponse{
			Success: false,
			Message: deductResp.Message,
		}, nil
	}

	// 查价格,计算总金额
	pResp, err := productClient.GetProduct(ctx, &pb.ProductRequest{ProductId: req.ProductId})
	if err != nil {
		rollbackStockAndMarkFailed("", req.ProductId, req.UserId, req.Count, "查询商品失败")
		return nil, err
	}

	totalAmount := pResp.Price * float32(req.Count)
	orderID := fmt.Sprintf("%d%d", time.Now().UnixNano(), rand.Intn(1000))

	orderMsg := OrderMessage{
		OrderID:   orderID,
		UserID:    req.UserId,
		ProductID: req.ProductId,
		Count:     req.Count,
		Amount:    totalAmount,
	}

	body, err := json.Marshal(orderMsg)
	if err != nil {
		rollbackStockAndMarkFailed(orderID, req.ProductId, req.UserId, req.Count, "序列化订单消息失败")
		return nil, fmt.Errorf("序列化订单消息失败: %w", err)
	}

	outboxCtx, outboxSpan := otel.Tracer("order-service").Start(ctx, "outbox.enqueue_order")
	defer outboxSpan.End()
	headers := marshalTraceHeaders(outboxCtx)

	if err := createPendingOrderWithOutbox(outboxCtx, orderMsg, body, headers); err != nil {
		outboxSpan.RecordError(err)
		rollbackStockAndMarkFailed(orderID, req.ProductId, req.UserId, req.Count, "创建排队订单和Outbox事件失败")
		return nil, fmt.Errorf("创建排队订单和Outbox事件失败: %w", err)
	}

	log.Printf("order enqueued order_id=%s user_id=%d product_id=%d count=%d", orderID, req.UserId, req.ProductId, req.Count)

	return &pb.CreateOrderResponse{
		OrderId: orderID,
		Success: true,
		Message: "排队中，请稍后查询结果",
	}, nil
}

// GetOrder 查询当前用户自己的订单状态。
func (s *server) GetOrder(ctx context.Context, req *pb.GetOrderRequest) (*pb.GetOrderResponse, error) {
	if req.OrderId == "" {
		return &pb.GetOrderResponse{
			Found:   false,
			Message: "订单号不能为空",
		}, nil
	}
	if req.UserId <= 0 {
		return &pb.GetOrderResponse{
			Found:   false,
			Message: "用户ID非法",
		}, nil
	}

	var order Order
	err := db.WithContext(ctx).
		Where("order_id = ? AND user_id = ?", req.OrderId, req.UserId).
		First(&order).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &pb.GetOrderResponse{
			Found:   false,
			OrderId: req.OrderId,
			Message: "订单不存在",
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询订单失败: %w", err)
	}

	message := orderStatusMessage(order.Status)
	if order.Status == OrderStatusFailed && order.FailReason != "" {
		message = order.FailReason
	}

	return &pb.GetOrderResponse{
		Found:      true,
		OrderId:    order.OrderID,
		UserId:     order.UserID,
		ProductId:  order.ProductID,
		Count:      order.Count,
		Amount:     order.Amount,
		Status:     order.Status,
		StatusText: orderStatusText(order.Status),
		Message:    message,
	}, nil
}
