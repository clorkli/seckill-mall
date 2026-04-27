package main

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/naming/endpoints"

	"seckill-mall/common/config"
)

// 初始化MySQL连接
func initDB() {
	dsn := config.Conf.MySQL.DSN
	if dsn == "" {
		log.Fatalf("mysql dsn missing config=config/order.yaml env=SECKILL_MYSQL_DSN")
	}

	var err error
	db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("mysql connect failed component=order_service err=%v", err)
	}

	log.Println("mysql connected component=order_service")
}

// === 注册自己到 Etcd ===
func registerEtcd(serviceAddr string) {
	etcdAddr := config.Conf.Etcd.Addr

	cli, _ := clientv3.New(clientv3.Config{Endpoints: []string{etcdAddr}})
	em, _ := endpoints.NewManager(cli, SERVICE_NAME)
	lease, _ := cli.Grant(context.TODO(), 10)

	em.AddEndpoint(context.TODO(), SERVICE_NAME+"/"+serviceAddr, endpoints.Endpoint{Addr: serviceAddr}, clientv3.WithLease(lease.ID))

	ch, _ := cli.KeepAlive(context.TODO(), lease.ID)
	go func() {
		for range ch {
		}
	}()
	log.Printf("etcd registered service=order addr=%s", serviceAddr)
}

func startMetricsServer() {
	//启动Prometheus监控(Port:9092)
	go func() {
		metricsAddr := fmt.Sprintf(":%s", config.Conf.Server.MetricsPort)
		http.Handle("/metrics", promhttp.Handler())
		log.Printf("order metrics server started addr=%s", metricsAddr)

		if err := http.ListenAndServe(metricsAddr, nil); err != nil {
			log.Printf("order metrics server failed: %v", err) // Do not stop the main service for metrics startup failure.
		}
	}()
}
