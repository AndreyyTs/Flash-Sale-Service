// saleitems.go

package db

import (
	"contest_notcoin/megacache"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// SaleItemsRepository инкапсулирует все методы работы с sale_items
type SaleItemsRepository struct {
	server           *Server
	db               *sql.DB
	purchaseItemStmt *sql.Stmt
	queryCache       map[string]string // Кеш для многострочных запросов
	cacheMutex       sync.RWMutex      // Мьютекс для защиты кеша
}

// NewSaleItemsRepository создает новый репозиторий с подготовленными выражениями
func NewSaleItemsRepository(server *Server) (*SaleItemsRepository, error) {
	db := server.DB()
	if db == nil {
		return nil, fmt.Errorf("database connection is nil")
	}

	ctx := context.Background()

	// Покупка одного лота
	purchaseItemStmt, err := db.PrepareContext(ctx, `
		UPDATE sale_items 
		SET purchased = true, purchased_by = $1, purchased_at = $2
		WHERE sale_id = $3 AND item_id = $4 AND purchased = false`)
	if err != nil {
		return nil, fmt.Errorf("prepare purchase item: %w", err)
	}

	return &SaleItemsRepository{
		server:           server,
		db:               db,
		purchaseItemStmt: purchaseItemStmt,
		queryCache:       make(map[string]string),
	}, nil
}

// Close освобождает ресурсы
func (r *SaleItemsRepository) Close() error {
	var errs []error

	if r.purchaseItemStmt != nil {
		if err := r.purchaseItemStmt.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors closing statements: %v", errs)
	}
	return nil
}

// PurchaseItem покупает лот (может быть свободным или зарезервированным)
func (r *SaleItemsRepository) PurchaseItem(ctx context.Context, saleID, itemID, userID int64) error {
	result, err := r.purchaseItemStmt.ExecContext(ctx, userID, time.Now(), saleID, itemID)
	if err != nil {
		return fmt.Errorf("execute purchase query: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("get rows affected: %w", err)
	}

	if affected == 0 {
		return fmt.Errorf("item not available for purchase: sale_id=%d, item_id=%d", saleID, itemID)
	}

	return nil
}

// BatchPurchaseItem многострочная покупка лотов
func (r *SaleItemsRepository) BatchPurchaseItem(ctx context.Context, purchases []ItemPurchase) error {
	if len(purchases) == 0 {
		return nil
	}

	// Генерируем запрос для множественного обновления
	query := r.getOrCreateBatchPurchaseQuery(len(purchases))

	// Подготавливаем значения: сначала время, потом все остальные параметры
	now := time.Now()
	values := make([]interface{}, 0, 1+len(purchases)*3)
	values = append(values, now) // Первый параметр - время

	for _, purchase := range purchases {
		values = append(values, purchase.UserID, purchase.SaleID, purchase.ItemID)
	}

	// Выполняем запрос
	result, err := r.server.ExecContext(ctx, query, values...)
	if err != nil {
		return fmt.Errorf("execute batch purchase: %w", err)
	}

	// Проверяем количество обновленных строк
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("get rows affected: %w", err)
	}

	if affected != int64(len(purchases)) {
		return fmt.Errorf("expected %d purchases, but %d items were updated", len(purchases), affected)
	}

	return nil
}

// getOrCreateBatchPurchaseQuery thread-safe получение или создание кешированного запроса покупки
func (r *SaleItemsRepository) getOrCreateBatchPurchaseQuery(count int) string {
	cacheKey := fmt.Sprintf("batch_purchase_%d", count)

	// Сначала пытаемся прочитать из кеша
	r.cacheMutex.RLock()
	if query, exists := r.queryCache[cacheKey]; exists {
		r.cacheMutex.RUnlock()
		return query
	}
	r.cacheMutex.RUnlock()

	// Если нет в кеше, генерируем и сохраняем
	r.cacheMutex.Lock()
	defer r.cacheMutex.Unlock()

	// Проверяем еще раз, возможно кто-то уже добавил пока мы ждали Lock
	if query, exists := r.queryCache[cacheKey]; exists {
		return query
	}

	// Генерируем новый запрос
	query := generateBatchPurchaseQuery(count)
	r.queryCache[cacheKey] = query
	return query
}

// generateBatchPurchaseQuery генерирует запрос для множественной покупки
func generateBatchPurchaseQuery(count int) string {
	// Создаем запрос с VALUES для множественного обновления
	// $1 - время покупки, остальные параметры - данные покупок
	query := `
		UPDATE sale_items
		SET purchased = true, purchased_by = updates.user_id, purchased_at = $1
		FROM (VALUES `

	valueParts := make([]string, count)
	for i := 0; i < count; i++ {
		// Параметры начинаются с $2 (т.к. $1 - время)
		valueParts[i] = fmt.Sprintf("($%d::integer, $%d::integer, $%d::integer)",
			i*3+2, i*3+3, i*3+4)
	}

	query += strings.Join(valueParts, ", ")
	query += `) AS updates(user_id, sale_id, item_id) 
		WHERE sale_items.sale_id = updates.sale_id 
		AND sale_items.item_id = updates.item_id 
		AND sale_items.purchased = false`

	return query
}

// ItemPurchase представляет информацию о покупке лота
type ItemPurchase struct {
	SaleID int64
	ItemID int64
	UserID int64
}

// SaleItem представляет лот в распродаже
type SaleItem struct {
	ID            int64      `json:"id" db:"id"`
	SaleID        int        `json:"sale_id" db:"sale_id"`
	SaleStartHour time.Time  `json:"sale_start_hour" db:"sale_start_hour"`
	ItemID        int        `json:"item_id" db:"item_id"`
	ItemName      string     `json:"item_name" db:"item_name"`
	ImageURL      string     `json:"image_url" db:"image_url"`
	Purchased     bool       `json:"purchased" db:"purchased"`
	PurchasedBy   *int       `json:"purchased_by" db:"purchased_by"`
	PurchasedAt   *time.Time `json:"purchased_at" db:"purchased_at"`
}

// BatchPurchaseUpdater накапливает покупки и выполняет пакетное обновление
type BatchPurchaseUpdater struct {
	repo      *SaleItemsRepository
	batchSize int
	timeout   time.Duration
	buffer    []pendingPurchase
	timer     *time.Timer
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
}

// pendingPurchase представляет покупку ожидающую выполнения
type pendingPurchase struct {
	purchase ItemPurchase
	result   chan error
}

// NewBatchPurchaseUpdater создает новый батчер для покупок
func NewBatchPurchaseUpdater(repo *SaleItemsRepository, batchSize int, timeout time.Duration) *BatchPurchaseUpdater {
	ctx, cancel := context.WithCancel(context.Background())

	return &BatchPurchaseUpdater{
		repo:      repo,
		batchSize: batchSize,
		timeout:   timeout,
		buffer:    make([]pendingPurchase, 0, batchSize),
		ctx:       ctx,
		cancel:    cancel,
	}
}

// Purchase добавляет покупку в буфер и ждет результата
func (bpu *BatchPurchaseUpdater) Purchase(saleID, itemID, userID int64) error {
	bpu.mu.Lock()

	// Создаем канал для получения результата
	resultChan := make(chan error, 1)

	// Добавляем покупку в буфер
	bpu.buffer = append(bpu.buffer, pendingPurchase{
		purchase: ItemPurchase{
			SaleID: saleID,
			ItemID: itemID,
			UserID: userID,
		},
		result: resultChan,
	})

	// Если буфер полный, выполняем обновление
	if len(bpu.buffer) >= bpu.batchSize {
		bpu.flushLocked()
		bpu.mu.Unlock()
	} else {
		// Если это первое обновление в буфере, запускаем таймер
		if len(bpu.buffer) == 1 {
			bpu.timer = time.AfterFunc(bpu.timeout, func() {
				bpu.mu.Lock()
				defer bpu.mu.Unlock()
				if len(bpu.buffer) > 0 {
					bpu.flushLocked()
				}
			})
		}
		bpu.mu.Unlock()
	}

	// Ждем результата
	select {
	case err := <-resultChan:
		return err
	case <-bpu.ctx.Done():
		return bpu.ctx.Err()
	}
}

// flushLocked выполняет обновление (должен вызываться под мьютексом)
func (bpu *BatchPurchaseUpdater) flushLocked() {
	if len(bpu.buffer) == 0 {
		return
	}

	// Останавливаем таймер если он есть
	if bpu.timer != nil {
		bpu.timer.Stop()
		bpu.timer = nil
	}

	// Копируем буфер для обновления
	pendingPurchases := make([]pendingPurchase, len(bpu.buffer))
	copy(pendingPurchases, bpu.buffer)

	// Очищаем буфер
	bpu.buffer = bpu.buffer[:0]

	// Выполняем обновление в отдельной горутине
	go func() {
		// Извлекаем покупки
		purchases := make([]ItemPurchase, len(pendingPurchases))
		for i, pp := range pendingPurchases {
			purchases[i] = pp.purchase
		}

		// Выполняем пакетную покупку
		err := bpu.repo.BatchPurchaseItem(bpu.ctx, purchases)

		// Отправляем результат всем ожидающим
		for _, pp := range pendingPurchases {
			select {
			case pp.result <- err:
			case <-bpu.ctx.Done():
				return
			}
		}
	}()
}

// Flush принудительно выполняет все накопленные покупки
func (bpu *BatchPurchaseUpdater) Flush() error {
	bpu.mu.Lock()

	if len(bpu.buffer) == 0 {
		bpu.mu.Unlock()
		return nil
	}

	// Копируем все ожидающие покупки
	allPending := make([]pendingPurchase, len(bpu.buffer))
	copy(allPending, bpu.buffer)

	// Очищаем буфер
	bpu.buffer = bpu.buffer[:0]

	// Останавливаем таймер если он есть
	if bpu.timer != nil {
		bpu.timer.Stop()
		bpu.timer = nil
	}

	bpu.mu.Unlock()

	// Выполняем обновление
	purchases := make([]ItemPurchase, len(allPending))
	for i, pp := range allPending {
		purchases[i] = pp.purchase
	}

	err := bpu.repo.BatchPurchaseItem(bpu.ctx, purchases)

	// Отправляем результат всем ожидающим
	for _, pp := range allPending {
		select {
		case pp.result <- err:
		case <-bpu.ctx.Done():
			return bpu.ctx.Err()
		}
	}

	return err
}

// Close завершает работу батчера
func (bpu *BatchPurchaseUpdater) Close() error {
	if err := bpu.Flush(); err != nil {
		return err
	}
	bpu.cancel()
	return nil
}

// GetAvailableItems возвращает доступные лоты для покупки
func (r *SaleItemsRepository) GetAvailableItems(ctx context.Context, saleID int64, limit int) ([]SaleItem, error) {
	query := `
		SELECT id, sale_id, sale_start_hour, item_id, item_name, image_url, 
		       purchased, purchased_by, purchased_at
		FROM sale_items 
		WHERE sale_id = $1 AND purchased = false 
		ORDER BY item_id 
		LIMIT $2`

	rows, err := r.db.QueryContext(ctx, query, saleID, limit)
	if err != nil {
		return nil, fmt.Errorf("query available items: %w", err)
	}
	defer rows.Close()

	var items []SaleItem
	for rows.Next() {
		var item SaleItem
		err := rows.Scan(&item.ID, &item.SaleID, &item.SaleStartHour, &item.ItemID,
			&item.ItemName, &item.ImageURL, &item.Purchased, &item.PurchasedBy, &item.PurchasedAt)
		if err != nil {
			return nil, fmt.Errorf("scan item: %w", err)
		}
		items = append(items, item)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return items, nil
}

// GetPurchasedItems возвращает купленные лоты пользователя
func (r *SaleItemsRepository) GetPurchasedItems(ctx context.Context, userID int64) ([]SaleItem, error) {
	query := `
		SELECT id, sale_id, sale_start_hour, item_id, item_name, image_url, 
		       purchased, purchased_by, purchased_at
		FROM sale_items 
		WHERE purchased_by = $1 
		ORDER BY purchased_at DESC`

	rows, err := r.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("query purchased items: %w", err)
	}
	defer rows.Close()

	var items []SaleItem
	for rows.Next() {
		var item SaleItem
		err := rows.Scan(&item.ID, &item.SaleID, &item.SaleStartHour, &item.ItemID,
			&item.ItemName, &item.ImageURL, &item.Purchased, &item.PurchasedBy, &item.PurchasedAt)
		if err != nil {
			return nil, fmt.Errorf("scan item: %w", err)
		}
		items = append(items, item)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return items, nil
}

// GetUserPurchaseStats возвращает статистику покупок пользователей для восстановления кеша
func (r *SaleItemsRepository) GetPurchaseStats(ctx context.Context, saleID int64) ([]megacache.SaleItems, error) {
	query := `
		SELECT item_id, purchased, purchased_by
		FROM sale_items 
		WHERE sale_id = $1 AND purchased = true AND purchased_by IS NOT NULL`

	rows, err := r.db.QueryContext(ctx, query, saleID)
	if err != nil {
		return nil, fmt.Errorf("query user purchase stats: %w", err)
	}
	defer rows.Close()

	var stats []megacache.SaleItems
	for rows.Next() {
		var stat megacache.SaleItems
		err := rows.Scan(&stat.ItemID, &stat.Purchased, &stat.UserID)
		if err != nil {
			return nil, fmt.Errorf("scan user purchase stat: %w", err)
		}
		stats = append(stats, stat)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return stats, nil
}

// GetSoldItemsForSale возвращает проданные лоты для конкретной продажи
func (r *SaleItemsRepository) GetSoldItemsForSale(ctx context.Context, saleID int64) (map[int64]bool, error) {
	query := `
		SELECT item_id
		FROM sale_items 
		WHERE sale_id = $1 AND purchased = true
		ORDER BY item_id`

	rows, err := r.db.QueryContext(ctx, query, saleID)
	if err != nil {
		return nil, fmt.Errorf("query sold items: %w", err)
	}
	defer rows.Close()

	soldItems := make(map[int64]bool)
	for rows.Next() {
		var itemID int64
		err := rows.Scan(&itemID)
		if err != nil {
			return nil, fmt.Errorf("scan sold item: %w", err)
		}
		soldItems[itemID] = true
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return soldItems, nil
}

// GetSaleItemsCount возвращает общее количество лотов в продаже
func (r *SaleItemsRepository) GetSaleItemsCount(ctx context.Context, saleID int64) (int64, error) {
	query := `SELECT COUNT(*) FROM sale_items WHERE sale_id = $1`

	var count int64
	err := r.db.QueryRowContext(ctx, query, saleID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get sale items count: %w", err)
	}

	return count, nil
}

// GetPurchasedItemsCount возвращает количество купленных лотов в продаже
func (r *SaleItemsRepository) GetPurchasedItemsCount(ctx context.Context, saleID int64) (int64, error) {
	query := `SELECT COUNT(*) FROM sale_items WHERE sale_id = $1 AND purchased = true`

	var count int64
	err := r.db.QueryRowContext(ctx, query, saleID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get purchased items count: %w", err)
	}

	return count, nil
}

// ===== Конвертер данных =====

// Converter для преобразования DB записей в формат кеша
type CacheDataConverter struct{}

// ConvertCheckoutRecordsToCache преобразует DB записи в формат для кеша
func (c *CacheDataConverter) ConvertCheckoutRecordsToCache(records []CheckoutRecord) []megacache.Checkout {
	checkouts := make([]megacache.Checkout, len(records))

	for i, record := range records {
		checkouts[i] = megacache.Checkout{
			Code:      record.Code,
			UserID:    record.UserID,
			LotIndex:  record.ItemID, // item_id соответствует LotIndex в кеше
			ExpiresAt: record.ExpiresAt,
			Status:    megacache.CheckoutStatusActive, // Все загружаемые резервы активны
			CreatedAt: record.CreatedAt,
		}
	}

	return checkouts
}

// ===== Пример использования для восстановления кеша =====

// CacheRecoveryService объединяет логику восстановления кеша
type CacheRecoveryService struct {
	checkoutRepo  *CheckoutRepository
	saleItemsRepo *SaleItemsRepository
	converter     *CacheDataConverter
}

// NewCacheRecoveryService создает новый сервис восстановления
func NewCacheRecoveryService(checkoutRepo *CheckoutRepository, saleItemsRepo *SaleItemsRepository) *CacheRecoveryService {
	return &CacheRecoveryService{
		checkoutRepo:  checkoutRepo,
		saleItemsRepo: saleItemsRepo,
		converter:     &CacheDataConverter{},
	}
}

// RecoverCache восстанавливает кеш из базы данных
func (s *CacheRecoveryService) RecoverCache(ctx context.Context, cache *megacache.Megacache, saleID int64) error {
	// 1. Загружаем активные резервации
	reservationRecords, err := s.checkoutRepo.GetActiveReservations(ctx)
	if err != nil {
		return fmt.Errorf("load reservations: %w", err)
	}

	// Конвертируем в формат кеша
	reservations := s.converter.ConvertCheckoutRecordsToCache(reservationRecords)

	// Загружаем в кеш
	cache.LoadReservationsFromDB(reservations)

	// 2. Загружаем статистику покупок пользователей
	userData, err := s.saleItemsRepo.GetPurchaseStats(ctx, saleID)
	if err != nil {
		return fmt.Errorf("load user stats: %w", err)
	}

	// Загружаем в кеш (данные уже в нужном формате megacache.UserPurchaseData)
	err = cache.LoadUserDataFromDB(userData)
	if err != nil {
		return fmt.Errorf("load user data to cache: %w", err)
	}

	// 3. Очищаем истекшие резервации из БД
	cleaned, err := s.checkoutRepo.CleanupExpiredReservations(ctx)
	if err != nil {
		return fmt.Errorf("cleanup expired reservations: %w", err)
	}

	// cache.

	fmt.Printf("Cache recovery completed: loaded %d reservations, %d users, cleaned %d expired\n",
		len(reservations), len(userData), cleaned)

	return nil
}

// RecoverCacheWithSoldItems восстанавливает кеш с учетом проданных лотов
func (s *CacheRecoveryService) RecoverCacheWithSoldItems(ctx context.Context, cache *megacache.Megacache, saleID int64) error {
	// Сначала стандартное восстановление
	err := s.RecoverCache(ctx, cache, saleID)
	if err != nil {
		return err
	}

	// Дополнительно загружаем проданные лоты для корректировки статусов
	soldItems, err := s.saleItemsRepo.GetSoldItemsForSale(ctx, saleID)
	if err != nil {
		return fmt.Errorf("load sold items: %w", err)
	}

	// Здесь можно добавить логику для установки статуса "продан"
	// для соответствующих лотов в кеше, если такой метод будет добавлен

	fmt.Printf("Loaded %d sold items for cache correction\n", len(soldItems))

	return nil
}
