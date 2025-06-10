package megacache

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

var (
	// Reservation errors / Ошибки резервирования

	ErrGeneral              = errors.New("something went wrong")          // ERROR: something went wrong / ОШИБКА: что-то пошло не так
	ErrItemAlreadyReserved  = errors.New("item already reserved")         // ERROR: item already reserved / ОШИБКА: лот уже зарезервирован
	ErrItemAlreadySold      = errors.New("item already sold")             // ERROR: item already sold / ОШИБКА: лот уже продан
	ErrInvalidItemID        = errors.New("invalid item ID")               // ERROR: invalid item ID / ОШИБКА: неверный ID лота
	ErrReservationNotFound  = errors.New("reservation not found")         // ERROR: reservation not found / ОШИБКА: резерв не найден
	ErrReservationCompleted = errors.New("reservation already completed") // ERROR: reservation already completed / ОШИБКА: резерв уже завершён

	// User limitation errors / Ошибки пользовательских ограничений

	ErrAllItemsPurchased  = errors.New("all items already purchased")                // ERROR: all items already purchased / ОШИБКА: все товары уже куплены
	ErrUserLimitExceeded  = errors.New("user purchase limit reached (max 10 items)") // ERROR: user purchase limit reached / ОШИБКА: достигнут лимит покупок (макс. 10)
	ErrServiceOverloaded  = errors.New("service overloaded, please try again later") // ERROR: service overloaded / ОШИБКА: сервис перегружен
	ErrPurchaseNotAllowed = errors.New("purchase not allowed")                       // ERROR: purchase not allowed / ОШИБКА: покупка невозможна
)

// Checkout timeout duration / Время блокировки лота
const checkoutTime = 3 * time.Second

