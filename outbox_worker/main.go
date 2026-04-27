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
		log.Fatal("mq url missing config=config/mq.yaml env=SECKILL_MQ_URL")
	}

	publisher := NewMQPublisher(mqURL)
	if err := publisher.Connect(); err != nil {
		log.Fatalf("outbox publisher init failed: %v", err)
	}

	log.Println("outbox worker started")
	runWorker(context.Background(), publisher)
}
