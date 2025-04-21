package main

import (
	"context"

	"taskrunner/internal/config"
	"taskrunner/internal/handlers"
	"taskrunner/pkg/rabbitmq"

	"github.com/gin-gonic/gin"
)

func main() {
	ctx := context.Background()
	cfg, err := config.InitConfig("PRODUCER")
	if err != nil {
		panic(err)
	}
	logger := config.GetLogger()

	// Подключение к RabbitMQ
	conn, ch, err := rabbitmq.SetupRabbitMQ(cfg.RabbitMQURL, "tasks")
	if err != nil {
		logger.Error("Failed to setup RabbitMQ: %v", err)
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
