package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"taskrunner/internal/config"
	"taskrunner/internal/models"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func checkURLStatus(url string) string {
	if url == "" {
		return "error: empty URL"
	}

	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "error: URL must start with http:// or https://"
	}

	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	sleepSeconds := rng.Intn(121) + 180
	time.Sleep(time.Second * time.Duration(sleepSeconds))

	return fmt.Sprintf("status: %d", resp.StatusCode)
}

func main() {
	ctx := context.Background()
	cfg, err := config.InitConfig("CONSUMER")
	if err != nil {
		panic(err)
	}
	logger := config.GetLogger()

	// Подключение к RabbitMQ
	conn, err := amqp.Dial(cfg.RabbitMQURL)
	if err != nil {
		logger.Error("Failed to connect to RabbitMQ: %v", err)
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		logger.Error("Failed to open a channel: %v", err)
	}
	defer ch.Close()

	queue, err := ch.QueueDeclare(
		"tasks",
		true,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		logger.Error("Failed to declare a queue: %v", err)
	}

	logger.Info("Queue '%s' declared: %+v", "tasks", queue)

	// Счетчик количества сообщений в очереди
	queueInfo, err := ch.QueueInspect("tasks")
	if err != nil {
		logger.Error("Failed to inspect queue: %v", err)
	}
	logger.Info("Queue '%s' has %d messages waiting", "tasks", queueInfo.Messages)

	// Настройка QoS (Quality of Service)
	err = ch.Qos(
		1,     // prefetch count
		0,     // prefetch size
		false, // global
	)
	if err != nil {
		logger.Error("Failed to set QoS: %v", err)
	}

	// Подписка на очередь
	msgs, err := ch.Consume(
		"tasks", // queue
		"",      // consumer (пустая строка = автогенерация имени)
		false,   // auto-ack (ИЗМЕНЕНО на false - ручное подтверждение)
		false,   // exclusive
		false,   // no-local
		false,   // no-wait
		nil,     // args
	)
	if err != nil {
		logger.Error("Failed to register a consumer: %v", err)
	}

	logger.Info("Successfully registered consumer for queue '%s'", "tasks")

	// Проверка соединения с Redis
	pong, err := cfg.DB.Ping(ctx).Result()
	if err != nil {
		logger.Panic("Failed to connect to Redis: %v", err)
	}
	logger.Info("Redis connection successful: %s", pong)
	defer cfg.DB.Close()

	// Обработка сообщений
	forever := make(chan bool)
	go func() {
		logger.Info("Starting message processing goroutine...")

		for d := range msgs {
			logger.Debug("Received a message: %s", d.Body)

			var task models.Task
			if err := json.Unmarshal(d.Body, &task); err != nil {
				logger.Error("Error decoding task: %v", err)
				d.Ack(false) // Подтверждаем получение, даже если не смогли обработать
				continue
			}

			logger.Info("Processing task %s: %s", task.ID, task.URL)

			if task.URL == "" {
				// Сохраняем ошибку в Redis
				err := cfg.DB.HSet(ctx, "task:"+task.ID,
					"status", "error",
					"result", "empty URL provided",
				).Err()
				if err != nil {
					logger.Error("Error saving error result to Redis: %v", err)
				}

				// Устанавливаем TTL
				cfg.DB.Expire(ctx, "task:"+task.ID, time.Hour)

				// Подтверждаем обработку сообщения
				d.Ack(false)
				logger.Info("Task %s failed: empty URL", task.ID)
				continue
			}

			status := checkURLStatus(task.URL)
			logger.Debug("URL check result: %s", status)

			// Сохраняем результат
			err := cfg.DB.HSet(ctx, "task:"+task.ID,
				"status", "done",
				"result", status,
				"completed_at", time.Now().Format(time.RFC3339),
			).Err()
			if err != nil {
				logger.Error("Error saving result to Redis: %v", err)
			} else {
				logger.Info("Result saved to Redis for task %s", task.ID)
			}

			// Устанавливаем TTL (например, 1 час)
			err = cfg.DB.Expire(ctx, "task:"+task.ID, time.Hour).Err()
			if err != nil {
				logger.Error("Error setting TTL: %v", err)
			}

			// Подтверждаем обработку сообщения
			d.Ack(false)
			logger.Info("Task %s completed and acknowledged", task.ID)
		}

		logger.Info("Message channel closed, exiting goroutine")
	}()

	logger.Info("Consumer started. Waiting for messages... Press Ctrl+C to exit.")
	<-forever
}
