package main

import (
	"errors"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

func setupDeadQueue(ch *amqp.Channel) (amqp.Queue, error) {
	if err := ch.ExchangeDeclare(DeadExchange, "direct", true, false, false, false, nil); err != nil {
		return amqp.Queue{}, fmt.Errorf("声明死信交换机失败: %w", err)
	}

	q, err := ch.QueueDeclare(DeadQueue, true, false, false, false, nil)
	if err != nil {
		return amqp.Queue{}, fmt.Errorf("声明死信队列失败: %w", err)
	}

	if err := ch.QueueBind(DeadQueue, DeadRoutingKey, DeadExchange, false, nil); err != nil {
		return amqp.Queue{}, fmt.Errorf("绑定死信队列失败: %w", err)
	}

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

	if err := ch.Qos(1, 0, false); err != nil {
		return fmt.Errorf("设置Qos失败: %w", err)
	}

	q, err := setupDeadQueue(ch)
	if err != nil {
		return err
	}

	msgs, err := ch.Consume(q.Name, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("启动死信消费失败: %w", err)
	}

	fmt.Println("🛠️ 死信补偿服务已启动，等待 dead_queue 消息...")

	connClosed := conn.NotifyClose(make(chan *amqp.Error, 1))
	chClosed := ch.NotifyClose(make(chan *amqp.Error, 1))

	for {
		select {
		case d, ok := <-msgs:
			if !ok {
				return errors.New("RabbitMQ dead_queue delivery channel 已关闭")
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
