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
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Воркер
type Worker struct {
	conn       *amqp.Connection
	channel    *amqp.Channel
	ctx        context.Context
	cfg        *config.Config
	logger     *logger.ColorfulLogger
	workerId   int
	workerPool *WorkerPool
}

// Структура для управления пулом воркеров
type WorkerPool struct {
	workers []*Worker
	wg      sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc
}

// Создание нового пула воркеров
func NewWorkerPool(workerCount int, cfg *config.Config, logger *logger.ColorfulLogger) (*WorkerPool, error) {
	ctx, cancel := context.WithCancel(context.Background())
	pool := &WorkerPool{
		workers: make([]*Worker, workerCount),
		ctx:     ctx,
		cancel:  cancel,
	}

	for i := 0; i < workerCount; i++ {
		worker, err := NewWorker(i, ctx, cfg, logger, pool)
		if err != nil {
			// Закрытие всех уже созданныъ воркеры при ошибке
			for j := 0; j < i; j++ {
				if pool.workers[j] != nil {
					pool.workers[j].Close()
				}
			}
			return nil, err
		}
		pool.workers[i] = worker
	}

	return pool, nil
}

// Запуск всех воркеров в пуле
func (wp *WorkerPool) Start() {
	wp.wg.Add(len(wp.workers))
	for _, worker := range wp.workers {
		go worker.Run()
	}
}

// Ожидание завершения всех воркеров
func (wp *WorkerPool) Wait() {
	wp.wg.Wait()
}

// Остановка всех воркеров
func (wp *WorkerPool) Stop() {
	wp.cancel()
	for _, worker := range wp.workers {
		worker.Close()
	}
	wp.Wait()
}

// Создание нового воркера
func NewWorker(id int, ctx context.Context, cfg *config.Config, logger *logger.ColorfulLogger, pool *WorkerPool) (*Worker, error) {
	conn, err := amqp.Dial(cfg.RabbitMQURL)
	if err != nil {
		return nil, fmt.Errorf("worker %d failed to connect to RabbitMQ: %v", id, err)
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("worker %d failed to open a channel: %v", id, err)
	}

	err = ch.Qos(
		1,
		0,
		false,
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return nil, fmt.Errorf("worker %d failed to set QoS: %v", id, err)
	}

	return &Worker{
		conn:       conn,
		channel:    ch,
		ctx:        ctx,
		cfg:        cfg,
		logger:     logger,
		workerId:   id,
		workerPool: pool,
	}, nil
}

// Запуск воркера
func (w *Worker) Run() {
	defer w.workerPool.wg.Done()

	w.logger.Info("[Worker %d] Starting worker", w.workerId)

	queue, err := w.channel.QueueDeclare(
		"tasks",
		true,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		w.logger.Error("[Worker %d] Failed to declare a queue: %v", w.workerId, err)
		return
	}

	msgs, err := w.channel.Consume(
		queue.Name,
		"",
		false,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		w.logger.Error("[Worker %d] Failed to register a consumer: %v", w.workerId, err)
		return
	}

	w.logger.Info("[Worker %d] Successfully registered consumer for queue '%s'", w.workerId, queue.Name)

	for {
		select {
		case <-w.ctx.Done():
			w.logger.Info("[Worker %d] Received shutdown signal, stopping", w.workerId)
			return
		case d, ok := <-msgs:
			if !ok {
				w.logger.Info("[Worker %d] Channel closed, stopping worker", w.workerId)
				return
			}

			w.logger.Debug("[Worker %d] Received a message: %s", w.workerId, d.Body)

			var task models.Task
			if err := json.Unmarshal(d.Body, &task); err != nil {
				w.logger.Error("[Worker %d] Error decoding task: %v", w.workerId, err)
				d.Nack(false, false)
				continue
			}

			w.logger.Info("[Worker %d] Processing task %s: %s", w.workerId, task.ID, task.URL)

			if task.URL == "" {
				err := w.cfg.DB.HSet(w.ctx, "task:"+task.ID,
					"status", "error",
					"result", "empty URL provided",
				).Err()
				if err != nil {
					w.logger.Error("[Worker %d] Error saving error result to Redis: %v", w.workerId, err)
				}

				w.cfg.DB.Expire(w.ctx, "task:"+task.ID, time.Hour)

				d.Nack(false, false)
				w.logger.Info("[Worker %d] Task %s failed: empty URL", w.workerId, task.ID)
				continue
			}

			status := w.checkURLStatus(task.URL)
			w.logger.Debug("[Worker %d] URL check result for task %s: %s", w.workerId, task.ID, status)

			err := w.cfg.DB.HSet(w.ctx, "task:"+task.ID,
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

			d.Ack(false)
			w.logger.Info("[Worker %d] Task %s completed and acknowledged", w.workerId, task.ID)
		}
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
	time.Sleep(time.Second * time.Duration(sleepSeconds))

	return fmt.Sprintf("status: %d", resp.StatusCode)
}

// Закрытие соединений воркера
func (w *Worker) Close() {
	if w.channel != nil {
		w.channel.Close()
	}
	if w.conn != nil {
		w.conn.Close()
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
	workerCount := 10 // Установите нужное количество параллельных воркеров
	workerPool, err := NewWorkerPool(workerCount, cfg, logger)
	if err != nil {
		logger.Panic("Failed to create worker pool: %v", err)
	}

	// Запускаем воркеры
	workerPool.Start()
	logger.Info("Started %d workers", workerCount)

	// Ожидаем сигнала для завершения
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
	<-signalChan

	logger.Info("Received shutdown signal, stopping workers...")
	workerPool.Stop()
	logger.Info("All workers stopped, exiting")
}
