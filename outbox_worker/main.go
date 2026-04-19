package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
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

	DeadExchange   = "dlx_exchange"
	DeadQueue      = "dead_queue"
	DeadRoutingKey = "dead_key"
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
)

const (
	BatchSize       = 10
	MaxRetryCount   = 5
	ScanInterval    = 2 * time.Second
	ClaimVisibility = 30 * time.Second
	MaxRetryDelay   = 2 * time.Minute
)

const ROLLBACK_LUA_SCRIPT = `
-- KEYS[1]: product:stock:{product_id}
-- KEYS[2]: product:users:{product_id}
-- KEYS[3]: order:rollback:{order_id}
-- ARGV[1]: rollback count
-- ARGV[2]: user_id

if redis.call("EXISTS", KEYS[3]) == 1 then
	return 3
end

if redis.call("EXISTS", KEYS[1]) == 0 then
	return 0
end

local rollback_count = tonumber(ARGV[1])
if rollback_count <= 0 then
	return 2
end

local current_buy = tonumber(redis.call("hget", KEYS[2], ARGV[2])) or 0
if current_buy == 0 then
	redis.call("set", KEYS[3], "no_purchase_record")
	return 4
end

if current_buy < rollback_count then
	return 5
end

redis.call("incrby", KEYS[1], rollback_count)
if current_buy == rollback_count then
	redis.call("hdel", KEYS[2], ARGV[2])
else
	redis.call("hincrby", KEYS[2], ARGV[2], -rollback_count)
end

redis.call("set", KEYS[3], "rolled_back")
return 1
`

type OutboxEvent struct {
	ID            uint64    `gorm:"column:id;primaryKey;autoIncrement"`
	EventID       string    `gorm:"column:event_id;uniqueIndex;not null"`
	AggregateType string    `gorm:"column:aggregate_type;not null"`
	AggregateID   string    `gorm:"column:aggregate_id;not null"`
	EventType     string    `gorm:"column:event_type;not null"`
	Payload       string    `gorm:"column:payload;type:json;not null"`
	Headers       string    `gorm:"column:headers;type:json"`
	Status        int32     `gorm:"column:status;default:0"`
	RetryCount    int       `gorm:"column:retry_count;default:0"`
	NextRetryAt   time.Time `gorm:"column:next_retry_at;not null"`
	LastError     string    `gorm:"column:last_error"`
	CreatedAt     time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt     time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (OutboxEvent) TableName() string { return "outbox_events" }

type Order struct {
	OrderID    string `gorm:"column:order_id"`
	Status     int32  `gorm:"column:status"`
	FailReason string `gorm:"column:fail_reason"`
}

func (Order) TableName() string { return "orders" }

type OrderMessage struct {
	OrderID   string  `json:"order_id"`
	UserID    int64   `json:"user_id"`
	ProductID int64   `json:"product_id"`
	Count     int32   `json:"count"`
	Amount    float32 `json:"amount"`
}

type MQPublisher struct {
	url      string
	conn     *amqp.Connection
	channel  *amqp.Channel
	confirms <-chan amqp.Confirmation
	returns  <-chan amqp.Return
}

var db *gorm.DB
var rdb *redis.Client

var (
	outboxPendingGauge = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "seckill_outbox_pending_events",
			Help: "Current number of pending outbox events.",
		},
	)
	outboxScanTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_outbox_scan_total",
			Help: "Total number of outbox scan attempts by result.",
		},
		[]string{"result"},
	)
	outboxClaimedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "seckill_outbox_claimed_total",
			Help: "Total number of outbox events claimed for processing.",
		},
	)
	outboxProcessDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "seckill_outbox_process_duration_seconds",
			Help:    "Outbox event processing duration in seconds by result.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"result"},
	)
	outboxPublishTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_outbox_publish_total",
			Help: "Total number of outbox RabbitMQ publish attempts by result.",
		},
		[]string{"result"},
	)
	outboxPublishDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "seckill_outbox_publish_duration_seconds",
			Help:    "Outbox RabbitMQ publish duration in seconds by result.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"result"},
	)
	outboxRetryTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_outbox_retry_total",
			Help: "Total number of outbox retries scheduled by reason.",
		},
		[]string{"reason"},
	)
	outboxCompensationTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seckill_outbox_compensation_total",
			Help: "Total number of outbox final Redis compensation attempts by result.",
		},
		[]string{"result"},
	)
	outboxReconnectTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "seckill_outbox_reconnect_total",
			Help: "Total number of RabbitMQ reconnect attempts made by the outbox worker.",
		},
	)
)

