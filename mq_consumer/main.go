package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"seckill-mall/common/config"
)

const (
	OrderQueue = "seckill_order_queue"

	DeadExchange   = "dlx_exchange" // 死信交换机
	DeadQueue      = "dead_queue"   // 死信队列
	DeadRoutingKey = "dead_key"     // 死信路由键
)

// 初始化队列系统
func setupQueue(ch *amqp.Channel) amqp.Queue {
	//声明死信交换机
	err := ch.ExchangeDeclare(DeadExchange, "direct", true, false, false, false, nil)
	if err != nil {
		log.Fatalf("无法声明死信交换机： %v", err)
	}

	//声明死信队列
	_, err = ch.QueueDeclare(DeadQueue, true, false, false, false, nil)
	if err != nil {
		log.Fatalf("无法声明死信队列： %v", err)
	}

	//绑定：死信交换机 -> 死信队列
	err = ch.QueueBind(DeadQueue, DeadRoutingKey, DeadExchange, false, nil)
	if err != nil {
		log.Fatalf("无法绑定死信队列： %v", err)
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
		log.Fatalf("无法声明主队列(可能参数冲突，请先去后台删除旧队列)： %v", err)
	}

	log.Printf("✅ RabbitMQ 队列结构初始化完成：主队列[%s] -> 死信[%s]", OrderQueue, DeadQueue)
	return q
}

// 对应数据库结构
type Order struct {
	// 对应数据库 id, bigint(20) unsigned, auto_increment
	ID uint64 `gorm:"column:id;primaryKey;autoIncrement"`
	// 对应数据库 order_id, varchar(64)
	OrderID string `gorm:"column:order_id;uniqueIndex;not null"`
	// 其他字段
	UserID    int64     `gorm:"column:user_id;not null"`
	ProductID int64     `gorm:"column:product_id;not null"`
	Amount    float32   `gorm:"column:amount;not null"`
	Status    int       `gorm:"column:status;default:0"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
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

func isDuplicateOrderError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Duplicate entry")
}

func persistOrder(msg OrderMessage) error {
	order := Order{
		OrderID:   msg.OrderID,
		UserID:    msg.UserID,
		ProductID: msg.ProductID,
		Amount:    msg.Amount,
		Status:    1, // 已支付/处理中
	}

	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&order).Error; err != nil {
			return err
		}

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
	})
}

func main() {
	config.InitConfig("mq")
	initDB()
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

	// 2. 这里的 Qos 很重要，保证消费者不被撑死
	ch.Qos(1, 0, false)

	// 3. 调用 setupQueue 获取配置好 DLQ 的队列对象
	q := setupQueue(ch)

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
		log.Fatal(err)
	}

	fmt.Println("📧 消费者服务已启动 (DLQ版)，等待订单中...")

	forever := make(chan struct{})

	go func() {
		for d := range msgs {
			var msg OrderMessage
			if err := json.Unmarshal(d.Body, &msg); err != nil {
				log.Printf("❌ 消息格式错误，直接丢弃: %v", err)
				d.Nack(false, false) // 这种一般不需要重试，直接进死信或丢弃
				continue
			}

			if msg.Count <= 0 {
				log.Printf("❌ 订单数量非法，进入死信: order_id=%s count=%d", msg.OrderID, msg.Count)
				d.Nack(false, false)
				continue
			}

			fmt.Printf("📦 接收订单: %s | 数量：%d | 金额：%.2f | 处理中...", msg.OrderID, msg.Count, msg.Amount)

			// 模拟业务处理耗时
			time.Sleep(50 * time.Millisecond)

			// 写入订单并同步扣减 MySQL 商品库存，保证最终库存账本一致。
			err = persistOrder(msg)
			if err != nil {
				// 场景 A: 重复消费 (幂等性保护)
				if isDuplicateOrderError(err) {
					fmt.Printf(" -> ⚠️ 订单已存在，确认消息\n")
					// 事务已回滚，避免重复扣减 product.stock。
					d.Ack(false)
				} else if errors.Is(err, errMySQLStockNotEnough) {
					log.Printf(" -> ❌ MySQL库存不足，发送 Nack(不重回队列)->进入死信")
					d.Nack(false, false)
				} else {
					// 场景 B: 真正的故障 (数据库挂了/网络抖动)
					log.Printf(" -> ❌ 落库失败: %v，发送 Nack(不重回队列)->进入死信", err)

					// 关键点：requeue=false + 配置了死信交换机 = 消息进入死信队列
					d.Nack(false, false)
				}
			} else {
				// 场景 C: 成功
				fmt.Printf(" -> ✅ 落库成功\n")
				d.Ack(false)
			}
		}
	}()

	<-forever
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
