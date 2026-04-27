package main

import (
	"errors"
	"fmt"
	"log"

	amqp "github.com/rabbitmq/amqp091-go"
)

func setupDeadQueue(ch *amqp.Channel) (amqp.Queue, error) {
	if err := ch.ExchangeDeclare(DeadExchange, "direct", true, false, false, false, nil); err != nil {
		return amqp.Queue{}, fmt.Errorf("declare dead exchange failed: %w", err)
	}

	q, err := ch.QueueDeclare(DeadQueue, true, false, false, false, nil)
	if err != nil {
		return amqp.Queue{}, fmt.Errorf("declare dead queue failed: %w", err)
	}

	if err := ch.QueueBind(DeadQueue, DeadRoutingKey, DeadExchange, false, nil); err != nil {
		return amqp.Queue{}, fmt.Errorf("bind dead queue failed: %w", err)
	}

	return q, nil
}

func runConsumer(mqURL string) error {
	conn, err := amqp.Dial(mqURL)
	if err != nil {
		return fmt.Errorf("rabbitmq connect failed: %w", err)
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("rabbitmq channel create failed: %w", err)
	}
	defer ch.Close()

	if err := ch.Qos(1, 0, false); err != nil {
		return fmt.Errorf("rabbitmq qos set failed: %w", err)
	}

	q, err := setupDeadQueue(ch)
	if err != nil {
		return err
	}

	msgs, err := ch.Consume(q.Name, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume dead queue failed: %w", err)
	}

	log.Printf("dlq consumer started queue=%s", q.Name)

	connClosed := conn.NotifyClose(make(chan *amqp.Error, 1))
	chClosed := ch.NotifyClose(make(chan *amqp.Error, 1))

	for {
		select {
		case d, ok := <-msgs:
			if !ok {
				return errors.New("rabbitmq dead queue delivery channel closed")
			}
			handleMessage(d)
		case err, ok := <-connClosed:
			if !ok || err == nil {
				return errors.New("rabbitmq connection closed")
			}
			return fmt.Errorf("rabbitmq connection closed with error: %w", err)
		case err, ok := <-chClosed:
			if !ok || err == nil {
				return errors.New("rabbitmq channel closed")
			}
			return fmt.Errorf("rabbitmq channel closed with error: %w", err)
		}
	}
}