func NewMQPublisher(url string) *MQPublisher {
	return &MQPublisher{url: url}
}

func (p *MQPublisher) Connect() error {
	p.close()

	conn, err := amqp.Dial(p.url)
	if err != nil {
		return fmt.Errorf("连接RabbitMQ失败: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return fmt.Errorf("创建RabbitMQ通道失败: %w", err)
	}

	if err := setupQueues(ch); err != nil {
		ch.Close()
		conn.Close()
		return err
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

func (p *MQPublisher) Publish(ctx context.Context, payload []byte, headers amqp.Table) error {
	if err := p.publish(ctx, payload, headers); err == nil {
		return nil
	} else {
		log.Printf("Outbox发布MQ失败，尝试重连后重试一次: %v", err)
	}

	outboxReconnectTotal.Inc()
	if err := p.Connect(); err != nil {
		return err
	}
	if err := p.publish(ctx, payload, headers); err != nil {
		p.close()
		return err
	}
	return nil
}

func (p *MQPublisher) publish(ctx context.Context, payload []byte, headers amqp.Table) error {
	if p.channel == nil {
		if err := p.Connect(); err != nil {
			return err
		}
	}

	if err := p.channel.PublishWithContext(
		ctx,
		"",
		OrderQueue,
		true,
		false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Headers:      headers,
			Timestamp:    time.Now(),
			Body:         payload,
		},
	); err != nil {
		return fmt.Errorf("发布消息失败: %w", err)
	}

	select {
	case ret, ok := <-p.returns:
		if !ok {
			return errors.New("RabbitMQ return channel 已关闭")
		}
		return formatReturnedMessage(ret)
	case confirm, ok := <-p.confirms:
		if !ok {
			return errors.New("RabbitMQ confirm channel 已关闭")
		}
		if !confirm.Ack {
			return fmt.Errorf("RabbitMQ Nack delivery_tag=%d", confirm.DeliveryTag)
		}
		if ret, ok := readReturnedMessage(p.returns); ok {
			return formatReturnedMessage(ret)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("等待RabbitMQ确认超时或取消: %w", ctx.Err())
	}
}

func readReturnedMessage(returns <-chan amqp.Return) (amqp.Return, bool) {
	select {
	case ret, ok := <-returns:
		return ret, ok
	default:
		return amqp.Return{}, false
	}
}

func formatReturnedMessage(ret amqp.Return) error {
	return fmt.Errorf("消息无法路由: reply_code=%d reply_text=%s exchange=%s routing_key=%s", ret.ReplyCode, ret.ReplyText, ret.Exchange, ret.RoutingKey)
}

func (p *MQPublisher) close() {
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

func setupQueues(ch *amqp.Channel) error {
	if err := ch.ExchangeDeclare(DeadExchange, "direct", true, false, false, false, nil); err != nil {
		return fmt.Errorf("声明死信交换机失败: %w", err)
	}

	if _, err := ch.QueueDeclare(DeadQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("声明死信队列失败: %w", err)
	}

	if err := ch.QueueBind(DeadQueue, DeadRoutingKey, DeadExchange, false, nil); err != nil {
		return fmt.Errorf("绑定死信队列失败: %w", err)
	}

	args := amqp.Table{
		"x-dead-letter-exchange":    DeadExchange,
		"x-dead-letter-routing-key": DeadRoutingKey,
	}
	if _, err := ch.QueueDeclare(OrderQueue, true, false, false, false, args); err != nil {
		return fmt.Errorf("声明主队列失败: %w", err)
	}

	return nil
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
	log.Println("MySQL 连接成功")
}

func initRedis() {
	rdb = redis.NewClient(&redis.Options{
		Addr:     config.Conf.Redis.Addr,
		Password: config.Conf.Redis.Password,
		DB:       config.Conf.Redis.DB,
	})

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("连接Redis失败: %v", err)
	}
	log.Println("Redis 连接成功")
}

func claimPendingEvents(ctx context.Context, limit int) ([]OutboxEvent, error) {
	var events []OutboxEvent
	now := time.Now()
	claimUntil := now.Add(ClaimVisibility)

	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ? AND next_retry_at <= ?", OutboxStatusPending, now).
			Order("next_retry_at ASC, id ASC").
			Limit(limit).
			Find(&events).Error; err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}

		ids := make([]uint64, 0, len(events))
		for _, event := range events {
			ids = append(ids, event.ID)
		}

		return tx.Model(&OutboxEvent{}).
			Where("id IN ? AND status = ?", ids, OutboxStatusPending).
			Update("next_retry_at", claimUntil).Error
	})
	if err != nil {
		return nil, err
	}

	return events, nil
}

