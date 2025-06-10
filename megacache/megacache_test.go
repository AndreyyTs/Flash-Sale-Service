package megacache

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewMegacache tests cache initialization
func TestNewMegacache(t *testing.T) {
	tests := []struct {
		name         string
		itemsCount   int64
		limitPerUser int64
	}{
		{"small cache", 10, 3},
		{"medium cache", 100, 5},
		{"large cache", 1000, 10},
		{"single item", 1, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := NewMegacache(tt.itemsCount, tt.limitPerUser)
			defer cache.Close()

			assert.Equal(t, tt.itemsCount, int64(len(cache.lots)))
			assert.Equal(t, tt.limitPerUser, cache.limitPerUser)
			assert.Equal(t, tt.itemsCount, cache.limitUsers)
			assert.Equal(t, tt.itemsCount, cache.nLots)
			assert.Equal(t, int64(0), cache.countLots)

			// Check all lots are initially available
			for i := int64(0); i < tt.itemsCount; i++ {
				status, err := cache.GetLotStatus(i)
				require.NoError(t, err)
				assert.Equal(t, StatusAvailable, status)
			}
		})
	}
}

// TestCheckoutBasic tests basic checkout functionality
func TestCheckoutBasic(t *testing.T) {
	cache := NewMegacache(10, 3)
	defer cache.Close()

	t.Run("successful checkout", func(t *testing.T) {
		checkout, err := cache.Checkout(1, 0)
		require.NoError(t, err)

		assert.NotEqual(t, uuid.Nil, checkout.Code)
		assert.Equal(t, int64(1), checkout.UserID)
		assert.Equal(t, int64(0), checkout.LotIndex)
		assert.Equal(t, CheckoutStatusActive, checkout.Status)
		assert.True(t, checkout.ExpiresAt.After(time.Now()))

		// Check lot status changed to reserved
		status, err := cache.GetLotStatus(0)
		require.NoError(t, err)
		assert.Equal(t, StatusReserved, status)
	})

	t.Run("invalid item ID", func(t *testing.T) {
		_, err := cache.Checkout(1, -1)
		assert.Equal(t, ErrInvalidItemID, err)

		_, err = cache.Checkout(1, 100)
		assert.Equal(t, ErrInvalidItemID, err)
	})
}

// TestCheckoutAlreadyReserved tests double reservation prevention
func TestCheckoutAlreadyReserved(t *testing.T) {
	cache := NewMegacache(10, 3)
	defer cache.Close()

	// First checkout should succeed
	_, err := cache.Checkout(1, 0)
	require.NoError(t, err)

	// Second checkout of same item should fail
	_, err = cache.Checkout(2, 0)
	assert.Equal(t, ErrItemAlreadyReserved, err)
}

// TestCheckoutUserLimits tests user purchase limits
func TestCheckoutUserLimits(t *testing.T) {
	cache := NewMegacache(10, 2) // limit 2 per user
	defer cache.Close()

	userID := int64(1)

	// First two checkouts should succeed
	checkout1, err := cache.Checkout(userID, 0)
	require.NoError(t, err)

	checkout2, err := cache.Checkout(userID, 1)
	require.NoError(t, err)

	// Purchase both items to increase user counter
	_, ok := cache.TryPurchase(checkout1.Code)
	require.True(t, ok)
	cache.ConfirmPurchase(checkout1.Code)

	_, ok = cache.TryPurchase(checkout2.Code)
	require.True(t, ok)
	cache.ConfirmPurchase(checkout2.Code)

	// Third checkout should fail due to user limit
	_, err = cache.Checkout(userID, 2)
	assert.Equal(t, ErrUserLimitExceeded, err)
}

// TestTryPurchase tests purchase functionality
func TestTryPurchase(t *testing.T) {
	cache := NewMegacache(10, 3)
	defer cache.Close()

	t.Run("successful purchase", func(t *testing.T) {
		checkout, err := cache.Checkout(1, 0)
		require.NoError(t, err)

		purchased, ok := cache.TryPurchase(checkout.Code)
		assert.True(t, ok)
		assert.Equal(t, checkout.UserID, purchased.UserID)
		assert.Equal(t, checkout.LotIndex, purchased.LotIndex)

		// Check lot status changed to sold
		status, err := cache.GetLotStatus(0)
		require.NoError(t, err)
		assert.Equal(t, StatusSold, status)

		// Check user purchase count
		count, exists := cache.GetPurchaseCount(1)
		assert.True(t, exists)
		assert.Equal(t, int64(1), count)
	})

	t.Run("purchase non-existent reservation", func(t *testing.T) {
		fakeCode := uuid.New()
		_, ok := cache.TryPurchase(fakeCode)
		assert.False(t, ok)
	})

	t.Run("purchase expired reservation", func(t *testing.T) {
		checkout, err := cache.Checkout(2, 1)
		require.NoError(t, err)

		// Wait for expiration
		time.Sleep(checkoutTime + 100*time.Millisecond)

		_, ok := cache.TryPurchase(checkout.Code)
		assert.False(t, ok)

		// Check lot is available again
		status, err := cache.GetLotStatus(1)
		require.NoError(t, err)
		assert.Equal(t, StatusAvailable, status)
	})
}

