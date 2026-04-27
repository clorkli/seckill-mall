package main

import (
	"log"
	"net"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"seckill-mall/common/config"
)

var (
	mqConsumeTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_mq_consume_total",
			Help: "Total number of RabbitMQ messages consumed by queue and result.",
		},
		[]string{"queue", "result"},
	)
	mqConsumeDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "seckill_mq_consume_duration_seconds",
			Help:    "RabbitMQ message handling duration in seconds by queue.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"queue"},
	)
	mqConsumerAckTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_mq_consumer_ack_total",
			Help: "Total number of RabbitMQ consumer acks by queue.",
		},
		[]string{"queue"},
	)
	mqConsumerNackTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_mq_consumer_nack_total",
			Help: "Total number of RabbitMQ consumer nacks by queue and requeue flag.",
		},
		[]string{"queue", "requeue"},
	)
	mqConsumerReconnectTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_mq_consumer_reconnect_total",
			Help: "Total number of RabbitMQ consumer reconnect attempts by consumer.",
		},
		[]string{"consumer"},
	)
)

func startMetricsServer() {
	port := config.Conf.Server.MetricsPort
	if port == "" {
		port = "9093"
	}

	addr := net.JoinHostPort("", port)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	go func() {
		log.Printf("MQ Consumer metrics 已启动: %s/metrics", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("MQ Consumer metrics 启动失败: %v", err)
		}
	}()
}
