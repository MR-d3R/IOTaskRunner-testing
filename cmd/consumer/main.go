package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"taskrunner/internal/config"
	"taskrunner/internal/models"
	"taskrunner/logger"
	"taskrunner/pkg/rabbitmq"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
)

// Константы для настройки воркеров и очередей
const (
	MinWorkerCount       = 5  // Минимальное количество воркеров
	MaxWorkerCount       = 50 // Максимальное количество воркеров
	InitialWorkerCount   = 10 // Начальное количество воркеров
	LoadCheckInterval    = 30 // Интервал проверки нагрузки (в секундах)
	ReconnectDelay       = 5  // Задержка перед повторным подключением (в секундах)
	MaxReconnectAttempts = 10 // Максимальное количество попыток переподключения
	MessageRetryCount    = 3  // Количество попыток обработки сообщения перед отправкой в DLQ
)

// Структура для работы с отдельными воркерами
type Worker struct {
	conn            *amqp.Connection
	channel         *amqp.Channel
	ctx             context.Context
	cfg             *config.Config
	logger          *logger.ColorfulLogger
	workerId        int
	workerPool      *WorkerPool
	active          bool
	connectionError chan *amqp.Error
	lastActivity    time.Time
	mutex           sync.Mutex
}

// Структура для управления пулом воркеров
type WorkerPool struct {
	workers         map[int]*Worker
	workersMutex    sync.RWMutex
	wg              sync.WaitGroup
	ctx             context.Context
	cancel          context.CancelFunc
	cfg             *config.Config
	logger          *logger.ColorfulLogger
	nextWorkerId    int
	queueStats      *QueueStats
	loadMonitorDone chan struct{}
	startTime       time.Time
	healthy         bool
}

// Статистика очереди для динамического масштабирования
type QueueStats struct {
	messagesInQueue     int
	avgProcessingTime   time.Duration
	processingTimeCount int
	messagesPerSecond   float64
	lastProcessedCount  int
	totalProcessedCount int
	mutex               sync.Mutex
	lastCheck           time.Time
}

// Создание нового пула воркеров
func NewWorkerPool(initialWorkerCount int, cfg *config.Config, logger *logger.ColorfulLogger) (*WorkerPool, error) {
	ctx, cancel := context.WithCancel(context.Background())

	pool := &WorkerPool{
		workers:         make(map[int]*Worker),
		ctx:             ctx,
		cancel:          cancel,
		cfg:             cfg,
		logger:          logger,
		nextWorkerId:    1,
		queueStats:      &QueueStats{lastCheck: time.Now()},
		loadMonitorDone: make(chan struct{}),
		startTime:       time.Now(),
		healthy:         true,
	}

	// Создаем начальное количество воркеров
	for i := 0; i < initialWorkerCount; i++ {
		if err := pool.AddWorker(); err != nil {
			logger.Error("Failed to create initial worker: %v", err)
			// Продолжаем, чтобы создать хотя бы некоторые воркеры
		}
	}

	if len(pool.workers) == 0 {
		return nil, fmt.Errorf("failed to create any workers")
	}

	return pool, nil
}

// Добавление нового воркера в пул
func (wp *WorkerPool) AddWorker() error {
	wp.workersMutex.Lock()
	defer wp.workersMutex.Unlock()

	workerId := wp.nextWorkerId
	wp.nextWorkerId++

	worker, err := NewWorker(workerId, wp.ctx, wp.cfg, wp.logger, wp)
	if err != nil {
		return err
	}

	wp.workers[workerId] = worker
	wp.logger.Info("Added new worker with ID %d. Total workers: %d", workerId, len(wp.workers))

	return nil
}