// TestConfirmPurchase tests purchase confirmation
func TestConfirmPurchase(t *testing.T) {
	cache := NewMegacache(10, 3)
	defer cache.Close()

	checkout, err := cache.Checkout(1, 0)
	require.NoError(t, err)

	// Purchase but don't confirm yet
	_, ok := cache.TryPurchase(checkout.Code)
	require.True(t, ok)

	initialCount := cache.countLots

	// Confirm purchase
	cache.ConfirmPurchase(checkout.Code)

	// Check countLots increased
	assert.Equal(t, initialCount+1, cache.countLots)

	// Check reservation is removed
	_, exists := cache.GetCheckoutInfo(checkout.Code)
	assert.False(t, exists)
}

// TestRollbackPurchase tests purchase rollback
func TestRollbackPurchase(t *testing.T) {
	cache := NewMegacache(10, 3)
	defer cache.Close()

	checkout, err := cache.Checkout(1, 0)
	require.NoError(t, err)

	// Purchase item
	_, ok := cache.TryPurchase(checkout.Code)
	require.True(t, ok)

	// Rollback purchase
	cache.RollbackPurchase(checkout.Code)

	// Check lot status returned to reserved
	status, err := cache.GetLotStatus(0)
	require.NoError(t, err)
	assert.Equal(t, StatusReserved, status)

	// Check reservation status returned to active
	info, exists := cache.GetCheckoutInfo(checkout.Code)
	require.True(t, exists)
	assert.Equal(t, CheckoutStatusActive, info.Status)

	// Check user purchase count decreased
	count, exists := cache.GetPurchaseCount(1)
	assert.True(t, exists)
	assert.Equal(t, int64(0), count)
}

// TestCancelCheckout tests reservation cancellation
func TestCancelCheckout(t *testing.T) {
	cache := NewMegacache(10, 3)
	defer cache.Close()

	t.Run("cancel existing reservation", func(t *testing.T) {
		checkout, err := cache.Checkout(1, 0)
		require.NoError(t, err)

		err = cache.CancelCheckout(checkout.Code)
		assert.NoError(t, err)

		// Check lot is available again
		status, err := cache.GetLotStatus(0)
		require.NoError(t, err)
		assert.Equal(t, StatusAvailable, status)

		// Check reservation status
		info, exists := cache.GetCheckoutInfo(checkout.Code)
		require.True(t, exists)
		assert.Equal(t, CheckoutStatusCancelled, info.Status)
	})

	t.Run("cancel non-existent reservation", func(t *testing.T) {
		fakeCode := uuid.New()
		err := cache.CancelCheckout(fakeCode)
		assert.Equal(t, ErrReservationNotFound, err)
	})
}

// TestDeleteCheckout tests reservation deletion
func TestDeleteCheckout(t *testing.T) {
	cache := NewMegacache(10, 3)
	defer cache.Close()

	checkout, err := cache.Checkout(1, 0)
	require.NoError(t, err)

	// Cancel first
	err = cache.CancelCheckout(checkout.Code)
	require.NoError(t, err)

	// Then delete
	cache.DeleteCheckout(checkout.Code)

	// Check reservation is completely removed
	_, exists := cache.GetCheckoutInfo(checkout.Code)
	assert.False(t, exists)
}

// TestGetPurchaseCount tests user purchase count retrieval
func TestGetPurchaseCount(t *testing.T) {
	cache := NewMegacache(10, 3)
	defer cache.Close()

	// New user should have 0 purchases
	count, exists := cache.GetPurchaseCount(1)
	assert.False(t, exists)
	assert.Equal(t, int64(0), count)

	// Make a purchase
	checkout, err := cache.Checkout(1, 0)
	require.NoError(t, err)

	_, ok := cache.TryPurchase(checkout.Code)
	require.True(t, ok)

	// Check count increased
	count, exists = cache.GetPurchaseCount(1)
	assert.True(t, exists)
	assert.Equal(t, int64(1), count)
}

