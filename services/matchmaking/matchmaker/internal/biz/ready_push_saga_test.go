// ready_push_saga_test.go — READY 推送 at-least-once 交付回归测试(2026-07-20 事故)。
//
// 事故形态:READY durable 提交后推送是 fire-and-forget,进程崩溃或 Kafka 不可用都会把
// 「非队长成员唯一能得知 READY / Battle 落点的通道」永久静默丢弃,队员停在 Hub。
// 修复不变量:READY ∈ active ZSET ⟺ READY 推送交付未确认;全员推送成功才移出 active,
// 滞留的 READY 由撮合循环 finalizeReadyMatch 幂等补推(重签新 jti)。
package biz

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/pkg/placement"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/model"
)

func activeContains(t *testing.T, ctx context.Context, f *fixture, matchID uint64) bool {
	t.Helper()
	ids, err := f.repo.RangeActiveMatches(ctx)
	if err != nil {
		t.Fatalf("range active: %v", err)
	}
	for _, id := range ids {
		if id == matchID {
			return true
		}
	}
	return false
}

// Kafka 在 READY 提交时不可用 → match 保留在 active;进程重启(新 usecase)+ Kafka 恢复后,
// 撮合循环补推全员 READY(各带本人新签 battle_ticket)并移出 active。
func TestReadyPushFailureKeepsActiveAndRestartRedelivers(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 9950)
	seedAllocatingMatch(t, ctx, f, 9950, time.Now().Add(time.Minute).UnixMilli())

	allocation := model.BattleAllocation{Address: "10.0.0.7:7787", Target: placement.Target{
		PodName: "battle-ready-push", InstanceUID: "uid-ready-push", InstanceEpoch: 1,
		AllocationID: "allocation-ready-push", ReleaseTrack: "stable",
	}}
	allocator := &switchingAllocationStub{first: allocation, second: allocation}
	f.uc.allocator = allocator
	f.pusher.failWith(errors.New("kafka unavailable"))

	// 分配推进到 READY;推送失败不能吞成局(READY 已 durable),也不能移出 active。
	if err := f.uc.advanceAllocationsOnce(ctx); err != nil {
		t.Fatalf("advance to READY with failing pusher: %v", err)
	}
	ready, found, err := f.repo.GetMatch(ctx, 9950)
	if err != nil || !found || ready.GetStage() != stageReady {
		t.Fatalf("match not READY: found=%v match=%+v err=%v", found, ready, err)
	}
	if _, got := f.pusher.readyTicketFor(6); got {
		t.Fatal("failing pusher must not record a delivered READY push")
	}
	if !activeContains(t, ctx, f, 9950) {
		t.Fatal("undelivered READY must stay in active ZSET for retry")
	}

	// Kafka 仍不可用期间的重试轮:失败上抛(记日志),active 保留。
	if err := f.uc.advanceAllocationsOnce(ctx); err == nil {
		t.Fatal("retry with kafka still down must surface the push error")
	}
	if !activeContains(t, ctx, f, 9950) {
		t.Fatal("READY must remain active while push keeps failing")
	}

	// 进程重启 + Kafka 恢复:新 usecase 仅凭 active ZSET 就能补推并收尾。
	f.pusher.failWith(nil)
	restarted := NewMatchUsecase(f.repo, nil, f.pusher, allocator, &fakeIDGen{next: 10000}, f.locator, f.cfg)
	if err := restarted.advanceAllocationsOnce(ctx); err != nil {
		t.Fatalf("redeliver after recovery: %v", err)
	}
	for player := uint64(1); player <= 10; player++ {
		ticket, got := f.pusher.readyTicketFor(player)
		if !got {
			t.Fatalf("player %d missed redelivered READY push", player)
		}
		if want := fmt.Sprintf("ticket-9950-%d", player); ticket != want {
			t.Fatalf("player %d battle_ticket = %q, want personally signed %q", player, ticket, want)
		}
	}
	if activeContains(t, ctx, f, 9950) {
		t.Fatal("delivered READY must leave active ZSET")
	}
}

// 确认期 deadline 已过的 READY 滞留局:expireOnce 不得清 active、不得判失败
// (否则拆掉补推驱动);补推成功后 active 才清空。
func TestExpireOnceKeepsUndeliveredReadyActive(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 9951)
	seedAllocatingMatch(t, ctx, f, 9951, time.Now().UnixMilli()-120_000)

	allocation := model.BattleAllocation{Address: "10.0.0.8:7787", Target: placement.Target{
		PodName: "battle-ready-expire", InstanceUID: "uid-ready-expire", InstanceEpoch: 1,
		AllocationID: "allocation-ready-expire", ReleaseTrack: "stable",
	}}
	allocator := &switchingAllocationStub{first: allocation, second: allocation}
	f.uc.allocator = allocator
	f.pusher.failWith(errors.New("kafka unavailable"))

	if err := f.uc.advanceAllocationsOnce(ctx); err != nil {
		t.Fatalf("advance to READY with failing pusher: %v", err)
	}
	if err := f.uc.expireOnce(ctx); err != nil {
		t.Fatalf("expireOnce: %v", err)
	}
	m, found, err := f.repo.GetMatch(ctx, 9951)
	if err != nil || !found || m.GetStage() != stageReady {
		t.Fatalf("expireOnce must not fail an undelivered READY: found=%v match=%+v err=%v", found, m, err)
	}
	if !activeContains(t, ctx, f, 9951) {
		t.Fatal("expireOnce must not evict an undelivered READY from active")
	}

	f.pusher.failWith(nil)
	if err := f.uc.advanceAllocationsOnce(ctx); err != nil {
		t.Fatalf("redeliver: %v", err)
	}
	if _, got := f.pusher.readyTicketFor(1); !got {
		t.Fatal("READY push not redelivered after recovery")
	}
	if activeContains(t, ctx, f, 9951) {
		t.Fatal("delivered READY must leave active ZSET")
	}
}
