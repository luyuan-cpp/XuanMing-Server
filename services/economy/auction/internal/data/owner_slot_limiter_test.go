package data

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newOwnerSlotLimiterForTest(t *testing.T) (*RedisOwnerSlotLimiter, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisOwnerSlotLimiter(rdb), mr
}

func TestRedisOwnerSlotLimiter_AtomicHardLimitAndIdempotency(t *testing.T) {
	limiter, _ := newOwnerSlotLimiterForTest(t)
	ctx := context.Background()
	const ownerID = uint64(42)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 1; i <= 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := limiter.Reserve(ctx, ownerID,
				OwnerOrderSlot{MarketID: uint32(i%3 + 1), OrderID: uint64(1000 + i)}, 2)
			if err != nil {
				t.Errorf("Reserve: %v", err)
				return
			}
			if ok {
				successes.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if got := successes.Load(); got != 2 {
		t.Fatalf("successful reserves=%d, want exactly 2", got)
	}
	cardinality, err := limiter.rdb.SCard(ctx, ownerSlotKey(ownerID)).Result()
	if err != nil {
		t.Fatalf("SCARD: %v", err)
	}
	if cardinality != 2 {
		t.Fatalf("SCARD=%d, want 2", cardinality)
	}
	listed, err := limiter.List(ctx, ownerID, 2)
	if err != nil || len(listed) != 2 {
		t.Fatalf("List len=%d err=%v", len(listed), err)
	}
	// SET 已满时，已有的 market_id+order_id 成员仍必须幂等成功。
	ok, err := limiter.Reserve(ctx, ownerID, listed[0], 2)
	if err != nil || !ok {
		t.Fatalf("idempotent reserve ok=%v err=%v", ok, err)
	}
}

func TestRedisOwnerSlotLimiter_ReleaseAndBoundedList(t *testing.T) {
	limiter, _ := newOwnerSlotLimiterForTest(t)
	ctx := context.Background()
	for i := uint64(1); i <= 3; i++ {
		ok, err := limiter.Reserve(ctx, 7, OwnerOrderSlot{MarketID: uint32(i), OrderID: 100 + i}, 3)
		if err != nil || !ok {
			t.Fatalf("reserve %d: ok=%v err=%v", i, ok, err)
		}
	}
	listed, err := limiter.List(ctx, 7, 2)
	if err != nil || len(listed) != 2 {
		t.Fatalf("bounded list len=%d err=%v", len(listed), err)
	}
	if err := limiter.Release(ctx, 7, listed[0]); err != nil {
		t.Fatalf("release: %v", err)
	}
	ok, err := limiter.Reserve(ctx, 7, OwnerOrderSlot{MarketID: 9, OrderID: 999}, 3)
	if err != nil || !ok {
		t.Fatalf("reserve after release: ok=%v err=%v", ok, err)
	}
}

func TestRedisOwnerSlotLimiter_SyncKeepsOrderedPrefixWithinHardLimit(t *testing.T) {
	limiter, _ := newOwnerSlotLimiterForTest(t)
	ctx := context.Background()
	slots := []OwnerOrderSlot{
		{MarketID: 10, OrderID: 100},
		{MarketID: 20, OrderID: 200},
		{MarketID: 30, OrderID: 300},
	}
	ok, err := limiter.Sync(ctx, 55, slots, 2)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if ok {
		t.Fatal("three authoritative members must not fit max=2")
	}
	for i, slot := range slots {
		member, err := limiter.rdb.SIsMember(ctx, ownerSlotKey(55), ownerSlotMember(slot)).Result()
		if err != nil {
			t.Fatalf("SISMEMBER %d: %v", i, err)
		}
		want := i < 2
		if member != want {
			t.Fatalf("slot[%d] present=%v, want %v", i, member, want)
		}
	}
	if cardinality, err := limiter.rdb.SCard(ctx, ownerSlotKey(55)).Result(); err != nil || cardinality != 2 {
		t.Fatalf("SCARD=%d err=%v, want 2", cardinality, err)
	}
	// 同一前缀重放必须幂等成功，不得因为 SET 已满误报。
	ok, err = limiter.Sync(ctx, 55, slots[:2], 2)
	if err != nil || !ok {
		t.Fatalf("idempotent Sync ok=%v err=%v", ok, err)
	}
}

func TestRedisOwnerSlotLimiter_SyncRejectsUnboundedInput(t *testing.T) {
	limiter, _ := newOwnerSlotLimiterForTest(t)
	_, err := limiter.Sync(context.Background(), 66, []OwnerOrderSlot{
		{MarketID: 1, OrderID: 1},
		{MarketID: 1, OrderID: 2},
		{MarketID: 1, OrderID: 3},
	}, 1)
	if err == nil {
		t.Fatal("Sync input over max+1 must be rejected before Redis")
	}
}