// TestGetActiveReservationsCount tests active reservations counting
func TestGetActiveReservationsCount(t *testing.T) {
	cache := NewMegacache(10, 3)
	defer cache.Close()

	// Initially no reservations
	assert.Equal(t, 0, cache.GetActiveReservationsCount())

	// Make reservations
	checkout1, err := cache.Checkout(1, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, cache.GetActiveReservationsCount())

	checkout2, err := cache.Checkout(2, 1)
	require.NoError(t, err)
	assert.Equal(t, 2, cache.GetActiveReservationsCount())

	// Purchase one
	_, ok := cache.TryPurchase(checkout1.Code)
	require.True(t, ok)
	assert.Equal(t, 1, cache.GetActiveReservationsCount())

	// Cancel another
	err = cache.CancelCheckout(checkout2.Code)
	require.NoError(t, err)
	assert.Equal(t, 0, cache.GetActiveReservationsCount())
}

// TestLoadUserDataFromDB tests loading user data from database
func TestLoadUserDataFromDB(t *testing.T) {
	cache := NewMegacache(10, 3)
	defer cache.Close()

	saleItems := []SaleItems{
		{ItemID: 0, Purchased: true, UserID: 1},
		{ItemID: 1, Purchased: true, UserID: 1},
		{ItemID: 2, Purchased: true, UserID: 2},
		{ItemID: 3, Purchased: false, UserID: 0},
		{ItemID: 100, Purchased: true, UserID: 3}, // Invalid itemID - should be skipped
	}

	err := cache.LoadUserDataFromDB(saleItems)
	require.NoError(t, err)

	// Check user purchase counts
	count1, exists1 := cache.GetPurchaseCount(1)
	assert.True(t, exists1)
	assert.Equal(t, int64(2), count1)

	count2, exists2 := cache.GetPurchaseCount(2)
	assert.True(t, exists2)
	assert.Equal(t, int64(1), count2)

	// Check lot statuses
	status0, _ := cache.GetLotStatus(0)
	assert.Equal(t, StatusSold, status0)

	status1, _ := cache.GetLotStatus(1)
	assert.Equal(t, StatusSold, status1)

	status2, _ := cache.GetLotStatus(2)
	assert.Equal(t, StatusSold, status2)

	status3, _ := cache.GetLotStatus(3)
	assert.Equal(t, StatusAvailable, status3)

	// Check total sold count
	assert.Equal(t, int64(3), cache.countLots)
}

// TestLoadReservationsFromDB tests loading reservations from database
func TestLoadReservationsFromDB(t *testing.T) {
	cache := NewMegacache(10, 3)
	defer cache.Close()

	now := time.Now()
	reservations := []Checkout{
		{
			Code:      uuid.New(),
			UserID:    1,
			LotIndex:  0,
			ExpiresAt: now.Add(time.Hour),
			Status:    CheckoutStatusActive,
			CreatedAt: now,
		},
		{
			Code:      uuid.New(),
			UserID:    2,
			LotIndex:  1,
			ExpiresAt: now.Add(-time.Hour), // Expired
			Status:    CheckoutStatusActive,
			CreatedAt: now.Add(-2 * time.Hour),
		},
		{
			Code:      uuid.New(),
			UserID:    3,
			LotIndex:  2,
			ExpiresAt: now.Add(time.Hour),
			Status:    CheckoutStatusPurchased,
			CreatedAt: now,
		},
	}

	cache.LoadReservationsFromDB(reservations)

	// Check reservations were loaded
	for _, reservation := range reservations {
		info, exists := cache.GetCheckoutInfo(reservation.Code)
		assert.True(t, exists)
		assert.Equal(t, reservation.UserID, info.UserID)
		assert.Equal(t, reservation.Status, info.Status)
	}

	// Check lot statuses
	status0, _ := cache.GetLotStatus(0)
	assert.Equal(t, StatusReserved, status0)

	status1, _ := cache.GetLotStatus(1)
	assert.Equal(t, StatusReserved, status1)

	status2, _ := cache.GetLotStatus(2)
	assert.Equal(t, StatusReserved, status2)
}

