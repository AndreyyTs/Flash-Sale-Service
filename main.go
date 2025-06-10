package main

import (
	"contest_notcoin/db"
	"contest_notcoin/megacache"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type ServerInstance struct {
	server           *db.Server
	checkoutRepo     *db.CheckoutRepository
	batchInserter    *db.BatchInserter
	saleItemsRepo    *db.SaleItemsRepository
	batchPurchase    *db.BatchPurchaseUpdater
	cache            *megacache.Megacache
	saleID           int64
	httpServer       *http.Server
	isAcceptingReqs  int32 // atomic boolean
	shutdownComplete chan struct{}
	dbHost           string
}

func init() {
	time.Local = time.UTC
}

var (
	currentInstance atomic.Value // *ServerInstance
)

var dbHost string

func main() {
	dbHost = os.Getenv("DB_HOST")
	if dbHost == "" {
		dbHost = "localhost"
	}

	// Запускаем первый экземпляр сервера
	if err := startNewServerInstance(); err != nil {
		log.Fatalf("❌ Failed to start initial server instance: %v", err)
	}

	// Настраиваем таймер для перезапуска каждый час
	setupHourlyRestart()

	// Блокируем main goroutine
	select {}
}

func startNewServerInstance() error {
	log.Println("🚀 Starting new server instance...")

	// Инициализация глобального сервера БД
	config := db.DefaultConfig()
	config.Host = dbHost
	if err := db.InitGlobalServer(config); err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}

	// Создаем новый экземпляр сервера
	instance := &ServerInstance{
		shutdownComplete: make(chan struct{}),
	}

	var err error

	// Инициализация БД компонентов
	instance.server = db.GetGlobalServer()
	if instance.server == nil {
		return fmt.Errorf("server is nil")
	}

	instance.saleID, err = instance.server.CreateInitialSale()
	if err != nil {
		return fmt.Errorf("failed to create initial sale: %w", err)
	}

	// Создаем репозиторий checkout
	instance.checkoutRepo, err = db.NewCheckoutRepository(instance.server)
	if err != nil {
		return fmt.Errorf("failed to create checkout repository: %w", err)
	}

	instance.batchInserter = db.NewBatchInserter(instance.checkoutRepo, 100, 50*time.Millisecond)

	instance.saleItemsRepo, err = db.NewSaleItemsRepository(instance.server)
	if err != nil {
		instance.cleanup()
		return fmt.Errorf("failed to create sale items repository: %w", err)
	}

	instance.batchPurchase = db.NewBatchPurchaseUpdater(instance.saleItemsRepo, 10, 10*time.Millisecond)

	// Инициализация локального кеша
	instance.cache = megacache.NewMegacache(10000, 10) // 10000 лотов, 10 покупок на пользователя

	// ===== ВОССТАНОВЛЕНИЕ КЕША ИЗ БД =====
	log.Println("🔄 Recovering cache from database...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Создаем сервис восстановления кеша
	recoveryService := db.NewCacheRecoveryService(instance.checkoutRepo, instance.saleItemsRepo)

	// Восстанавливаем кеш с учетом проданных лотов
	if err := recoveryService.RecoverCacheWithSoldItems(ctx, instance.cache, instance.saleID); err != nil {
		instance.cleanup()
		return fmt.Errorf("failed to recover cache: %w", err)
	}

	log.Println("✅ Cache recovery completed successfully")

	// Устанавливаем флаг приема запросов
	atomic.StoreInt32(&instance.isAcceptingReqs, 1)

	// Настройка HTTP сервера
	mux := http.NewServeMux()
	mux.HandleFunc("/checkout", instance.checkoutHandler)
	mux.HandleFunc("/purchase", instance.purchaseHandler)

	instance.httpServer = &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	// Останавливаем предыдущий экземпляр и ждем его завершения
	if oldInstance := getCurrentInstance(); oldInstance != nil {
		log.Println("🔄 Stopping previous server instance...")
		go oldInstance.gracefulShutdown()
		// Ждем завершения старого сервера
		<-oldInstance.shutdownComplete
	}

	// Устанавливаем новый текущий экземпляр
	currentInstance.Store(instance)

	// Запускаем HTTP сервер в отдельной горутине
	go func() {
		log.Printf("🌐 Server starting on port 8080... Sale ID: %d", instance.saleID)
		if err := instance.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("❌ HTTP server error: %v", err)
		}
	}()

	return nil
}

