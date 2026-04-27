package main

import (
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	clientv3 "go.etcd.io/etcd/client/v3"
	resolver "go.etcd.io/etcd/client/v3/naming/resolver"

	"seckill-mall/common/config"
	"seckill-mall/common/pb"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
)

type grpcClients struct {
	product pb.ProductServiceClient
	order   pb.OrderServiceClient
}

func initGRPCClients() grpcClients {
	// 使用配置里的 Etcd 地址
	etcdAddr := config.Conf.Etcd.Addr

	// 初始化 Etcd 连接
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{etcdAddr}, // 使用配置变量
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		log.Fatalf("连接 Etcd 失败: %v", err)
	}
	etcdResolver, err := resolver.NewBuilder(cli)
	if err != nil {
		log.Fatalf("创建解析器失败: %v", err)
	}

	// 连接【商品服务】
	connProduct, err := grpc.Dial(
		"etcd:///seckill/product",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithResolvers(etcdResolver),
		grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"round_robin"}`),
	)
	if err != nil {
		log.Fatalf("无法连接商品服务: %v", err)
	}
	productClient := pb.NewProductServiceClient(connProduct)

	// 连接【订单服务】
	connOrder, err := grpc.Dial(
		"etcd:///seckill/order",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithResolvers(etcdResolver),
		grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"round_robin"}`),
	)
	if err != nil {
		log.Fatalf("无法连接订单服务: %v", err)
	}
	orderClient := pb.NewOrderServiceClient(connOrder)

	return grpcClients{
		product: productClient,
		order:   orderClient,
	}
}
