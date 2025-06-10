// checkout.go

package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// CheckoutRepository инкапсулирует все методы работы с checkouts
type CheckoutRepository struct {
	server              *Server // Ссылка на сервер для переподключений
	db                  *sql.DB
	insertStmt          *sql.Stmt
	updatePurchaseStmt  *sql.Stmt
	batchInsertStmt     *sql.Stmt
	multiRowInsertCache map[int]string // Кеш для многострочных запросов
}

// NewCheckoutRepository создает новый репозиторий с подготовленными выражениями
func NewCheckoutRepository(server *Server) (*CheckoutRepository, error) {
	db := server.DB()
	if db == nil {
		return nil, fmt.Errorf("database connection is nil")
	}

	ctx := context.Background()

	// Подготавливаем базовые выражения
	insertStmt, err := db.PrepareContext(ctx, `
		INSERT INTO checkouts (user_id, item_id, code, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`)
	if err != nil {
		return nil, fmt.Errorf("prepare insert: %w", err)
	}

	updateStmt, err := db.PrepareContext(ctx, `
		UPDATE checkouts SET expires_at = $1 WHERE code = $2`)
	if err != nil {
		return nil, fmt.Errorf("prepare update: %w", err)
	}

	batchInsertStmt, err := db.PrepareContext(ctx, `
		INSERT INTO checkouts (user_id, item_id, code, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5)`)
	if err != nil {
		return nil, fmt.Errorf("prepare batch insert: %w", err)
	}

	return &CheckoutRepository{
		server:              server,
		db:                  db,
		insertStmt:          insertStmt,
		updatePurchaseStmt:  updateStmt,
		batchInsertStmt:     batchInsertStmt,
		multiRowInsertCache: make(map[int]string),
	}, nil
}

// Close освобождает ресурсы
func (r *CheckoutRepository) Close() error {
	var errs []error

	if err := r.insertStmt.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := r.updatePurchaseStmt.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := r.batchInsertStmt.Close(); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors closing statements: %v", errs)
	}
	return nil
}

// InsertOne подготовленный INSERT для одной записи, возвращает сгенерированный ID
func (r *CheckoutRepository) InsertOne(ctx context.Context, record CheckoutRecord) (int64, error) {
	var id int64
	err := r.insertStmt.QueryRowContext(ctx,
		record.UserID,
		record.ItemID,
		record.Code,
		record.CreatedAt,
		record.ExpiresAt,
	).Scan(&id)
	return id, err
}

