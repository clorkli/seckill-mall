package main

import "time"

const (
	OrderQueue = "seckill_order_queue"

	DeadExchange   = "dlx_exchange" // 死信交换机
	DeadQueue      = "dead_queue"   // 死信队列
	DeadRoutingKey = "dead_key"     // 死信路由键

	ReconnectDelay = 3 * time.Second
)

const (
	OrderStatusPending int32 = 0
	OrderStatusSuccess int32 = 1
	OrderStatusFailed  int32 = 2
)
