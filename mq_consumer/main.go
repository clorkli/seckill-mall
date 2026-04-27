package main

import (
	"context"
	"log"
	"time"

	"seckill-mall/common/config"
	"seckill-mall/common/tracer"
)

func main() {
	shutdown := tracer.InitTracer("mq-consumer", "localhost:4318")
	defer shutdown(context.Background())

	config.InitConfig("mq")
	initDB()
	startMetricsServer()
	mqURL := config.Conf.MQ.URL
	if mqURL == "" {
		log.Fatal("mq.url 为空，请在 config/mq.yaml 设置或通过环境变量 SECKILL_MQ_URL 注入")
	}

	for {
		if err := runConsumer(mqURL); err != nil {
			log.Printf("主队列消费者停止: %v，%s 后重连", err, ReconnectDelay)
			mqConsumerReconnectTotal.WithLabelValues("order").Inc()
		}
		time.Sleep(ReconnectDelay)
	}
}
