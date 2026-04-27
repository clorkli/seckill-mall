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
		log.Fatal("mq url missing config=config/mq.yaml env=SECKILL_MQ_URL")
	}

	for {
		if err := runConsumer(mqURL); err != nil {
			log.Printf("mq consumer stopped err=%v reconnect_after=%s", err, ReconnectDelay)
			mqConsumerReconnectTotal.WithLabelValues("order").Inc()
		}
		time.Sleep(ReconnectDelay)
	}
}
