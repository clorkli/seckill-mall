package main

import (
	"log"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"seckill-mall/common/config"
)

func initDB() {
	dsn := config.Conf.MySQL.DSN
	if dsn == "" {
		log.Fatal("mysql dsn missing config=config/mq.yaml env=SECKILL_MYSQL_DSN")
	}

	var err error
	db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("mysql connect failed component=mq_consumer err=%v", err)
	}
	// 表结构已固定，注释掉 AutoMigrate 防止改动
	// db.AutoMigrate(&Order{})
	log.Println("mysql connected component=mq_consumer")
}
