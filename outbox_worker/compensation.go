package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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

func compensateRedis(ctx context.Context, msg OrderMessage) (string, error) {
	code, err := rollbackRedis(ctx, msg)
	if err != nil {
		return "", err
	}

	switch code {
	case 1:
		return "Outbox超过最大重试次数，Redis库存和用户购买记录已补偿，订单处理失败", nil
	case 2:
		return "Outbox超过最大重试次数，回滚数量非法，订单处理失败", nil
	case 3:
		return "Outbox超过最大重试次数，Redis已补偿过，订单处理失败", nil
	case 4:
		return "Outbox超过最大重试次数，未找到用户购买记录，订单处理失败", nil
	case 5:
		return "Outbox超过最大重试次数，用户购买记录小于回滚数量，需人工核查", nil
	case 0:
		return "", errors.New("Redis库存Key不存在，暂缓最终失败处理")
	default:
		return "", fmt.Errorf("未知Redis回滚状态: %d", code)
	}
}

func rollbackRedis(ctx context.Context, msg OrderMessage) (int, error) {
	stockKey := "product:stock:" + strconv.FormatInt(msg.ProductID, 10)
	userSetKey := "product:users:" + strconv.FormatInt(msg.ProductID, 10)
	rollbackKey := "order:rollback:" + msg.OrderID

	return rdb.Eval(ctx, ROLLBACK_LUA_SCRIPT, []string{stockKey, userSetKey, rollbackKey}, msg.Count, msg.UserID).Int()
}