// TestConcurrentCheckouts tests concurrent checkout operations
func TestConcurrentCheckouts(t *testing.T) {
	cache := NewMegacache(100, 10)
	defer cache.Close()

	const numGoroutines = 50
	const itemsPerGoroutine = 2

	var wg sync.WaitGroup
	results := make(chan error, numGoroutines*itemsPerGoroutine)

	// Launch concurrent checkouts
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(userID int64) {
			defer wg.Done()
			for j := 0; j < itemsPerGoroutine; j++ {
				itemID := userID*itemsPerGoroutine + int64(j)
				if itemID >= 100 {
					break
				}
				_, err := cache.Checkout(userID, itemID)
				results <- err
			}
		}(int64(i))
	}

	wg.Wait()
	close(results)

	// Count successful checkouts
	successCount := 0
	for err := range results {
		if err == nil {
			successCount++
		}
	}

	assert.Greater(t, successCount, 0)
	assert.LessOrEqual(t, successCount, numGoroutines*itemsPerGoroutine)
}

// TestConcurrentSameItem tests concurrent checkouts of same item
func TestConcurrentSameItem(t *testing.T) {
	cache := NewMegacache(10, 3)
	defer cache.Close()

	const numGoroutines = 10
	var wg sync.WaitGroup
	results := make(chan error, numGoroutines)

	// All goroutines try to checkout the same item
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(userID int64) {
			defer wg.Done()
			_, err := cache.Checkout(userID, 0) // Same item ID
			results <- err
		}(int64(i))
	}

	wg.Wait()
	close(results)

	// Count results
	successCount := 0
	alreadyReservedCount := 0

	for err := range results {
		if err == nil {
			successCount++
		} else if err == ErrItemAlreadyReserved {
			alreadyReservedCount++
		}
	}

	// Exactly one should succeed, others should get "already reserved"
	assert.Equal(t, 1, successCount)
	assert.Equal(t, numGoroutines-1, alreadyReservedCount)
}

// TestConcurrentPurchases tests concurrent purchase operations
func TestConcurrentPurchases(t *testing.T) {
	cache := NewMegacache(50, 10)
	defer cache.Close()

	// Create reservations first
	codes := make([]uuid.UUID, 10)
	for i := 0; i < 10; i++ {
		checkout, err := cache.Checkout(int64(i), int64(i))
		require.NoError(t, err)
		codes[i] = checkout.Code
	}

	var wg sync.WaitGroup
	results := make(chan bool, len(codes))

	// Concurrent purchases
	for _, code := range codes {
		wg.Add(1)
		go func(c uuid.UUID) {
			defer wg.Done()
			_, ok := cache.TryPurchase(c)
			results <- ok
		}(code)
	}

	wg.Wait()
	close(results)

	// All purchases should succeed
	successCount := 0
	for ok := range results {
		if ok {
			successCount++
		}
	}

	assert.Equal(t, len(codes), successCount)
}

// TestExpiredReservationCleanup tests automatic cleanup of expired reservations
func TestExpiredReservationCleanup(t *testing.T) {
	cache := NewMegacache(10, 3)
	defer cache.Close()

	// Create a reservation
	_, err := cache.Checkout(1, 0)
	require.NoError(t, err)

	// Initially should be active
	assert.Equal(t, 1, cache.GetActiveReservationsCount())

	// Wait for expiration + cleanup cycle
	time.Sleep(checkoutTime + 6*time.Second)

	// Should be cleaned up
	assert.Equal(t, 0, cache.GetActiveReservationsCount())

	// Lot should be available again
	status, err := cache.GetLotStatus(0)
	require.NoError(t, err)
	assert.Equal(t, StatusAvailable, status)
}

// TestGlobalLimitExceeded tests global item limit
func TestGlobalLimitExceeded(t *testing.T) {
	cache := NewMegacache(2, 5) // Only 2 items total
	defer cache.Close()

	// Buy all items
	codes := make([]uuid.UUID, 2)
	for i := 0; i < 2; i++ {
		checkout, err := cache.Checkout(int64(i+1), int64(i))
		require.NoError(t, err)
		codes[i] = checkout.Code

		_, ok := cache.TryPurchase(codes[i])
		require.True(t, ok)
		cache.ConfirmPurchase(codes[i])
	}

	// Try to checkout when all items are sold
	_, err := cache.Checkout(3, 0)
	assert.Equal(t, ErrAllItemsPurchased, err)
}

// TestIncrementUserPurchaseRaceCondition tests race conditions in user purchase increment
func TestIncrementUserPurchaseRaceCondition(t *testing.T) {
	cache := NewMegacache(100, 5)
	defer cache.Close()

	userID := int64(1)
	const numGoroutines = 20

	var wg sync.WaitGroup
	results := make(chan int64, numGoroutines)

	// Concurrent increments
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			newCount, err := cache.incrementUserPurchase(userID)
			if err == nil {
				results <- newCount
			}
		}()
	}

	wg.Wait()
	close(results)

	// Check final count
	finalCount, exists := cache.GetPurchaseCount(userID)
	assert.True(t, exists)
	assert.LessOrEqual(t, finalCount, int64(5)) // Should not exceed limit

	// Verify no duplicate counts
	counts := make(map[int64]bool)
	for count := range results {
		assert.False(t, counts[count], "Duplicate count detected: %d", count)
		counts[count] = true
		assert.LessOrEqual(t, count, int64(5))
	}
}

