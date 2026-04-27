package main

import (
	"context"
	"errors"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	clientv3 "go.etcd.io/etcd/client/v3"
	resolver "go.etcd.io/etcd/client/v3/naming/resolver"

	"seckill-mall/common/config"
	"seckill-mall/common/pb"

	otelgrpc "go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
)

var productClient pb.ProductServiceClient

// 初始化Product Client
func initProductClient() {
	etcdAddr := config.Conf.Etcd.Addr

	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{etcdAddr},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		log.Fatal(err)
	}

	etcdResolver, err := resolver.NewBuilder(cli)
	if err != nil {
		log.Fatal(err)
	}

	conn, err := grpc.Dial(
		PRODUCT_SERVICE_NAME,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithResolvers(etcdResolver),

		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),

		grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"round_robin"}`),
	)
	if err != nil {
		log.Fatal(err)
	}

	productClient = pb.NewProductServiceClient(conn)
	log.Println("product service grpc client ready")
}

func rollbackStock(productID, userID int64, count int32) error {
	rollbackCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	rollbackResp, err := productClient.RollbackStock(rollbackCtx, &pb.DeductStockRequest{
		ProductId: productID,
		Count:     count,
		UserId:    userID,
	})
	if err != nil {
		return err
	}
	if !rollbackResp.Success {
		return errors.New(rollbackResp.Message)
	}

	return nil
}
