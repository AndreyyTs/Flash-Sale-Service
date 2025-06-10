# Megacache 🚀

A high-performance, thread-safe in-memory cache for managing item reservations and purchases with user limits. Specifically designed for the [NOT Back Contest](https://contest.notco.in/dev-backend) — a backend developer competition to build a robust, high-throughput flash-sale service from scratch.

**🏆 NOT Back Contest**: Backend engineers compete to build a robust, high-throughput flash‑sale service entirely from scratch. Using Go, Redis, Postgres, and Docker—without frameworks and with minimal dependencies—you will implement a reliable system that sells exactly 10,000 items every hour.

Designed for scenarios requiring atomic operations, concurrent access, and automatic cleanup of expired reservations.

## Features ✨

- **Thread-Safe Operations**: All operations are atomic and safe for concurrent access
- **Lock-Free Design**: Minimal locking with extensive use of Compare-And-Swap (CAS) operations
- **User Purchase Limits**: Configurable per-user purchase limits with atomic counting
- **Automatic Cleanup**: Background goroutine automatically cleans expired reservations
- **High Performance**: Optimized for high-throughput scenarios (17M+ ops/sec for checkouts)
- **Memory Efficient**: Lock-free atomic operations where possible
- **Persistence Support**: Load/save functionality for database integration

## Architecture 🏗️

### Core Components

```
┌─────────────────────────────────────────────────────────────┐
│                        MEGACACHE                            │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  ┌─────────────────┐    ┌─────────────────┐                 │
│  │   RESERVATIONS  │    │      USERS      │                 │
│  │                 │    │                 │                 │
│  │ checkouts: map  │    │ users: map      │                 │
│  │ [UUID]Checkout  │    │ [userID]*count  │                 │
│  │                 │    │                 │                 │
│  │ Protected by:   │    │ Protected by:   │                 │
│  │ checkoutMu      │    │ userMu          │                 │
│  └─────────────────┘    └─────────────────┘                 │
│                                                             │
│  ┌────────────────────────────────────────────────────────┐ │
│  │                    LOTS ARRAY                          │ │
│  │                                                        │ │
│  │  [0] [1] [2] [3] [4] ... [N]                           │ │
│  │   ↓   ↓   ↓   ↓   ↓       ↓                            │ │
│  │  Lot Lot Lot Lot Lot ... Lot                           │ │
│  │                                                        │ │
│  │  Each Lot contains atomic uint32 status:               │ │
│  │  • 0 = Available                                       │ │
│  │  • 1 = Reserved                                        │ │
│  │  • 2 = Sold                                            │ │
│  └────────────────────────────────────────────────────────┘ │
│                                                             │
│  ┌────────────────────────────────────────────────────────┐ │
│  │               BACKGROUND CLEANUP                       │ │
│  │                                                        │ │
│  │  ┌───────────────┐    ┌─────────────────────────────┐  │ │
│  │  │   Ticker      │───▶│     cleanupExpired()        │  │ │
│  │  │  (5 seconds)  │    │                             │  │ │
│  │  └───────────────┘    │ • Expired reservations      │  │ │
│  │                       │ • Old completed records     │  │ │
│  │                       │ • Status updates            │  │ │
│  │                       └─────────────────────────────┘  │ │
│  └────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
```

### Data Flow

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   Checkout  │────▶│ TryPurchase │────▶│   Confirm   │────▶│   Delete    │
│             │     │             │     │  Purchase   │     │  Checkout   │
└─────────────┘     └─────────────┘     └─────────────┘     └─────────────┘
       │                    │                    │
       ▼                    ▼                    ▼
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   Reserve   │     │    Sold     │     │  Completed  │
│   Status    │     │   Status    │     │   (Remove)  │
└─────────────┘     └─────────────┘     └─────────────┘
       │                    
       ▼                    
┌─────────────┐     
│   Cancel    │     
│  Checkout   │     
└─────────────┘     
       │            
       ▼            
┌─────────────┐     
│ Available   │     
│  Status     │     
└─────────────┘     
```

### Thread Safety Model

#### Minimal Locking Strategy
The cache is designed with **minimal blocking** approach using:
- **Compare-And-Swap (CAS)** operations for critical atomic updates
- **Read-Write Mutexes** only for complex data structures
- **Lock-free algorithms** for high-frequency operations

#### Lock Hierarchy (Used Sparingly)
1. **userMu** (RWMutex) - Protects user purchase counters map structure only
2. **checkoutMu** (RWMutex) - Protects checkout reservations map structure only
3. **Atomic Operations** - For all lot statuses and counters (lock-free)

#### CAS-Based Operations (Lock-Free)
- **Lot Reservations**: `atomic.CompareAndSwapUint32` for status transitions
- **User Counters**: `atomic.CompareAndSwapInt64` for purchase count updates
- **Global Counters**: `atomic.AddInt64` and `atomic.LoadInt64` for statistics
- **Rollback Operations**: Precise CAS-based rollbacks without locks

```go
// Example: Lock-free lot reservation
if atomic.CompareAndSwapUint32(&lot.status, StatusAvailable, StatusReserved) {
    // Reservation successful - no locks needed!
}

// Example: Lock-free user counter increment
for {
    currentCount := atomic.LoadInt64(userCount)
    if currentCount >= c.limitPerUser {
        return 0, ErrUserLimitExceeded
    }
    if atomic.CompareAndSwapInt64(userCount, currentCount, currentCount+1) {
        return currentCount + 1, nil  // Success!
    }
    // Retry if CAS failed due to concurrent modification
}
```

## Performance Benchmarks 📊

Based on AMD Ryzen 5 8400F 6-Core Processor:

| Operation | Throughput | Latency | Lock Usage |
|-----------|------------|---------|------------|
| **Checkout** | 17.9M ops/sec | 55.82 ns/op | Minimal (map access only) |
| **TryPurchase** | 10.3M ops/sec | 134.4 ns/op | CAS-based, lock-free |
| **Mixed Operations** | 14.6M ops/sec | 68.60 ns/op | Hybrid CAS + minimal locks |

**Why So Fast?**
- **CAS Operations**: Most critical path uses lock-free Compare-And-Swap
- **Minimal Blocking**: Locks only used for map structure protection, not data
- **Atomic Counters**: All statistics and limits use atomic operations
- **Optimized Memory Layout**: Cache-friendly data structures


### Basic Usage

```go
package main

import (
    "fmt"
    "log"
    "time"
    
    "github.com/yourrepo/megacache"
)

func main() {
    // Create cache with 10000 items, max 10 purchases per user
    cache := megacache.NewMegacache(1000, 10)
    defer cache.Close()
    
    // Reserve an item
    checkout, err := cache.Checkout(userID, itemID)
    if err != nil {
        log.Fatal(err)
    }
    
    // Purchase the reserved item
    purchased, ok := cache.TryPurchase(checkout.Code)
    if !ok {
        log.Fatal("Purchase failed")
    }
    
    // Confirm the purchase
    cache.ConfirmPurchase(checkout.Code)
    
    fmt.Printf("Successfully purchased item %d\n", purchased.LotIndex)
}
```

## Configuration ⚙️

### Constants

```go
const checkoutTime = 3 * time.Second  // Reservation timeout
```


## Data Structures 📋

### Checkout
```go
type Checkout struct {
    Code      uuid.UUID      // Unique reservation identifier
    UserID    int64          // User who made the reservation
    LotIndex  int64          // Index of the reserved item
    ExpiresAt time.Time      // When the reservation expires
    Status    CheckoutStatus // Current reservation status
    CreatedAt time.Time      // When the reservation was created
}
```


### SaleItems (for database loading)
```go
type SaleItems struct {
    ItemID    int64  // Item identifier
    Purchased bool   // Whether the item was purchased
    UserID    int64  // User who purchased the item
}
```

## Testing 🧪


```bash
# Run all tests
go test -v

# Run benchmarks
go test -bench=. -run=^$ -benchmem
```
#
**Built with ❤️ specifically for the [NOT Back Contest](https://contest.notco.in/dev-backend) and high-performance applications requiring atomic reservation management.**

------

# Megacache 🚀

Высокопроизводительный, потокобезопасный кэш в памяти для управления резервированием и покупками товаров с лимитами для пользователей. Специально разработан для [NOT Back Contest](https://contest.notco.in/dev-backend) — соревнования backend-разработчиков по созданию надежного высокопроизводительного сервиса flash-продаж с нуля.

**🏆 NOT Back Contest**: Backend-инженеры соревнуются в создании надежного высокопроизводительного сервиса flash-продаж полностью с нуля. Используя Go, Redis, Postgres и Docker — без фреймворков и с минимальными зависимостями — нужно реализовать надежную систему, которая продает ровно 10,000 товаров каждый час.

Создан для сценариев, требующих атомарных операций, конкурентного доступа и автоматической очистки истекших резерваций.

## Особенности ✨

- **Потокобезопасные операции**: Все операции атомарны и безопасны для конкурентного доступа
- **Дизайн без блокировок**: Минимальное блокирование с интенсивным использованием операций Compare-And-Swap (CAS)
- **Лимиты покупок пользователей**: Настраиваемые лимиты покупок на пользователя с атомарным подсчетом
- **Автоматическая очистка**: Фоновая горутина автоматически очищает истекшие резервации
- **Высокая производительность**: Оптимизирован для высокопроизводительных сценариев (17M+ операций/сек для чекаутов)
- **Эффективность памяти**: Атомарные операции без блокировок там, где это возможно
- **Поддержка персистентности**: Функциональность загрузки/сохранения для интеграции с базой данных

## Архитектура 🏗️

### Основные компоненты

```
┌─────────────────────────────────────────────────────────────┐
│                        MEGACACHE                            │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  ┌─────────────────┐    ┌─────────────────┐                 │
│  │   РЕЗЕРВАЦИИ    │    │   ПОЛЬЗОВАТЕЛИ  │                 │
│  │                 │    │                 │                 │
│  │ checkouts: map  │    │ users: map      │                 │
│  │ [UUID]Checkout  │    │ [userID]*count  │                 │
│  │                 │    │                 │                 │
│  │ Защищено:       │    │ Защищено:       │                 │
│  │ checkoutMu      │    │ userMu          │                 │
│  └─────────────────┘    └─────────────────┘                 │
│                                                             │
│  ┌────────────────────────────────────────────────────────┐ │
│  │                   МАССИВ ЛОТОВ                         │ │
│  │                                                        │ │
│  │  [0] [1] [2] [3] [4] ... [N]                           │ │
│  │   ↓   ↓   ↓   ↓   ↓       ↓                            │ │
│  │  Лот Лот Лот Лот Лот ... Лот                           │ │
│  │                                                        │ │
│  │  Каждый лот содержит атомарный uint32 статус:          │ │
│  │  • 0 = Доступен                                        │ │
│  │  • 1 = Зарезервирован                                  │ │
│  │  • 2 = Продан                                          │ │
│  └────────────────────────────────────────────────────────┘ │
│                                                             │
│  ┌────────────────────────────────────────────────────────┐ │
│  │              ФОНОВАЯ ОЧИСТКА                           │ │
│  │                                                        │ │
│  │  ┌───────────────┐    ┌─────────────────────────────┐  │ │
│  │  │   Тикер       │───▶│     cleanupExpired()        │  │ │
│  │  │  (5 секунд)   │    │                             │  │ │
│  │  └───────────────┘    │ • Истекшие резервации       │  │ │
│  │                       │ • Старые завершенные записи │  │ │
│  │                       │ • Обновления статуса        │  │ │
│  │                       └─────────────────────────────┘  │ │
│  └────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
```

### Поток данных

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   Чекаут    │────▶│ TryPurchase │────▶│ Подтвердить │────▶│   Удалить   │
│             │     │             │     │   покупку   │     │   чекаут    │
└─────────────┘     └─────────────┘     └─────────────┘     └─────────────┘
       │                    │                    │
       ▼                    ▼                    ▼
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│ Резервация  │     │   Продано   │     │ Завершено   │
│   статус    │     │   статус    │     │  (Удалить)  │
└─────────────┘     └─────────────┘     └─────────────┘
       │                    
       ▼                    
┌─────────────┐     
│  Отменить   │     
│   чекаут    │     
└─────────────┘     
       │            
       ▼            
┌─────────────┐     
│  Доступен   │     
│   статус    │     
└─────────────┘     
```

### Модель потокобезопасности

#### Стратегия минимальных блокировок
Кэш разработан с подходом **минимальной блокировки**, используя:
- **Compare-And-Swap (CAS)** операции для критических атомарных обновлений
- **Read-Write мьютексы** только для сложных структур данных
- **Алгоритмы без блокировок** для высокочастотных операций

#### Иерархия блокировок (используется редко)
1. **userMu** (RWMutex) - Защищает только структуру map счетчиков покупок пользователей
2. **checkoutMu** (RWMutex) - Защищает только структуру map резерваций чекаутов
3. **Атомарные операции** - Для всех статусов лотов и счетчиков (без блокировок)

#### CAS-операции (без блокировок)
- **Резервации лотов**: `atomic.CompareAndSwapUint32` для переходов статусов
- **Счетчики пользователей**: `atomic.CompareAndSwapInt64` для обновления счетчиков покупок
- **Глобальные счетчики**: `atomic.AddInt64` и `atomic.LoadInt64` для статистики
- **Операции отката**: Точные откаты на основе CAS без блокировок

```go
// Пример: Резервация лота без блокировок
if atomic.CompareAndSwapUint32(&lot.status, StatusAvailable, StatusReserved) {
    // Резервация успешна - блокировки не нужны!
}

// Пример: Инкремент счетчика пользователя без блокировок
for {
    currentCount := atomic.LoadInt64(userCount)
    if currentCount >= c.limitPerUser {
        return 0, ErrUserLimitExceeded
    }
    if atomic.CompareAndSwapInt64(userCount, currentCount, currentCount+1) {
        return currentCount + 1, nil  // Успех!
    }
    // Повтор, если CAS не удался из-за конкурентного изменения
}
```

## Бенчмарки производительности 📊

На базе AMD Ryzen 5 8400F 6-ядерного процессора:

| Операция | Пропускная способность | Задержка | Использование блокировок |
|----------|------------------------|----------|--------------------------|
| **Чекаут** | 17.9M операций/сек | 55.82 нс/операция | Минимальное (только доступ к map) |
| **TryPurchase** | 10.3M операций/сек | 134.4 нс/операция | На основе CAS, без блокировок |
| **Смешанные операции** | 14.6M операций/сек | 68.60 нс/операция | Гибридные CAS + минимальные блокировки |

**Почему так быстро?**
- **CAS-операции**: Большинство критических путей используют Compare-And-Swap без блокировок
- **Минимальные блокировки**: Блокировки используются только для защиты структуры map, а не данных
- **Атомарные счетчики**: Вся статистика и лимиты используют атомарные операции
- **Оптимизированная компоновка памяти**: Дружественная к кэшу структура данных

### Базовое использование

```go
package main

import (
    "fmt"
    "log"
    "time"
    
    "github.com/yourrepo/megacache"
)

func main() {
    // Создать кэш с 10000 товарами, максимум 10 покупок на пользователя
    cache := megacache.NewMegacache(1000, 10)
    defer cache.Close()
    
    // Зарезервировать товар
    checkout, err := cache.Checkout(userID, itemID)
    if err != nil {
        log.Fatal(err)
    }
    
    // Купить зарезервированный товар
    purchased, ok := cache.TryPurchase(checkout.Code)
    if !ok {
        log.Fatal("Покупка не удалась")
    }
    
    // Подтвердить покупку
    cache.ConfirmPurchase(checkout.Code)
    
    fmt.Printf("Успешно куплен товар %d\n", purchased.LotIndex)
}
```

## Конфигурация ⚙️

### Константы

```go
const checkoutTime = 3 * time.Second  // Тайм-аут резервации
```

## Структуры данных 📋

### Checkout
```go
type Checkout struct {
    Code      uuid.UUID      // Уникальный идентификатор резервации
    UserID    int64          // Пользователь, сделавший резервацию
    LotIndex  int64          // Индекс зарезервированного товара
    ExpiresAt time.Time      // Когда истекает резервация
    Status    CheckoutStatus // Текущий статус резервации
    CreatedAt time.Time      // Когда была создана резервация
}
```

### SaleItems (для загрузки из базы данных)
```go
type SaleItems struct {
    ItemID    int64  // Идентификатор товара
    Purchased bool   // Был ли товар куплен
    UserID    int64  // Пользователь, купивший товар
}
```

## Тестирование 🧪

```bash
# Запустить все тесты
go test -v

# Запустить бенчмарки
go test -bench=. -run=^$ -benchmem
```

#

**Создано с ❤️ специально для [NOT Back Contest](https://contest.notco.in/dev-backend) и высокопроизводительных приложений, требующих атомарного управления резервациями.**