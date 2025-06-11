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

// ServerInstance represents a single server instance with all its dependencies / представляет один экземпляр сервера со всеми его зависимостями
type ServerInstance struct {
	server           *db.Server               // Database server connection / Подключение к серверу базы данных
	checkoutRepo     *db.CheckoutRepository   // Repository for checkout operations / Репозиторий для операций checkout
	batchInserter    *db.BatchInserter        // Batch inserter for performance / Пакетная вставка для производительности
	saleItemsRepo    *db.SaleItemsRepository  // Repository for sale items / Репозиторий для товаров в продаже
	batchPurchase    *db.BatchPurchaseUpdater // Batch purchase updater / Пакетное обновление покупок
	cache            *megacache.Megacache     // Local cache for fast operations / Локальный кеш для быстрых операций
	saleID           int64                    // Current sale ID / ID текущей распродажи
	httpServer       *http.Server             // HTTP server instance / Экземпляр HTTP сервера
	isAcceptingReqs  int32                    // Atomic boolean for request acceptance / Атомарный флаг приема запросов
	shutdownComplete chan struct{}            // Channel to signal shutdown completion / Канал для сигнала завершения остановки
	dbHost           string                   // Database host address / Адрес хоста базы данных
}

// Initialize timezone to UTC for consistent time handling / Инициализация временной зоны в UTC для консистентной работы с временем
func init() {
	time.Local = time.UTC
}

var (
	currentInstance atomic.Value // *ServerInstance - Current active server instance / Текущий активный экземпляр сервера
)

// Global database host variable / Глобальная переменная хоста базы данных
var dbHost string

// Main function - entry point of the application / точка входа в приложение
func main() {
	// Get database host from environment variable or use default / Получение хоста базы данных из переменной окружения или использование значения по умолчанию
	dbHost = os.Getenv("DB_HOST")
	if dbHost == "" {
		dbHost = "localhost"
	}

	// Start the first server instance / Запускаем первый экземпляр сервера
	if err := startNewServerInstance(); err != nil {
		log.Fatalf("❌ Failed to start initial server instance: %v", err)
	}

	// Setup timer for hourly restarts /  Настраиваем таймер для перезапуска каждый час
	setupHourlyRestart()

	// Block main goroutine indefinitely / Блокируем main goroutine
	select {}
}

