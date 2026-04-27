package main

import (
	"context"
	"fmt"
	"log"
	"net"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	"seckill-mall/common/config"
	"seckill-mall/common/pb"
	"seckill-mall/common/tracer"
)

func main() {
	shutdown := tracer.InitTracer("product-service", "localhost:4318")
	defer shutdown(context.Background())
	config.InitConfig("product")
	// 统一端口键：server.port
	port := config.Conf.Server.Port
	if port == "" {
		port = "50051"
	}

	initDB()
	initRedis()    // 1. 连 Redis
	preheatStock() // 2. 预热库存
	RegisterEtcd(port)
	startMetricsServer()

	grpcAddr := fmt.Sprintf(":%s", config.Conf.Server.Port)
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		log.Fatalf("product listen failed addr=%s err=%v", grpcAddr, err)
	}
	s := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),

		//加上prometheus监控拦截器
		grpc.StreamInterceptor(grpc_prometheus.StreamServerInterceptor),
		grpc.UnaryInterceptor(grpc_prometheus.UnaryServerInterceptor),
	)

	pb.RegisterProductServiceServer(s, &server{})

	grpc_prometheus.Register(s)

	log.Printf("product service started addr=%s", grpcAddr)

	if err := s.Serve(lis); err != nil {
		log.Fatalf("product service serve failed: %v", err)
	}
}