// Удаление воркера из пула
func (wp *WorkerPool) RemoveWorker() bool {
	wp.workersMutex.Lock()
	defer wp.workersMutex.Unlock()

	if len(wp.workers) <= MinWorkerCount {
		return false
	}

	var oldestWorker *Worker
	var oldestWorkerId int
	oldestTime := time.Now()

	for id, worker := range wp.workers {
		worker.mutex.Lock()
		lastActivity := worker.lastActivity
		worker.mutex.Unlock()

		if lastActivity.Before(oldestTime) {
			oldestTime = lastActivity
			oldestWorker = worker
			oldestWorkerId = id
		}
	}

	if oldestWorker != nil {
		wp.logger.Info("Removing worker with ID %d. Total workers before removal: %d", oldestWorkerId, len(wp.workers))
		oldestWorker.Stop()
		delete(wp.workers, oldestWorkerId)
		return true
	}

	return false
}

// Запуск всех воркеров в пуле
func (wp *WorkerPool) Start() {
	wp.workersMutex.RLock()
	workerCount := len(wp.workers)
	wp.workersMutex.RUnlock()

	wp.wg.Add(workerCount)

	wp.workersMutex.RLock()
	for _, worker := range wp.workers {
		go worker.Run()
	}
	wp.workersMutex.RUnlock()

	go wp.monitorQueueLoad()
}

// Мониторинг нагрузки для динамического масштабирования
func (wp *WorkerPool) monitorQueueLoad() {
	ticker := time.NewTicker(LoadCheckInterval * time.Second)
	defer ticker.Stop()
	defer close(wp.loadMonitorDone)

	wp.logger.Info("Starting load monitoring with interval %d seconds", LoadCheckInterval)

	for {
		select {
		case <-wp.ctx.Done():
			wp.logger.Info("Load monitor stopping due to context cancellation")
			return
		case <-ticker.C:
			wp.adjustWorkerCount()
		}
	}
}

// Корректировка количества воркеров на основе нагрузки
func (wp *WorkerPool) adjustWorkerCount() {
	wp.workersMutex.RLock()
	currentWorkerCount := len(wp.workers)
	wp.workersMutex.RUnlock()

	// Получаем текущую статистику очереди
	messagesInQueue := wp.getQueueDepth()
	wp.queueStats.mutex.Lock()
	messagesPerSecond := wp.queueStats.messagesPerSecond
	avgProcessingTime := wp.queueStats.avgProcessingTime
	wp.queueStats.mutex.Unlock()

	wp.logger.Info("Load stats: workers=%d, queue_depth=%d, msgs_per_sec=%.2f, avg_proc_time=%v",
		currentWorkerCount, messagesInQueue, messagesPerSecond, avgProcessingTime)

	// Принимаем решение о масштабировании

	// Правило 1: Если очередь растет, добавляем воркеров
	if messagesInQueue > currentWorkerCount*2 && currentWorkerCount < MaxWorkerCount {
		numToAdd := min((messagesInQueue/2)-currentWorkerCount, 5)
		numToAdd = min(numToAdd, MaxWorkerCount-currentWorkerCount)

		wp.logger.Info("Queue is growing, adding %d workers", numToAdd)
		for i := 0; i < numToAdd; i++ {
			if err := wp.AddWorker(); err != nil {
				wp.logger.Error("Failed to add worker: %v", err)
				break
			}
			wp.wg.Add(1)

			// Запускаем созданного воркера
			wp.workersMutex.RLock()
			for _, worker := range wp.workers {
				if !worker.active {
					go worker.Run()
					break
				}
			}
			wp.workersMutex.RUnlock()
		}
	}

	// Правило 2: Если очередь маленькая, и утилизация низкая, уменьшаем количество воркеров
	if messagesInQueue < currentWorkerCount/2 && currentWorkerCount > MinWorkerCount {
		numToRemove := min(currentWorkerCount-max(MinWorkerCount, currentWorkerCount/2), 3)

		wp.logger.Info("Queue is small, removing %d workers", numToRemove)
		for i := 0; i < numToRemove; i++ {
			if !wp.RemoveWorker() {
				break
			}
		}
	}
}

