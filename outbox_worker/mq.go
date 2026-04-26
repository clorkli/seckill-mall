package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type MQPublisher struct {
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
