// allocator_test.go — ds_allocator biz 层测试(miniredis 真实跑通)。
package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/config"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/data"
)

func testCfg() conf.AllocatorConf {
	return conf.AllocatorConf{
		HeartbeatTimeout: config.Duration(15 * time.Second),
		SweepInterval:    config.Duration(5 * time.Second),
		BattleTTL:        config.Duration(2 * time.Hour),
		MockDSAddrHost:   "127.0.0.1",
		MockDSPortBase:   30000,
		MockDSPortRange:  1000,
	}
}

// newUsecaseWithAlloc 用指定分配器装配 usecase + 真实 miniredis 仓储(返回 mr 供 TTL 断言)。
func newUsecaseWithAlloc(t *testing.T, alloc GameServerAllocator) (*AllocatorUsecase, *data.RedisBattleRepo, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis run: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	repo := data.NewRedisBattleRepo(rdb)
	return NewAllocatorUsecase(repo, alloc, testCfg()), repo, mr
}

func newUsecase(t *testing.T) (*AllocatorUsecase, *data.RedisBattleRepo) {
	t.Helper()
	uc, repo, _ := newUsecaseWithAlloc(t, NewMockGameServerAllocator(testCfg()))
	return uc, repo
}

// backdate 把 match 的 last_heartbeat_ms 回拨到远古,模拟心跳超时。
func backdate(t *testing.T, repo *data.RedisBattleRepo, matchID uint64) {
	t.Helper()
	if err := repo.UpdateBattleWithLock(context.Background(), matchID, 3, func(b *dsv1.BattleStorageRecord) error {
		b.LastHeartbeatMs = 1
		return nil
	}, 2*time.Hour); err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

// countingAllocator 包 Mock 分配器并统计 Release 次数,验证补偿重试期间 pod 只回收一次。
type countingAllocator struct {
	inner    GameServerAllocator
	releases int
}

func (c *countingAllocator) Allocate(ctx context.Context, matchID uint64, mapID uint32, gameMode string) (string, string, error) {
	return c.inner.Allocate(ctx, matchID, mapID, gameMode)
}

func (c *countingAllocator) Release(ctx context.Context, podName string) error {
	c.releases++
	return c.inner.Release(ctx, podName)
}

// mockLifecycle 记录 PublishLifecycle 调用;前 failFirst 次返回错误(模拟 Kafka 临时不可用)。
type mockLifecycle struct {
	failFirst int
	calls     int
	delivered []uint64
}

func (m *mockLifecycle) PublishLifecycle(_ context.Context, evt *dsv1.DSLifecycleEvent) error {
	m.calls++
	if m.calls <= m.failFirst {
		return errors.New("kafka unavailable")
	}
	m.delivered = append(m.delivered, evt.GetMatchId())
	return nil
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

// TestSweepDeliversAbandonedFirstTry:配置 kafka 且首次投递成功 → 发 1 次事件、移出 active、回收 1 次。
func TestSweepDeliversAbandonedFirstTry(t *testing.T) {
	ctx := context.Background()
	alloc := &countingAllocator{inner: NewMockGameServerAllocator(testCfg())}
	uc, repo, _ := newUsecaseWithAlloc(t, alloc)
	life := &mockLifecycle{}
	uc.SetLifecyclePusher(life)

	if _, err := uc.AllocateBattle(ctx, 5, []uint64{1, 2}, 1, "5v5_ranked"); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	backdate(t, repo, 5)

	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 0 {
		t.Fatalf("active = %v, want empty after delivery", ids)
	}
	if life.calls != 1 || len(life.delivered) != 1 || life.delivered[0] != 5 {
		t.Fatalf("publish calls=%d delivered=%v, want 1 / [5]", life.calls, life.delivered)
	}
	if alloc.releases != 1 {
		t.Fatalf("releases=%d, want 1", alloc.releases)
	}
}

// TestSweepReliableCompensation_RetryUntilDelivered:Kafka 前两轮不可用 → abandoned 对局保留在
// active 重试,第三轮投递成功才移出;pod 只在首次转 abandoned 回收一次(不变量 §4 可靠补偿)。
func TestSweepReliableCompensation_RetryUntilDelivered(t *testing.T) {
	ctx := context.Background()
	alloc := &countingAllocator{inner: NewMockGameServerAllocator(testCfg())}
	uc, repo, _ := newUsecaseWithAlloc(t, alloc)
	life := &mockLifecycle{failFirst: 2} // 前两轮投递失败,第三轮成功
	uc.SetLifecyclePusher(life)

	if _, err := uc.AllocateBattle(ctx, 7, []uint64{10, 20}, 1, "5v5_ranked"); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	backdate(t, repo, 7)

	// sweep #1:投递失败 → 标记 abandoned、回收 pod、保留在 active 待重试
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep1: %v", err)
	}
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 1 {
		t.Fatalf("after sweep1 active = %v, want still 1 (retry pending)", ids)
	}
	if got, _, _ := repo.GetBattle(ctx, 7); got.State != "abandoned" {
		t.Fatalf("after sweep1 state = %q, want abandoned", got.State)
	}

	// sweep #2:仍失败 → 仍保留 active,pod 不重复回收
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep2: %v", err)
	}
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 1 {
		t.Fatalf("after sweep2 active = %v, want still 1", ids)
	}

	// sweep #3:投递成功 → 移出 active
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep3: %v", err)
	}
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 0 {
		t.Fatalf("after sweep3 active = %v, want empty (delivered)", ids)
	}

	if alloc.releases != 1 {
		t.Fatalf("pod released %d times, want exactly 1 (no re-release during retry)", alloc.releases)
	}
	if life.calls != 3 {
		t.Fatalf("publish called %d times, want 3 (2 fail + 1 success)", life.calls)
	}
	if len(life.delivered) != 1 || life.delivered[0] != 7 {
		t.Fatalf("delivered = %v, want [7]", life.delivered)
	}
	// 终态镜像仍可查
	if rec, found, _ := repo.GetBattle(ctx, 7); !found || rec.State != "abandoned" {
		t.Fatalf("terminal record missing/wrong: found=%v rec=%+v", found, rec)
	}
}

