// allocator_test.go — ds_allocator biz 层测试(miniredis 真实跑通)。
package biz

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/config"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/data"
)

func newUsecase(t *testing.T) (*AllocatorUsecase, *data.RedisBattleRepo) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis run: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	cfg := conf.AllocatorConf{
		HeartbeatTimeout: config.Duration(15 * time.Second),
		SweepInterval:    config.Duration(5 * time.Second),
		BattleTTL:        config.Duration(2 * time.Hour),
		MockDSAddrHost:   "127.0.0.1",
		MockDSPortBase:   30000,
		MockDSPortRange:  1000,
	}
	repo := data.NewRedisBattleRepo(rdb)
	alloc := NewMockGameServerAllocator(cfg)
	return NewAllocatorUsecase(repo, alloc, cfg), repo
}

func TestAllocateBattle(t *testing.T) {
	ctx := context.Background()
	uc, _ := newUsecase(t)

	res, err := uc.AllocateBattle(ctx, 7, []uint64{10, 20, 30}, 1, "5v5_ranked")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if res.DSPodName != "pandora-battle-7" {
		t.Fatalf("pod = %q, want pandora-battle-7", res.DSPodName)
	}
	if res.DSAddr != "127.0.0.1:30007" {
		t.Fatalf("addr = %q, want 127.0.0.1:30007", res.DSAddr)
	}
}

func TestAllocateBattleIdempotent(t *testing.T) {
	ctx := context.Background()
	uc, _ := newUsecase(t)

	first, err := uc.AllocateBattle(ctx, 7, []uint64{10, 20}, 1, "5v5_ranked")
	if err != nil {
		t.Fatalf("first allocate: %v", err)
	}
	second, err := uc.AllocateBattle(ctx, 7, []uint64{10, 20}, 1, "5v5_ranked")
	if err != nil {
		t.Fatalf("second allocate: %v", err)
	}
	if first.DSAddr != second.DSAddr || first.AllocatedAtMs != second.AllocatedAtMs {
		t.Fatalf("idempotent mismatch: %+v vs %+v", first, second)
	}
}

func TestReleaseBattleIdempotent(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)

	if _, err := uc.AllocateBattle(ctx, 7, []uint64{10}, 1, "5v5_ranked"); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := uc.ReleaseBattle(ctx, 7, "completed"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, found, _ := repo.GetBattle(ctx, 7); found {
		t.Fatal("battle 7 should be gone after release")
	}
	// 再次释放(已不存在)应幂等成功
	if err := uc.ReleaseBattle(ctx, 7, "completed"); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}
}

func TestHeartbeatUpdatesState(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)

	if _, err := uc.AllocateBattle(ctx, 7, []uint64{10, 20}, 1, "5v5_ranked"); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	res, err := uc.Heartbeat(ctx, 7, "pandora-battle-7", 8, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if res.Command != "" {
		t.Fatalf("command = %q, want empty", res.Command)
	}
	got, _, _ := repo.GetBattle(ctx, 7)
	if got.State != "running" || got.PlayerCount != 8 {
		t.Fatalf("after heartbeat: %+v", got)
	}
}

func TestHeartbeatOrphanReturnsStop(t *testing.T) {
	ctx := context.Background()
	uc, _ := newUsecase(t)

	// 无对应镜像的孤儿 DS 上报心跳 → 应被告知 stop
	res, err := uc.Heartbeat(ctx, 999, "pandora-battle-999", 1, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if res.Command != "stop" {
		t.Fatalf("command = %q, want stop", res.Command)
	}
}

func TestListBattles(t *testing.T) {
	ctx := context.Background()
	uc, _ := newUsecase(t)

	if _, err := uc.AllocateBattle(ctx, 1, []uint64{10}, 1, "5v5_ranked"); err != nil {
		t.Fatalf("allocate 1: %v", err)
	}
	if _, err := uc.AllocateBattle(ctx, 2, []uint64{20}, 1, "5v5_ranked"); err != nil {
		t.Fatalf("allocate 2: %v", err)
	}

	all, err := uc.ListBattles(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("list all = %d, want 2", len(all))
	}

	// 状态过滤:ready 全中,running 无
	ready, _ := uc.ListBattles(ctx, "ready")
	if len(ready) != 2 {
		t.Fatalf("list ready = %d, want 2", len(ready))
	}
	running, _ := uc.ListBattles(ctx, "running")
	if len(running) != 0 {
		t.Fatalf("list running = %d, want 0", len(running))
	}
}

func TestSweepMarksAbandoned(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)

	if _, err := uc.AllocateBattle(ctx, 7, []uint64{10}, 1, "5v5_ranked"); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	// 手动把 last_heartbeat_ms 回拨到远古,模拟心跳超时
	if err := repo.UpdateBattleWithLock(ctx, 7, 3, func(b *dsv1.BattleStorageRecord) error {
		b.LastHeartbeatMs = 1
		return nil
	}, 2*time.Hour); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, found, _ := repo.GetBattle(ctx, 7)
	if !found {
		t.Fatal("battle should still exist (terminal record retained)")
	}
	if got.State != "abandoned" {
		t.Fatalf("state = %q, want abandoned", got.State)
	}
	// 已移出 active,不再被扫描
	ids, _ := repo.RangeActiveBattles(ctx)
	if len(ids) != 0 {
		t.Fatalf("active should be empty after sweep, got %v", ids)
	}
}