// Получение текущей глубины очереди
func (wp *WorkerPool) getQueueDepth() int {
	conn, err := amqp.Dial(wp.cfg.RabbitMQURL)
	if err != nil {
		wp.logger.Error("Failed to connect to RabbitMQ to check queue depth: %v", err)
		return 0
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		wp.logger.Error("Failed to open a channel to check queue depth: %v", err)
		return 0
	}
	defer ch.Close()

	queue, err := ch.QueueInspect(rabbitmq.MainQueueName)
	if err != nil {
		wp.logger.Error("Failed to inspect queue: %v", err)
		return 0
	}

	wp.queueStats.mutex.Lock()
	wp.queueStats.messagesInQueue = queue.Messages
	wp.queueStats.mutex.Unlock()

	return queue.Messages
}

// Обновление статистики обработки
func (wp *WorkerPool) updateProcessingStats(processingTime time.Duration) {
	wp.queueStats.mutex.Lock()
	defer wp.queueStats.mutex.Unlock()

	// Обновляем среднее время обработки
	wp.queueStats.avgProcessingTime = (wp.queueStats.avgProcessingTime*time.Duration(wp.queueStats.processingTimeCount) + processingTime) /
		time.Duration(wp.queueStats.processingTimeCount+1)
	wp.queueStats.processingTimeCount++

	// Обновляем количество обработанных сообщений
	wp.queueStats.totalProcessedCount++

	// Обновляем сообщения в секунду
	now := time.Now()
	timeSinceLastCheck := now.Sub(wp.queueStats.lastCheck)
	if timeSinceLastCheck >= time.Second {
		messagesProcessed := wp.queueStats.totalProcessedCount - wp.queueStats.lastProcessedCount
		wp.queueStats.messagesPerSecond = float64(messagesProcessed) / timeSinceLastCheck.Seconds()
		wp.queueStats.lastProcessedCount = wp.queueStats.totalProcessedCount
		wp.queueStats.lastCheck = now
	}
}

// Ожидание завершения всех воркеров
func (wp *WorkerPool) Wait() {
	wp.wg.Wait()
	<-wp.loadMonitorDone
}

// Остановка всех воркеров
func (wp *WorkerPool) Stop() {
	wp.cancel()

	wp.workersMutex.RLock()
	for _, worker := range wp.workers {
		worker.Stop()
	}
	wp.workersMutex.RUnlock()

	wp.Wait()
}

// Создание нового воркера
func NewWorker(id int, ctx context.Context, cfg *config.Config, logger *logger.ColorfulLogger, pool *WorkerPool) (*Worker, error) {
	worker := &Worker{
		ctx:             ctx,
		cfg:             cfg,
		logger:          logger,
		workerId:        id,
		workerPool:      pool,
		active:          false,
		connectionError: make(chan *amqp.Error, 1),
		lastActivity:    time.Now(),
	}

	return worker, nil
}

// Установка соединения с RabbitMQ для воркера
func (w *Worker) connect() error {
	var err error
	var attempts int

	for attempts < MaxReconnectAttempts {
		w.logger.Info("[Worker %d] Attempting to connect to RabbitMQ (attempt %d/%d)",
			w.workerId, attempts+1, MaxReconnectAttempts)

		// Создаем соединение
		w.conn, err = amqp.Dial(w.cfg.RabbitMQURL)
		if err == nil {
			// Регистрируем обработчик уведомлений о закрытии соединения
			closeChan := make(chan *amqp.Error, 1)
			w.conn.NotifyClose(closeChan)

			// Создаем канал
			w.channel, err = w.conn.Channel()
			if err == nil {
				// Настраиваем DLX (Dead Letter Exchange)
				err = w.setupDeadLetterExchange()
				if err == nil {
					// Настраиваем QoS
					err = w.channel.Qos(1, 0, false)
					if err == nil {
						// Запускаем горутину для мониторинга закрытия соединения
						go func() {
							select {
							case err := <-closeChan:
								if err != nil {
									w.logger.Error("[Worker %d] Connection closed with error: %v", w.workerId, err)
								}
								// Уведомляем основную горутину о закрытии соединения
								select {
								case w.connectionError <- err:
								default:
								}
							case <-w.ctx.Done():
								// Контекст отменен, ничего не делаем
							}
						}()

						w.logger.Info("[Worker %d] Successfully connected to RabbitMQ", w.workerId)
						return nil
					}
				}
				w.channel.Close()
			}
			w.conn.Close()
		}

		w.logger.Error("[Worker %d] Failed to connect: %v. Retrying in %d seconds...",
			w.workerId, err, ReconnectDelay)

		// Проверяем, что контекст не отменен перед следующей попыткой
		select {
		case <-w.ctx.Done():
			return fmt.Errorf("context canceled during reconnect")
		case <-time.After(time.Second * time.Duration(ReconnectDelay)):
		}

		attempts++
	}

	return fmt.Errorf("failed to connect after %d attempts: %v", MaxReconnectAttempts, err)
}