// BatchInsert пакетная вставка в транзакции
func (r *CheckoutRepository) BatchInsert(ctx context.Context, records []CheckoutRecord) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt := tx.StmtContext(ctx, r.batchInsertStmt)
	defer stmt.Close()

	for _, record := range records {
		if _, err := stmt.ExecContext(ctx,
			record.UserID,
			record.ItemID,
			record.Code,
			record.CreatedAt,
			record.ExpiresAt,
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// MultiRowInsert многострочный INSERT (VALUES (..), (..), ...)
func (r *CheckoutRepository) MultiRowInsert(ctx context.Context, records []CheckoutRecord) error {
	// Используем кешированный запрос если есть
	query, ok := r.multiRowInsertCache[len(records)]
	if !ok {
		// Генерируем запрос с нужным количеством плейсхолдеров
		query = generateMultiRowQuery(len(records))
		r.multiRowInsertCache[len(records)] = query
	}

	// Подготавливаем значения
	values := make([]interface{}, 0, len(records)*5)
	for _, record := range records {
		values = append(values,
			record.UserID,
			record.ItemID,
			record.Code,
			record.CreatedAt,
			record.ExpiresAt,
		)
	}

	// Используем метод сервера с автоматическим переподключением
	_, err := r.server.ExecContext(ctx, query, values...)
	return err
}

// UpdatePurchase обновляет время покупки по коду
func (r *CheckoutRepository) UpdatePurchase(ctx context.Context, code uuid.UUID, purchaseTime time.Time) error {
	_, err := r.updatePurchaseStmt.ExecContext(ctx, purchaseTime, code)
	return err
}

func generateMultiRowQuery(count int) string {
	var sb strings.Builder
	sb.WriteString(`INSERT INTO checkouts (user_id, item_id, code, created_at, expires_at) VALUES `)

	placeholders := make([]string, count)
	for i := 0; i < count; i++ {
		placeholders[i] = fmt.Sprintf("($%d,$%d,$%d,$%d,$%d)",
			i*5+1, i*5+2, i*5+3, i*5+4, i*5+5)
	}

	sb.WriteString(strings.Join(placeholders, ","))
	return sb.String()
}

// CheckoutRecord представляет запись о checkout
type CheckoutRecord struct {
	ID        int64     `json:"id" db:"id"`
	UserID    int64     `json:"user_id" db:"user_id"`
	ItemID    int64     `json:"item_id" db:"item_id"`
	Code      uuid.UUID `json:"code" db:"code"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	ExpiresAt time.Time `json:"expires_at" db:"expires_at"`
}

// pendingRecord представляет запись ожидающую вставки
type pendingRecord struct {
	record CheckoutRecord
	result chan error
}

// BatchInserter накапливает записи и выполняет пакетную вставку
// Исправленная версия без дедлоков
type BatchInserter struct {
	repo      *CheckoutRepository
	batchSize int
	timeout   time.Duration
	buffer    []pendingRecord
	timer     *time.Timer
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	flushCh   chan struct{} // Канал для принудительного флеша
}

// NewBatchInserter создает новый батчер
func NewBatchInserter(repo *CheckoutRepository, batchSize int, timeout time.Duration) *BatchInserter {
	ctx, cancel := context.WithCancel(context.Background())

	bi := &BatchInserter{
		repo:      repo,
		batchSize: batchSize,
		timeout:   timeout,
		buffer:    make([]pendingRecord, 0, batchSize),
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		flushCh:   make(chan struct{}, 1), // Буферизованный канал
	}

	// Запускаем воркер для обработки флешей
	go bi.worker()

	return bi
}

// worker обрабатывает флеши в отдельной горутине
func (bi *BatchInserter) worker() {
	defer close(bi.done)

	for {
		select {
		case <-bi.flushCh:
			bi.performFlush()
		case <-bi.ctx.Done():
			// Финальный флеш перед завершением
			bi.performFlush()
			return
		}
	}
}

// Add добавляет запись в буфер и ждет результата вставки
func (bi *BatchInserter) Add(record CheckoutRecord) error {
	// Создаем канал для получения результата
	resultChan := make(chan error, 1)

	bi.mu.Lock()

	// Добавляем запись в буфер
	bi.buffer = append(bi.buffer, pendingRecord{
		record: record,
		result: resultChan,
	})

	shouldFlush := len(bi.buffer) >= bi.batchSize
	shouldStartTimer := len(bi.buffer) == 1 && !shouldFlush

	bi.mu.Unlock()

	// Обработка флеша и таймера ВНЕ критической секции
	if shouldFlush {
		// Неблокирующая отправка сигнала флеша
		select {
		case bi.flushCh <- struct{}{}:
		default:
			// Если канал полный, флеш уже запланирован
		}
	} else if shouldStartTimer {
		// Останавливаем предыдущий таймер если есть
		bi.stopTimer()

		// Запускаем новый таймер
		bi.mu.Lock()
		bi.timer = time.AfterFunc(bi.timeout, func() {
			// Неблокирующая отправка сигнала флеша
			select {
			case bi.flushCh <- struct{}{}:
			default:
				// Флеш уже запланирован
			}
		})
		bi.mu.Unlock()
	}

	// Ждем результата
	select {
	case err := <-resultChan:
		return err
	case <-bi.ctx.Done():
		return bi.ctx.Err()
	}
}

// stopTimer безопасно останавливает таймер
func (bi *BatchInserter) stopTimer() {
	bi.mu.Lock()
	if bi.timer != nil {
		bi.timer.Stop()
		bi.timer = nil
	}
	bi.mu.Unlock()
}

// performFlush выполняет фактический флеш
func (bi *BatchInserter) performFlush() {
	bi.mu.Lock()

	if len(bi.buffer) == 0 {
		bi.mu.Unlock()
		return
	}

	// Останавливаем таймер
	if bi.timer != nil {
		bi.timer.Stop()
		bi.timer = nil
	}

	// Копируем буфер для вставки
	pendingRecords := make([]pendingRecord, len(bi.buffer))
	copy(pendingRecords, bi.buffer)

	// Очищаем буфер
	bi.buffer = bi.buffer[:0]

	bi.mu.Unlock()

	// Извлекаем записи для вставки
	records := make([]CheckoutRecord, len(pendingRecords))
	for i, pr := range pendingRecords {
		records[i] = pr.record
	}

	// Выполняем вставку
	err := bi.repo.MultiRowInsert(bi.ctx, records)

	// Отправляем результат всем ожидающим
	for _, pr := range pendingRecords {
		select {
		case pr.result <- err:
		case <-bi.ctx.Done():
			return
		}
	}
}

// Flush принудительно выполняет вставку всех накопленных записей
func (bi *BatchInserter) Flush() error {
	// Отправляем сигнал флеша
	select {
	case bi.flushCh <- struct{}{}:
	case <-bi.ctx.Done():
		return bi.ctx.Err()
	}

	// Ждем небольшой период для завершения флеша
	// В реальном коде можно добавить синхронизацию через дополнительный канал
	time.Sleep(10 * time.Millisecond)

	return nil
}

// FlushAndWait выполняет флеш и ждет его завершения
func (bi *BatchInserter) FlushAndWait() error {
	// Создаем фиктивную запись для синхронизации
	dummyRecord := CheckoutRecord{
		UserID:    -1, // Специальный маркер
		CreatedAt: time.Now(),
	}

	// Добавляем фиктивную запись, которая вызовет флеш
	return bi.Add(dummyRecord)
}

// Close завершает работу батчера
func (bi *BatchInserter) Close() error {
	// Останавливаем таймер
	bi.stopTimer()

	// Отменяем контекст для завершения воркера
	bi.cancel()

	// Ждем завершения воркера
	<-bi.done

	return nil
}

// Дополнительный безопасный метод для получения статистики
func (bi *BatchInserter) Stats() (buffered int, isActive bool) {
	bi.mu.Lock()
	defer bi.mu.Unlock()

	return len(bi.buffer), bi.timer != nil
}

// GetActiveReservations возвращает все активные резервации для восстановления кеша
func (r *CheckoutRepository) GetActiveReservations(ctx context.Context) ([]CheckoutRecord, error) {
	query := `
		SELECT id, user_id, item_id, code, created_at, expires_at
		FROM checkouts 
		WHERE expires_at > NOW()
		ORDER BY created_at`

	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query active reservations: %w", err)
	}
	defer rows.Close()

	var reservations []CheckoutRecord
	for rows.Next() {
		var reservation CheckoutRecord
		err := rows.Scan(
			&reservation.ID,
			&reservation.UserID,
			&reservation.ItemID,
			&reservation.Code,
			&reservation.CreatedAt,
			&reservation.ExpiresAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan reservation: %w", err)
		}
		reservations = append(reservations, reservation)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return reservations, nil
}

// // CleanupExpiredReservations удаляет истекшие резервации из БД
// func (r *CheckoutRepository) CleanupExpiredReservations(ctx context.Context) (int64, error) {
// 	query := `DELETE FROM checkouts WHERE expires_at <= NOW()`

// 	result, err := r.db.ExecContext(ctx, query)
// 	if err != nil {
// 		return 0, fmt.Errorf("cleanup expired reservations: %w", err)
// 	}

// 	affected, err := result.RowsAffected()
// 	if err != nil {
// 		return 0, fmt.Errorf("get rows affected: %w", err)
// 	}

// 	return affected, nil
// }

// // DeleteReservation удаляет конкретную резервацию
// func (r *CheckoutRepository) DeleteReservation(ctx context.Context, code uuid.UUID) error {
// 	query := `DELETE FROM checkouts WHERE code = $1`

// 	_, err := r.db.ExecContext(ctx, query, code)
// 	if err != nil {
// 		return fmt.Errorf("delete reservation: %w", err)
// 	}

// 	return nil
// }

// GetReservationByCode получает резервацию по коду
func (r *CheckoutRepository) GetReservationByCode(ctx context.Context, code uuid.UUID) (*CheckoutRecord, error) {
	query := `
		SELECT id, user_id, item_id, code, created_at, expires_at
		FROM checkouts 
		WHERE code = $1`

	var reservation CheckoutRecord
	err := r.db.QueryRowContext(ctx, query, code).Scan(
		&reservation.ID,
		&reservation.UserID,
		&reservation.ItemID,
		&reservation.Code,
		&reservation.CreatedAt,
		&reservation.ExpiresAt,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // Резервация не найдена
		}
		return nil, fmt.Errorf("get reservation by code: %w", err)
	}

	return &reservation, nil
}

// BatchDeleteReservations удаляет несколько резерваций за раз
func (r *CheckoutRepository) BatchDeleteReservations(ctx context.Context, codes []uuid.UUID) error {
	if len(codes) == 0 {
		return nil
	}

	// Создаем плейсхолдеры для IN clause
	placeholders := make([]string, len(codes))
	args := make([]interface{}, len(codes))

	for i, code := range codes {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = code
	}

	query := fmt.Sprintf(`DELETE FROM checkouts WHERE code IN (%s)`,
		strings.Join(placeholders, ","))

	_, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("batch delete reservations: %w", err)
	}

	return nil
}
