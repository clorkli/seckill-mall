package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"seckill-mall/common/config"
	"seckill-mall/common/tracer"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

const (
	OrderQueue = "seckill_order_queue"

	DeadExchange   = "dlx_exchange" // 死信交换机
	DeadQueue      = "dead_queue"   // 死信队列
	DeadRoutingKey = "dead_key"     // 死信路由键

	ReconnectDelay = 3 * time.Second
)

const (
	OrderStatusPending int32 = 0
	OrderStatusSuccess int32 = 1
	OrderStatusFailed  int32 = 2
)

// 初始化队列系统
func setupQueue(ch *amqp.Channel) (amqp.Queue, error) {
	//声明死信交换机
	err := ch.ExchangeDeclare(DeadExchange, "direct", true, false, false, false, nil)
	if err != nil {
		return amqp.Queue{}, fmt.Errorf("无法声明死信交换机: %w", err)
	}

	//声明死信队列
	_, err = ch.QueueDeclare(DeadQueue, true, false, false, false, nil)
	if err != nil {
		return amqp.Queue{}, fmt.Errorf("无法声明死信队列: %w", err)
	}

	//绑定：死信交换机 -> 死信队列
	err = ch.QueueBind(DeadQueue, DeadRoutingKey, DeadExchange, false, nil)
	if err != nil {
		return amqp.Queue{}, fmt.Errorf("无法绑定死信队列: %w", err)
	}

	//声明主队列（业务队列），并配置它“连接”到死信交换机
	args := amqp.Table{
		"x-dead-letter-exchange":    DeadExchange,   // 报错后发给谁？
		"x-dead-letter-routing-key": DeadRoutingKey, // 带什么暗号发？
	}

	q, err := ch.QueueDeclare(
		OrderQueue,
		true,
		false,
		false,
		false,
		args, //把死信参数传进去
	)
	if err != nil {
		return amqp.Queue{}, fmt.Errorf("无法声明主队列(可能参数冲突，请先去后台删除旧队列): %w", err)
	}

	log.Printf("✅ RabbitMQ 队列结构初始化完成：主队列[%s] -> 死信[%s]", OrderQueue, DeadQueue)
	return q, nil
}

// 对应数据库结构
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

// MQ 消息结构
type OrderMessage struct {
	OrderID   string  `json:"order_id"`
	UserID    int64   `json:"user_id"`
	ProductID int64   `json:"product_id"`
	Count     int32   `json:"count"`
	Amount    float32 `json:"amount"`
}

var db *gorm.DB

var errMySQLStockNotEnough = errors.New("mysql stock not enough")
var errOrderAlreadyFinished = errors.New("order already finished")

var (
	mqConsumeTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_mq_consume_total",
			Help: "Total number of RabbitMQ messages consumed by queue and result.",
		},
		[]string{"queue", "result"},
	)
	mqConsumeDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "seckill_mq_consume_duration_seconds",
			Help:    "RabbitMQ message handling duration in seconds by queue.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"queue"},
	)
	mqConsumerAckTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_mq_consumer_ack_total",
			Help: "Total number of RabbitMQ consumer acks by queue.",
		},
		[]string{"queue"},
	)
	mqConsumerNackTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_mq_consumer_nack_total",
			Help: "Total number of RabbitMQ consumer nacks by queue and requeue flag.",
		},
		[]string{"queue", "requeue"},
	)
	mqConsumerReconnectTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_mq_consumer_reconnect_total",
			Help: "Total number of RabbitMQ consumer reconnect attempts by consumer.",
		},
		[]string{"consumer"},
	)
)

func persistOrder(ctx context.Context, msg OrderMessage) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var order Order
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("order_id = ?", msg.OrderID).
			First(&order).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return persistLegacyOrder(tx, msg)
		}
		if err != nil {
			return err
		}

		if order.Status == OrderStatusSuccess || order.Status == OrderStatusFailed {
			return errOrderAlreadyFinished
		}
		if order.Status != OrderStatusPending {
			return fmt.Errorf("未知订单状态: order_id=%s status=%d", msg.OrderID, order.Status)
		}

		if err := decrementMySQLStock(tx, msg); err != nil {
			return err
		}

		result := tx.Model(&Order{}).
			Where("order_id = ? AND status = ?", msg.OrderID, OrderStatusPending).
			Updates(map[string]any{
				"product_id":  msg.ProductID,
				"count":       msg.Count,
				"amount":      msg.Amount,
				"status":      OrderStatusSuccess,
				"fail_reason": "",
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errOrderAlreadyFinished
		}

		return nil
	})
}

