// db.go

package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var server *Server

// Config конфигурация подключения к БД
type Config struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
	SSLMode  string

	// Настройки пула соединений
	MaxOpenConns    int           // Максимальное количество открытых соединений
	MaxIdleConns    int           // Максимальное количество неактивных соединений
	ConnMaxLifetime time.Duration // Максимальное время жизни соединения
	ConnMaxIdleTime time.Duration // Максимальное время простоя соединения

	// Настройки переподключения
	RetryAttempts       int
	RetryDelay          time.Duration
	HealthCheckInterval time.Duration

	// Настройки схемы
	AutoCreateSchema bool // Автоматически создавать схему при подключении
}

// DefaultConfig возвращает конфигурацию по умолчанию для высокого RPS
func DefaultConfig() *Config {
	return &Config{
		Host:     "postgres", // localhost postgres
		Port:     5432,
		User:     "postgres",
		Password: "password123",
		Database: "myapp",
		SSLMode:  "disable",

		// Настройки для высокого RPS
		MaxOpenConns:    200,              // Много соединений для параллельных запросов
		MaxIdleConns:    50,               // Держим соединения в пуле
		ConnMaxLifetime: 30 * time.Minute, // Обновляем соединения каждые 30 минут
		ConnMaxIdleTime: 5 * time.Minute,  // Закрываем простаивающие через 5 минут

		// Переподключение
		RetryAttempts:       5,
		RetryDelay:          time.Second,
		HealthCheckInterval: 10 * time.Second,

		// Схема
		AutoCreateSchema: true, // По умолчанию создаем схему автоматически
	}
}

// Server представляет сервер базы данных с пулом соединений
type Server struct {
	db     *sql.DB
	config *Config
	mu     sync.RWMutex
	ctx    context.Context
	cancel context.CancelFunc

	// Метрики
	connectionAttempts int64
	connectionFailures int64
	lastError          error
	lastConnectTime    time.Time
}

var serverOnce sync.Once

// Connect создает подключение к PostgreSQL с оптимизациями для высокого RPS
func Connect(config *Config) (*Server, error) {
	if config == nil {
		config = DefaultConfig()
	}

	ctx, cancel := context.WithCancel(context.Background())

	s := &Server{
		config: config,
		ctx:    ctx,
		cancel: cancel,
	}

	// Инициальное подключение
	if err := s.connect(); err != nil {
		cancel()
		return nil, fmt.Errorf("initial connection failed: %w", err)
	}

	// Устанавливаем временную зону UTC для соединения
	if _, err := s.db.Exec("SET TIME ZONE 'UTC'"); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to set UTC timezone: %w", err)
	}

	// Создаем схему если нужно
	if s.config.AutoCreateSchema {
		if err := s.createSchema(); err != nil {
			cancel()
			return nil, fmt.Errorf("schema creation failed: %w", err)
		}
	}

	// Запускаем мониторинг здоровья соединения
	go s.healthMonitor()

	return s, nil
}

// GetGlobalServer возвращает глобальный экземпляр сервера (singleton)
func GetGlobalServer() *Server {
	return server
}

// InitGlobalServer инициализирует глобальный сервер
func InitGlobalServer(config *Config) error {
	var err error
	serverOnce.Do(func() {
		server, err = Connect(config)
	})
	return err
}

// connect выполняет подключение к базе данных
func (s *Server) connect() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Строка подключения
	dsn := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		s.config.Host,
		s.config.Port,
		s.config.User,
		s.config.Password,
		s.config.Database,
		s.config.SSLMode,
	)

	// Дополнительные параметры для производительности
	dsn += " application_name=high_rps_app"
	dsn += " connect_timeout=10"
	dsn += " statement_timeout=30000"                   // 30 секунд
	dsn += " idle_in_transaction_session_timeout=60000" // 1 минута

	s.connectionAttempts++

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		s.connectionFailures++
		s.lastError = err
		return fmt.Errorf("failed to open database: %w", err)
	}

	// Настраиваем пул соединений для высокого RPS
	db.SetMaxOpenConns(s.config.MaxOpenConns)
	db.SetMaxIdleConns(s.config.MaxIdleConns)
	db.SetConnMaxLifetime(s.config.ConnMaxLifetime)
	db.SetConnMaxIdleTime(s.config.ConnMaxIdleTime)

	// Проверяем соединение
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		s.connectionFailures++
		s.lastError = err
		return fmt.Errorf("failed to ping database: %w", err)
	}

	// Закрываем старое соединение если есть
	if s.db != nil {
		s.db.Close()
	}

	s.db = db
	s.lastError = nil
	s.lastConnectTime = time.Now()

	log.Printf("📶 Connected to PostgreSQL: %s:%d/%s",
		s.config.Host, s.config.Port, s.config.Database)

	return nil
}