// Настройка Dead Letter Exchange
func (w *Worker) setupDeadLetterExchange() error {
	// Объявляем Dead Letter Exchange
	err := w.channel.ExchangeDeclare(
		rabbitmq.DeadLetterExchangeName, // name
		"direct",                        // type
		true,                            // durable
		false,                           // auto-deleted
		false,                           // internal
		false,                           // no-wait
		nil,                             // arguments
	)
	if err != nil {
		return fmt.Errorf("failed to declare dead letter exchange: %v", err)
	}

	// Объявляем Dead Letter Queue
	_, err = w.channel.QueueDeclare(
		rabbitmq.DeadLetterQueueName, // name
		true,                         // durable
		false,                        // delete when unused
		false,                        // exclusive
		false,                        // no-wait
		nil,                          // arguments
	)
	if err != nil {
		return fmt.Errorf("failed to declare dead letter queue: %v", err)
	}

	// Связываем DLQ с Exchange
	err = w.channel.QueueBind(
		rabbitmq.DeadLetterQueueName,    // queue name
		"",                              // routing key
		rabbitmq.DeadLetterExchangeName, // exchange
		false,                           // no-wait
		nil,                             // arguments
	)
	if err != nil {
		return fmt.Errorf("failed to bind dead letter queue: %v", err)
	}

	// Объявляем основную очередь с настройками DLX
	args := amqp.Table{
		"x-dead-letter-exchange": rabbitmq.DeadLetterExchangeName,
	}

	_, err = w.channel.QueueDeclare(
		rabbitmq.MainQueueName, // name
		true,                   // durable
		false,                  // delete when unused
		false,                  // exclusive
		false,                  // no-wait
		args,                   // arguments с DLX
	)
	if err != nil {
		return fmt.Errorf("failed to declare main queue with DLX: %v", err)
	}

	w.logger.Info("[Worker %d] Successfully set up dead letter exchange and queues", w.workerId)
	return nil
}

// Запуск воркера
func (w *Worker) Run() {
	defer w.workerPool.wg.Done()

	w.mutex.Lock()
	w.active = true
	w.mutex.Unlock()

	w.logger.Info("[Worker %d] Starting worker", w.workerId)

runLoop:
	for {
		select {
		case <-w.ctx.Done():
			w.logger.Info("[Worker %d] Received shutdown signal, stopping", w.workerId)
			break runLoop
		default:
		}
		if w.conn == nil || w.channel == nil {
			if err := w.connect(); err != nil {
				w.logger.Error("[Worker %d] Failed to connect: %v. Stopping worker.", w.workerId, err)
				break runLoop
			}
		}

		// Подписка на очередь
		msgs, err := w.channel.Consume(
			rabbitmq.MainQueueName,
			"",
			false,
			false,
			false,
			false,
			nil,
		)
		if err != nil {
			w.logger.Error("[Worker %d] Failed to register as consumer: %v", w.workerId, err)
			w.closeConnections()
			continue
		}

		w.logger.Info("[Worker %d] Successfully registered as consumer", w.workerId)

		// Обработка сообщений
	consumeLoop:
		for {
			select {
			case <-w.ctx.Done():
				w.logger.Info("[Worker %d] Received shutdown signal during message processing", w.workerId)
				break runLoop

			case err := <-w.connectionError:
				w.logger.Error("[Worker %d] Connection error detected: %v", w.workerId, err)
				w.closeConnections()
				break consumeLoop

			case d, ok := <-msgs:
				if !ok {
					w.logger.Info("[Worker %d] Channel closed by server", w.workerId)
					w.closeConnections()
					break consumeLoop
				}

				w.mutex.Lock()
				w.lastActivity = time.Now()
				w.mutex.Unlock()

				w.processMessage(d)
			}
		}
	}

	w.closeConnections()
	w.mutex.Lock()
	w.active = false
	w.mutex.Unlock()
	w.logger.Info("[Worker %d] Worker stopped", w.workerId)
}