// UnifiedCache - unified cache for reservations and user limitations / бъединенный кеш для резервирования и ограничений пользователей
type Megacache struct {
	// Mutexes for data protection / Мьютексы для защиты доступа
	checkoutMu sync.RWMutex // protects checkouts / для защиты checkouts
	userMu     sync.RWMutex // protects users / для защиты users

	// Reservation data / Данные резервирования
	checkouts map[uuid.UUID]Checkout // checkout cache / кеш для хранения checkout
	lots      []Lot                  // array of lots / массив лотов

	// User data / Данные пользователей
	users        map[int64]*int64 // userID -> purchaseCount
	limitPerUser int64            // max purchases per user / макс. количество покупок у пользователя
	// countUsers   int64            // current count of users who purchased something / текущее кол-во пользователей которые что-то купили
	limitUsers int64 // max number of users / макс. количество пользователей
	countLots  int64 // сколько лотов уже купленно
	nLots      int64 // кол-во лотов

	// Background task management / Для управления фоновой задачей
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// CheckoutStatus represents the status of a reservation / представляет статус резервирования
type CheckoutStatus int

const (
	CheckoutStatusActive    CheckoutStatus = iota // 0 - active reservation / активный резерв
	CheckoutStatusPurchased                       // 1 - purchase confirmed / покупка подтверждена
	CheckoutStatusCancelled                       // 2 - reservation cancelled / резерв отменен
)

// Checkout represents a reservation record / представляет запись о резервировании
type Checkout struct {
	Code      uuid.UUID
	UserID    int64          // User ID / ID пользователя
	LotIndex  int64          // Lot index / индекс лота
	ExpiresAt time.Time      // Reservation expiration time / время истечения резерва
	Status    CheckoutStatus // Reservation status / статус резерва
	CreatedAt time.Time      // Creation time (for logging) / время создания (для логирования)
}

// Lot represents a single lot with atomic status / представляет отдельный лот с атомарным статусом
type Lot struct {
	status uint32 // lot status (atomic variable) / статус лота (атомарная переменная)
}

// Lot status constants / Константы статуса лота
const (
	StatusAvailable uint32 = iota // 0 - lot available / лот доступен
	StatusReserved                // 1 - lot reserved / лот зарезервирован
	StatusSold                    // 2 - lot sold / лот продан
)

// SaleItems -  данные таблицы sale_items БД
type SaleItems struct {
	ItemID    int64
	Purchased bool
	UserID    int64
}

// NewUnifiedCache creates a new unified cache / создает новый объединенный кеш
func NewMegacache(itemsCount int64, limitPerUser int64) *Megacache {
	ctx, cancel := context.WithCancel(context.Background())

	cache := &Megacache{
		// Initialize reservation data / Инициализация данных резервирования
		checkouts: make(map[uuid.UUID]Checkout),
		lots:      make([]Lot, itemsCount),

		// Initialize user data / Инициализация пользовательских данных
		users:        make(map[int64]*int64, itemsCount),
		limitPerUser: limitPerUser,
		limitUsers:   itemsCount,
		countLots:    0,
		nLots:        itemsCount,

		// Context for background tasks / Контекст для фоновых задач
		ctx:    ctx,
		cancel: cancel,
	}

	// Start background task for cleaning expired reservations / Запускаем фоновую задачу для удаления истекших резервов
	cache.wg.Add(1)
	go func() {
		cache.cleanupExpiredReservations()
	}()

	return cache
}

// Checkout reserves a lot for a user with limit checks / резервирует лот для пользователя с проверкой лимитов
func (c *Megacache) Checkout(userID int64, itemID int64) (checkout Checkout, err error) {
	if c.countLots >= int64(len(c.lots)) {
		return Checkout{}, ErrAllItemsPurchased
	}

	// Check array bounds / Проверяем границы массива
	if itemID < 0 || itemID >= int64(len(c.lots)) {
		return Checkout{}, ErrInvalidItemID
	}

	// Check user limits BEFORE reserving / Проверяем лимиты пользователя ПЕРЕД резервированием
	if err := c.checkUserLimits(userID); err != nil {
		return Checkout{}, err
	}

	// Get pointer to lot for correct atomic operations / Получаем указатель на лот для корректной работы atomic операций
	lot := &c.lots[itemID]

	// Check current lot status / Проверяем текущий статус лота
	currentStatus := atomic.LoadUint32(&lot.status)

	// Lot already reserved / Лот уже зарезервирован
	if currentStatus == StatusReserved {
		return Checkout{}, ErrItemAlreadyReserved
	}

	// Lot already sold / Лот уже продан
	if currentStatus == StatusSold {
		return Checkout{}, ErrItemAlreadySold
	}

	// Attempt to reserve the lot / Попытка зарезервировать лот
	if atomic.CompareAndSwapUint32(&lot.status, StatusAvailable, StatusReserved) {
		code := uuid.New()
		now := time.Now()
		expiresAt := now.Add(checkoutTime)

		checkout := Checkout{
			Code:      code,
			UserID:    userID,
			LotIndex:  itemID,
			ExpiresAt: expiresAt,
			Status:    CheckoutStatusActive,
			CreatedAt: now,
		}

		// Safely add reservation to map / Безопасно добавляем резерв в map
		c.checkoutMu.Lock()
		c.checkouts[code] = checkout
		c.checkoutMu.Unlock()

		return checkout, nil
	}

	// If reservation failed, check final status / Если не удалось зарезервировать, проверяем окончательный статус
	finalStatus := atomic.LoadUint32(&lot.status)
	if finalStatus == StatusSold {
		return Checkout{}, ErrItemAlreadySold
	}
	return Checkout{}, ErrItemAlreadyReserved
}

// checkUserLimits checks user limits (internal method) / проверяет лимиты пользователя (внутренний метод)
func (c *Megacache) checkUserLimits(userID int64) error {
	// Check if there are still items available for purchase / Проверка что еще есть товары для покупок
	if atomic.LoadInt64(&c.countLots) >= c.limitUsers {
		return ErrAllItemsPurchased
	}

	c.userMu.RLock()
	userCount, exists := c.users[userID]
	c.userMu.RUnlock()

	if exists && atomic.LoadInt64(userCount) >= c.limitPerUser {
		return ErrUserLimitExceeded
	}

	return nil
}

// TryPurchase attempts to purchase a reserved lot with user limit checks / попытка купить зарезервированный лот с учетом лимитов пользователя
func (c *Megacache) TryPurchase(code uuid.UUID) (Checkout, bool) {
	if c.countLots >= int64(len(c.lots)) {
		return Checkout{}, false
	}
	// Safely read reservation information / Безопасно читаем информацию о резерве
	c.checkoutMu.RLock()
	checkout, exists := c.checkouts[code]
	c.checkoutMu.RUnlock()

	if !exists {
		return Checkout{}, false // reservation not found / резерв не найден
	}

	// Check reservation status / Проверяем статус резерва
	if checkout.Status != CheckoutStatusActive {
		return Checkout{}, false // reservation already completed or cancelled / резерв уже завершен или отменен
	}

	// Check if reservation has expired / Проверяем, не истек ли срок резерва
	if checkout.ExpiresAt.Before(time.Now()) {
		c.CancelCheckout(code)
		return Checkout{}, false
	}

	// Check array bounds / Проверяем границы массива
	if checkout.LotIndex < 0 || checkout.LotIndex >= int64(len(c.lots)) {
		c.CancelCheckout(code)
		return Checkout{}, false
	}

	// Check and increment user purchase counter / Проверяем и увеличиваем счетчик покупок пользователя
	newCount, err := c.incrementUserPurchase(checkout.UserID)
	if err != nil {
		return Checkout{}, false
	}

	// Attempt to purchase lot (change status from "reserved" to "sold")/ Попытка купить лот (изменить статус с "зарезервирован" на "продан")
	lot := &c.lots[checkout.LotIndex]
	if atomic.CompareAndSwapUint32(&lot.status, StatusReserved, StatusSold) {
		// Change reservation status to "purchased" / Меняем статус резерва на "куплен"
		c.checkoutMu.Lock()
		if existingCheckout, exists := c.checkouts[code]; exists && existingCheckout.Status == CheckoutStatusActive {
			existingCheckout.Status = CheckoutStatusPurchased
			c.checkouts[code] = existingCheckout
		}
		c.checkoutMu.Unlock()
		return checkout, true
	}

	// Instead rollback directly / Вместо этого откатываем напрямую
	c.rollbackUserPurchase(checkout.UserID, newCount)
	return Checkout{}, false
}

// rollbackUserPurchase rolls back specific counter increment (without blocking) / откатывает конкретное увеличение счетчика (без блокировки)
func (c *Megacache) rollbackUserPurchase(userID int64, expectedCount int64) {
	c.userMu.RLock()
	userCount, exists := c.users[userID]
	c.userMu.RUnlock()

	if exists {
		// Try to rollback exactly the value we incremented / Пытаемся откатить именно то значение, которое мы увеличили
		atomic.CompareAndSwapInt64(userCount, expectedCount, expectedCount-1)
	}
}

// incrementUserPurchase increments user purchase counter / увеличивает счетчик покупок пользователя
func (c *Megacache) incrementUserPurchase(userID int64) (int64, error) {
	// Check global limit / Проверяем глобальный лимит
	if atomic.LoadInt64(&c.countLots) >= c.nLots {
		return 0, ErrAllItemsPurchased
	}

	c.userMu.Lock()
	defer c.userMu.Unlock()

	if userCount, exists := c.users[userID]; exists {
		// User already exists / Пользователь уже существует
		currentCount := atomic.LoadInt64(userCount)
		if currentCount >= c.limitPerUser {
			return 0, ErrUserLimitExceeded
		}

		// Atomically increment counter / Атомарно увеличиваем счетчик
		for {
			if currentCount >= c.limitPerUser {
				return 0, ErrUserLimitExceeded
			}
			if atomic.CompareAndSwapInt64(userCount, currentCount, currentCount+1) {
				return currentCount + 1, nil
			}
			currentCount = atomic.LoadInt64(userCount)
		}
	} else {
		// New user / Новый пользователь
		count := int64(1)
		c.users[userID] = &count
		return 1, nil
	}
}

// decrementUserPurchase decrements user purchase counter (for rollback) / уменьшает счетчик покупок пользователя (для отката)
func (c *Megacache) decrementUserPurchase(userID int64) {
	c.userMu.RLock()
	userCount, exists := c.users[userID]
	c.userMu.RUnlock()

	if exists {
		for {
			currentCount := atomic.LoadInt64(userCount)
			if currentCount <= 0 {
				break
			}
			if atomic.CompareAndSwapInt64(userCount, currentCount, currentCount-1) {
				break
			}
		}
	}
}

// ConfirmPurchase confirms purchase and removes reservation / подтверждает покупку и удаляет резерв
func (c *Megacache) ConfirmPurchase(code uuid.UUID) {
	c.checkoutMu.Lock()
	defer c.checkoutMu.Unlock()

	checkout, exists := c.checkouts[code]
	if !exists || checkout.Status != CheckoutStatusPurchased {
		return
	}

	atomic.AddInt64(&c.countLots, 1)
	// Remove reservation - purchase confirmed / Удаляем резерв - покупка подтверждена
	delete(c.checkouts, code)
}

// RollbackPurchase rolls back a purchase / откатывает покупку
func (c *Megacache) RollbackPurchase(code uuid.UUID) {
	c.checkoutMu.Lock()
	checkout, exists := c.checkouts[code]
	if exists && checkout.Status == CheckoutStatusPurchased {
		// Return reservation status to active / Возвращаем статус резерва в активный
		checkout.Status = CheckoutStatusActive
		c.checkouts[code] = checkout
	}
	c.checkoutMu.Unlock()

	if !exists {
		return
	}

	// Rollback user counter / Откатываем счетчик пользователя
	c.decrementUserPurchase(checkout.UserID)

	// Rollback lot status / Откатываем статус лота
	if checkout.LotIndex >= 0 && checkout.LotIndex < int64(len(c.lots)) {
		lot := &c.lots[checkout.LotIndex]
		atomic.CompareAndSwapUint32(&lot.status, StatusSold, StatusReserved)
	}
}

// CancelCheckout cancels a reservation / отменяет резерв
func (c *Megacache) CancelCheckout(code uuid.UUID) error {
	c.checkoutMu.Lock()
	checkout, exists := c.checkouts[code]
	if exists {
		checkout.Status = CheckoutStatusCancelled
		c.checkouts[code] = checkout
	}
	c.checkoutMu.Unlock()

	if !exists {
		return ErrReservationNotFound
	}

	// Release the lot / Освобождаем лот
	if checkout.LotIndex >= 0 && checkout.LotIndex < int64(len(c.lots)) {
		lot := &c.lots[checkout.LotIndex]
		atomic.CompareAndSwapUint32(&lot.status, StatusReserved, StatusAvailable)
	}

	return nil
}

// DeleteCheckout completely removes reservation from memory / полностью удаляет резерв из памяти
func (c *Megacache) DeleteCheckout(code uuid.UUID) {
	c.checkoutMu.Lock()
	defer c.checkoutMu.Unlock()

	if checkout, exists := c.checkouts[code]; exists {
		if checkout.Status == CheckoutStatusCancelled || checkout.Status == CheckoutStatusPurchased {
			delete(c.checkouts, code)
		}
	}
}

// GetPurchaseCount returns user's purchase count / возвращает количество покупок пользователя
func (c *Megacache) GetPurchaseCount(userID int64) (int64, bool) {
	c.userMu.RLock()
	defer c.userMu.RUnlock()

	userCount, exists := c.users[userID]
	if !exists {
		return 0, false
	}
	return atomic.LoadInt64(userCount), true
}

// GetCheckoutInfo returns reservation information / возвращает информацию о резерве
func (c *Megacache) GetCheckoutInfo(code uuid.UUID) (Checkout, bool) {
	c.checkoutMu.RLock()
	defer c.checkoutMu.RUnlock()
	checkout, exists := c.checkouts[code]
	return checkout, exists
}

// GetLotStatus returns current lot status / возвращает текущий статус лота
func (c *Megacache) GetLotStatus(itemID int64) (uint32, error) {
	if itemID < 0 || itemID >= int64(len(c.lots)) {
		return 0, ErrInvalidItemID
	}
	return atomic.LoadUint32(&c.lots[itemID].status), nil
}

// GetActiveReservationsCount returns number of active reservations / возвращает количество активных резервов
func (c *Megacache) GetActiveReservationsCount() int {
	c.checkoutMu.RLock()
	defer c.checkoutMu.RUnlock()

	count := 0
	for _, checkout := range c.checkouts {
		if checkout.Status == CheckoutStatusActive {
			count++
		}
	}
	return count
}

// cleanupExpiredReservations - background task for cleaning expired reservations / фоновая задача для очистки истекших резервов
func (c *Megacache) cleanupExpiredReservations() {
	defer c.wg.Done() // Mark goroutine as done / Отмечаем завершение горутины

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return // Context cancelled / Контекст отменен
		case <-ticker.C:
			c.cleanupExpired()
		}
	}
}