// startNewServerInstance creates and starts a new server instance / создает и запускает новый экземпляр сервера
func startNewServerInstance() error {
	log.Println("🚀 Starting new server instance...")

	// Initialize global database server / Инициализация глобального сервера БД
	config := db.DefaultConfig()
	config.Host = dbHost
	if err := db.InitGlobalServer(config); err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}

	// Create new server instance / Создаем новый экземпляр сервера
	instance := &ServerInstance{
		shutdownComplete: make(chan struct{}),
	}

	var err error

	// Initialize database components / Инициализация БД компонентов
	instance.server = db.GetGlobalServer()
	if instance.server == nil {
		return fmt.Errorf("server is nil")
	}

	// Create initial sale record / Создание записи начальной распродажи
	instance.saleID, err = instance.server.CreateInitialSale()
	if err != nil {
		return fmt.Errorf("failed to create initial sale: %w", err)
	}

	// Create checkout repository / Создаем репозиторий checkout
	instance.checkoutRepo, err = db.NewCheckoutRepository(instance.server)
	if err != nil {
		return fmt.Errorf("failed to create checkout repository: %w", err)
	}

	// Initialize batch inserter with 100 batch size and 50ms flush interval / Инициализация пакетной вставки с размером пакета 100 и интервалом сброса 50мс
	instance.batchInserter = db.NewBatchInserter(instance.checkoutRepo, 100, 50*time.Millisecond)

	// Create sale items repository / Создание репозитория товаров в продаже
	instance.saleItemsRepo, err = db.NewSaleItemsRepository(instance.server)
	if err != nil {
		instance.cleanup()
		return fmt.Errorf("failed to create sale items repository: %w", err)
	}

	// Initialize batch purchase updater with 10 batch size and 10ms flush interval / Инициализация пакетного обновления покупок с размером пакета 10 и интервалом сброса 10мс
	instance.batchPurchase = db.NewBatchPurchaseUpdater(instance.saleItemsRepo, 10, 10*time.Millisecond)

	// Initialize local cache with 10000 lots and 10 purchases per user / Инициализация локального кеша с 10000 лотов и 10 покупок на пользователя
	instance.cache = megacache.NewMegacache(10000, 10)

	// ===== CACHE RECOVERY FROM DATABASE =====
	// ===== ВОССТАНОВЛЕНИЕ КЕША ИЗ БД =====
	log.Println("🔄 Recovering cache from database...")

	// Create context with timeout for cache recovery / Создание контекста с таймаутом для восстановления кеша
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create cache recovery service / Создаем сервис восстановления кеша
	recoveryService := db.NewCacheRecoveryService(instance.checkoutRepo, instance.saleItemsRepo)

	// Recover cache considering sold lots / Восстанавливаем кеш с учетом проданных лотов
	if err := recoveryService.RecoverCacheWithSoldItems(ctx, instance.cache, instance.saleID); err != nil {
		instance.cleanup()
		return fmt.Errorf("failed to recover cache: %w", err)
	}

	log.Println("✅ Cache recovery completed successfully")

	// Set flag to accept requests / Устанавливаем флаг приема запросов
	atomic.StoreInt32(&instance.isAcceptingReqs, 1)

	// Setup HTTP server with routes / Настройка HTTP сервера
	mux := http.NewServeMux()
	mux.HandleFunc("/checkout", instance.checkoutHandler)
	mux.HandleFunc("/purchase", instance.purchaseHandler)

	instance.httpServer = &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	// Stop previous instance and wait for completion / Останавливаем предыдущий экземпляр и ждем его завершения
	if oldInstance := getCurrentInstance(); oldInstance != nil {
		log.Println("🔄 Stopping previous server instance...")
		go oldInstance.gracefulShutdown()
		// Wait for old server to complete shutdown / Ждем завершения старого сервера
		<-oldInstance.shutdownComplete
	}

	// Set new current instance / Устанавливаем новый текущий экземпляр
	currentInstance.Store(instance)

	// Start HTTP server in separate goroutine / Запускаем HTTP сервер в отдельной горутине
	go func() {
		log.Printf("🌐 Server starting on port 8080... Sale ID: %d", instance.saleID)
		if err := instance.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("❌ HTTP server error: %v", err)
		}
	}()

	return nil
}

// getCurrentInstance returns the current active server instance / возвращает текущий активный экземпляр сервера
func getCurrentInstance() *ServerInstance {
	if instance := currentInstance.Load(); instance != nil {
		return instance.(*ServerInstance)
	}
	return nil
}

// setupHourlyRestart configures automatic hourly server restarts / настраивает автоматические ежечасные перезапуски сервера
func setupHourlyRestart() {
	go func() {
		// Calculate time until next hour / Вычисляем время до следующего часа
		now := time.Now()
		nextHour := now.Truncate(time.Hour).Add(time.Hour)
		//nextHour := now.Truncate(time.Minute).Add(time.Minute) // For testing: restart every minute / Для тестирования: перезапуск каждую минуту
		timeUntilNextHour := nextHour.Sub(now)

		log.Printf("⏰ Next restart scheduled at: %s (in %v)", nextHour.Format("15:04:05"), timeUntilNextHour)

		// First timer until next hour / Первый таймер до следующего часа
		timer := time.NewTimer(timeUntilNextHour)

		for {
			<-timer.C

			log.Println("🔄 Hourly restart triggered")

			// Start new server instance / Запускаем новый экземпляр сервера
			if err := startNewServerInstance(); err != nil {
				log.Printf("❌ Failed to restart server: %v", err)
			}

			// Set timer for next hour / Устанавливаем таймер на следующий час
			timer.Reset(time.Hour)
			//timer.Reset(time.Minute) // For testing: restart every minute / Для тестирования: перезапуск каждую минуту
		}
	}()
}