func updateOutboxPendingGauge(ctx context.Context) {
	var count int64
	if err := db.WithContext(ctx).
		Model(&OutboxEvent{}).
		Where("status = ?", OutboxStatusPending).
		Count(&count).Error; err != nil {
		log.Printf("统计Outbox待投递事件失败: %v", err)
		return
	}

	outboxPendingGauge.Set(float64(count))
}

func processEvent(ctx context.Context, publisher *MQPublisher, event OutboxEvent) {
	start := time.Now()
	processResult := "unknown"
	defer func() {
		outboxProcessDuration.WithLabelValues(processResult).Observe(time.Since(start).Seconds())
	}()

	order, exists, err := getOrder(ctx, event.AggregateID)
	if err != nil {
		log.Printf("查询订单失败，延迟重试Outbox事件: event_id=%s err=%v", event.EventID, err)
		processResult = "query_order_failed"
		scheduleRetry(ctx, event, err)
		return
	}
	if !exists {
		processResult = "order_missing"
		markEventFailed(ctx, event, "订单不存在，Outbox事件终止")
		return
	}
	if order.Status == OrderStatusSuccess {
		processResult = "order_success"
		if err := markEventSent(ctx, event.EventID); err != nil {
			log.Printf("订单已成功但标记Outbox已发送失败: event_id=%s err=%v", event.EventID, err)
			processResult = "mark_sent_failed"
		}
		return
	}
	if order.Status == OrderStatusFailed {
		processResult = "order_failed"
		markEventFailed(ctx, event, "订单已失败，Outbox事件终止")
		return
	}

	msg, err := parseOrderMessage(event.Payload)
	if err != nil {
		log.Printf("Outbox事件payload非法，终止事件: event_id=%s err=%v", event.EventID, err)
		processResult = "invalid_payload"
		markEventAndOrderFailed(ctx, event, "Outbox事件payload非法: "+err.Error())
		return
	}

	headers, err := parseHeaders(event.Headers)
	if err != nil {
		log.Printf("Outbox事件headers非法，将不带父trace继续发布: event_id=%s err=%v", event.EventID, err)
		headers = amqp.Table{}
	}

	traceCtx := tracer.ExtractAMQPHeaders(ctx, headers)
	traceCtx, span := otel.Tracer("outbox-worker").Start(traceCtx, "outbox.publish_order")
	defer span.End()
	headers = tracer.InjectAMQPHeaders(traceCtx, headers)

	publishCtx, cancel := context.WithTimeout(traceCtx, 5*time.Second)
	defer cancel()

	publishStart := time.Now()
	if err := publisher.Publish(publishCtx, []byte(event.Payload), headers); err != nil {
		log.Printf("Outbox事件发布失败: event_id=%s retry_count=%d err=%v", event.EventID, event.RetryCount, err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "outbox publish failed")
		processResult = "publish_failed"
		outboxPublishTotal.WithLabelValues("failed").Inc()
		outboxPublishDuration.WithLabelValues("failed").Observe(time.Since(publishStart).Seconds())
		handlePublishFailure(ctx, event, msg, err)
		return
	}
	outboxPublishTotal.WithLabelValues("success").Inc()
	outboxPublishDuration.WithLabelValues("success").Observe(time.Since(publishStart).Seconds())

	if err := markEventSent(ctx, event.EventID); err != nil {
		log.Printf("Outbox事件已发布但标记Sent失败，后续可能重复投递: event_id=%s err=%v", event.EventID, err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "mark outbox sent failed")
		processResult = "mark_sent_failed"
		return
	}

	span.SetStatus(codes.Ok, "outbox event published")
	processResult = "published"
	log.Printf("Outbox事件发布成功: event_id=%s order_id=%s", event.EventID, msg.OrderID)
}