// cleanupExpired cleans expired reservations WITHOUT DEADLOCK / очищает истекшие резервы БЕЗ ДЕДЛОКА
func (c *Megacache) cleanupExpired() {
	now := time.Now()
	var expiredCodes []uuid.UUID
	var oldCodes []uuid.UUID

	// Collect codes of expired active reservations / Собираем коды истекших активных резервов
	c.checkoutMu.RLock()
	for code, checkout := range c.checkouts {
		if checkout.Status == CheckoutStatusActive && checkout.ExpiresAt.Before(now) {
			expiredCodes = append(expiredCodes, code)
		}

		// Collect old completed reservations (older than 1 hour) in the same loop / Собираем старые завершенные резервы (старше 1 часа) в том же цикле
		oldThreshold := now.Add(-1 * time.Hour)
		if (checkout.Status == CheckoutStatusCancelled || checkout.Status == CheckoutStatusPurchased) &&
			checkout.CreatedAt.Before(oldThreshold) {
			oldCodes = append(oldCodes, code)
		}
	}
	c.checkoutMu.RUnlock() // IMPORTANT! Release lock BEFORE calling other methods / ВАЖНО! Освобождаем блокировку ДО вызова других методов

	// Now cancel all expired reservations (WITHOUT holding RLock) / Теперь отменяем все истекшие резервы (БЕЗ удержания RLock)
	for _, code := range expiredCodes {
		c.CancelCheckout(code) // Now safe - RLock already released / Теперь безопасно - RLock уже освобожден
	}

	// Remove old completed reservations / Удаляем старые завершенные резервы
	for _, code := range oldCodes {
		c.DeleteCheckout(code)
	}
}

