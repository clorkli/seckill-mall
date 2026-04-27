package main

import (
	"context"
	"fmt"
	"log"
	"net"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"google.golang.org/grpc"

	"seckill-mall/common/config"
	"seckill-mall/common/pb"
	"seckill-mall/common/tracer"
)

func main() {
	//初始化链路追踪
	shutdown := tracer.InitTracer("order-service", "localhost:4318")
	defer shutdown(context.Background())

	//最先加载配置
	config.InitConfig("order")

	port := config.Conf.Server.Port
	if port == "" {
		port = "50052"
		log.Println("配置文件未指定端口，使用默认端口 50052")
	}
	//最好使用宿主机真实IP地址，避免容器重启后地址变化导致注册失败
	myAddr := "127.0.0.1:" + port

	initDB()
	initProductClient()
	registerEtcd(myAddr)
	startMetricsServer()

	grpcAddr := fmt.Sprintf(":%s", config.Conf.Server.Port)
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		log.Fatalf("监听端口失败 %s: %v", port, err)
	}

	//创建gRPC服务器时添加拦截器
	s := grpc.NewServer(
		grpc.StreamInterceptor(grpc_prometheus.StreamServerInterceptor),
		grpc.UnaryInterceptor(grpc_prometheus.UnaryServerInterceptor),
	)
	pb.RegisterOrderServiceServer(s, &server{})

	grpc_prometheus.Register(s)

	fmt.Printf("=== 订单微服务已启动 (Port: %s) ===", grpcAddr)
	if err := s.Serve(lis); err != nil {
		log.Fatalf("服务启动失败: %v", err)
	}
}