func parseOrderMessage(payload string) (OrderMessage, error) {
	var msg OrderMessage
	if err := json.Unmarshal([]byte(payload), &msg); err != nil {
		return msg, err
	}
	if msg.OrderID == "" {
		return msg, errors.New("order_id为空")
	}
	if msg.UserID <= 0 {
		return msg, fmt.Errorf("user_id非法: %d", msg.UserID)
	}
	if msg.ProductID <= 0 {
		return msg, fmt.Errorf("product_id非法: %d", msg.ProductID)
	}
	if msg.Count <= 0 {
		return msg, fmt.Errorf("count非法: %d", msg.Count)
	}
	return msg, nil
}

func parseHeaders(raw string) (amqp.Table, error) {
	if raw == "" {
		return amqp.Table{}, nil
	}

	values := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, err
	}

	headers := amqp.Table{}
	for key, value := range values {
		headers[key] = value
	}
	return headers, nil
}

func handlePublishFailure(ctx context.Context, event OutboxEvent, msg OrderMessage, publishErr error) {
	nextRetryCount := event.RetryCount + 1
	if nextRetryCount < MaxRetryCount {
		outboxRetryTotal.WithLabelValues("publish").Inc()
		if err := scheduleRetryWithCount(ctx, event, nextRetryCount, publishErr); err != nil {
			log.Printf("安排Outbox重试失败: event_id=%s err=%v", event.EventID, err)
		}
		return
	}

	reason, err := compensateRedis(ctx, msg)
	if err != nil {
		outboxCompensationTotal.WithLabelValues("failed").Inc()
		outboxRetryTotal.WithLabelValues("compensation").Inc()
		combinedErr := fmt.Errorf("MQ发布达到最大重试且Redis补偿失败: publish_err=%v compensation_err=%w", publishErr, err)
		if err := scheduleRetryWithCount(ctx, event, nextRetryCount, combinedErr); err != nil {
			log.Printf("安排Outbox补偿重试失败: event_id=%s err=%v", event.EventID, err)
		}
		return
	}
	outboxCompensationTotal.WithLabelValues("success").Inc()

	if err := markEventAndOrderFailed(ctx, event, reason); err != nil {
		log.Printf("标记Outbox和订单失败状态失败，稍后重试: event_id=%s err=%v", event.EventID, err)
		outboxRetryTotal.WithLabelValues("mark_failed").Inc()
		if errRetry := scheduleRetryWithCount(ctx, event, nextRetryCount, err); errRetry != nil {
			log.Printf("安排Outbox状态重试失败: event_id=%s err=%v", event.EventID, errRetry)
		}
		return
	}

	log.Printf("Outbox事件超过最大重试，已补偿Redis并标记订单失败: event_id=%s order_id=%s", event.EventID, msg.OrderID)
}

func scheduleRetry(ctx context.Context, event OutboxEvent, err error) {
	outboxRetryTotal.WithLabelValues("process").Inc()
	if errRetry := scheduleRetryWithCount(ctx, event, event.RetryCount+1, err); errRetry != nil {
		log.Printf("安排Outbox重试失败: event_id=%s err=%v", event.EventID, errRetry)
	}
}

func scheduleRetryWithCount(ctx context.Context, event OutboxEvent, retryCount int, err error) error {
	return db.WithContext(ctx).
		Model(&OutboxEvent{}).
		Where("event_id = ? AND status = ?", event.EventID, OutboxStatusPending).
		Updates(map[string]any{
			"retry_count":   retryCount,
			"next_retry_at": time.Now().Add(retryDelay(retryCount)),
			"last_error":    truncateText(err.Error(), 255),
		}).Error
}

func retryDelay(retryCount int) time.Duration {
	if retryCount <= 0 {
		return ScanInterval
	}
	delay := time.Duration(1<<uint(min(retryCount-1, 5))) * 5 * time.Second
	if delay > MaxRetryDelay {
		return MaxRetryDelay
	}
	return delay
}

func markEventSent(ctx context.Context, eventID string) error {
	return db.WithContext(ctx).
		Model(&OutboxEvent{}).
		Where("event_id = ? AND status = ?", eventID, OutboxStatusPending).
		Updates(map[string]any{
			"status":     OutboxStatusSent,
			"last_error": "",
		}).Error
}