// LoadUserDataFromDB loads user data from database on startup / загружает данные пользователей из БД при старте
func (c *Megacache) LoadUserDataFromDB(saleItems []SaleItems) error {
	c.userMu.Lock()
	defer c.userMu.Unlock()

	// Clear current data / Очищаем текущие данные
	c.users = make(map[int64]*int64, len(saleItems))
	atomic.StoreInt64(&c.countLots, 0)

	c.countLots = 0

	// Counters for statistics / Счетчики для статистики
	var totalPurchasedItems int64
	var uniqueUsers int64
	userPurchaseCounts := make(map[int64]int64)

	for _, val := range saleItems {
		// Validate data / Проверяем валидность данных
		if val.ItemID < 0 || val.ItemID >= int64(len(c.lots)) {
			continue // Skip invalid itemIDs / Пропускаем невалидные itemID
		}

		if val.Purchased {
			// Increase purchase counter for user / Увеличиваем счетчик покупок для пользователя
			userPurchaseCounts[val.UserID]++
			totalPurchasedItems++
			c.countLots++

			// Mark lot as sold / Устанавливаем статус лота как проданный
			atomic.StoreUint32(&c.lots[val.ItemID].status, StatusSold)
		}
	}

	// Update users structure / Обновляем структуру пользователей
	for userID, purchaseCount := range userPurchaseCounts {
		count := purchaseCount
		c.users[userID] = &count
		uniqueUsers++
	}

	// Calculate additional statistics / Вычисляем дополнительную статистику
	var availableItems int64
	var reservedItems int64
	var soldItems int64

	for i := range c.lots {
		status := atomic.LoadUint32(&c.lots[i].status)
		switch status {
		case StatusAvailable:
			availableItems++
		case StatusReserved:
			reservedItems++
		case StatusSold:
			soldItems++
		}
	}

	// Print restoration statistics / Вывод статистики восстановления
	log.Printf("📊 Cache restoration statistics:")
	log.Printf("   📦 Total items: %d", len(c.lots))
	log.Printf("   ✅ Available items: %d", availableItems)
	log.Printf("   🔒 Reserved items: %d", reservedItems)
	log.Printf("   💰 Sold items: %d", soldItems)
	log.Printf("   👥 Unique users: %d", uniqueUsers)
	log.Printf("   🛒 Total purchases: %d", totalPurchasedItems)
	log.Printf("   📈 Sales rate: %.2f%%", float64(soldItems)/float64(len(c.lots))*100)

	// User statistics (top buyers) / Статистика по пользователям (топ покупателей)
	if len(userPurchaseCounts) > 0 {
		log.Printf("   🏆 User purchase distribution:")

		// Group users by purchase count / Группируем пользователей по количеству покупок
		purchaseDistribution := make(map[int64]int64) // purchase count -> user count / количество покупок -> количество пользователей
		maxPurchases := int64(0)

		for _, count := range userPurchaseCounts {
			purchaseDistribution[count]++
			if count > maxPurchases {
				maxPurchases = count
			}
		}

		// Print distribution / Выводим распределение
		for purchases := int64(1); purchases <= maxPurchases; purchases++ {
			if userCount, exists := purchaseDistribution[purchases]; exists {
				log.Printf("     - %d purchase(s): %d users", purchases, userCount)
			}
		}

		log.Printf("   📊 Average purchases per user: %.2f", float64(totalPurchasedItems)/float64(uniqueUsers))
	}

	return nil
}

