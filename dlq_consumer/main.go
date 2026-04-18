package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"seckill-mall/common/config"
)

const (
	DeadExchange   = "dlx_exchange"
	DeadQueue      = "dead_queue"
	DeadRoutingKey = "dead_key"
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

type Order struct {
	OrderID string `gorm:"column:order_id"`
}

func (Order) TableName() string { return "orders" }

type OrderMessage struct {
	OrderID   string  `json:"order_id"`
	UserID    int64   `json:"user_id"`
	ProductID int64   `json:"product_id"`
	Count     int32   `json:"count"`
	Amount    float32 `json:"amount"`
}

var db *gorm.DB
var rdb *redis.Client

func setupDeadQueue(ch *amqp.Channel) amqp.Queue {
	if err := ch.ExchangeDeclare(DeadExchange, "direct", true, false, false, false, nil); err != nil {
		log.Fatalf("声明死信交换机失败: %v", err)
	}

	q, err := ch.QueueDeclare(DeadQueue, true, false, false, false, nil)
	if err != nil {
		log.Fatalf("声明死信队列失败: %v", err)
	}

	if err := ch.QueueBind(DeadQueue, DeadRoutingKey, DeadExchange, false, nil); err != nil {
		log.Fatalf("绑定死信队列失败: %v", err)
	}

	return q
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
	fmt.Println("✅ MySQL 连接成功")
}

func initRedis() {
	rdb = redis.NewClient(&redis.Options{
		Addr:     config.Conf.Redis.Addr,
		Password: config.Conf.Redis.Password,
		DB:       config.Conf.Redis.DB,
	})

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("连接 Redis 失败: %v", err)
	}
	fmt.Println("✅ Redis 连接成功")
}

func orderExists(ctx context.Context, orderID string) (bool, error) {
	var count int64
	err := db.WithContext(ctx).Model(&Order{}).Where("order_id = ?", orderID).Count(&count).Error
	return count > 0, err
}

func validateMessage(msg OrderMessage) error {
	if msg.OrderID == "" {
		return fmt.Errorf("order_id 为空")
	}
	if msg.UserID <= 0 {
		return fmt.Errorf("user_id 非法: %d", msg.UserID)
	}
	if msg.ProductID <= 0 {
		return fmt.Errorf("product_id 非法: %d", msg.ProductID)
	}
	if msg.Count <= 0 {
		return fmt.Errorf("count 非法: %d", msg.Count)
	}
	return nil
}

func rollbackRedis(ctx context.Context, msg OrderMessage) (int, error) {
	stockKey := "product:stock:" + strconv.FormatInt(msg.ProductID, 10)
	userSetKey := "product:users:" + strconv.FormatInt(msg.ProductID, 10)
	rollbackKey := "order:rollback:" + msg.OrderID

	return rdb.Eval(ctx, ROLLBACK_LUA_SCRIPT, []string{stockKey, userSetKey, rollbackKey}, msg.Count, msg.UserID).Int()
}

func handleRollbackResult(code int, msg OrderMessage, d amqp.Delivery) {
	switch code {
	case 0:
		log.Printf("⚠️ Redis库存Key不存在，稍后重试: order_id=%s product_id=%d", msg.OrderID, msg.ProductID)
		time.Sleep(3 * time.Second)
		d.Nack(false, true)
	case 1:
		log.Printf("✅ 死信补偿成功: order_id=%s user_id=%d product_id=%d count=%d", msg.OrderID, msg.UserID, msg.ProductID, msg.Count)
		d.Ack(false)
	case 2:
		log.Printf("❌ 死信消息数量非法，无法补偿，确认消息: order_id=%s count=%d", msg.OrderID, msg.Count)
		d.Ack(false)
	case 3:
		log.Printf("✅ 死信已补偿过，直接确认: order_id=%s", msg.OrderID)
		d.Ack(false)
	case 4:
		log.Printf("✅ 未找到用户购买记录，视为无需重复补偿: order_id=%s", msg.OrderID)
		d.Ack(false)
	case 5:
		log.Printf("❌ 用户购买记录小于回滚数量，需人工核查，确认消息: order_id=%s user_id=%d count=%d", msg.OrderID, msg.UserID, msg.Count)
		d.Ack(false)
	default:
		log.Printf("⚠️ 未知回滚状态，稍后重试: order_id=%s code=%d", msg.OrderID, code)
		time.Sleep(3 * time.Second)
		d.Nack(false, true)
	}
}

func handleMessage(d amqp.Delivery) {
	var msg OrderMessage
	if err := json.Unmarshal(d.Body, &msg); err != nil {
		log.Printf("❌ 死信消息格式错误，无法补偿，确认消息: %v", err)
		d.Ack(false)
		return
	}

	if err := validateMessage(msg); err != nil {
		log.Printf("❌ 死信消息字段非法，无法自动补偿，确认消息: %v", err)
		d.Ack(false)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	exists, err := orderExists(ctx, msg.OrderID)
	if err != nil {
		log.Printf("⚠️ 查询订单失败，稍后重试: order_id=%s err=%v", msg.OrderID, err)
		time.Sleep(3 * time.Second)
		d.Nack(false, true)
		return
	}
	if exists {
		log.Printf("✅ 订单已落库，无需回滚Redis，确认死信: order_id=%s", msg.OrderID)
		d.Ack(false)
		return
	}

	code, err := rollbackRedis(ctx, msg)
	if err != nil {
		log.Printf("⚠️ Redis回滚失败，稍后重试: order_id=%s err=%v", msg.OrderID, err)
		time.Sleep(3 * time.Second)
		d.Nack(false, true)
		return
	}

	handleRollbackResult(code, msg, d)
}

func main() {
	config.InitConfig("mq")
	initDB()
	initRedis()

	mqURL := config.Conf.MQ.URL
	if mqURL == "" {
		log.Fatal("mq.url 为空，请在 config/mq.yaml 设置或通过环境变量 SECKILL_MQ_URL 注入")
	}

	conn, err := amqp.Dial(mqURL)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		log.Fatal(err)
	}
	defer ch.Close()

	if err := ch.Qos(1, 0, false); err != nil {
		log.Fatalf("设置Qos失败: %v", err)
	}

	q := setupDeadQueue(ch)

	msgs, err := ch.Consume(q.Name, "", false, false, false, false, nil)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("🛠️ 死信补偿服务已启动，等待 dead_queue 消息...")

	forever := make(chan struct{})
	go func() {
		for d := range msgs {
			handleMessage(d)
		}
	}()

	<-forever
}