// TestRollbackUserPurchaseAccuracy tests accuracy of purchase rollback
func TestRollbackUserPurchaseAccuracy(t *testing.T) {
	cache := NewMegacache(10, 5)
	defer cache.Close()

	userID := int64(1)

	// Increment to 3
	for i := 0; i < 3; i++ {
		_, err := cache.incrementUserPurchase(userID)
		require.NoError(t, err)
	}

	count, exists := cache.GetPurchaseCount(userID)
	require.True(t, exists)
	require.Equal(t, int64(3), count)

	// Rollback specific value
	cache.rollbackUserPurchase(userID, 3)

	// Should be 2 now
	count, exists = cache.GetPurchaseCount(userID)
	require.True(t, exists)
	assert.Equal(t, int64(2), count)
}

// TestComplexWorkflow tests a complex workflow with multiple operations
func TestComplexWorkflow(t *testing.T) {
	cache := NewMegacache(10, 3)
	defer cache.Close()

	// Step 1: User 1 reserves 2 items
	checkout1, err := cache.Checkout(1, 0)
	require.NoError(t, err)

	checkout2, err := cache.Checkout(1, 1)
	require.NoError(t, err)

	// Step 2: User 2 reserves 1 item
	checkout3, err := cache.Checkout(2, 2)
	require.NoError(t, err)

	// Step 3: User 1 purchases first item
	_, ok := cache.TryPurchase(checkout1.Code)
	require.True(t, ok)
	cache.ConfirmPurchase(checkout1.Code)

	// Step 4: User 1 cancels second item
	err = cache.CancelCheckout(checkout2.Code)
	require.NoError(t, err)

	// Step 5: User 2 purchases their item
	_, ok = cache.TryPurchase(checkout3.Code)
	require.True(t, ok)
	cache.ConfirmPurchase(checkout3.Code)

	// Step 6: User 3 should be able to reserve the cancelled item
	_, err = cache.Checkout(3, 1)
	require.NoError(t, err)

	// Verify final state
	count1, _ := cache.GetPurchaseCount(1)
	assert.Equal(t, int64(1), count1)

	count2, _ := cache.GetPurchaseCount(2)
	assert.Equal(t, int64(1), count2)

	status0, _ := cache.GetLotStatus(0)
	assert.Equal(t, StatusSold, status0)

	status1, _ := cache.GetLotStatus(1)
	assert.Equal(t, StatusReserved, status1)

	status2, _ := cache.GetLotStatus(2)
	assert.Equal(t, StatusSold, status2)

	assert.Equal(t, int64(2), cache.countLots)
	assert.Equal(t, 1, cache.GetActiveReservationsCount())
}

// BenchmarkCheckout benchmarks checkout operation
func BenchmarkCheckout(b *testing.B) {
	cache := NewMegacache(int64(b.N), 1000)
	defer cache.Close()

	b.ResetTimer()
	start := time.Now()

	b.RunParallel(func(pb *testing.PB) {
		itemID := int64(0)
		userID := int64(1)
		for pb.Next() {
			cache.Checkout(userID, itemID%int64(b.N))
			itemID++
			if itemID%100 == 0 {
				userID++
			}
		}
	})

	elapsed := time.Since(start)
	rps := float64(b.N) / elapsed.Seconds()
	b.ReportMetric(rps, "checkout_ops/sec")
	b.Logf("Checkout RPS: %.0f operations/second", rps)
}

// BenchmarkTryPurchase benchmarks purchase operation
func BenchmarkTryPurchase(b *testing.B) {
	cache := NewMegacache(int64(b.N), 1000)
	defer cache.Close()

	// Pre-create reservations
	codes := make([]uuid.UUID, b.N)
	for i := 0; i < b.N; i++ {
		checkout, _ := cache.Checkout(int64(i/100+1), int64(i))
		codes[i] = checkout.Code
	}

	b.ResetTimer()
	start := time.Now()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if i < len(codes) {
				cache.TryPurchase(codes[i])
				i++
			}
		}
	})

	elapsed := time.Since(start)
	rps := float64(b.N) / elapsed.Seconds()
	b.ReportMetric(rps, "purchase_ops/sec")
	b.Logf("Purchase RPS: %.0f operations/second", rps)
}