// LoadReservationsFromDB loads reservations from database on startup / загружает резервы из БД при старте
func (c *Megacache) LoadReservationsFromDB(reservations []Checkout) {
	c.checkoutMu.Lock()
	defer c.checkoutMu.Unlock()

	var activeReservations int64
	var expiredReservations int64
	var completedReservations int64
	now := time.Now()

	for _, reservation := range reservations {
		// Check lot index validity / Проверяем валидность индекса лота
		if reservation.LotIndex >= 0 && reservation.LotIndex < int64(len(c.lots)) {
			atomic.StoreUint32(&c.lots[reservation.LotIndex].status, StatusReserved)
		}

		c.checkouts[reservation.Code] = reservation

		// Analyze reservation status / Анализируем статус резервации
		switch reservation.Status {
		case CheckoutStatusActive:
			if reservation.ExpiresAt.Before(now) {
				expiredReservations++
			} else {
				activeReservations++
			}
		case CheckoutStatusPurchased:
			completedReservations++
		case CheckoutStatusCancelled:
			completedReservations++
		}
	}

	// Print reservation restoration statistics / Вывод статистики восстановления резерваций
	log.Printf("🔄 Reservations restoration statistics:")
	log.Printf("   📋 Total reservations loaded: %d", len(reservations))
	log.Printf("   ✅ Active reservations: %d", activeReservations)
	log.Printf("   ⏰ Expired reservations: %d", expiredReservations)
	log.Printf("   ✔️ Completed reservations: %d", completedReservations)

	if len(reservations) > 0 {
		log.Printf("   📊 Active rate: %.2f%%", float64(activeReservations)/float64(len(reservations))*100)
	}
}

// Close stops background tasks and releases resources / останавливает фоновые задачи и освобождает ресурсы
func (c *Megacache) Close() {
	c.cancel()
	c.wg.Wait()
}
