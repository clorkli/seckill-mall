package main

import (
	"context"
	"log"

	"seckill-mall/common/config"
	"seckill-mall/common/tracer"
)

func main() {
	shutdown := tracer.InitTracer("outbox-worker", "localhost:4318")
	defer shutdown(context.Background())

	config.InitConfig("mq")
	initDB()
	initRedis()
	startMetricsServer()

	mqURL := config.Conf.MQ.URL
	if mqURL == "" {
		log.Fatal("mq.url 为空，请在 config/mq.yaml 设置或通过环境变量 SECKILL_MQ_URL 注入")
	}

	publisher := NewMQPublisher(mqURL)
	if err := publisher.Connect(); err != nil {
		log.Fatalf("初始化RabbitMQ发布器失败: %v", err)
	}

	log.Println("Outbox Worker 已启动，等待待投递事件...")
	runWorker(context.Background(), publisher)
}
