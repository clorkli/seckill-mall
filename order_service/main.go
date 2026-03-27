package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	amqp "github.com/rabbitmq/amqp091-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/naming/endpoints"
	resolver "go.etcd.io/etcd/client/v3/naming/resolver"

	"seckill-mall/common/config"
	"seckill-mall/common/pb"
	"seckill-mall/common/tracer"

	"net/http"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	otelgrpc "go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
)

const (
	SERVICE_NAME = "seckill/order"

	PRODUCT_SERVICE_NAME = "etcd:///seckill/product"
	MQ_QUEUE_NAME        = "seckill_order_queue"
	DeadExchange         = "dlx_exchange" // 死信交换机
	DeadRoutingKey       = "dead_key"
)

// 数据库模型
type OrderMessage struct {
	OrderID   string  `json:"order_id"`
	UserID    int64   `json:"user_id"`
	ProductID int64   `json:"product_id"`
	Amount    float32 `json:"amount"`
}

var productClient pb.ProductServiceClient
var mqChannel *amqp.Channel //全局MQ通道

type server struct {
	pb.UnimplementedOrderServiceServer
}

// CreateOrder 下单逻辑 (异步版)
func (s *server) CreateOrder(ctx context.Context, req *pb.CreateOrderRequest) (*pb.CreateOrderResponse, error) {
	fmt.Printf("收到下单请求，用户: %d, 商品: %d\n", req.UserId, req.ProductId)

	//扣减 Redis 库存作为防超卖第一道防线
	deductResp, err := productClient.DeductStock(ctx, &pb.DeductStockRequest{
		ProductId: req.ProductId,
		Count:     req.Count,
		UserId:    req.UserId, //新增用户ID字段防止重复购买
	})
	if err != nil {
		return nil, fmt.Errorf("调用商品服务失败: %v", err)
	}

	if !deductResp.Success {
		fmt.Printf("库存不足，秒杀失败\n")
		return &pb.CreateOrderResponse{
			Success: false,
			Message: deductResp.Message,
		}, nil
	}

	// 查价格,计算总金额
	pResp, err := productClient.GetProduct(ctx, &pb.ProductRequest{ProductId: req.ProductId})
	if err != nil {
		return nil, err
	}

	totalAmount := pResp.Price * float32(req.Count)
	orderID := fmt.Sprintf("%d%d", time.Now().UnixNano(), rand.Intn(1000))

	orderMsg := OrderMessage{
		OrderID:   orderID,
		UserID:    req.UserId,
		ProductID: req.ProductId,
		Amount:    totalAmount,
	}

	body, _ := json.Marshal(orderMsg)

	// 发送消息到 RabbitMQ
	err = mqChannel.PublishWithContext(ctx,
		"",            //默认交换机
		MQ_QUEUE_NAME, //队列名
		false,
		false,
		amqp.Publishing{
			ContentType: "application/json",
			Body:        body,
		},
	)

	//发MQ失败应该回滚Redis库存，这里先打日志
	if err != nil {
		log.Printf("发送MQ失败: %v，正在执行回滚...", err)

		//使用新Context避免因超时导致回滚被取消
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		_, errRb := productClient.RollbackStock(rollbackCtx, &pb.DeductStockRequest{
			ProductId: req.ProductId,
			Count:     req.Count,
		})

		if errRb != nil {
			log.Printf("X! MQ发送失败且回滚库存失败，请人工介入，CRITICAL ERROR: %v", errRb)
		} else {
			log.Printf("库存回滚成功")
		}

		return nil, fmt.Errorf("系统繁忙，请稍后重试")
	}

	fmt.Printf("下单请求已发送到MQ，订单ID: %s\n", orderID)

	return &pb.CreateOrderResponse{
		OrderId: orderID,
		Success: true,
		Message: "排队中，请稍后查询结果",
	}, nil
}

// 初始化RabbitMQ连接
func initMQ() {
	mqURL := config.Conf.MQ.URL
	if mqURL == "" {
		log.Fatalf("mq.url 为空，请在 config/order.yaml 设置或通过环境变量 SECKILL_MQ_URL 注入")
	}

	conn, err := amqp.Dial(mqURL)
	if err != nil {
		log.Fatalf("连接RabbitMQ失败: %v", err)
	}

	mqChannel, err = conn.Channel()
	if err != nil {
		log.Fatalf("创建MQ通道失败: %v", err)
	}

	args := amqp.Table{
		"x-dead-letter-exchange":    DeadExchange,   // 报错后发给谁？
		"x-dead-letter-routing-key": DeadRoutingKey, // 带什么暗号发？
	}

	_, err = mqChannel.QueueDeclare(
		MQ_QUEUE_NAME,
		true, //持久化确保重启后队列还在
		false,
		false,
		false,
		args,
	)
	if err != nil {
		log.Fatalf("声明队列失败: %v", err)
	}

	fmt.Println("已连接到 RabbitMQ (MQ Ready)")
}

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
	fmt.Println("已连接到商品服务 (RPC Client Ready)")
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
	fmt.Printf("✅ 订单服务已注册到 Etcd: %s\n", serviceAddr)
}

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

	initMQ()
	initProductClient()
	registerEtcd(myAddr)

	//启动Prometheus监控(Port:9092)
	go func() {
		metricsAddr := fmt.Sprintf(":%s", config.Conf.Server.MetricsPort)
		http.Handle("/metrics", promhttp.Handler())
		fmt.Printf("订单服务监控已启动 %s/metrics\n", metricsAddr)

		if err := http.ListenAndServe(metricsAddr, nil); err != nil {
			fmt.Printf("启动订单服务监控失败: %v", err) //这里没选择挂掉主服务
		}
	}()

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