// BenchmarkMixedOperations benchmarks mixed operations workflow
func BenchmarkMixedOperations(b *testing.B) {
	cache := NewMegacache(int64(b.N*2), 1000)
	defer cache.Close()

	b.ResetTimer()
	start := time.Now()

	b.RunParallel(func(pb *testing.PB) {
		itemID := int64(0)
		userID := int64(1)
		codes := make([]uuid.UUID, 0, 10)

		for pb.Next() {
			switch itemID % 4 {
			case 0, 1: // 50% checkout
				if checkout, err := cache.Checkout(userID, itemID%int64(b.N*2)); err == nil {
					codes = append(codes, checkout.Code)
				}
			case 2: // 25% purchase
				if len(codes) > 0 {
					cache.TryPurchase(codes[0])
					codes = codes[1:]
				}
			case 3: // 25% cancel
				if len(codes) > 0 {
					cache.CancelCheckout(codes[0])
					codes = codes[1:]
				}
			}

			itemID++
			if itemID%100 == 0 {
				userID++
			}
		}
	})

	elapsed := time.Since(start)
	rps := float64(b.N) / elapsed.Seconds()
	b.ReportMetric(rps, "mixed_ops/sec")
	b.Logf("Mixed Operations RPS: %.0f operations/second", rps)
}

// TestMemoryLeaks tests for potential memory leaks
func TestMemoryLeaks(t *testing.T) {
	cache := NewMegacache(100, 10)
	defer cache.Close()

	// Create many reservations and clean them up
	for i := 0; i < 1000; i++ {
		checkout, err := cache.Checkout(int64(i%10+1), int64(i%100))
		if err != nil {
			continue
		}

		// Randomly purchase, cancel, or let expire
		switch i % 3 {
		case 0:
			if _, ok := cache.TryPurchase(checkout.Code); ok {
				cache.ConfirmPurchase(checkout.Code)
			}
		case 1:
			cache.CancelCheckout(checkout.Code)
			cache.DeleteCheckout(checkout.Code)
		case 2:
			// Let it expire naturally
		}
	}

	// Force cleanup
	cache.cleanupExpired()

	// Check that old reservations are cleaned up
	// This is more of a manual inspection test
	activeCount := cache.GetActiveReservationsCount()
	t.Logf("Active reservations after cleanup: %d", activeCount)
}

// TestHighConcurrency tests behavior under high concurrency
func TestHighConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping high concurrency test in short mode")
	}

	cache := NewMegacache(1000, 50)
	defer cache.Close()

	const numWorkers = 100
	const operationsPerWorker = 100

	var wg sync.WaitGroup

	// Statistics channels
	checkoutSuccesses := make(chan int, numWorkers)
	purchaseSuccesses := make(chan int, numWorkers)
	cancellations := make(chan int, numWorkers)

	// Launch workers
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			checkoutCount := 0
			purchaseCount := 0
			cancelCount := 0

			codes := make([]uuid.UUID, 0, operationsPerWorker)

			for op := 0; op < operationsPerWorker; op++ {
				userID := int64(workerID*1000 + op)
				itemID := int64((workerID*operationsPerWorker + op) % 1000)

				// Try checkout
				if checkout, err := cache.Checkout(userID, itemID); err == nil {
					checkoutCount++
					codes = append(codes, checkout.Code)

					// Randomly decide what to do with the reservation
					switch op % 4 {
					case 0, 1: // 50% chance to purchase
						if _, ok := cache.TryPurchase(checkout.Code); ok {
							purchaseCount++
							cache.ConfirmPurchase(checkout.Code)
						}
					case 2: // 25% chance to cancel
						if cache.CancelCheckout(checkout.Code) == nil {
							cancelCount++
							cache.DeleteCheckout(checkout.Code)
						}
					case 3: // 25% chance to let expire
						// Do nothing, let it expire
					}
				}
			}

			checkoutSuccesses <- checkoutCount
			purchaseSuccesses <- purchaseCount
			cancellations <- cancelCount
		}(w)
	}

	wg.Wait()
	close(checkoutSuccesses)
	close(purchaseSuccesses)
	close(cancellations)

	// Collect statistics
	totalCheckouts := 0
	totalPurchases := 0
	totalCancellations := 0

	for count := range checkoutSuccesses {
		totalCheckouts += count
	}
	for count := range purchaseSuccesses {
		totalPurchases += count
	}
	for count := range cancellations {
		totalCancellations += count
	}

	t.Logf("High concurrency test results:")
	t.Logf("  Total checkout attempts: %d", numWorkers*operationsPerWorker)
	t.Logf("  Successful checkouts: %d", totalCheckouts)
	t.Logf("  Successful purchases: %d", totalPurchases)
	t.Logf("  Successful cancellations: %d", totalCancellations)
	t.Logf("  Final sold count: %d", cache.countLots)
	t.Logf("  Active reservations: %d", cache.GetActiveReservationsCount())

	// Basic sanity checks
	assert.GreaterOrEqual(t, totalCheckouts, totalPurchases+totalCancellations)
	assert.LessOrEqual(t, cache.countLots, int64(1000))
}

