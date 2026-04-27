package main

import (
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// 数据库模型
type Product struct {
	ID          int64   `gorm:"primaryKey"`
	Name        string  `gorm:"type:varchar(255)"`
	Price       float32 `gorm:"type:decimal(10,2)"`
	Stock       int32   `gorm:"type:int"`
	Description string  `gorm:"type:varchar(255)"`
}

func (Product) TableName() string { return "product" }

var db *gorm.DB
var rdb *redis.Client // 全局 Redis 客户端
