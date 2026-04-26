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
	outboxPendingGauge = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "seckill_outbox_pending_events",
			Help: "Current number of pending outbox events.",
		},
	)
	outboxScanTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_outbox_scan_total",
			Help: "Total number of outbox scan attempts by result.",
		},
		[]string{"result"},
	)
	outboxClaimedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "seckill_outbox_claimed_total",
			Help: "Total number of outbox events claimed for processing.",
		},
	)
	outboxProcessDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "seckill_outbox_process_duration_seconds",
			Help:    "Outbox event processing duration in seconds by result.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"result"},
	)
	outboxPublishTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_outbox_publish_total",
			Help: "Total number of outbox RabbitMQ publish attempts by result.",
		},
		[]string{"result"},
	)
	outboxPublishDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "seckill_outbox_publish_duration_seconds",
			Help:    "Outbox RabbitMQ publish duration in seconds by result.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"result"},
	)
	outboxRetryTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_outbox_retry_total",
			Help: "Total number of outbox retries scheduled by reason.",
		},
		[]string{"reason"},
	)
	outboxCompensationTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_outbox_compensation_total",
			Help: "Total number of outbox final Redis compensation attempts by result.",
		},
		[]string{"result"},
	)
	outboxReconnectTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "seckill_outbox_reconnect_total",
			Help: "Total number of RabbitMQ reconnect attempts made by the outbox worker.",
		},
	)
)

func startMetricsServer() {
	port := os.Getenv("SECKILL_OUTBOX_METRICS_PORT")
	if port == "" {
		port = "9095"
	}

	addr := net.JoinHostPort("", port)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	go func() {
		log.Printf("Outbox Worker metrics 已启动: %s/metrics", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("Outbox Worker metrics 启动失败: %v", err)
		}
	}()
}
