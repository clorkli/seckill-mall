package main

import (
	"context"
	"log"
	"strconv"

	"seckill-mall/common/config"
	"seckill-mall/common/pb"
)

// 定义 Lua 脚本
// KEYS[1]: 商品的 Redis Key (例如 product:stock:1)
// ARGV[1]: 要扣减的数量
const LUA_SCRIPT = `
-- 商品Key不存在（未预热/错误ID）
if redis.call("EXISTS", KEYS[1]) == 0 then
	return 0
end

-- 重复购买
local current_buy = tonumber(redis.call('hget', KEYS[2], ARGV[2])) or 0
local want_buy = tonumber(ARGV[1])
local limit = tonumber(ARGV[3])  -- 每人限购ARGV[3]件

if current_buy + want_buy > limit then
	return 3
end

local stock = tonumber(redis.call("GET", KEYS[1]))

-- 库存不足
if stock < want_buy then
	return 2
end

-- 扣减库存
redis.call("decrby", KEYS[1], want_buy)
redis.call("hincrby", KEYS[2], ARGV[2], want_buy) --记录用户购买行为
return 1
`

// 回滚脚本同时恢复库存和用户购买记录，避免 MQ 发送失败后用户被误判已购买。
const ROLLBACK_LUA_SCRIPT = `
if redis.call("EXISTS", KEYS[1]) == 0 then
	return 0
end

local rollback_count = tonumber(ARGV[1])
if rollback_count <= 0 then
	return 2
end

redis.call("incrby", KEYS[1], rollback_count)

local current_buy = tonumber(redis.call("hget", KEYS[2], ARGV[2])) or 0
if current_buy <= rollback_count then
	redis.call("hdel", KEYS[2], ARGV[2])
else
	redis.call("hincrby", KEYS[2], ARGV[2], -rollback_count)
end

return 1
`

// 升级 DeductStock 接口，区分库存为零与商品不存在两种情况
func (s *server) DeductStock(ctx context.Context, req *pb.DeductStockRequest) (*pb.DeductStockResponse, error) {
	log.Printf("stock deduct requested user_id=%d product_id=%d count=%d", req.UserId, req.ProductId, req.Count)

	if req.Count <= 0 {
		return &pb.DeductStockResponse{
			Success: false,
			Message: "购买数量必须大于0",
		}, nil
	}

	// 拼接 Key: product:stock:1
	stockKey := "product:stock:" + strconv.FormatInt(req.ProductId, 10)
	userSetKey := "product:users:" + strconv.FormatInt(req.ProductId, 10) //新增用户购买集合Key

	PurchaseLimit := config.Conf.Seckill.PurchaseLimit // 限购数据在配置文件中设置

	if PurchaseLimit <= 0 {
		PurchaseLimit = 1 // 预防限购未设置，默认每人限购1件
	}

	// 执行 Lua 脚本
	val, err := rdb.Eval(ctx, LUA_SCRIPT, []string{stockKey, userSetKey}, req.Count, req.UserId, PurchaseLimit).Int()

	if err != nil {
		log.Printf("stock deduct redis eval failed user_id=%d product_id=%d count=%d err=%v", req.UserId, req.ProductId, req.Count, err)
		return nil, err
	}

	// 根据 Lua 返回的状态码进行精准处理
	switch val {
	case 0: // 商品不存在
		log.Printf("stock deduct rejected product_id=%d reason=missing_stock_key", req.ProductId)
		return &pb.DeductStockResponse{
			Success: false,
			Message: "商品不存在或未上架", //给出明确的错误提示
		}, nil
	case 2: // 库存不足
		log.Printf("stock deduct rejected product_id=%d reason=stock_not_enough", req.ProductId)
		return &pb.DeductStockResponse{
			Success: false,
			Message: "库存不足",
		}, nil
	case 1: // 成功
		log.Printf("stock deducted user_id=%d product_id=%d count=%d", req.UserId, req.ProductId, req.Count)
		return &pb.DeductStockResponse{Success: true, Message: "扣减成功"}, nil
	case 3: // 重复购买
		log.Printf("stock deduct rejected user_id=%d product_id=%d count=%d limit=%d reason=purchase_limit_exceeded", req.UserId, req.ProductId, req.Count, PurchaseLimit)
		return &pb.DeductStockResponse{
			Success: false,
			Message: "每人限购一件，您已购买过该商品，不能重复购买",
		}, nil
	default:
		return &pb.DeductStockResponse{Success: false, Message: "未知错误"}, nil
	}
}

// 实现 RollbackStock 接口
func (s *server) RollbackStock(ctx context.Context, req *pb.DeductStockRequest) (*pb.DeductStockResponse, error) {
	log.Printf("stock rollback requested user_id=%d product_id=%d count=%d", req.UserId, req.ProductId, req.Count)

	if req.Count <= 0 {
		return &pb.DeductStockResponse{
			Success: false,
			Message: "回滚数量必须大于0",
		}, nil
	}

	stockKey := "product:stock:" + strconv.FormatInt(req.ProductId, 10)
	userSetKey := "product:users:" + strconv.FormatInt(req.ProductId, 10)

	val, err := rdb.Eval(ctx, ROLLBACK_LUA_SCRIPT, []string{stockKey, userSetKey}, req.Count, req.UserId).Int()
	if err != nil {
		log.Printf("stock rollback redis eval failed user_id=%d product_id=%d count=%d err=%v", req.UserId, req.ProductId, req.Count, err)
		return &pb.DeductStockResponse{Success: false, Message: "回滚失败: " + err.Error()}, nil
	}

	switch val {
	case 0:
		return &pb.DeductStockResponse{Success: false, Message: "商品库存不存在，无法回滚"}, nil
	case 1:
		log.Printf("stock rollback succeeded user_id=%d product_id=%d count=%d", req.UserId, req.ProductId, req.Count)
		return &pb.DeductStockResponse{Success: true, Message: "回滚成功"}, nil
	case 2:
		return &pb.DeductStockResponse{Success: false, Message: "回滚数量必须大于0"}, nil
	default:
		return &pb.DeductStockResponse{Success: false, Message: "未知回滚状态"}, nil
	}
}
