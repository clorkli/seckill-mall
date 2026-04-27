package main

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"seckill-mall/common/config"
)

func startMetricsServer() {
	//新端口暴露 Prometheus
	go func() {
		//拼接冒号":9091"
		metricsAddr := fmt.Sprintf(":%s", config.Conf.Server.MetricsPort)

		//===新增开发环境重置接口===
		if config.Conf.Server.Mode == "debug" {
			log.Println("dev reset endpoint enabled path=/dev/reset")

			// 仅限开发环境使用
			http.HandleFunc("/dev/reset", func(w http.ResponseWriter, r *http.Request) {
				//清空Redis
				err := rdb.FlushDB(context.Background()).Err()
				if err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte("清空Redis失败: " + err.Error()))
					return
				}

				if err := db.Exec("TRUNCATE TABLE `orders`").Error; err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte("MySQL 订单表重置失败: " + err.Error()))
					return
				}
				log.Println("dev reset truncated table=orders")

				preheatStock()

				w.Write([]byte("环境已重置，Redis已清空并重新预热库存"))
			})
		}
		http.Handle("/metrics", promhttp.Handler())
		log.Printf("product metrics server started addr=%s", metricsAddr)
		if err := http.ListenAndServe(metricsAddr, nil); err != nil {
			log.Printf("product metrics server failed: %v", err)
		}
	}()
}
