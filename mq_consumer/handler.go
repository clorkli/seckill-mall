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

func handleMessage(d amqp.Delivery) {
	start := time.Now()
	defer func() {
		mqConsumeDuration.WithLabelValues(OrderQueue).Observe(time.Since(start).Seconds())
	}()

	traceCtx := tracer.ExtractAMQPHeaders(context.Background(), d.Headers)
	traceCtx, span := otel.Tracer("mq-consumer").Start(traceCtx, "rabbitmq.consume_order")
	defer span.End()

	var msg OrderMessage
	if err := json.Unmarshal(d.Body, &msg); err != nil {
		log.Printf("❌ 消息格式错误，直接丢弃: %v", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid message json")
		mqConsumeTotal.WithLabelValues(OrderQueue, "invalid").Inc()
		mqConsumerNackTotal.WithLabelValues(OrderQueue, "false").Inc()
		d.Nack(false, false) // 这种一般不需要重试，直接进死信或丢弃
		return
	}

	if msg.Count <= 0 {
		log.Printf("❌ 订单数量非法，进入死信: order_id=%s count=%d", msg.OrderID, msg.Count)
		span.SetStatus(codes.Error, "invalid order count")
		mqConsumeTotal.WithLabelValues(OrderQueue, "invalid").Inc()
		mqConsumerNackTotal.WithLabelValues(OrderQueue, "false").Inc()
		d.Nack(false, false)
		return
	}

	fmt.Printf("📦 接收订单: %s | 数量：%d | 金额：%.2f | 处理中...", msg.OrderID, msg.Count, msg.Amount)

	// 模拟业务处理耗时
	time.Sleep(50 * time.Millisecond)

	// 写入订单并同步扣减 MySQL 商品库存，保证最终库存账本一致。
	err := persistOrder(traceCtx, msg)
	if err != nil {
		// 场景 A: 重复消费或已失败订单，幂等确认，避免重复扣减 product.stock。
		if errors.Is(err, errOrderAlreadyFinished) {
			fmt.Printf(" -> ⚠️ 订单已结束，确认消息\n")
			span.SetStatus(codes.Ok, "finished order acknowledged")
			mqConsumeTotal.WithLabelValues(OrderQueue, "duplicate").Inc()
			mqConsumerAckTotal.WithLabelValues(OrderQueue).Inc()
			d.Ack(false)
		} else if errors.Is(err, errMySQLStockNotEnough) {
			log.Printf(" -> ❌ MySQL库存不足，发送 Nack(不重回队列)->进入死信")
			span.RecordError(err)
			span.SetStatus(codes.Error, "mysql stock not enough")
			mqConsumeTotal.WithLabelValues(OrderQueue, "mysql_stock_not_enough").Inc()
			mqConsumerNackTotal.WithLabelValues(OrderQueue, "false").Inc()
			d.Nack(false, false)
		} else {
			// 场景 B: 真正的故障 (数据库挂了/网络抖动)
			log.Printf(" -> ❌ 落库失败: %v，发送 Nack(不重回队列)->进入死信", err)
			span.RecordError(err)
			span.SetStatus(codes.Error, "persist order failed")

			// 关键点：requeue=false + 配置了死信交换机 = 消息进入死信队列
			mqConsumeTotal.WithLabelValues(OrderQueue, "failed").Inc()
			mqConsumerNackTotal.WithLabelValues(OrderQueue, "false").Inc()
			d.Nack(false, false)
		}
	} else {
		// 场景 C: 成功
		fmt.Printf(" -> ✅ 落库成功\n")
		span.SetStatus(codes.Ok, "order persisted")
		mqConsumeTotal.WithLabelValues(OrderQueue, "success").Inc()
		mqConsumerAckTotal.WithLabelValues(OrderQueue).Inc()
		d.Ack(false)
	}
}