func decrementMySQLStock(tx *gorm.DB, msg OrderMessage) error {
	result := tx.Exec(
		"UPDATE product SET stock = stock - ? WHERE id = ? AND stock >= ?",
		msg.Count,
		msg.ProductID,
		msg.Count,
	)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errMySQLStockNotEnough
	}

	return nil
}

func persistLegacyOrder(tx *gorm.DB, msg OrderMessage) error {
	if err := decrementMySQLStock(tx, msg); err != nil {
		return err
	}

	order := Order{
		OrderID:   msg.OrderID,
		UserID:    msg.UserID,
		ProductID: msg.ProductID,
		Count:     msg.Count,
		Amount:    msg.Amount,
		Status:    OrderStatusSuccess,
	}

	if err := tx.Create(&order).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return errOrderAlreadyFinished
		}
		return err
	}

	return nil
}

func runConsumer(mqURL string) error {
	conn, err := amqp.Dial(mqURL)
	if err != nil {
		return fmt.Errorf("连接RabbitMQ失败: %w", err)
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("创建MQ通道失败: %w", err)
	}
	defer ch.Close()

	// 2. 这里的 Qos 很重要，保证消费者不被撑死
	if err := ch.Qos(1, 0, false); err != nil {
		return fmt.Errorf("设置Qos失败: %w", err)
	}

	// 3. 调用 setupQueue 获取配置好 DLQ 的队列对象
	q, err := setupQueue(ch)
	if err != nil {
		return err
	}

	// 4. 监听这个正确的队列
	msgs, err := ch.Consume(
		q.Name, // 使用 setupQueue 返回的名字
		"",
		false, // Auto-Ack 必须为 false
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		return fmt.Errorf("启动消费失败: %w", err)
	}

	fmt.Println("📧 消费者服务已启动 (DLQ版)，等待订单中...")

	connClosed := conn.NotifyClose(make(chan *amqp.Error, 1))
	chClosed := ch.NotifyClose(make(chan *amqp.Error, 1))

	for {
		select {
		case d, ok := <-msgs:
			if !ok {
				return errors.New("RabbitMQ delivery channel 已关闭")
			}
			handleMessage(d)
		case err, ok := <-connClosed:
			if !ok || err == nil {
				return errors.New("RabbitMQ connection 已关闭")
			}
			return fmt.Errorf("RabbitMQ connection 异常关闭: %w", err)
		case err, ok := <-chClosed:
			if !ok || err == nil {
				return errors.New("RabbitMQ channel 已关闭")
			}
			return fmt.Errorf("RabbitMQ channel 异常关闭: %w", err)
		}
	}
}

