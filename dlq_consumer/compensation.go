package main

import (
	"context"
	"log"
	"strconv"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const ROLLBACK_LUA_SCRIPT = `
-- KEYS[1]: product:stock:{product_id}
-- KEYS[2]: product:users:{product_id}
-- KEYS[3]: order:rollback:{order_id}
-- ARGV[1]: rollback count
-- ARGV[2]: user_id

if redis.call("EXISTS", KEYS[3]) == 1 then
	return 3
end

if redis.call("EXISTS", KEYS[1]) == 0 then
	return 0
end

local rollback_count = tonumber(ARGV[1])
if rollback_count <= 0 then
	return 2
end

local current_buy = tonumber(redis.call("hget", KEYS[2], ARGV[2])) or 0
if current_buy == 0 then
	redis.call("set", KEYS[3], "no_purchase_record")
	return 4
end

if current_buy < rollback_count then
	return 5
end

redis.call("incrby", KEYS[1], rollback_count)
if current_buy == rollback_count then
	redis.call("hdel", KEYS[2], ARGV[2])
else
	redis.call("hincrby", KEYS[2], ARGV[2], -rollback_count)
end

redis.call("set", KEYS[3], "rolled_back")
return 1
`

func rollbackRedis(ctx context.Context, msg OrderMessage) (int, error) {
	stockKey := "product:stock:" + strconv.FormatInt(msg.ProductID, 10)
	userSetKey := "product:users:" + strconv.FormatInt(msg.ProductID, 10)
	rollbackKey := "order:rollback:" + msg.OrderID

	return rdb.Eval(ctx, ROLLBACK_LUA_SCRIPT, []string{stockKey, userSetKey, rollbackKey}, msg.Count, msg.UserID).Int()
}

func markOrderFailedOrRetry(ctx context.Context, msg OrderMessage, reason string, d amqp.Delivery, span trace.Span) bool {
	if err := markOrderFailed(ctx, msg, reason); err != nil {
		log.Printf("⚠️ 标记死信订单失败，稍后重试: order_id=%s err=%v", msg.OrderID, err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "mark order failed")
		dlqConsumeTotal.WithLabelValues("retry").Inc()
		mqConsumerNackTotal.WithLabelValues(DeadQueue, "true").Inc()
		time.Sleep(3 * time.Second)
		d.Nack(false, true)
		return false
	}

	return true
}

func handleRollbackResult(ctx context.Context, code int, msg OrderMessage, d amqp.Delivery, span trace.Span) {
	switch code {
	case 0:
		log.Printf("⚠️ Redis库存Key不存在，稍后重试: order_id=%s product_id=%d", msg.OrderID, msg.ProductID)
		span.SetStatus(codes.Error, "redis stock key missing")
		dlqConsumeTotal.WithLabelValues("retry").Inc()
		dlqCompensationTotal.WithLabelValues("retry").Inc()
		mqConsumerNackTotal.WithLabelValues(DeadQueue, "true").Inc()
		time.Sleep(3 * time.Second)
		d.Nack(false, true)
	case 1:
		if !markOrderFailedOrRetry(ctx, msg, "死信补偿成功，订单处理失败", d, span) {
			return
		}
		log.Printf("✅ 死信补偿成功: order_id=%s user_id=%d product_id=%d count=%d", msg.OrderID, msg.UserID, msg.ProductID, msg.Count)
		span.SetStatus(codes.Ok, "dlq compensation succeeded")
		dlqConsumeTotal.WithLabelValues("success").Inc()
		dlqCompensationTotal.WithLabelValues("success").Inc()
		mqConsumerAckTotal.WithLabelValues(DeadQueue).Inc()
		d.Ack(false)
	case 2:
		if !markOrderFailedOrRetry(ctx, msg, "死信消息数量非法，订单处理失败", d, span) {
			return
		}
		log.Printf("❌ 死信消息数量非法，无法补偿，确认消息: order_id=%s count=%d", msg.OrderID, msg.Count)
		span.SetStatus(codes.Error, "invalid rollback count")
		dlqConsumeTotal.WithLabelValues("invalid").Inc()
		dlqCompensationTotal.WithLabelValues("invalid").Inc()
		mqConsumerAckTotal.WithLabelValues(DeadQueue).Inc()
		d.Ack(false)
	case 3:
		if !markOrderFailedOrRetry(ctx, msg, "死信已补偿过，订单处理失败", d, span) {
			return
		}
		log.Printf("✅ 死信已补偿过，直接确认: order_id=%s", msg.OrderID)
		span.SetStatus(codes.Ok, "already rolled back")
		dlqConsumeTotal.WithLabelValues("success").Inc()
		dlqCompensationTotal.WithLabelValues("already_rolled_back").Inc()
		mqConsumerAckTotal.WithLabelValues(DeadQueue).Inc()
		d.Ack(false)
	case 4:
		if !markOrderFailedOrRetry(ctx, msg, "未找到用户购买记录，订单处理失败", d, span) {
			return
		}
		log.Printf("✅ 未找到用户购买记录，视为无需重复补偿: order_id=%s", msg.OrderID)
		span.SetStatus(codes.Ok, "no purchase record")
		dlqConsumeTotal.WithLabelValues("success").Inc()
		dlqCompensationTotal.WithLabelValues("no_purchase_record").Inc()
		mqConsumerAckTotal.WithLabelValues(DeadQueue).Inc()
		d.Ack(false)
	case 5:
		if !markOrderFailedOrRetry(ctx, msg, "用户购买记录小于回滚数量，需人工核查", d, span) {
			return
		}
		log.Printf("❌ 用户购买记录小于回滚数量，需人工核查，确认消息: order_id=%s user_id=%d count=%d", msg.OrderID, msg.UserID, msg.Count)
		span.SetStatus(codes.Error, "manual check required")
		dlqConsumeTotal.WithLabelValues("manual_check").Inc()
		dlqCompensationTotal.WithLabelValues("manual_check").Inc()
		mqConsumerAckTotal.WithLabelValues(DeadQueue).Inc()
		d.Ack(false)
	default:
		log.Printf("⚠️ 未知回滚状态，稍后重试: order_id=%s code=%d", msg.OrderID, code)
		span.SetStatus(codes.Error, "unknown rollback status")
		dlqConsumeTotal.WithLabelValues("retry").Inc()
		dlqCompensationTotal.WithLabelValues("retry").Inc()
		mqConsumerNackTotal.WithLabelValues(DeadQueue, "true").Inc()
		time.Sleep(3 * time.Second)
		d.Nack(false, true)
	}
}
