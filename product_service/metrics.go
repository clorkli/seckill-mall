package main

import (
	"context"
	"fmt"
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
			fmt.Println("警告：当前为开发环境，启用重置接口 /dev/reset")

			//警告：仅限开发环境使用
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
				fmt.Println("MySQL 订单表已清空")

				preheatStock()

				w.Write([]byte("环境已重置，Redis已清空并重新预热库存"))
			})
		}
		http.Handle("/metrics", promhttp.Handler())
		fmt.Println("商品监控服务已启动：" + metricsAddr)
		http.ListenAndServe(metricsAddr, nil)
	}()
}
