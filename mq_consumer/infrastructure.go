package main

import (
	"fmt"
	"log"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"seckill-mall/common/config"
)

func initDB() {
	dsn := config.Conf.MySQL.DSN
	if dsn == "" {
		log.Fatal("mysql.dsn 为空，请在 config/mq.yaml 设置或通过环境变量 SECKILL_MYSQL_DSN 注入")
	}

	var err error
	db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("连接MySQL失败: %v", err)
	}
	// 表结构已固定，注释掉 AutoMigrate 防止改动
	// db.AutoMigrate(&Order{})
	fmt.Println("✅ MySQL 连接成功")
}