// TestDataConsistency tests data consistency across operations
func TestDataConsistency(t *testing.T) {
	cache := NewMegacache(50, 10)
	defer cache.Close()

	const numUsers = 10
	const numItems = 50

	// Track expected state
	expectedUserCounts := make(map[int64]int64)
	expectedSoldItems := make(map[int64]bool)

	// Perform a series of operations and track expected state
	operations := []struct {
		op     string
		userID int64
		itemID int64
	}{
		{"checkout", 1, 0}, {"purchase", 1, 0}, {"confirm", 1, 0},
		{"checkout", 1, 1}, {"purchase", 1, 1}, {"confirm", 1, 1},
		{"checkout", 2, 2}, {"cancel", 2, 2},
		{"checkout", 3, 2}, {"purchase", 3, 2}, {"rollback", 3, 2},
		{"checkout", 3, 3}, {"purchase", 3, 3}, {"confirm", 3, 3},
		{"checkout", 1, 4}, {"purchase", 1, 4}, {"confirm", 1, 4}, // Should fail due to user limit
	}

	checkoutCodes := make(map[string]uuid.UUID)

	for i, op := range operations {
		key := fmt.Sprintf("%s_%d_%d", op.op, op.userID, op.itemID)

		switch op.op {
		case "checkout":
			if checkout, err := cache.Checkout(op.userID, op.itemID); err == nil {
				checkoutCodes[key] = checkout.Code
			}

		case "purchase":
			if code, exists := checkoutCodes[fmt.Sprintf("checkout_%d_%d", op.userID, op.itemID)]; exists {
				cache.TryPurchase(code)
			}

		case "confirm":
			if code, exists := checkoutCodes[fmt.Sprintf("checkout_%d_%d", op.userID, op.itemID)]; exists {
				cache.ConfirmPurchase(code)
				expectedUserCounts[op.userID]++
				expectedSoldItems[op.itemID] = true
			}

		case "cancel":
			if code, exists := checkoutCodes[fmt.Sprintf("checkout_%d_%d", op.userID, op.itemID)]; exists {
				cache.CancelCheckout(code)
			}

		case "rollback":
			if code, exists := checkoutCodes[fmt.Sprintf("checkout_%d_%d", op.userID, op.itemID)]; exists {
				cache.RollbackPurchase(code)
			}
		}

		t.Logf("Step %d: %s user=%d item=%d", i+1, op.op, op.userID, op.itemID)
	}

	// Verify final state matches expected state
	for userID, expectedCount := range expectedUserCounts {
		actualCount, exists := cache.GetPurchaseCount(userID)
		if expectedCount > 0 {
			assert.True(t, exists, "User %d should exist", userID)
			assert.Equal(t, expectedCount, actualCount, "User %d count mismatch", userID)
		}
	}

	for itemID, shouldBeSold := range expectedSoldItems {
		status, err := cache.GetLotStatus(itemID)
		require.NoError(t, err)
		if shouldBeSold {
			assert.Equal(t, StatusSold, status, "Item %d should be sold", itemID)
		}
	}

	// Count actual sold items
	actualSoldCount := int64(0)
	for i := int64(0); i < numItems; i++ {
		if status, err := cache.GetLotStatus(i); err == nil && status == StatusSold {
			actualSoldCount++
		}
	}

	assert.Equal(t, cache.countLots, actualSoldCount, "Sold count mismatch")
}

// TestCleanupTiming tests cleanup timing accuracy
func TestCleanupTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping timing test in short mode")
	}

	cache := NewMegacache(10, 3)
	defer cache.Close()

	// Create reservation
	_, err := cache.Checkout(1, 0)
	require.NoError(t, err)

	startTime := time.Now()

	// Wait for expiration
	for {
		if time.Since(startTime) > checkoutTime+10*time.Second {
			t.Fatal("Cleanup took too long")
		}

		status, err := cache.GetLotStatus(0)
		require.NoError(t, err)

		if status == StatusAvailable {
			break
		}

		time.Sleep(100 * time.Millisecond)
	}

	// Cleanup should happen within reasonable time after expiration
	cleanupTime := time.Since(startTime)
	t.Logf("Cleanup completed in: %v", cleanupTime)

	// Should be cleaned up within expiration time + cleanup interval + some buffer
	maxExpectedTime := checkoutTime + 5*time.Second + 2*time.Second
	assert.Less(t, cleanupTime, maxExpectedTime, "Cleanup took too long")
}