func getCurrentInstance() *ServerInstance {
	if instance := currentInstance.Load(); instance != nil {
		return instance.(*ServerInstance)
	}
	return nil
}

func setupHourlyRestart() {
	go func() {
		// Вычисляем время до следующего часа
		now := time.Now()
		nextHour := now.Truncate(time.Hour).Add(time.Hour)
		//nextHour := now.Truncate(time.Minute).Add(time.Minute)
		timeUntilNextHour := nextHour.Sub(now)

		log.Printf("⏰ Next restart scheduled at: %s (in %v)", nextHour.Format("15:04:05"), timeUntilNextHour)

		// Первый таймер до следующего часа
		timer := time.NewTimer(timeUntilNextHour)

		for {
			<-timer.C

			log.Println("🔄 Hourly restart triggered")

			// Запускаем новый экземпляр сервера
			if err := startNewServerInstance(); err != nil {
				log.Printf("❌ Failed to restart server: %v", err)
			}

			// Устанавливаем таймер на следующий час
			timer.Reset(time.Hour)
			//timer.Reset(time.Minute)
		}
	}()
}

func (s *ServerInstance) gracefulShutdown() {
	log.Println("🛑 Starting graceful shutdown of server instance...")

	// Прекращаем прием новых запросов
	atomic.StoreInt32(&s.isAcceptingReqs, 0)

	// Даем время на завершение текущих запросов
	time.Sleep(500 * time.Millisecond)

	// Останавливаем HTTP сервер
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(ctx); err != nil {
		log.Printf("❌ HTTP server shutdown error: %v", err)
		s.httpServer.Close()
	}

	// Очищаем ресурсы
	s.cleanup()

	close(s.shutdownComplete)
	log.Println("✅ Server instance shutdown complete")
}

func (s *ServerInstance) cleanup() {
	if s.cache != nil {
		s.cache.Close()
	}

	if s.batchPurchase != nil {
		s.batchPurchase.Close()
	}

	if s.saleItemsRepo != nil {
		s.saleItemsRepo.Close()
	}

	if s.batchInserter != nil {
		s.batchInserter.Close()
	}

	if s.checkoutRepo != nil {
		s.checkoutRepo.Close()
	}
}

func (s *ServerInstance) isAcceptingRequests() bool {
	return atomic.LoadInt32(&s.isAcceptingReqs) == 1
}

func (s *ServerInstance) checkoutHandler(w http.ResponseWriter, r *http.Request) {
	// Проверяем, принимаем ли мы запросы
	if !s.isAcceptingRequests() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	// Этап 0: валидация запроса
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	queryParams, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	userIDStr := queryParams.Get("user_id")
	itemIDStr := queryParams.Get("item_id")

	// Преобразование userID в int64
	userID, err := strconv.ParseInt(userIDStr, 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Преобразование itemID в int64
	itemID, err := strconv.ParseInt(itemIDStr, 10, 64)
	if err != nil || itemID < 0 || itemID >= 10_000 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Этап 1: резервирование в локальном кеше
	checkout, err := s.cache.Checkout(userID, itemID)
	if err != nil {
		w.WriteHeader(http.StatusConflict)
		return
	}

	// Этап 2: сохранение резервирования в БД
	record := db.CheckoutRecord{
		UserID:    userID,
		ItemID:    itemID,
		Code:      checkout.Code,
		CreatedAt: checkout.CreatedAt,
		ExpiresAt: checkout.ExpiresAt,
	}

	if err := s.batchInserter.Add(record); err != nil {
		s.cache.DeleteCheckout(checkout.Code)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "%s", checkout.Code)
}

func (s *ServerInstance) purchaseHandler(w http.ResponseWriter, r *http.Request) {
	// Проверяем, принимаем ли мы запросы
	if !s.isAcceptingRequests() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	// Этап 0: валидация запроса
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	queryParams, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	codeStr := queryParams.Get("code")

	// Парсим строку в UUID
	code, err := uuid.Parse(codeStr)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Этап 1: попытка покупки в кеше
	checkout, success := s.cache.TryPurchase(code)
	if !success {
		w.WriteHeader(http.StatusConflict)
		return
	}

	// Этап 2: попытка покупки в БД
	err = s.batchPurchase.Purchase(s.saleID, checkout.LotIndex, checkout.UserID)
	if err != nil {
		// откат покупки в кеше
		s.cache.RollbackPurchase(code)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Этап 3: закрываем покупку в кеше
	s.cache.ConfirmPurchase(code)

	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "text/plain")
}