// createSchema создает все необходимые таблицы, индексы и функции
func (s *Server) createSchema() error {
	log.Println("📃 Creating database schema...")

	ctx, cancel := context.WithTimeout(s.ctx, 60*time.Second)
	defer cancel()

	// Получаем список SQL команд для создания схемы
	sqlCommands := s.getSchemaSQLCommands()

	for i, sqlCmd := range sqlCommands {
		if strings.TrimSpace(sqlCmd) == "" {
			continue
		}

		log.Printf("⚙️  Executing schema command %d/%d", i+1, len(sqlCommands))

		if _, err := s.db.ExecContext(ctx, sqlCmd); err != nil {
			// Игнорируем ошибки "already exists"
			if isAlreadyExistsError(err) {
				log.Printf("Schema object already exists (skipping): %v", err)
				continue
			}
			return fmt.Errorf("failed to execute schema command %d: %w", i+1, err)
		}
	}

	log.Println("✅ Database schema created successfully")
	return nil
}

// getSchemaSQLCommands возвращает список SQL команд для создания полной схемы
func (s *Server) getSchemaSQLCommands() []string {
	return []string{
		// Создание таблицы checkouts
		`CREATE TABLE IF NOT EXISTS checkouts (
			id BIGSERIAL PRIMARY KEY,
			user_id INTEGER NOT NULL,
			item_id INTEGER NOT NULL,
			code UUID UNIQUE NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT NOW(),
			expires_at TIMESTAMP NOT NULL
		)`,

		// Индекс для таблицы checkouts
		`CREATE INDEX IF NOT EXISTS idx_checkouts_expires_at ON checkouts(expires_at)`,

		// Создание таблицы sale_items
		`CREATE TABLE sale_items (
			id BIGSERIAL PRIMARY KEY,
			sale_id INTEGER NOT NULL,           		-- ID распродажи (например, hour of day)
			sale_start_hour TIMESTAMP NOT NULL, 		-- Час начала распродажи
			item_id INTEGER NOT NULL,           		-- ID лота от 0 до 9999 (10000 лотов)
			item_name VARCHAR(255) NOT NULL,    		-- Название товара
			image_url VARCHAR(500) NOT NULL,    		-- URL картинки
			purchased BOOLEAN NOT NULL DEFAULT FALSE, 	-- Флаг, куплен ли лот
			purchased_by INTEGER NULL,          		-- ID пользователя, кто купил
			purchased_at TIMESTAMP NULL         		-- Время покупки
		);`,

		// Уникальный индекс для sale_items
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_sale_items_sale_item ON sale_items(sale_id, item_id)`,

		// Функция create_new_sale
		`CREATE OR REPLACE FUNCTION create_new_sale() RETURNS INTEGER AS $$
		DECLARE
			max_sale_hour TIMESTAMP;
			max_sale_id INTEGER;
			new_sale_id INTEGER;
			new_sale_hour TIMESTAMP;
			current_hour TIMESTAMP;
			items_generated INTEGER;
		BEGIN
			-- Получаем текущий час (округленный)
			current_hour := date_trunc('hour', NOW());
			
			-- Находим максимальную запись в таблице
			SELECT sale_start_hour, sale_id 
			INTO max_sale_hour, max_sale_id
			FROM sale_items 
			ORDER BY sale_start_hour DESC, sale_id DESC
			LIMIT 1;
			
			-- Логика создания новой распродажи
			IF max_sale_hour IS NULL THEN
				-- Таблица пустая - создаем первую распродажу
				new_sale_id := 1;
				new_sale_hour := current_hour;
				RAISE NOTICE 'Table is empty. Creating first sale with ID % for hour %', new_sale_id, new_sale_hour;
				
			ELSIF max_sale_hour < current_hour THEN
				-- Последняя распродажа старше текущего часа - создаем на текущий час
				new_sale_id := max_sale_id + 1;
				new_sale_hour := current_hour;
				RAISE NOTICE 'Last sale % was at %. Creating new sale % for current hour %', 
					max_sale_id, max_sale_hour, new_sale_id, new_sale_hour;
					
			ELSIF max_sale_hour = current_hour THEN
				-- Распродажа на текущий час уже существует
				RAISE NOTICE 'Sale % for current hour % already exists. Returning existing sale_id.', 
					max_sale_id, current_hour;
				RETURN max_sale_id;
				
			ELSE
				-- Последняя распродажа в будущем - создаем следующую по порядку
				new_sale_id := max_sale_id + 1;
				new_sale_hour := max_sale_hour + INTERVAL '1 hour';
				RAISE NOTICE 'Creating next sequential sale % for hour %', new_sale_id, new_sale_hour;
			END IF;
			
			-- Проверяем, не существует ли уже распродажа с таким ID
			IF EXISTS (SELECT 1 FROM sale_items WHERE sale_id = new_sale_id LIMIT 1) THEN
				RAISE NOTICE 'Sale with ID % already exists. Returning existing sale_id.', new_sale_id;
				RETURN new_sale_id;
			END IF;
			
			-- Создаем 10,000 лотов для новой распродажи
			INSERT INTO sale_items (
				sale_id,
				sale_start_hour,
				item_id,
				item_name,
				image_url,
				purchased,
				purchased_by,
				purchased_at
			)
			SELECT 
				new_sale_id,
				new_sale_hour,
				item_counter,
				'Flash Item #' || item_counter || ' (Sale ' || new_sale_id || ')',
				'https://picsum.photos/200/200?random=' || new_sale_id || '_' || item_counter,
				false,  
				NULL,
				NULL
			FROM generate_series(0, 9999) AS item_counter;
			
			-- Проверяем количество созданных записей
			GET DIAGNOSTICS items_generated = ROW_COUNT;
			
			IF items_generated = 10000 THEN
				RAISE NOTICE 'Successfully created sale % with % items for hour %', 
					new_sale_id, items_generated, new_sale_hour;
			ELSE
				RAISE WARNING 'Expected 10000 items but created % for sale %', 
					items_generated, new_sale_id;
			END IF;
			
			RETURN new_sale_id;
			
		EXCEPTION
			WHEN OTHERS THEN
				RAISE EXCEPTION 'Error creating new sale: % (SQLSTATE: %)', SQLERRM, SQLSTATE;
				RETURN NULL; -- В случае ошибки вернет NULL
		END;
		$$ LANGUAGE plpgsql;`,
	}
}

// isAlreadyExistsError проверяет, является ли ошибка связанной с уже существующим объектом
func isAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}

	errStr := strings.ToLower(err.Error())
	existsErrors := []string{
		"already exists",
		"duplicate key",
		"relation already exists",
		"function already exists",
		"index already exists",
	}

	for _, existsErr := range existsErrors {
		if strings.Contains(errStr, existsErr) {
			return true
		}
	}

	return false
}

// CreateInitialSale создает первую распродажу если таблица пустая
func (s *Server) CreateInitialSale() (saleID int64, err error) {
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()

	log.Println("🔍 Checking if initial sale creation is needed...")

	// Используем QueryRowContext так как функция возвращает одно значение
	err = s.db.QueryRowContext(ctx, "SELECT create_new_sale()").Scan(&saleID)
	if err != nil {
		return 0, fmt.Errorf("❌ Failed to create initial sale: %w", err)
	}

	log.Printf("✅ Initial sale created successfully with saleID: %d", saleID)
	return saleID, nil
}

// reconnect выполняет переподключение с retry логикой
func (s *Server) reconnect() error {
	for attempt := 1; attempt <= s.config.RetryAttempts; attempt++ {
		log.Printf("⛓️‍💥 Attempting to reconnect to database (attempt %d/%d)",
			attempt, s.config.RetryAttempts)

		if err := s.connect(); err == nil {
			log.Printf("✅ Successfully reconnected to database")
			return nil
		}

		if attempt < s.config.RetryAttempts {
			select {
			case <-s.ctx.Done():
				return s.ctx.Err()
			case <-time.After(s.config.RetryDelay * time.Duration(attempt)):
				// Экспоненциальная задержка
			}
		}
	}

	return fmt.Errorf("❌ Failed to reconnect after %d attempts: %w",
		s.config.RetryAttempts, s.lastError)
}

// healthMonitor мониторит здоровье соединения и переподключается при необходимости
func (s *Server) healthMonitor() {
	ticker := time.NewTicker(s.config.HealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if err := s.ping(); err != nil {
				log.Printf("❌ Database health check failed: %v", err)
				if err := s.reconnect(); err != nil {
					log.Printf("❌ Failed to reconnect: %v", err)
				}
			}
		}
	}
}

// ping проверяет соединение с базой данных
func (s *Server) ping() error {
	s.mu.RLock()
	db := s.db
	s.mu.RUnlock()

	if db == nil {
		return fmt.Errorf("database connection is nil")
	}

	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	return db.PingContext(ctx)
}

// DB возвращает объект *sql.DB
func (s *Server) DB() *sql.DB {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db
}

// Stats возвращает статистику соединений
func (s *Server) Stats() sql.DBStats {
	s.mu.RLock()
	db := s.db
	s.mu.RUnlock()

	if db == nil {
		return sql.DBStats{}
	}

	return db.Stats()
}

// IsHealthy проверяет здоровье соединения
func (s *Server) IsHealthy() bool {
	return s.ping() == nil
}

// GetConnectionInfo возвращает информацию о соединении
func (s *Server) GetConnectionInfo() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := s.Stats()

	return map[string]interface{}{
		"connection_attempts": s.connectionAttempts,
		"connection_failures": s.connectionFailures,
		"last_connect_time":   s.lastConnectTime,
		"last_error":          s.lastError,
		"open_connections":    stats.OpenConnections,
		"in_use":              stats.InUse,
		"idle":                stats.Idle,
		"wait_count":          stats.WaitCount,
		"wait_duration":       stats.WaitDuration,
		"max_idle_closed":     stats.MaxIdleClosed,
		"max_lifetime_closed": stats.MaxLifetimeClosed,
	}
}

// Close закрывает соединение с базой данных
func (s *Server) Close() error {
	s.cancel()

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db != nil {
		return s.db.Close()
	}

	return nil
}

// ExecContext выполняет запрос с контекстом и автоматическим переподключением
func (s *Server) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	db := s.DB()
	if db == nil {
		return nil, fmt.Errorf("database connection is nil")
	}

	result, err := db.ExecContext(ctx, query, args...)
	if err != nil && isConnectionError(err) {
		log.Printf("❌ Connection error detected, attempting reconnect: %v", err)
		if reconnectErr := s.reconnect(); reconnectErr == nil {
			// Повторяем запрос после переподключения
			db = s.DB()
			if db != nil {
				return db.ExecContext(ctx, query, args...)
			}
		}
	}

	return result, err
}

// QueryContext выполняет запрос с контекстом и автоматическим переподключением
func (s *Server) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	db := s.DB()
	if db == nil {
		return nil, fmt.Errorf("database connection is nil")
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil && isConnectionError(err) {
		log.Printf("Connection error detected, attempting reconnect: %v", err)
		if reconnectErr := s.reconnect(); reconnectErr == nil {
			// Повторяем запрос после переподключения
			db = s.DB()
			if db != nil {
				return db.QueryContext(ctx, query, args...)
			}
		}
	}

	return rows, err
}

// isConnectionError проверяет, является ли ошибка проблемой соединения
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()
	connectionErrors := []string{
		"connection refused",
		"connection reset",
		"broken pipe",
		"no such host",
		"network is unreachable",
		"connection timed out",
		"driver: bad connection",
		"EOF",
	}

	for _, connErr := range connectionErrors {
		if contains(errStr, connErr) {
			return true
		}
	}

	return false
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr ||
		(len(s) > len(substr) &&
			(s[:len(substr)] == substr ||
				s[len(s)-len(substr):] == substr ||
				indexOf(s, substr) >= 0)))
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// Пример использования с автоматическим созданием схемы
// func ExampleUsage() {
// 	// Создание конфигурации с автоматическим созданием схемы
// 	config := &Config{
// 		Host:                "localhost",
// 		Port:                5432,
// 		User:                "myuser",
// 		Password:            "mypassword",
// 		Database:            "mydb",
// 		SSLMode:             "disable",
// 		MaxOpenConns:        200, // Для очень высокого RPS
// 		MaxIdleConns:        50,
// 		ConnMaxLifetime:     30 * time.Minute,
// 		ConnMaxIdleTime:     5 * time.Minute,
// 		RetryAttempts:       3,
// 		RetryDelay:          2 * time.Second,
// 		HealthCheckInterval: 30 * time.Second,
// 		AutoCreateSchema:    true, // Автоматически создаем схему
// 	}

// 	// Инициализация глобального сервера (автоматически создаст схему)
// 	if err := InitGlobalServer(config); err != nil {
// 		log.Fatal("Failed to initialize database:", err)
// 	}

// 	// Получаем сервер
// 	server := GetGlobalServer()

// 	// Создаем первую распродажу если нужно
// 	if err := server.CreateInitialSale(); err != nil {
// 		log.Printf("Failed to create initial sale: %v", err)
// 	}

// 	// Использование
// 	ctx := context.Background()
// 	rows, err := server.QueryContext(ctx, "SELECT * FROM get_sales_status()")
// 	if err != nil {
// 		log.Printf("Query failed: %v", err)
// 		return
// 	}
// 	defer rows.Close()

// 	// Обработка результатов...

// 	// Получение статистики
// 	info := server.GetConnectionInfo()
// 	log.Printf("DB Stats: %+v", info)
// }