// Обработка сообщения
func (w *Worker) processMessage(d amqp.Delivery) {
	startTime := time.Now()
	w.logger.Debug("[Worker %d] Received a message: %s", w.workerId, d.Body)

	headers := d.Headers
	var retryCount int32 = 0

	if headers != nil {
		if val, exists := headers["x-retry-count"]; exists {
			if count, ok := val.(int32); ok {
				retryCount = count
			}
		}
	}

	var task models.Task
	if err := json.Unmarshal(d.Body, &task); err != nil {
		w.logger.Error("[Worker %d] Error decoding task: %v", w.workerId, err)
		// Непарсящиеся сообщения отправляем в DLQ сразу
		d.Nack(false, false)
		return
	}

	w.logger.Info("[Worker %d] Processing task %s: %s (retry: %d)",
		w.workerId, task.ID, task.URL, retryCount)

	if task.URL == "" {
		err := w.cfg.DB.HSet(w.ctx, "task:"+task.ID,
			"status", "error",
			"result", "empty URL provided",
		).Err()
		if err != nil {
			w.logger.Error("[Worker %d] Error saving error result to Redis: %v", w.workerId, err)
		}

		w.cfg.DB.Expire(w.ctx, "task:"+task.ID, time.Hour)

		// Пустые URL отправляем в DLQ после первой же попытки
		d.Nack(false, false)
		w.logger.Info("[Worker %d] Task %s failed: empty URL, sending to DLQ", w.workerId, task.ID)
		return
	}

	var status string
	var err error

	// Симулируем возможную ошибку для демонстрации повторных попыток (1% шанс)
	if rand.Intn(100) == 0 && retryCount < MessageRetryCount-1 {
		status = "error: simulated failure for retry demonstration"
		err = fmt.Errorf("simulated failure")
	} else {
		status = w.checkURLStatus(task.URL)
	}

	if strings.HasPrefix(status, "error:") && retryCount < MessageRetryCount-1 {
		retryCount++
		if headers == nil {
			headers = amqp.Table{}
		}
		headers["x-retry-count"] = retryCount
		err = w.channel.Publish(
			"",                     // exchange
			rabbitmq.MainQueueName, // routing key
			false,                  // mandatory
			false,                  // immediate
			amqp.Publishing{
				ContentType:  "application/json",
				Body:         d.Body,
				Headers:      headers,
				DeliveryMode: amqp.Persistent,
			},
		)

		if err != nil {
			w.logger.Error("[Worker %d] Error re-publishing message: %v", w.workerId, err)
		}

		d.Ack(false)
		w.logger.Info("[Worker %d] Task %s failed with error, retrying (attempt %d/%d)",
			w.workerId, task.ID, retryCount, MessageRetryCount)
		return
	}

	err = w.cfg.DB.HSet(w.ctx, "task:"+task.ID,
		"status", "done",
		"result", status,
		"completed_at", time.Now().Format(time.RFC3339),
	).Err()
	if err != nil {
		w.logger.Error("[Worker %d] Error saving result to Redis: %v", w.workerId, err)
	} else {
		w.logger.Info("[Worker %d] Result saved to Redis for task %s", w.workerId, task.ID)
	}

	err = w.cfg.DB.Expire(w.ctx, "task:"+task.ID, time.Hour).Err()
	if err != nil {
		w.logger.Error("[Worker %d] Error setting TTL: %v", w.workerId, err)
	}

	processingTime := time.Since(startTime)
	w.workerPool.updateProcessingStats(processingTime)

	// Если это была последняя попытка и всё еще ошибка, отправляем в DLQ
	if strings.HasPrefix(status, "error:") && retryCount >= MessageRetryCount-1 {
		w.logger.Info("[Worker %d] Task %s failed after %d retries, sending to DLQ",
			w.workerId, task.ID, retryCount+1)
		d.Nack(false, false)
	} else {
		d.Ack(false)
		w.logger.Info("[Worker %d] Task %s completed and acknowledged in %v",
			w.workerId, task.ID, processingTime)
	}
}

