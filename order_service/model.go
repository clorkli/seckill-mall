package main

import (
	"time"

	"gorm.io/gorm"
)

// OrderMessage 是投递给 RabbitMQ 的订单消息。
type OrderMessage struct {
	OrderID   string  `json:"order_id"`
	UserID    int64   `json:"user_id"`
	ProductID int64   `json:"product_id"`
	Count     int32   `json:"count"`
	Amount    float32 `json:"amount"`
}

// Order 对应 orders 表。Count/UpdatedAt 用于后续订单状态闭环，旧表缺列时查询会保持零值。
type Order struct {
	ID         uint64    `gorm:"column:id;primaryKey;autoIncrement"`
	OrderID    string    `gorm:"column:order_id;uniqueIndex;not null"`
	UserID     int64     `gorm:"column:user_id;not null"`
	ProductID  int64     `gorm:"column:product_id;not null"`
	Count      int32     `gorm:"column:count"`
	Amount     float32   `gorm:"column:amount;not null"`
	Status     int32     `gorm:"column:status;default:0"`
	FailReason string    `gorm:"column:fail_reason"`
	CreatedAt  time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt  time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (Order) TableName() string { return "orders" }

// OutboxEvent 记录需要可靠投递到 MQ 的领域事件，后续由 outbox_worker 扫描发送。
type OutboxEvent struct {
	ID            uint64    `gorm:"column:id;primaryKey;autoIncrement"`
	EventID       string    `gorm:"column:event_id;uniqueIndex;not null"`
	AggregateType string    `gorm:"column:aggregate_type;not null"`
	AggregateID   string    `gorm:"column:aggregate_id;not null"`
	EventType     string    `gorm:"column:event_type;not null"`
	Payload       string    `gorm:"column:payload;type:json;not null"`
	Headers       string    `gorm:"column:headers;type:json"`
	Status        int32     `gorm:"column:status;default:0"`
	RetryCount    int       `gorm:"column:retry_count;default:0"`
	NextRetryAt   time.Time `gorm:"column:next_retry_at;not null"`
	LastError     string    `gorm:"column:last_error"`
	CreatedAt     time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt     time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (OutboxEvent) TableName() string { return "outbox_events" }

var db *gorm.DB