func markEventFailed(ctx context.Context, event OutboxEvent, reason string) {
	if err := db.WithContext(ctx).
		Model(&OutboxEvent{}).
		Where("event_id = ? AND status = ?", event.EventID, OutboxStatusPending).
		Updates(map[string]any{
			"status":     OutboxStatusFailed,
			"last_error": truncateText(reason, 255),
		}).Error; err != nil {
		log.Printf("标记Outbox事件失败状态失败: event_id=%s err=%v", event.EventID, err)
	}
}

func markEventAndOrderFailed(ctx context.Context, event OutboxEvent, reason string) error {
	reason = truncateText(reason, 255)
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&OutboxEvent{}).
			Where("event_id = ? AND status = ?", event.EventID, OutboxStatusPending).
			Updates(map[string]any{
				"status":     OutboxStatusFailed,
				"last_error": reason,
			}).Error; err != nil {
			return err
		}

		return tx.Model(&Order{}).
			Where("order_id = ? AND status <> ?", event.AggregateID, OrderStatusSuccess).
			Updates(map[string]any{
				"status":      OrderStatusFailed,
				"fail_reason": reason,
			}).Error
	})
}

func getOrder(ctx context.Context, orderID string) (*Order, bool, error) {
	var order Order
	err := db.WithContext(ctx).Where("order_id = ?", orderID).First(&order).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &order, true, nil
}

func compensateRedis(ctx context.Context, msg OrderMessage) (string, error) {
	code, err := rollbackRedis(ctx, msg)
	if err != nil {
		return "", err
	}

	switch code {
	case 1:
		return "Outbox超过最大重试次数，Redis库存和用户购买记录已补偿，订单处理失败", nil
	case 2:
		return "Outbox超过最大重试次数，回滚数量非法，订单处理失败", nil
	case 3:
		return "Outbox超过最大重试次数，Redis已补偿过，订单处理失败", nil
	case 4:
		return "Outbox超过最大重试次数，未找到用户购买记录，订单处理失败", nil
	case 5:
		return "Outbox超过最大重试次数，用户购买记录小于回滚数量，需人工核查", nil
	case 0:
		return "", errors.New("Redis库存Key不存在，暂缓最终失败处理")
	default:
		return "", fmt.Errorf("未知Redis回滚状态: %d", code)
	}
}

func rollbackRedis(ctx context.Context, msg OrderMessage) (int, error) {
	stockKey := "product:stock:" + strconv.FormatInt(msg.ProductID, 10)
	userSetKey := "product:users:" + strconv.FormatInt(msg.ProductID, 10)
	rollbackKey := "order:rollback:" + msg.OrderID

	return rdb.Eval(ctx, ROLLBACK_LUA_SCRIPT, []string{stockKey, userSetKey, rollbackKey}, msg.Count, msg.UserID).Int()
}

func truncateText(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}

func runWorker(ctx context.Context, publisher *MQPublisher) {
	ticker := time.NewTicker(ScanInterval)
	defer ticker.Stop()

	for {
		events, err := claimPendingEvents(ctx, BatchSize)
		if err != nil {
			log.Printf("扫描Outbox事件失败: %v", err)
			outboxScanTotal.WithLabelValues("failed").Inc()
		} else {
			outboxScanTotal.WithLabelValues("success").Inc()
			outboxClaimedTotal.Add(float64(len(events)))
		}
		updateOutboxPendingGauge(ctx)
		for _, event := range events {
			processEvent(ctx, publisher, event)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func startMetricsServer() {
	port := os.Getenv("SECKILL_OUTBOX_METRICS_PORT")
	if port == "" {
		port = "9095"
	}

	addr := net.JoinHostPort("", port)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	go func() {
		log.Printf("Outbox Worker metrics 已启动: %s/metrics", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("Outbox Worker metrics 启动失败: %v", err)
		}
	}()
}

func main() {
	shutdown := tracer.InitTracer("outbox-worker", "localhost:4318")
	defer shutdown(context.Background())

	config.InitConfig("mq")
	initDB()
	initRedis()
	startMetricsServer()

	mqURL := config.Conf.MQ.URL
	if mqURL == "" {
		log.Fatal("mq.url 为空，请在 config/mq.yaml 设置或通过环境变量 SECKILL_MQ_URL 注入")
	}

	publisher := NewMQPublisher(mqURL)
	if err := publisher.Connect(); err != nil {
		log.Fatalf("初始化RabbitMQ发布器失败: %v", err)
	}

	log.Println("Outbox Worker 已启动，等待待投递事件...")
	runWorker(context.Background(), publisher)
}