// gracefulShutdown performs graceful shutdown of the server instance / выполняет корректное завершение работы экземпляра сервера
func (s *ServerInstance) gracefulShutdown() {
	log.Println("🛑 Starting graceful shutdown of server instance...")

	// Stop accepting new requests / Прекращаем прием новых запросов
	atomic.StoreInt32(&s.isAcceptingReqs, 0)

	// Give time for current requests to complete / Даем время на завершение текущих запросов
	time.Sleep(500 * time.Millisecond)

	// Stop HTTP server with timeout /  Останавливаем HTTP сервер
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(ctx); err != nil {
		log.Printf("❌ HTTP server shutdown error: %v", err)
		s.httpServer.Close()
	}

	// Clean up resources / Очищаем ресурсы
	s.cleanup()

	close(s.shutdownComplete)
	log.Println("✅ Server instance shutdown complete")
}

// cleanup releases all resources used by the server instance / освобождает все ресурсы, используемые экземпляром сервера
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

// isAcceptingRequests checks if the server instance is accepting new requests / проверяет, принимает ли экземпляр сервера новые запросы
func (s *ServerInstance) isAcceptingRequests() bool {
	return atomic.LoadInt32(&s.isAcceptingReqs) == 1
}

// checkoutHandler handles POST requests to reserve items for users / обрабатывает POST запросы для резервирования товаров пользователями
func (s *ServerInstance) checkoutHandler(w http.ResponseWriter, r *http.Request) {
	// Check if we're accepting requests / Проверяем, принимаем ли мы запросы
	if !s.isAcceptingRequests() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	// Stage 0: Request validation / валидация запроса
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Parse query parameters / Парсинг параметров запроса
	queryParams, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	userIDStr := queryParams.Get("user_id")
	itemIDStr := queryParams.Get("item_id")

	userID, err := strconv.ParseInt(userIDStr, 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	itemID, err := strconv.ParseInt(itemIDStr, 10, 64)
	if err != nil || itemID < 0 || itemID >= 10_000 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Stage 1: Reserve in local cache / резервирование в локальном кеше
	checkout, err := s.cache.Checkout(userID, itemID)
	if err != nil {
		w.WriteHeader(http.StatusConflict)
		return
	}

	// Stage 2: Save reservation to database / сохранение резервирования в БД
	record := db.CheckoutRecord{
		UserID:    userID,
		ItemID:    itemID,
		Code:      checkout.Code,
		CreatedAt: checkout.CreatedAt,
		ExpiresAt: checkout.ExpiresAt,
	}

	// Add to batch inserter, rollback cache on failure / Добавление в пакетную вставку, откат кеша при ошибке
	if err := s.batchInserter.Add(record); err != nil {
		s.cache.DeleteCheckout(checkout.Code)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Return checkout code to client / Возвращаем код checkout клиенту
	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "%s", checkout.Code)
}

// purchaseHandler handles POST requests to complete purchases using checkout codes / обрабатывает POST запросы для завершения покупок с использованием кодов checkout
func (s *ServerInstance) purchaseHandler(w http.ResponseWriter, r *http.Request) {
	// Check if we're accepting requests / Проверяем, принимаем ли мы запросы
	if !s.isAcceptingRequests() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	// Stage 0: Request validation / Этап 0: валидация запроса
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Parse query parameters / Парсинг параметров запроса
	queryParams, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	codeStr := queryParams.Get("code")

	// Parse string to UUID / Парсим строку в UUID
	code, err := uuid.Parse(codeStr)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Stage 1: Attempt purchase in cache / попытка покупки в кеше
	checkout, success := s.cache.TryPurchase(code)
	if !success {
		w.WriteHeader(http.StatusConflict)
		return
	}

	// Stage 2: Attempt purchase in database / попытка покупки в БД
	err = s.batchPurchase.Purchase(s.saleID, checkout.LotIndex, checkout.UserID)
	if err != nil {
		// Rollback purchase in cache on database failure / откат покупки в кеше
		s.cache.RollbackPurchase(code)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Stage 3: Confirm purchase in cache / закрываем покупку в кеше
	s.cache.ConfirmPurchase(code)

	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "text/plain")
}
