package main

import (
	"context"

	"taskrunner/internal/config"
	"taskrunner/internal/handlers"
	"taskrunner/pkg/rabbitmq"

	"github.com/gin-gonic/gin"
	amqp "github.com/rabbitmq/amqp091-go"
)

func main() {
	ctx := context.Background()
	cfg, err := config.InitConfig("PRODUCER")
	if err != nil {
		panic(err)
	}
	logger := config.GetLogger()

	// Подключение к RabbitMQ
	conn, err := amqp.Dial(cfg.RabbitMQURL)
	if err != nil {
		logger.Panic("error connecting to rabbitmq: %v", err)
	}
	ch, err := rabbitmq.SetupRabbitMQ(conn)
	if err != nil {
		logger.Panic("Failed to setup RabbitMQ: %v", err)
	}
	defer conn.Close()
	defer ch.Close()

	// Проверка соединения с Redis
	pong, err := cfg.DB.Ping(ctx).Result()
	if err != nil {
		logger.Panic("Failed to connect to Redis: %v", err)
	}
	logger.Info("Redis connection successful: %s", pong)
	defer cfg.DB.Close()

	// Создание обработчиков
	taskHandler := handlers.NewTaskHandler(ctx, ch, cfg.DB, logger)

	// Создание Gin роутера
	r := gin.Default()

	// Регистрация маршрутов
	taskHandler.RegisterRoutes(r)

	// Запуск HTTP-сервера
	logger.Info("Starting producer server on http://localhost%s", cfg.ServerPort)
	if err := r.Run(cfg.ServerPort); err != nil {
		logger.Error("Failed to start server: %v", err)
	}
}
