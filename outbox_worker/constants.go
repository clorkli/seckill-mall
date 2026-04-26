package main

import "time"

const (
	OrderQueue = "seckill_order_queue"

	DeadExchange   = "dlx_exchange"
	DeadQueue      = "dead_queue"
	DeadRoutingKey = "dead_key"
)

const (
	OrderStatusPending int32 = 0
	OrderStatusSuccess int32 = 1
	OrderStatusFailed  int32 = 2
)

const (
	OutboxStatusPending int32 = 0
	OutboxStatusSent    int32 = 1
	OutboxStatusFailed  int32 = 2
)

const (
	BatchSize       = 10
	MaxRetryCount   = 5
	ScanInterval    = 2 * time.Second
	ClaimVisibility = 30 * time.Second
	MaxRetryDelay   = 2 * time.Minute
)
