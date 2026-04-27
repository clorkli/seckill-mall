package main

import (
	"context"
	"log"

	"github.com/redis/go-redis/v9"
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
		log.Fatalf("mysql connect failed component=dlq_consumer err=%v", err)
	}
	log.Println("mysql connected component=dlq_consumer")
}

func initRedis() {
	rdb = redis.NewClient(&redis.Options{
		Addr:     config.Conf.Redis.Addr,
		Password: config.Conf.Redis.Password,
		DB:       config.Conf.Redis.DB,
	})

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("redis connect failed component=dlq_consumer err=%v", err)
	}
	log.Println("redis connected component=dlq_consumer")
}