// Проверка статуса URL
func (w *Worker) checkURLStatus(url string) string {
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
	sleepSeconds := rng.Intn(121) + 180 // Задержка между 3-5 минут
	w.logger.Info("[Worker %d] Sleeping for %d seconds", w.workerId, sleepSeconds)

	// Проверяем контекст во время сна, чтобы можно было прервать длительную операцию
	timer := time.NewTimer(time.Second * time.Duration(sleepSeconds))
	select {
	case <-timer.C:
		// Таймер истек нормально
	case <-w.ctx.Done():
		// Контекст отменен, прерываем операцию
		if !timer.Stop() {
			<-timer.C
		}
		return "error: operation canceled"
	}

	return fmt.Sprintf("status: %d", resp.StatusCode)
}

// Закрытие соединений воркера
func (w *Worker) closeConnections() {
	if w.channel != nil {
		w.channel.Close()
		w.channel = nil
	}
	if w.conn != nil {
		w.conn.Close()
		w.conn = nil
	}
}

// Остановка воркера
func (w *Worker) Stop() {
	w.closeConnections()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Добавьте функцию для обновления статистики в Redis
func (wp *WorkerPool) updateStatsInRedis(redisClient *redis.Client) {
	// Запускаем периодическое обновление
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-wp.ctx.Done():
			return
		case <-ticker.C:
			wp.workersMutex.RLock()
			workerCount := len(wp.workers)
			wp.workersMutex.RUnlock()

			wp.queueStats.mutex.Lock()
			stats := map[string]interface{}{
				"healthy":             wp.healthy,
				"uptime":              time.Since(wp.startTime).Seconds(),
				"workers":             workerCount,
				"messages_in_queue":   wp.queueStats.messagesInQueue,
				"messages_per_second": wp.queueStats.messagesPerSecond,
				"avg_processing_time": wp.queueStats.avgProcessingTime.Milliseconds(),
				"total_processed":     wp.queueStats.totalProcessedCount,
				"last_update":         time.Now().Unix(),
			}
			wp.queueStats.mutex.Unlock()

			// Сохраняем в Redis
			jsonData, err := json.Marshal(stats)
			if err == nil {
				redisClient.Set(wp.ctx, "consumer:health_stats", jsonData, 30*time.Second)
			}
		}
	}
}

func main() {
	// Инициализация конфигурации
	cfg, err := config.InitConfig("CONSUMER")
	if err != nil {
		panic(err)
	}
	logger := config.GetLogger()

	// Проверка соединения с Redis
	ctx := context.Background()
	pong, err := cfg.DB.Ping(ctx).Result()
	if err != nil {
		logger.Panic("Failed to connect to Redis: %v", err)
	}
	logger.Info("Redis connection successful: %s", pong)
	defer cfg.DB.Close()

	// Создаем пул воркеров
	workerPool, err := NewWorkerPool(InitialWorkerCount, cfg, logger)
	if err != nil {
		logger.Panic("Failed to create worker pool: %v", err)
	}

	// Запускаем воркеры
	workerPool.Start()
	logger.Info("Started worker pool with %d initial workers", InitialWorkerCount)

	go workerPool.updateStatsInRedis(cfg.DB)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigChan
	logger.Info("Received shutdown signal: %v", sig)

	logger.Info("Initiating graceful shutdown...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Запускаем горутину для отслеживания превышения таймаута
	go func() {
		<-shutdownCtx.Done()
		if shutdownCtx.Err() == context.DeadlineExceeded {
			logger.Error("Graceful shutdown timed out, forcing exit")
			os.Exit(1)
		}
	}()

	logger.Info("Stopping worker pool...")
	workerPool.Stop()

	logger.Info("Worker pool stopped, application shutdown complete")
}
