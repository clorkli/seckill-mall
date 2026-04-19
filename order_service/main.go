package main

import (
	"context"
	"encoding/json"
	"errors"
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
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/naming/endpoints"
	resolver "go.etcd.io/etcd/client/v3/naming/resolver"

	"seckill-mall/common/config"
	"seckill-mall/common/pb"
	"seckill-mall/common/tracer"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	otelgrpc "go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

const (
	SERVICE_NAME = "seckill/order"

	PRODUCT_SERVICE_NAME = "etcd:///seckill/product"
	MQ_QUEUE_NAME        = "seckill_order_queue"
	DeadExchange         = "dlx_exchange" // 死信交换机
	DeadRoutingKey       = "dead_key"
)

const (
	OrderStatusPending int32 = 0
	OrderStatusSuccess int32 = 1
	OrderStatusFailed  int32 = 2
)

const (
	OutboxStatusPending int32 = 0
	OutboxStatusSent    int32 = 1
	OutboxStatusFailed  int32 = 2

	OutboxAggregateOrder    = "order"
	OutboxEventOrderCreated = "order.created"
)

// OrderMessage 是投递给 RabbitMQ 的订单消息。
type OrderMessage struct {
	OrderID   string  `json:"order_id"`
	UserID    int64   `json:"user_id"`
	ProductID int64   `json:"product_id"`
	Count     int32   `json:"count"`
	Amount    float32 `json:"amount"`
}

// Order 对应 orders 表。Count/UpdatedAt 用于后续订单状态闭环，旧表缺列时查询会保持零值。
type Order struct {
	ID         uint64    `gorm:"column:id;primaryKey;autoIncrement"`
	OrderID    string    `gorm:"column:order_id;uniqueIndex;not null"`
	UserID     int64     `gorm:"column:user_id;not null"`
	ProductID  int64     `gorm:"column:product_id;not null"`
	Count      int32     `gorm:"column:count"`
	Amount     float32   `gorm:"column:amount;not null"`
	Status     int32     `gorm:"column:status;default:0"`
	FailReason string    `gorm:"column:fail_reason"`
	CreatedAt  time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt  time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (Order) TableName() string { return "orders" }

// OutboxEvent 记录需要可靠投递到 MQ 的领域事件，后续由 outbox_worker 扫描发送。
type OutboxEvent struct {
	ID            uint64    `gorm:"column:id;primaryKey;autoIncrement"`
	EventID       string    `gorm:"column:event_id;uniqueIndex;not null"`
	AggregateType string    `gorm:"column:aggregate_type;not null"`
	AggregateID   string    `gorm:"column:aggregate_id;not null"`
	EventType     string    `gorm:"column:event_type;not null"`
	Payload       string    `gorm:"column:payload;type:json;not null"`
	Status        int32     `gorm:"column:status;default:0"`
	RetryCount    int       `gorm:"column:retry_count;default:0"`
	NextRetryAt   time.Time `gorm:"column:next_retry_at;not null"`
	LastError     string    `gorm:"column:last_error"`
	CreatedAt     time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt     time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (OutboxEvent) TableName() string { return "outbox_events" }

var productClient pb.ProductServiceClient
var mqPublisher *MQPublisher
var db *gorm.DB

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

func (p *MQPublisher) PublishOrder(ctx context.Context, body []byte, headers amqp.Table) (err error) {
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

	if err := p.publishLocked(ctx, body, headers); err == nil {
		return nil
	} else {
		log.Printf("MQ发布失败，尝试重连后重试一次: %v", err)
	}

	mqPublishRetryTotal.Inc()
	mqPublisherReconnectTotal.Inc()
	if err := p.connectLocked(); err != nil {
		return err
	}

	return p.publishLocked(ctx, body, headers)
}

func (p *MQPublisher) publishLocked(ctx context.Context, body []byte, headers amqp.Table) error {
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
			Headers:      headers,
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

func orderStatusText(status int32) string {
	switch status {
	case OrderStatusPending:
		return "排队中"
	case OrderStatusSuccess:
		return "已成功"
	case OrderStatusFailed:
		return "已失败"
	default:
		return "未知状态"
	}
}

func orderStatusMessage(status int32) string {
	switch status {
	case OrderStatusPending:
		return "订单排队处理中，请稍后查询"
	case OrderStatusSuccess:
		return "订单已成功创建"
	case OrderStatusFailed:
		return "订单处理失败"
	default:
		return "订单状态未知，请联系管理员核查"
	}
}

func orderCreatedEventID(orderID string) string {
	return "order.created:" + orderID
}

func truncateText(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}

func createPendingOrderWithOutbox(ctx context.Context, msg OrderMessage, payload []byte) error {
	order := Order{
		OrderID:   msg.OrderID,
		UserID:    msg.UserID,
		ProductID: msg.ProductID,
		Count:     msg.Count,
		Amount:    msg.Amount,
		Status:    OrderStatusPending,
	}
	event := OutboxEvent{
		EventID:       orderCreatedEventID(msg.OrderID),
		AggregateType: OutboxAggregateOrder,
		AggregateID:   msg.OrderID,
		EventType:     OutboxEventOrderCreated,
		Payload:       string(payload),
		Status:        OutboxStatusPending,
		NextRetryAt:   time.Now(),
	}

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&order).Error; err != nil {
			return err
		}
		if err := tx.Create(&event).Error; err != nil {
			return err
		}

		return nil
	})
}

func markOutboxSent(ctx context.Context, orderID string) error {
	return db.WithContext(ctx).
		Model(&OutboxEvent{}).
		Where("event_id = ? AND status = ?", orderCreatedEventID(orderID), OutboxStatusPending).
		Updates(map[string]any{
			"status":     OutboxStatusSent,
			"last_error": "",
		}).Error
}

func markOutboxFailed(ctx context.Context, orderID string, err error) error {
	return db.WithContext(ctx).
		Model(&OutboxEvent{}).
		Where("event_id = ? AND status = ?", orderCreatedEventID(orderID), OutboxStatusPending).
		Updates(map[string]any{
			"status":     OutboxStatusFailed,
			"last_error": truncateText(err.Error(), 255),
		}).Error
}

func markOrderFailed(ctx context.Context, orderID string, userID int64, reason string) error {
	return db.WithContext(ctx).
		Model(&Order{}).
		Where("order_id = ? AND user_id = ? AND status = ?", orderID, userID, OrderStatusPending).
		Updates(map[string]any{
			"status":      OrderStatusFailed,
			"fail_reason": truncateText(reason, 255),
		}).Error
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

func markPendingOrderFailed(orderID string, userID int64, reason string) {
	updateCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := markOrderFailed(updateCtx, orderID, userID, reason); err != nil {
		log.Printf("X! 标记订单失败状态失败，请人工介入: order_id=%s err=%v", orderID, err)
	}
}

func markOutboxEventFailed(orderID string, err error) {
	updateCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if errUpdate := markOutboxFailed(updateCtx, orderID, err); errUpdate != nil {
		log.Printf("X! 标记Outbox事件失败状态失败，请人工介入: order_id=%s err=%v", orderID, errUpdate)
	}
}

func rollbackStockAndMarkFailed(orderID string, productID, userID int64, count int32, reason string) {
	if err := rollbackStock(productID, userID, count); err != nil {
		log.Printf("X! %s 且回滚库存失败，请人工介入，CRITICAL ERROR: %v", reason, err)
		markPendingOrderFailed(orderID, userID, reason+"，Redis回滚失败")
		return
	}

	log.Printf("库存回滚成功: order_id=%s", orderID)
	markPendingOrderFailed(orderID, userID, reason)
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
		rollbackStockAndMarkFailed("", req.ProductId, req.UserId, req.Count, "查询商品失败")
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
		rollbackStockAndMarkFailed(orderID, req.ProductId, req.UserId, req.Count, "序列化订单消息失败")
		return nil, fmt.Errorf("序列化订单消息失败: %w", err)
	}

	if err := createPendingOrderWithOutbox(ctx, orderMsg, body); err != nil {
		rollbackStockAndMarkFailed(orderID, req.ProductId, req.UserId, req.Count, "创建排队订单和Outbox事件失败")
		return nil, fmt.Errorf("创建排队订单和Outbox事件失败: %w", err)
	}

	// 发送消息到 RabbitMQ
	publishTraceCtx, publishSpan := otel.Tracer("order-service").Start(ctx, "rabbitmq.publish_order")
	headers := tracer.InjectAMQPHeaders(publishTraceCtx, nil)
	defer publishSpan.End()

	publishCtx, cancelPublish := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPublish()

	err = mqPublisher.PublishOrder(publishCtx, body, headers)

	//发MQ失败应该回滚Redis库存，这里先打日志
	if err != nil {
		publishSpan.RecordError(err)
		publishSpan.SetStatus(codes.Error, "publish order message failed")
		log.Printf("发送MQ失败: %v，正在执行回滚...", err)
	} else {
		publishSpan.SetStatus(codes.Ok, "publish order message confirmed")
	}

	if err != nil {
		rollbackStockAndMarkFailed(orderID, req.ProductId, req.UserId, req.Count, "MQ发送失败")
		markOutboxEventFailed(orderID, err)
		return nil, fmt.Errorf("系统繁忙，请稍后重试")
	}

	updateOutboxCtx, cancelUpdateOutbox := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelUpdateOutbox()
	if err := markOutboxSent(updateOutboxCtx, orderID); err != nil {
		log.Printf("标记Outbox事件已发送失败，后续Worker可能重复投递: order_id=%s err=%v", orderID, err)
	}

	fmt.Printf("下单请求已发送到MQ，订单ID: %s\n", orderID)

	return &pb.CreateOrderResponse{
		OrderId: orderID,
		Success: true,
		Message: "排队中，请稍后查询结果",
	}, nil
}

// GetOrder 查询当前用户自己的订单状态。
func (s *server) GetOrder(ctx context.Context, req *pb.GetOrderRequest) (*pb.GetOrderResponse, error) {
	if req.OrderId == "" {
		return &pb.GetOrderResponse{
			Found:   false,
			Message: "订单号不能为空",
		}, nil
	}
	if req.UserId <= 0 {
		return &pb.GetOrderResponse{
			Found:   false,
			Message: "用户ID非法",
		}, nil
	}

	var order Order
	err := db.WithContext(ctx).
		Where("order_id = ? AND user_id = ?", req.OrderId, req.UserId).
		First(&order).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &pb.GetOrderResponse{
			Found:   false,
			OrderId: req.OrderId,
			Message: "订单不存在",
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询订单失败: %w", err)
	}

	message := orderStatusMessage(order.Status)
	if order.Status == OrderStatusFailed && order.FailReason != "" {
		message = order.FailReason
	}

	return &pb.GetOrderResponse{
		Found:      true,
		OrderId:    order.OrderID,
		UserId:     order.UserID,
		ProductId:  order.ProductID,
		Count:      order.Count,
		Amount:     order.Amount,
		Status:     order.Status,
		StatusText: orderStatusText(order.Status),
		Message:    message,
	}, nil
}

// 初始化MySQL连接
func initDB() {
	dsn := config.Conf.MySQL.DSN
	if dsn == "" {
		log.Fatalf("mysql.dsn 为空，请在 config/order.yaml 设置或通过环境变量 SECKILL_MYSQL_DSN 注入")
	}

	var err error
	db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("连接MySQL失败: %v", err)
	}

	fmt.Println("已连接到 MySQL (Order Query Ready)")
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

	initDB()
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