// TestStressUserLimits stress tests user limits under concurrent load
func TestStressUserLimits(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	cache := NewMegacache(1000, 5) // Limit 5 per user
	defer cache.Close()

	const numUsers = 100
	const attemptsPerUser = 20

	var wg sync.WaitGroup
	userStats := make([]int64, numUsers)

	// Each user tries to make many purchases
	for userID := 0; userID < numUsers; userID++ {
		wg.Add(1)
		go func(uid int) {
			defer wg.Done()

			successCount := int64(0)
			for attempt := 0; attempt < attemptsPerUser; attempt++ {
				itemID := int64(uid*attemptsPerUser + attempt)
				if itemID >= 1000 {
					break
				}

				if checkout, err := cache.Checkout(int64(uid+1), itemID); err == nil {
					if _, ok := cache.TryPurchase(checkout.Code); ok {
						cache.ConfirmPurchase(checkout.Code)
						successCount++
					}
				}
			}
			userStats[uid] = successCount
		}(userID)
	}

	wg.Wait()

	// Verify no user exceeded their limit
	for userID, successCount := range userStats {
		assert.LessOrEqual(t, successCount, int64(5), "User %d exceeded limit with %d purchases", userID+1, successCount)

		// Verify against cache state
		if successCount > 0 {
			cacheCount, exists := cache.GetPurchaseCount(int64(userID + 1))
			assert.True(t, exists)
			assert.Equal(t, successCount, cacheCount, "Cache count mismatch for user %d", userID+1)
		}
	}

	totalPurchases := int64(0)
	for _, count := range userStats {
		totalPurchases += count
	}

	t.Logf("Stress test completed:")
	t.Logf("  Users: %d", numUsers)
	t.Logf("  Total purchase attempts: %d", numUsers*attemptsPerUser)
	t.Logf("  Successful purchases: %d", totalPurchases)
	t.Logf("  Cache sold count: %d", cache.countLots)

	assert.Equal(t, totalPurchases, cache.countLots, "Total purchases mismatch")
}

// TestInvalidOperations tests handling of invalid operations
func TestInvalidOperations(t *testing.T) {
	cache := NewMegacache(5, 2)
	defer cache.Close()

	t.Run("operations on non-existent checkout", func(t *testing.T) {
		fakeCode := uuid.New()

		// Try purchase
		_, ok := cache.TryPurchase(fakeCode)
		assert.False(t, ok)

		// Try confirm
		cache.ConfirmPurchase(fakeCode) // Should not panic

		// Try rollback
		cache.RollbackPurchase(fakeCode) // Should not panic

		// Try cancel
		err := cache.CancelCheckout(fakeCode)
		assert.Equal(t, ErrReservationNotFound, err)

		// Try delete
		cache.DeleteCheckout(fakeCode) // Should not panic
	})

	t.Run("operations on sold items", func(t *testing.T) {
		// Create and sell an item
		checkout, err := cache.Checkout(1, 0)
		require.NoError(t, err)

		_, ok := cache.TryPurchase(checkout.Code)
		require.True(t, ok)
		cache.ConfirmPurchase(checkout.Code)

		// Try to checkout the same item again
		_, err = cache.Checkout(2, 0)
		assert.Equal(t, ErrItemAlreadySold, err)
	})

	t.Run("boundary item IDs", func(t *testing.T) {
		// Test boundary values
		_, err := cache.Checkout(1, -1)
		assert.Equal(t, ErrInvalidItemID, err)

		_, err = cache.Checkout(1, 5) // >= len(lots)
		assert.Equal(t, ErrInvalidItemID, err)

		_, err = cache.Checkout(1, 4) // Valid boundary
		assert.NoError(t, err)
	})
}

// TestContextCancellation tests proper context handling
func TestContextCancellation(t *testing.T) {
	cache := NewMegacache(10, 3)

	// Close immediately to test cancellation
	cache.Close()

	// Should not panic and should stop gracefully
	time.Sleep(100 * time.Millisecond)

	// Operations should still work even after context cancellation
	_, err := cache.Checkout(1, 0)
	assert.NoError(t, err, "Operations should still work after context cancellation")
}