func handleMessage(d amqp.Delivery) {
	start := time.Now()
	defer func() {
		mqConsumeDuration.WithLabelValues(OrderQueue).Observe(time.Since(start).Seconds())
	}()

	traceCtx := tracer.ExtractAMQPHeaders(context.Background(), d.Headers)
	traceCtx, span := otel.Tracer("mq-consumer").Start(traceCtx, "rabbitmq.consume_order")
	defer span.End()

	var msg OrderMessage
	if err := json.Unmarshal(d.Body, &msg); err != nil {
		log.Printf("❌ 消息格式错误，直接丢弃: %v", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid message json")
		mqConsumeTotal.WithLabelValues(OrderQueue, "invalid").Inc()
		mqConsumerNackTotal.WithLabelValues(OrderQueue, "false").Inc()
		d.Nack(false, false) // 这种一般不需要重试，直接进死信或丢弃
		return
	}

	if msg.Count <= 0 {
		log.Printf("❌ 订单数量非法，进入死信: order_id=%s count=%d", msg.OrderID, msg.Count)
		span.SetStatus(codes.Error, "invalid order count")
		mqConsumeTotal.WithLabelValues(OrderQueue, "invalid").Inc()
		mqConsumerNackTotal.WithLabelValues(OrderQueue, "false").Inc()
		d.Nack(false, false)
		return
	}

	fmt.Printf("📦 接收订单: %s | 数量：%d | 金额：%.2f | 处理中...", msg.OrderID, msg.Count, msg.Amount)

	// 模拟业务处理耗时
	time.Sleep(50 * time.Millisecond)

	// 写入订单并同步扣减 MySQL 商品库存，保证最终库存账本一致。
	err := persistOrder(traceCtx, msg)
	if err != nil {
		// 场景 A: 重复消费或已失败订单，幂等确认，避免重复扣减 product.stock。
		if errors.Is(err, errOrderAlreadyFinished) {
			fmt.Printf(" -> ⚠️ 订单已结束，确认消息\n")
			span.SetStatus(codes.Ok, "finished order acknowledged")
			mqConsumeTotal.WithLabelValues(OrderQueue, "duplicate").Inc()
			mqConsumerAckTotal.WithLabelValues(OrderQueue).Inc()
			d.Ack(false)
		} else if errors.Is(err, errMySQLStockNotEnough) {
			log.Printf(" -> ❌ MySQL库存不足，发送 Nack(不重回队列)->进入死信")
			span.RecordError(err)
			span.SetStatus(codes.Error, "mysql stock not enough")
			mqConsumeTotal.WithLabelValues(OrderQueue, "mysql_stock_not_enough").Inc()
			mqConsumerNackTotal.WithLabelValues(OrderQueue, "false").Inc()
			d.Nack(false, false)
		} else {
			// 场景 B: 真正的故障 (数据库挂了/网络抖动)
			log.Printf(" -> ❌ 落库失败: %v，发送 Nack(不重回队列)->进入死信", err)
			span.RecordError(err)
			span.SetStatus(codes.Error, "persist order failed")

			// 关键点：requeue=false + 配置了死信交换机 = 消息进入死信队列
			mqConsumeTotal.WithLabelValues(OrderQueue, "failed").Inc()
			mqConsumerNackTotal.WithLabelValues(OrderQueue, "false").Inc()
			d.Nack(false, false)
		}
	} else {
		// 场景 C: 成功
		fmt.Printf(" -> ✅ 落库成功\n")
		span.SetStatus(codes.Ok, "order persisted")
		mqConsumeTotal.WithLabelValues(OrderQueue, "success").Inc()
		mqConsumerAckTotal.WithLabelValues(OrderQueue).Inc()
		d.Ack(false)
	}
}

func startMetricsServer() {
	port := config.Conf.Server.MetricsPort
	if port == "" {
		port = "9093"
	}

	addr := net.JoinHostPort("", port)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	go func() {
		log.Printf("MQ Consumer metrics 已启动: %s/metrics", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("MQ Consumer metrics 启动失败: %v", err)
		}
	}()
}

func main() {
	shutdown := tracer.InitTracer("mq-consumer", "localhost:4318")
	defer shutdown(context.Background())

	config.InitConfig("mq")
	initDB()
	startMetricsServer()
	mqURL := config.Conf.MQ.URL
	if mqURL == "" {
		log.Fatal("mq.url 为空，请在 config/mq.yaml 设置或通过环境变量 SECKILL_MQ_URL 注入")
	}

	for {
		if err := runConsumer(mqURL); err != nil {
			log.Printf("主队列消费者停止: %v，%s 后重连", err, ReconnectDelay)
			mqConsumerReconnectTotal.WithLabelValues("order").Inc()
		}
		time.Sleep(ReconnectDelay)
	}
}

func initDB() {
	dsn := config.Conf.MySQL.DSN
	if dsn == "" {
		log.Fatal("mysql.dsn 为空，请在 config/mq.yaml 设置或通过环境变量 SECKILL_MYSQL_DSN 注入")
	}

	var err error
	db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("连接MySQL失败: %v", err)
	}
	// 表结构已固定，注释掉 AutoMigrate 防止改动
	// db.AutoMigrate(&Order{})
	fmt.Println("✅ MySQL 连接成功")
}
