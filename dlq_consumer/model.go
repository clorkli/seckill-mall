package main

import (
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type Order struct {
	OrderID    string `gorm:"column:order_id"`
	UserID     int64  `gorm:"column:user_id"`
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
