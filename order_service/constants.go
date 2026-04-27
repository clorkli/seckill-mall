package main

const (
	SERVICE_NAME = "seckill/order"

	PRODUCT_SERVICE_NAME = "etcd:///seckill/product"
)

const (
	OrderStatusPending int32 = 0
	OrderStatusSuccess int32 = 1
	OrderStatusFailed  int32 = 2
)

const (
	OutboxStatusPending int32 = 0

	OutboxAggregateOrder    = "order"
	OutboxEventOrderCreated = "order.created"
)
