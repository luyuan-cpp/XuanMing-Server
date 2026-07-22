package data

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestRedisSessionDeleteIfJTIRejectsConcurrentLateLogoutAfterRotation 验证旧设备迟到的
// Logout 即使并发重试，也不能删除新设备刚轮换出的 session。
func TestRedisSessionDeleteIfJTIRejectsConcurrentLateLogoutAfterRotation(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	repo := NewRedisSessionRepo(rdb)
	ctx := context.Background()
	const playerID = uint64(7001)

	if err := repo.Set(ctx, playerID, "old-token", "old-jti", "old-device", time.Hour); err != nil {
		t.Fatalf("set old session: %v", err)
	}
	if err := repo.Set(ctx, playerID, "new-token", "new-jti", "new-device", time.Hour); err != nil {
		t.Fatalf("rotate session: %v", err)
	}

	const attempts = 32
	errCh := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(attempt int) {
			defer wg.Done()
			deleted, err := repo.DeleteIfJTI(ctx, playerID, "old-jti")
			if err != nil {
				errCh <- fmt.Errorf("late logout attempt %d: %w", attempt, err)
				return
			}
			if deleted {
				errCh <- fmt.Errorf("late logout attempt %d deleted the rotated session", attempt)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	jti, found, err := repo.GetJTI(ctx, playerID)
	if err != nil || !found || jti != "new-jti" {
		t.Fatalf("rotated session changed after stale logout retries: jti=%q found=%v err=%v", jti, found, err)
	}

	deleted, err := repo.DeleteIfJTI(ctx, playerID, "new-jti")
	if err != nil || !deleted {
		t.Fatalf("current logout should delete exactly once: deleted=%v err=%v", deleted, err)
	}
	deleted, err = repo.DeleteIfJTI(ctx, playerID, "new-jti")
	if err != nil || deleted {
		t.Fatalf("replayed current logout should be idempotent: deleted=%v err=%v", deleted, err)
	}
	if jti, found, err = repo.GetJTI(ctx, playerID); err != nil || found || jti != "" {
		t.Fatalf("session should be absent after current logout: jti=%q found=%v err=%v", jti, found, err)
	}
}
