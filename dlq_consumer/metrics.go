package main

import (
	"log"
	"net"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	dlqConsumeTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_dlq_consume_total",
			Help: "Total number of RabbitMQ dead-letter messages handled by result.",
		},
		[]string{"result"},
	)
	dlqCompensationTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_dlq_compensation_total",
			Help: "Total number of DLQ compensation attempts by result.",
		},
		[]string{"result"},
	)
	dlqCompensationDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "seckill_dlq_compensation_duration_seconds",
			Help:    "DLQ message handling and compensation duration in seconds.",
			Buckets: prometheus.DefBuckets,
		},
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
	port := os.Getenv("SECKILL_DLQ_METRICS_PORT")
	if port == "" {
		port = "9094"
	}

	addr := net.JoinHostPort("", port)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	go func() {
		log.Printf("DLQ Consumer metrics 已启动: %s/metrics", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("DLQ Consumer metrics 启动失败: %v", err)
		}
	}()
}
