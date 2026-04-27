package main

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// 对应数据库结构
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

// MQ 消息结构
type OrderMessage struct {
	OrderID   string  `json:"order_id"`
	UserID    int64   `json:"user_id"`
	ProductID int64   `json:"product_id"`
	Count     int32   `json:"count"`
	Amount    float32 `json:"amount"`
}

var db *gorm.DB

var errMySQLStockNotEnough = errors.New("mysql stock not enough")
var errOrderAlreadyFinished = errors.New("order already finished")
