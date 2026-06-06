// battle_test.go — ds_allocator data 层 Redis 实现测试(miniredis)。
package data

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/errcode"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
)

const testTTL = 2 * time.Hour

func newRepo(t *testing.T) (*RedisBattleRepo, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis run: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisBattleRepo(rdb), mr
}

func sampleBattle(matchID uint64, lastHbMs int64) *dsv1.BattleStorageRecord {
	return &dsv1.BattleStorageRecord{
		MatchId:         matchID,
		DsPodName:       "pandora-battle-1",
		DsAddr:          "127.0.0.1:30001",
		State:           "ready",
		PlayerIds:       []uint64{10, 20, 30},
		MapId:           1,
		GameMode:        "5v5_ranked",
		AllocatedAtMs:   1000,
		LastHeartbeatMs: lastHbMs,
		PlayerCount:     3,
	}
}

func TestCreateGetRoundtrip(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	if err := repo.CreateBattle(ctx, sampleBattle(1, 5000), testTTL); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, found, err := repo.GetBattle(ctx, 1)
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if got.DsAddr != "127.0.0.1:30001" || got.PlayerCount != 3 || len(got.PlayerIds) != 3 {
		t.Fatalf("battle mismatch: %+v", got)
	}

	// active ZSET 应含 match 1
	ids, err := repo.RangeActiveBattles(ctx)
	if err != nil {
		t.Fatalf("range active: %v", err)
	}
	if len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("active = %v, want [1]", ids)
	}
}

func TestGetMissReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	if _, found, err := repo.GetBattle(ctx, 999); err != nil || found {
		t.Fatalf("expected not found, got found=%v err=%v", found, err)
	}
}

func TestRangeStaleBattles(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	// match 1 心跳 1000(旧),match 2 心跳 9000(新)
	if err := repo.CreateBattle(ctx, sampleBattle(1, 1000), testTTL); err != nil {
		t.Fatalf("create 1: %v", err)
	}
	if err := repo.CreateBattle(ctx, sampleBattle(2, 9000), testTTL); err != nil {
		t.Fatalf("create 2: %v", err)
	}

	// 阈值 5000:只有 match 1(1000 ≤ 5000)算超时
	stale, err := repo.RangeStaleBattles(ctx, 5000)
	if err != nil {
		t.Fatalf("range stale: %v", err)
	}
	if len(stale) != 1 || stale[0] != 1 {
		t.Fatalf("stale = %v, want [1]", stale)
	}
}

func TestUpdateBattleWithLock(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	if err := repo.CreateBattle(ctx, sampleBattle(1, 1000), testTTL); err != nil {
		t.Fatalf("create: %v", err)
	}

	err := repo.UpdateBattleWithLock(ctx, 1, 3, func(b *dsv1.BattleStorageRecord) error {
		b.State = "running"
		b.LastHeartbeatMs = 8000
		b.PlayerCount = 10
		return nil
	}, testTTL)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	got, _, _ := repo.GetBattle(ctx, 1)
	if got.State != "running" || got.PlayerCount != 10 {
		t.Fatalf("after update: %+v", got)
	}
	// active ZSET score 应刷新到 8000
	stale, _ := repo.RangeStaleBattles(ctx, 5000)
	if len(stale) != 0 {
		t.Fatalf("after heartbeat refresh, stale should be empty, got %v", stale)
	}
}

func TestUpdateBattleWithLockNotFound(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	err := repo.UpdateBattleWithLock(ctx, 42, 3, func(*dsv1.BattleStorageRecord) error { return nil }, testTTL)
	if errcode.As(err) != errcode.ErrDSPodNotFound {
		t.Fatalf("expected ErrDSPodNotFound, got %v", err)
	}
}

func TestDeleteBattle(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	if err := repo.CreateBattle(ctx, sampleBattle(1, 1000), testTTL); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.DeleteBattle(ctx, 1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := repo.GetBattle(ctx, 1); found {
		t.Fatal("battle 1 should be deleted")
	}
	ids, _ := repo.RangeActiveBattles(ctx)
	if len(ids) != 0 {
		t.Fatalf("active should be empty, got %v", ids)
	}
}