// TestSweepReliableCompensation_KeepsTTLOnFailure:Kafka 持续不可用时,abandoned 标记 + 每轮重试
// 走 UpdateBattleKeepTTL(KEEPTTL),保留镜像原 TTL 不刷新 → BattleTTL 是补偿重试的天然上界
// (不变量 §4)。若误用刷新 TTL 的更新路径,会导致镜像永不过期、active 无限堆积。
func TestSweepReliableCompensation_KeepsTTLOnFailure(t *testing.T) {
	ctx := context.Background()
	alloc := &countingAllocator{inner: NewMockGameServerAllocator(testCfg())}
	uc, repo, mr := newUsecaseWithAlloc(t, alloc)
	life := &mockLifecycle{failFirst: 1000} // 始终投递失败
	uc.SetLifecyclePusher(life)

	if _, err := uc.AllocateBattle(ctx, 7, []uint64{10, 20}, 1, "5v5_ranked"); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	backdate(t, repo, 7)

	// 把 TTL 钉到一个已知的小值,便于检测是否被重试刷新(CreateBattle/backdate 会先设成 BattleTTL 2h)
	key := "pandora:ds:battle:{7}"
	mr.SetTTL(key, 90*time.Second)
	ttlBefore := mr.TTL(key)
	if ttlBefore <= 0 {
		t.Fatalf("precondition: ttl not pinned, got %v", ttlBefore)
	}

	// 连续多轮 sweep,全部投递失败 → abandoned 对局保留在 active 重试
	for i := 0; i < 3; i++ {
		if err := uc.sweepOnce(ctx); err != nil {
			t.Fatalf("sweep #%d: %v", i+1, err)
		}
	}

	// 关键断言:TTL 没被重试刷新(仍 ≤ 钉住的 90s,而非回弹到 BattleTTL 2h)
	ttlAfter := mr.TTL(key)
	if ttlAfter > ttlBefore {
		t.Fatalf("TTL refreshed on retry: before=%v after=%v(KEEPTTL 未生效,BattleTTL 上界不成立)", ttlBefore, ttlAfter)
	}
	// 仍保留在 active 等待重试,状态 abandoned,pod 只回收一次
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 1 {
		t.Fatalf("active = %v, want still 1 (retry pending)", ids)
	}
	if got, _, _ := repo.GetBattle(ctx, 7); got.State != "abandoned" {
		t.Fatalf("state = %q, want abandoned", got.State)
	}
	if alloc.releases != 1 {
		t.Fatalf("pod released %d times, want exactly 1 (no re-release during retry)", alloc.releases)
	}
}
