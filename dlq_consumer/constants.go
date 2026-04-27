package main

import "time"

const (
	DeadExchange   = "dlx_exchange"
	DeadQueue      = "dead_queue"
	DeadRoutingKey = "dead_key"

	ReconnectDelay = 3 * time.Second
)

const (
	OrderStatusPending int32 = 0
	OrderStatusSuccess int32 = 1
	OrderStatusFailed  int32 = 2
)
