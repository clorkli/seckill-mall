package main

import (
	"errors"
	"fmt"
	"log"

	amqp "github.com/rabbitmq/amqp091-go"
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
