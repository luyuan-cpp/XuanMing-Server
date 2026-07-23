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

	if err := repo.Set(ctx, playerID, "old-token", "old-jti", "old-device", time.Hour, 1); err != nil {
		t.Fatalf("set old session: %v", err)
	}
	if err := repo.Set(ctx, playerID, "new-token", "new-jti", "new-device", time.Hour, 2); err != nil {
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

// TestRedisSessionSetGenerationOrdering 验证并发 Login 定序(R7 收口):迟到的低代际
// 条件写必须被拒且零覆盖,两存储收敛到最高代际;dev(gen=0)保持无条件覆盖。
func TestRedisSessionSetGenerationOrdering(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	repo := NewRedisSessionRepo(rdb)
	ctx := context.Background()
	const playerID = uint64(7002)

	// P0-1 复现交错:B(gen2)先完成 Redis 写,A(gen1)迟到 → 必拒,会话仍是 B。
	if err := repo.Set(ctx, playerID, "token-B", "jti-B", "device-B", time.Hour, 2); err != nil {
		t.Fatalf("set gen2: %v", err)
	}
	err := repo.Set(ctx, playerID, "token-A", "jti-A", "device-A", time.Hour, 1)
	if err == nil {
		t.Fatal("late lower-generation write must be rejected")
	}
	if jti, found, gerr := repo.GetJTI(ctx, playerID); gerr != nil || !found || jti != "jti-B" {
		t.Fatalf("session must remain the highest generation: jti=%q found=%v err=%v", jti, found, gerr)
	}

	// 同代际重放同样拒(代际每登录唯一,相等 = 重放)。
	if err := repo.Set(ctx, playerID, "token-B2", "jti-B2", "device-B", time.Hour, 2); err == nil {
		t.Fatal("equal-generation replay must be rejected")
	}

	// 更高代际正常覆盖。
	if err := repo.Set(ctx, playerID, "token-C", "jti-C", "device-C", time.Hour, 3); err != nil {
		t.Fatalf("higher generation must overwrite: %v", err)
	}
	if jti, _, _ := repo.GetJTI(ctx, playerID); jti != "jti-C" {
		t.Fatalf("want jti-C after gen3 write, got %q", jti)
	}

	// dev 裸跑(gen=0):无条件覆盖,且清掉残留 gen 字段,后续 dev 登录不被误拒。
	if err := repo.Set(ctx, playerID, "token-D", "jti-D", "device-D", time.Hour, 0); err != nil {
		t.Fatalf("gen0 unconditional overwrite: %v", err)
	}
	if err := repo.Set(ctx, playerID, "token-E", "jti-E", "device-E", time.Hour, 0); err != nil {
		t.Fatalf("second gen0 overwrite must not be fenced: %v", err)
	}
	if jti, _, _ := repo.GetJTI(ctx, playerID); jti != "jti-E" {
		t.Fatalf("want jti-E after dev overwrites, got %q", jti)
	}
}
