package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
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
	Count     int32   `json:"count"`
	Amount    float32 `json:"amount"`
}

var productClient pb.ProductServiceClient
var mqPublisher *MQPublisher

var (
	mqPublishTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_mq_publish_total",
			Help: "Total number of RabbitMQ publish attempts by result.",
		},
		[]string{"result"},
	)
	mqPublishRetryTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "seckill_mq_publish_retry_total",
			Help: "Total number of RabbitMQ publish retries.",
		},
	)
	mqPublishReturnTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "seckill_mq_publish_return_total",
			Help: "Total number of RabbitMQ returned messages.",
		},
	)
	mqPublishConfirmNackTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "seckill_mq_publish_confirm_nack_total",
			Help: "Total number of RabbitMQ publisher confirm nacks.",
		},
	)
	mqPublisherReconnectTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "seckill_mq_publisher_reconnect_total",
			Help: "Total number of RabbitMQ publisher reconnect attempts.",
		},
	)
	mqPublishDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "seckill_mq_publish_duration_seconds",
			Help:    "RabbitMQ publish duration in seconds.",
			Buckets: prometheus.DefBuckets,
		},
	)
)

type MQPublisher struct {
	mu       sync.Mutex
	url      string
	conn     *amqp.Connection
	channel  *amqp.Channel
	confirms <-chan amqp.Confirmation
	returns  <-chan amqp.Return
}

func NewMQPublisher(url string) *MQPublisher {
	return &MQPublisher{url: url}
}

func (p *MQPublisher) Connect() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.connectLocked()
}

func (p *MQPublisher) connectLocked() error {
	p.closeLocked()

	conn, err := amqp.Dial(p.url)
	if err != nil {
		return fmt.Errorf("连接RabbitMQ失败: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return fmt.Errorf("创建MQ通道失败: %w", err)
	}

	args := amqp.Table{
		"x-dead-letter-exchange":    DeadExchange,
		"x-dead-letter-routing-key": DeadRoutingKey,
	}

	if _, err := ch.QueueDeclare(MQ_QUEUE_NAME, true, false, false, false, args); err != nil {
		ch.Close()
		conn.Close()
		return fmt.Errorf("声明队列失败: %w", err)
	}

	if err := ch.Confirm(false); err != nil {
		ch.Close()
		conn.Close()
		return fmt.Errorf("开启Publisher Confirm失败: %w", err)
	}

	p.conn = conn
	p.channel = ch
	p.confirms = ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	p.returns = ch.NotifyReturn(make(chan amqp.Return, 1))

	return nil
}

func (p *MQPublisher) closeLocked() {
	if p.channel != nil {
		_ = p.channel.Close()
		p.channel = nil
	}
	if p.conn != nil {
		_ = p.conn.Close()
		p.conn = nil
	}
	p.confirms = nil
	p.returns = nil
}

func (p *MQPublisher) PublishOrder(ctx context.Context, body []byte) (err error) {
	start := time.Now()
	defer func() {
		mqPublishDuration.Observe(time.Since(start).Seconds())
		if err != nil {
			mqPublishTotal.WithLabelValues("failed").Inc()
		} else {
			mqPublishTotal.WithLabelValues("success").Inc()
		}
	}()

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.publishLocked(ctx, body); err == nil {
		return nil
	} else {
		log.Printf("MQ发布失败，尝试重连后重试一次: %v", err)
	}

	mqPublishRetryTotal.Inc()
	mqPublisherReconnectTotal.Inc()
	if err := p.connectLocked(); err != nil {
		return err
	}

	return p.publishLocked(ctx, body)
}

func (p *MQPublisher) publishLocked(ctx context.Context, body []byte) error {
	if p.channel == nil {
		if err := p.connectLocked(); err != nil {
			return err
		}
	}

	if err := p.channel.PublishWithContext(
		ctx,
		"",
		MQ_QUEUE_NAME,
		true,
		false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Timestamp:    time.Now(),
			Body:         body,
		},
	); err != nil {
		return fmt.Errorf("发布消息失败: %w", err)
	}

	select {
	case ret, ok := <-p.returns:
		if !ok {
			return fmt.Errorf("RabbitMQ return channel 已关闭")
		}
		mqPublishReturnTotal.Inc()
		return fmt.Errorf("消息无法路由: reply_code=%d reply_text=%s exchange=%s routing_key=%s", ret.ReplyCode, ret.ReplyText, ret.Exchange, ret.RoutingKey)
	case confirm, ok := <-p.confirms:
		if !ok {
			return fmt.Errorf("RabbitMQ confirm channel 已关闭")
		}
		if !confirm.Ack {
			mqPublishConfirmNackTotal.Inc()
			return fmt.Errorf("RabbitMQ Nack delivery_tag=%d", confirm.DeliveryTag)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("等待RabbitMQ确认超时或取消: %w", ctx.Err())
	}
}

type server struct {
	pb.UnimplementedOrderServiceServer
}

// CreateOrder 下单逻辑 (异步版)
func (s *server) CreateOrder(ctx context.Context, req *pb.CreateOrderRequest) (*pb.CreateOrderResponse, error) {
	fmt.Printf("收到下单请求，用户: %d, 商品: %d\n", req.UserId, req.ProductId)

	if req.Count <= 0 {
		return &pb.CreateOrderResponse{
			Success: false,
			Message: "购买数量必须大于0",
		}, nil
	}

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
		Count:     req.Count,
		Amount:    totalAmount,
	}

	body, err := json.Marshal(orderMsg)
	if err != nil {
		return nil, fmt.Errorf("序列化订单消息失败: %w", err)
	}

	// 发送消息到 RabbitMQ
	publishCtx, cancelPublish := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPublish()

	err = mqPublisher.PublishOrder(publishCtx, body)

	//发MQ失败应该回滚Redis库存，这里先打日志
	if err != nil {
		log.Printf("发送MQ失败: %v，正在执行回滚...", err)

		//使用新Context避免因超时导致回滚被取消
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		rollbackResp, errRb := productClient.RollbackStock(rollbackCtx, &pb.DeductStockRequest{
			ProductId: req.ProductId,
			Count:     req.Count,
			UserId:    req.UserId,
		})

		if errRb != nil {
			log.Printf("X! MQ发送失败且回滚库存失败，请人工介入，CRITICAL ERROR: %v", errRb)
		} else if !rollbackResp.Success {
			log.Printf("X! MQ发送失败且回滚库存失败，请人工介入，CRITICAL ERROR: %s", rollbackResp.Message)
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

	mqPublisher = NewMQPublisher(mqURL)
	if err := mqPublisher.Connect(); err != nil {
		log.Fatalf("初始化RabbitMQ发布器失败: %v", err)
	}

	fmt.Println("已连接到 RabbitMQ (MQ Publisher Ready)")
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
