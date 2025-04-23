// In package shared/rabbitmq/setup.go
package rabbitmq

import (
	amqp "github.com/rabbitmq/amqp091-go"
)

// Constants for RabbitMQ configuration
const (
	MainQueueName          = "tasks"
	DeadLetterQueueName    = "tasks.dead"
	DeadLetterExchangeName = "tasks.dead.exchange"
)

// SetupRabbitMQ sets up the queues and exchanges
func SetupRabbitMQ(conn *amqp.Connection) (*amqp.Channel, error) {
	ch, err := conn.Channel()
	if err != nil {
		return nil, err
	}

	// Set up dead letter exchange
	err = ch.ExchangeDeclare(
		DeadLetterExchangeName,
		"direct",
		true,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		ch.Close()
		return nil, err
	}

	// Set up dead letter queue
	_, err = ch.QueueDeclare(
		DeadLetterQueueName,
		true,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		ch.Close()
		return nil, err
	}

	// Bind dead letter queue to exchange
	err = ch.QueueBind(
		DeadLetterQueueName,
		"",
		DeadLetterExchangeName,
		false,
		nil,
	)
	if err != nil {
		ch.Close()
		return nil, err
	}

	// Set up main queue with DLX
	args := amqp.Table{
		"x-dead-letter-exchange": DeadLetterExchangeName,
	}

	_, err = ch.QueueDeclare(
		MainQueueName,
		true,
		false,
		false,
		false,
		args,
	)
	if err != nil {
		ch.Close()
		return nil, err
	}

	return ch, nil
}
