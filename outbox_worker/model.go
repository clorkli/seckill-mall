package main

import (
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

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

type Order struct {
	OrderID    string `gorm:"column:order_id"`
	Status     int32  `gorm:"column:status"`
	FailReason string `gorm:"column:fail_reason"`
}

func (Order) TableName() string { return "orders" }

type OrderMessage struct {
	OrderID   string  `json:"order_id"`
	UserID    int64   `json:"user_id"`
	ProductID int64   `json:"product_id"`
	Count     int32   `json:"count"`
	Amount    float32 `json:"amount"`
}

var db *gorm.DB
var rdb *redis.Client
