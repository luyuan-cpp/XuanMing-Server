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
	"github.com/luyuancpp/pandora/pkg/errcode"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/data"
)

// 生产 readyPollInterval 是 1s;单测把它调小,避免每次 AllocateBattle 都等满一个轮询周期。
func init() { readyPollInterval = 10 * time.Millisecond }

func testCfg() conf.AllocatorConf {
	return conf.AllocatorConf{
		HeartbeatTimeout:   config.Duration(15 * time.Second),
		SweepInterval:      config.Duration(5 * time.Second),
		BattleTTL:          config.Duration(2 * time.Hour),
		ReadyWaitTimeout:   config.Duration(1 * time.Second), // 测试用短超时,避免慢测
		EmptyBattleTimeout: config.Duration(5 * time.Minute),
		MockDSAddrHost:     "127.0.0.1",
		MockDSPortBase:     30000,
		MockDSPortRange:    1000,
	}
}

// allocateReady 模拟正常时序:并发跑 AllocateBattle,待 warming 镜像出现后用对应 pod 上报一次
// running 心跳,使 DS 进入 running,AllocateBattle 等到 ready 后返回。
func allocateReady(t *testing.T, uc *AllocatorUsecase, repo *data.RedisBattleRepo, matchID uint64, playerIDs []uint64, mapID uint32, gameMode string) *AllocateResult {
	t.Helper()
	ctx := context.Background()
	type out struct {
		res *AllocateResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := uc.AllocateBattle(ctx, matchID, playerIDs, mapID, gameMode)
		done <- out{res, err}
	}()
	feedReadyHeartbeat(t, uc, repo, matchID, int32(len(playerIDs)))
	r := <-done
	if r.err != nil {
		t.Fatalf("allocate match %d: %v", matchID, r.err)
	}
	return r.res
}

// feedReadyHeartbeat 等 warming 镜像出现后,用其记录的 pod 上报一次 running 心跳。
// 上报前确保 wall clock 已越过 AllocatedAtMs,保证 LastHeartbeatMs 严格大于分配时刻(满足 ready 判定)。
func feedReadyHeartbeat(t *testing.T, uc *AllocatorUsecase, repo *data.RedisBattleRepo, matchID uint64, playerCount int32) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(3 * time.Second)
	var rec *dsv1.BattleStorageRecord
	for {
		b, found, err := repo.GetBattle(ctx, matchID)
		if err == nil && found && b.DsPodName != "" {
			rec = b
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("warming record for match %d never appeared", matchID)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for time.Now().UnixMilli() <= rec.AllocatedAtMs {
		time.Sleep(time.Millisecond)
	}
	if _, err := uc.Heartbeat(ctx, matchID, rec.DsPodName, playerCount, "running", time.Now().UnixMilli()); err != nil {
		t.Fatalf("heartbeat match %d: %v", matchID, err)
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
	uc, repo := newUsecase(t)

	res := allocateReady(t, uc, repo, 7, []uint64{10, 20, 30}, 1, "5v5_ranked")
	if res.DSPodName != "pandora-battle-7" {
		t.Fatalf("pod = %q, want pandora-battle-7", res.DSPodName)
	}
	if res.DSAddr != "127.0.0.1:30007" {
		t.Fatalf("addr = %q, want 127.0.0.1:30007", res.DSAddr)
	}
	// AllocateBattle 返回前 DS 必须已用心跳确认 ready/running
	got, _, _ := repo.GetBattle(context.Background(), 7)
	if got.State != stateRunning {
		t.Fatalf("state = %q, want running", got.State)
	}
	if got.LastHeartbeatMs <= got.AllocatedAtMs {
		t.Fatalf("LastHeartbeatMs %d must be > AllocatedAtMs %d (real heartbeat)", got.LastHeartbeatMs, got.AllocatedAtMs)
	}
}

// TestAllocateBattleReadyWaitTimeout:没有 DS 心跳 → 等待超时 → 回收 pod + 删镜像 + 返回分配失败
// (绝不把 ds_addr 回给 matchmaker)。
func TestAllocateBattleReadyWaitTimeout(t *testing.T) {
	ctx := context.Background()
	alloc := &countingAllocator{inner: NewMockGameServerAllocator(testCfg())}
	uc, repo, _ := newUsecaseWithAlloc(t, alloc)

	_, err := uc.AllocateBattle(ctx, 7, []uint64{10, 20}, 1, "5v5_ranked")
	if err == nil {
		t.Fatal("expected allocation failure on ready wait timeout")
	}
	if errcode.As(err) != errcode.ErrDSAllocationFailed {
		t.Fatalf("err code = %v, want ErrDSAllocationFailed", errcode.As(err))
	}
	if _, found, _ := repo.GetBattle(ctx, 7); found {
		t.Fatal("battle record must be deleted after ready wait timeout")
	}
	if alloc.releases != 1 {
		t.Fatalf("pod released %d times, want exactly 1", alloc.releases)
	}
}

// TestAllocateBattleRejectsMismatchedPodHeartbeat:证明 match_id ↔ pod 绑定不可绕过。
// 一个携带正确 match_id 但 pod 名不符(旧 DS / 孤儿 DS / 抢跑的别局 DS)的心跳,
// 必须被拒(返回 commandStop)、不得写回镜像、更不得打开 AllocateBattle 的就绪门控:
// 最终 AllocateBattle 仍因等不到本局 pod 的真实心跳而超时失败,绝不回 ds_addr。
func TestAllocateBattleRejectsMismatchedPodHeartbeat(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)

	type out struct {
		res *AllocateResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := uc.AllocateBattle(ctx, 7, []uint64{10, 20}, 1, "5v5_ranked")
		done <- out{res, err}
	}()

	// 等 warming 镜像出现(本局 pod = pandora-battle-7),记录其分配时刻。
	deadline := time.Now().Add(3 * time.Second)
	var rec *dsv1.BattleStorageRecord
	for {
		b, found, err := repo.GetBattle(ctx, 7)
		if err == nil && found && b.DsPodName != "" {
			rec = b
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("warming record for match 7 never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for time.Now().UnixMilli() <= rec.AllocatedAtMs {
		time.Sleep(time.Millisecond)
	}

	// 用「错误的 pod 名」上报 running 心跳:必须被门控拒绝并令其停机。
	hbRes, err := uc.Heartbeat(ctx, 7, "pandora-battle-999", 2, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("mismatched heartbeat returned hard error: %v", err)
	}
	if hbRes.Command != commandStop {
		t.Fatalf("mismatched-pod heartbeat command = %q, want stop", hbRes.Command)
	}

	// 镜像不得被异局心跳污染:仍停在 warming、未刷新到分配时刻之后。
	got, _, _ := repo.GetBattle(ctx, 7)
	if got.State != stateWarming {
		t.Fatalf("state = %q, want warming (foreign heartbeat must not flip state)", got.State)
	}
	if got.LastHeartbeatMs > got.AllocatedAtMs {
		t.Fatalf("LastHeartbeatMs %d must stay <= AllocatedAtMs %d (foreign heartbeat must not refresh)",
			got.LastHeartbeatMs, got.AllocatedAtMs)
	}

	// 门控不得放行:AllocateBattle 仍超时失败,绝不返回 ds_addr。
	r := <-done
	if r.err == nil {
		t.Fatalf("AllocateBattle must fail when only a mismatched pod heartbeat arrived, got addr=%q", r.res.DSAddr)
	}
	if errcode.As(r.err) != errcode.ErrDSAllocationFailed {
		t.Fatalf("err code = %v, want ErrDSAllocationFailed", errcode.As(r.err))
	}
	if _, found, _ := repo.GetBattle(ctx, 7); found {
		t.Fatal("battle record must be cleaned up after ready wait timeout")
	}
}

func TestAllocateBattleIdempotent(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)

	first := allocateReady(t, uc, repo, 7, []uint64{10, 20}, 1, "5v5_ranked")
	// 幂等:已 ready/running 且有有效心跳 → 第二次直接返回已分配地址(不再等心跳)
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

	allocateReady(t, uc, repo, 7, []uint64{10}, 1, "5v5_ranked")
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

	// allocateReady 已上报一次 running 心跳;再上报一次刷 player_count=8
	allocateReady(t, uc, repo, 7, []uint64{10, 20}, 1, "5v5_ranked")
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

// TestHeartbeatPodMismatchRejected:镜像已绑定某 pod,另一个 pod(旧/孤儿 DS)上报 → 返回 stop
// 且不写回镜像(不污染新对局的 state/心跳/player_count)。
func TestHeartbeatPodMismatchRejected(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)

	now := time.Now().UnixMilli()
	rec := &dsv1.BattleStorageRecord{
		MatchId: 7, DsPodName: "pandora-battle-7", DsAddr: "127.0.0.1:30007",
		State: stateWarming, AllocatedAtMs: now, LastHeartbeatMs: now, PlayerCount: 2,
	}
	if err := repo.CreateBattle(ctx, rec, 2*time.Hour); err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := uc.Heartbeat(ctx, 7, "pandora-battle-OLD", 9, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if res.Command != "stop" {
		t.Fatalf("command = %q, want stop", res.Command)
	}
	got, _, _ := repo.GetBattle(ctx, 7)
	if got.State != stateWarming || got.PlayerCount == 9 || got.LastHeartbeatMs != now {
		t.Fatalf("mismatched pod must not update record: %+v", got)
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

// recordingAllocator 记录 Release 的 podName 并经 channel 通知,供异步 kill 断言。
type recordingAllocator struct {
	inner    GameServerAllocator
	released chan string
}

func (r *recordingAllocator) Allocate(ctx context.Context, matchID uint64, mapID uint32, gameMode string) (string, string, error) {
	return r.inner.Allocate(ctx, matchID, mapID, gameMode)
}

func (r *recordingAllocator) Release(ctx context.Context, podName string) error {
	_ = r.inner.Release(ctx, podName)
	r.released <- podName
	return nil
}

// TestHeartbeatOrphanKillsStrandedDS:local 模式(killOrphanOnStop=true)下,orphan 心跳除了回 stop,
// 还必须主动 Release 幽灵 pod——UE DS 收 stop 不自杀,不主动 kill 会让它占端口污染下一局。
func TestHeartbeatOrphanKillsStrandedDS(t *testing.T) {
	ctx := context.Background()
	rec := &recordingAllocator{inner: NewMockGameServerAllocator(testCfg()), released: make(chan string, 1)}
	uc, _, _ := newUsecaseWithAlloc(t, rec)
	uc.SetKillOrphanOnStop(true)

	res, err := uc.Heartbeat(ctx, 999, "pandora-battle-999", 1, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if res.Command != commandStop {
		t.Fatalf("command = %q, want stop", res.Command)
	}
	select {
	case pod := <-rec.released:
		if pod != "pandora-battle-999" {
			t.Fatalf("released pod = %q, want pandora-battle-999", pod)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected stranded DS to be released on orphan stop")
	}
}

// TestHeartbeatPodMismatchKillsOldDS:local 模式下 pod 不匹配(旧 DS 上报)时,主动 kill 的是**上报方**
// (旧 DS 的 pod),不动镜像里绑定的新 pod。
func TestHeartbeatPodMismatchKillsOldDS(t *testing.T) {
	ctx := context.Background()
	rec := &recordingAllocator{inner: NewMockGameServerAllocator(testCfg()), released: make(chan string, 1)}
	uc, repo, _ := newUsecaseWithAlloc(t, rec)
	uc.SetKillOrphanOnStop(true)

	now := time.Now().UnixMilli()
	b := &dsv1.BattleStorageRecord{
		MatchId: 7, DsPodName: "pandora-battle-7", DsAddr: "127.0.0.1:30007",
		State: stateWarming, AllocatedAtMs: now, LastHeartbeatMs: now, PlayerCount: 2,
	}
	if err := repo.CreateBattle(ctx, b, 2*time.Hour); err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := uc.Heartbeat(ctx, 7, "pandora-battle-OLD", 9, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if res.Command != commandStop {
		t.Fatalf("command = %q, want stop", res.Command)
	}
	select {
	case pod := <-rec.released:
		if pod != "pandora-battle-OLD" {
			t.Fatalf("released pod = %q, want pandora-battle-OLD (the stale reporter)", pod)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected stale DS to be released on pod_mismatch stop")
	}
}

// TestHeartbeatOrphanNoKillWhenDisabled:killOrphanOnStop 关闭(Agones/默认)时,orphan 心跳只回
// stop,不主动 Release——孤儿 pod 回收交 Agones 生命周期,避免 Redis 抖动误判 orphan 误删正常 pod。
func TestHeartbeatOrphanNoKillWhenDisabled(t *testing.T) {
	ctx := context.Background()
	rec := &recordingAllocator{inner: NewMockGameServerAllocator(testCfg()), released: make(chan string, 1)}
	uc, _, _ := newUsecaseWithAlloc(t, rec)
	// 不调 SetKillOrphanOnStop → 默认 false

	if _, err := uc.Heartbeat(ctx, 999, "pandora-battle-999", 1, "running", time.Now().UnixMilli()); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	select {
	case pod := <-rec.released:
		t.Fatalf("must NOT release when killOrphanOnStop disabled, got %q", pod)
	case <-time.After(200 * time.Millisecond):
		// 期望:无 Release 发生
	}
}

// TestHeartbeatOnAbandonedReturnsStopNoRefresh:abandoned 对局的 DS 若继续心跳(pod release
// 失败/延迟终止),Heartbeat 必须返回 stop 且**不写回记录**——不刷新 LastHeartbeatMs/TTL,也不
// 重新 ZAdd active。否则补偿重试会被推迟、BattleTTL 上界被不断刷新(W4 ⑧ Codex 复审 P1)。
func TestHeartbeatOnAbandonedReturnsStopNoRefresh(t *testing.T) {
	ctx := context.Background()
	alloc := &countingAllocator{inner: NewMockGameServerAllocator(testCfg())}
	uc, repo, mr := newUsecaseWithAlloc(t, alloc)
	life := &mockLifecycle{failFirst: 1000} // 始终投递失败,abandoned 对局保留在 active 重试
	uc.SetLifecyclePusher(life)

	allocateReady(t, uc, repo, 7, []uint64{10, 20}, 1, "5v5_ranked")
	backdate(t, repo, 7) // LastHeartbeatMs=1

	// sweep #1:投递失败 → 标记 abandoned、回收 pod、保留在 active 待重试
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep1: %v", err)
	}

	// 把 TTL 钉到已知小值,便于检测心跳是否误刷新
	key := "pandora:ds:battle:{7}"
	mr.SetTTL(key, 90*time.Second)
	ttlBefore := mr.TTL(key)
	if ttlBefore <= 0 {
		t.Fatalf("precondition: ttl not pinned, got %v", ttlBefore)
	}

	// abandoned 后 DS 继续心跳:必须返回 stop,且不写回记录
	res, err := uc.Heartbeat(ctx, 7, "pandora-battle-7", 9, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if res.Command != "stop" {
		t.Fatalf("command = %q, want stop", res.Command)
	}

	// 记录未被写回:LastHeartbeatMs 仍是回拨值 1(active score = LastHeartbeatMs 也未刷新),
	// state 仍 abandoned,PlayerCount 未被改成 9
	got, _, _ := repo.GetBattle(ctx, 7)
	if got.LastHeartbeatMs != 1 {
		t.Fatalf("LastHeartbeatMs = %d, want 1 (terminal heartbeat must not write back)", got.LastHeartbeatMs)
	}
	if got.State != "abandoned" {
		t.Fatalf("state = %q, want abandoned", got.State)
	}
	if got.PlayerCount == 9 {
		t.Fatalf("PlayerCount refreshed to 9, terminal record must not be written")
	}

	// TTL 未被心跳刷新(仍 ≤ 钉住的 90s)
	if ttlAfter := mr.TTL(key); ttlAfter > ttlBefore {
		t.Fatalf("TTL refreshed by terminal heartbeat: before=%v after=%v", ttlBefore, ttlAfter)
	}

	// active score 仍是陈旧值 → 下一轮 sweep 仍会命中重试
	stale, _ := repo.RangeStaleBattles(ctx, 1000)
	if len(stale) != 1 || stale[0] != 7 {
		t.Fatalf("stale = %v, want [7] (active score not refreshed, sweep still retries)", stale)
	}

	// 下一轮 sweep 仍重试投递(补偿没被心跳推迟)
	callsBefore := life.calls
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep2: %v", err)
	}
	if life.calls != callsBefore+1 {
		t.Fatalf("sweep2 publish calls = %d, want %d (retry continues)", life.calls, callsBefore+1)
	}
	if alloc.releases != 1 {
		t.Fatalf("pod released %d times, want exactly 1 (no re-release)", alloc.releases)
	}
}

func TestListBattles(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)

	allocateReady(t, uc, repo, 1, []uint64{10}, 1, "5v5_ranked")
	allocateReady(t, uc, repo, 2, []uint64{20}, 1, "5v5_ranked")

	all, err := uc.ListBattles(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("list all = %d, want 2", len(all))
	}

	// 状态过滤:等到 ready 心跳后两局都是 running,ready 无
	running, _ := uc.ListBattles(ctx, "running")
	if len(running) != 2 {
		t.Fatalf("list running = %d, want 2", len(running))
	}
	ready, _ := uc.ListBattles(ctx, "ready")
	if len(ready) != 0 {
		t.Fatalf("list ready = %d, want 0", len(ready))
	}
}

func TestSweepMarksAbandoned(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)

	allocateReady(t, uc, repo, 7, []uint64{10}, 1, "5v5_ranked")
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

// TestSweepEndedReclaimsLocalDS:local 模式(killOrphanOnStop=true)下,正常结算(ended)且失联的 DS
// 被扫到时,除了移出 active 还必须主动 Release——battle_result 不再直杀,DS 发完 ended 心跳即停心跳
// (无第二跳触发终态 kill),local Agones Shutdown 又是 no-op 不自退,不在此兜底 taskkill 就会幽灵占端口。
func TestSweepEndedReclaimsLocalDS(t *testing.T) {
	ctx := context.Background()
	rec := &recordingAllocator{inner: NewMockGameServerAllocator(testCfg()), released: make(chan string, 1)}
	uc, repo, _ := newUsecaseWithAlloc(t, rec)
	uc.SetKillOrphanOnStop(true)

	res := allocateReady(t, uc, repo, 8, []uint64{11, 22}, 1, "5v5_ranked")
	// 上报一次 ended 心跳:state → ended(首跳不 kill),仍留在 active 待扫描收尾。
	if _, err := uc.Heartbeat(ctx, 8, res.DSPodName, 0, "ended", time.Now().UnixMilli()); err != nil {
		t.Fatalf("ended heartbeat: %v", err)
	}
	backdate(t, repo, 8) // 心跳超时,进入 sweep 扫描窗口

	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	select {
	case pod := <-rec.released:
		if pod != res.DSPodName {
			t.Fatalf("released pod = %q, want %q", pod, res.DSPodName)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected ended DS to be reclaimed on sweep (local ghost-process leak)")
	}
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 0 {
		t.Fatalf("active should be empty after ended sweep, got %v", ids)
	}
}

// TestSweepEndedNoReclaimOnAgones:Agones 模式(killOrphanOnStop=false)下,ended 对局扫到只移出
// active,绝不主动 Release——DS 已自身 Agones Shutdown,pod 回收交 Fleet 生命周期,后端不越权 kill。
func TestSweepEndedNoReclaimOnAgones(t *testing.T) {
	ctx := context.Background()
	rec := &recordingAllocator{inner: NewMockGameServerAllocator(testCfg()), released: make(chan string, 1)}
	uc, repo, _ := newUsecaseWithAlloc(t, rec)
	// 不调 SetKillOrphanOnStop → 默认 false(Agones 模式)

	res := allocateReady(t, uc, repo, 9, []uint64{33, 44}, 1, "5v5_ranked")
	if _, err := uc.Heartbeat(ctx, 9, res.DSPodName, 0, "ended", time.Now().UnixMilli()); err != nil {
		t.Fatalf("ended heartbeat: %v", err)
	}
	backdate(t, repo, 9)

	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	select {
	case pod := <-rec.released:
		t.Fatalf("must NOT release ended DS on Agones (killOrphanOnStop off), got %q", pod)
	case <-time.After(200 * time.Millisecond):
		// 期望:无 Release 发生
	}
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 0 {
		t.Fatalf("active should be empty after ended sweep, got %v", ids)
	}
}

// TestSweepDeliversAbandonedFirstTry:配置 kafka 且首次投递成功 → 发 1 次事件、移出 active、回收 1 次。
func TestSweepDeliversAbandonedFirstTry(t *testing.T) {
	ctx := context.Background()
	alloc := &countingAllocator{inner: NewMockGameServerAllocator(testCfg())}
	uc, repo, _ := newUsecaseWithAlloc(t, alloc)
	life := &mockLifecycle{}
	uc.SetLifecyclePusher(life)

	allocateReady(t, uc, repo, 5, []uint64{1, 2}, 1, "5v5_ranked")
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

	allocateReady(t, uc, repo, 7, []uint64{10, 20}, 1, "5v5_ranked")
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
}

// ── 空场超时兜底(2026-07-06,全员掉线/从未连入的 DS 防空转)──────────────────────

// backdateEmptySince 把 EmptySinceMs 回拨到远古,模拟空场已持续超过 EmptyBattleTimeout。
func backdateEmptySince(t *testing.T, repo *data.RedisBattleRepo, matchID uint64) {
	t.Helper()
	if err := repo.UpdateBattleWithLock(context.Background(), matchID, 3, func(b *dsv1.BattleStorageRecord) error {
		b.EmptySinceMs = 1
		return nil
	}, 2*time.Hour); err != nil {
		t.Fatalf("backdate empty_since: %v", err)
	}
}

// TestHeartbeatEmptyBattleTimeout:running 对局 player_count==0 持续超 EmptyBattleTimeout →
// 心跳内判 abandoned + 回 stop + 回收 pod + 投递补偿事件 + 移出 active(空场兜底)。
func TestHeartbeatEmptyBattleTimeout(t *testing.T) {
	ctx := context.Background()
	alloc := &countingAllocator{inner: NewMockGameServerAllocator(testCfg())}
	uc, repo, _ := newUsecaseWithAlloc(t, alloc)
	life := &mockLifecycle{}
	uc.SetLifecyclePusher(life)

	res := allocateReady(t, uc, repo, 7, []uint64{10, 20}, 1, "5v5_ranked")

	// 第一跳空场:只盖 EmptySinceMs 起计时,不判弃
	hb, err := uc.Heartbeat(ctx, 7, res.DSPodName, 0, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if hb.Command != commandNone {
		t.Fatalf("command = %q, want none (first empty beat only starts timer)", hb.Command)
	}
	got, _, _ := repo.GetBattle(ctx, 7)
	if got.EmptySinceMs == 0 {
		t.Fatal("EmptySinceMs should be set on first empty heartbeat")
	}
	if got.State != stateRunning {
		t.Fatalf("state = %q, want still running", got.State)
	}

	// 空场持续超时(回拨 EmptySinceMs)→ 下一跳判 abandoned
	backdateEmptySince(t, repo, 7)
	hb2, err := uc.Heartbeat(ctx, 7, res.DSPodName, 0, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat2: %v", err)
	}
	if hb2.Command != commandStop {
		t.Fatalf("command = %q, want stop", hb2.Command)
	}
	got2, _, _ := repo.GetBattle(ctx, 7)
	if got2.State != stateAbandoned {
		t.Fatalf("state = %q, want abandoned", got2.State)
	}
	if alloc.releases != 1 {
		t.Fatalf("releases = %d, want 1", alloc.releases)
	}
	if len(life.delivered) != 1 || life.delivered[0] != 7 {
		t.Fatalf("delivered = %v, want [7] (段位回滚补偿事件)", life.delivered)
	}
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 0 {
		t.Fatalf("active = %v, want empty after empty-timeout abandon", ids)
	}
}

// TestHeartbeatEmptyResetWhenPlayersReturn:空场计时后有人重连回来 → EmptySinceMs 清零,不判弃
// (全员短暂掉线正在重连的局绝不能被误杀,阈值语义是「持续空场」)。
func TestHeartbeatEmptyResetWhenPlayersReturn(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)

	res := allocateReady(t, uc, repo, 7, []uint64{10, 20}, 1, "5v5_ranked")
	if _, err := uc.Heartbeat(ctx, 7, res.DSPodName, 0, "running", time.Now().UnixMilli()); err != nil {
		t.Fatalf("empty heartbeat: %v", err)
	}
	if got, _, _ := repo.GetBattle(ctx, 7); got.EmptySinceMs == 0 {
		t.Fatal("EmptySinceMs should be set")
	}
	// 玩家重连回来 → 清零
	if _, err := uc.Heartbeat(ctx, 7, res.DSPodName, 2, "running", time.Now().UnixMilli()); err != nil {
		t.Fatalf("rejoin heartbeat: %v", err)
	}
	got, _, _ := repo.GetBattle(ctx, 7)
	if got.EmptySinceMs != 0 {
		t.Fatalf("EmptySinceMs = %d, want 0 after players return", got.EmptySinceMs)
	}
	if got.State != stateRunning {
		t.Fatalf("state = %q, want running", got.State)
	}
}

// TestHeartbeatEmptyTimeoutDisabled:EmptyBattleTimeout 配负值 → 空场只计时不判弃(显式禁用)。
func TestHeartbeatEmptyTimeoutDisabled(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)
	cfg := testCfg()
	cfg.EmptyBattleTimeout = config.Duration(-1) // 显式禁用
	ucDisabled := NewAllocatorUsecase(repo, NewMockGameServerAllocator(cfg), cfg)

	res := allocateReady(t, uc, repo, 7, []uint64{10, 20}, 1, "5v5_ranked")
	if _, err := ucDisabled.Heartbeat(ctx, 7, res.DSPodName, 0, "running", time.Now().UnixMilli()); err != nil {
		t.Fatalf("empty heartbeat: %v", err)
	}
	backdateEmptySince(t, repo, 7)
	hb, err := ucDisabled.Heartbeat(ctx, 7, res.DSPodName, 0, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if hb.Command != commandNone {
		t.Fatalf("command = %q, want none (empty timeout disabled)", hb.Command)
	}
	if got, _, _ := repo.GetBattle(ctx, 7); got.State != stateRunning {
		t.Fatalf("state = %q, want running (disabled must not abandon)", got.State)
	}
}

// TestHeartbeatEmptyTimeoutDeliveryRetry:空场判弃时 Kafka 不可用 → 保留在 active;
// 后续 sweep 以 firstAbandon=false 路径重试投递(不重复回收 pod),闭环同心跳超时补偿(不变量 §4)。
func TestHeartbeatEmptyTimeoutDeliveryRetry(t *testing.T) {
	ctx := context.Background()
	alloc := &countingAllocator{inner: NewMockGameServerAllocator(testCfg())}
	uc, repo, _ := newUsecaseWithAlloc(t, alloc)
	life := &mockLifecycle{failFirst: 1} // 心跳内首投失败,sweep 重试成功
	uc.SetLifecyclePusher(life)

	res := allocateReady(t, uc, repo, 7, []uint64{10, 20}, 1, "5v5_ranked")
	if _, err := uc.Heartbeat(ctx, 7, res.DSPodName, 0, "running", time.Now().UnixMilli()); err != nil {
		t.Fatalf("empty heartbeat: %v", err)
	}
	backdateEmptySince(t, repo, 7)
	hb, err := uc.Heartbeat(ctx, 7, res.DSPodName, 0, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if hb.Command != commandStop {
		t.Fatalf("command = %q, want stop", hb.Command)
	}
	// 投递失败 → 保留在 active 等 sweep 重试
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 1 {
		t.Fatalf("active = %v, want still 1 (delivery retry pending)", ids)
	}

	// DS 收 stop 停跳 → 心跳超时后 sweep 扫到,firstAbandon=false 路径重试投递成功
	backdate(t, repo, 7)
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 0 {
		t.Fatalf("active = %v, want empty after retry delivered", ids)
	}
	if alloc.releases != 1 {
		t.Fatalf("releases = %d, want exactly 1 (no re-release on retry)", alloc.releases)
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

	allocateReady(t, uc, repo, 7, []uint64{10, 20}, 1, "5v5_ranked")
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

// ---- 断线重连:心跳续期 BATTLE 位置(docs/design/battle-reconnect.md)----

type refreshCall struct {
	players []uint64
	matchID uint64
	dsAddr  string
}

// fakeRefresher 记录 RefreshBattleLocations 调用(异步续期,用带缓冲 channel 接收)。
type fakeRefresher struct {
	calls chan refreshCall
}

func newFakeRefresher() *fakeRefresher { return &fakeRefresher{calls: make(chan refreshCall, 8)} }

func (f *fakeRefresher) RefreshBattleLocations(_ context.Context, players []uint64, matchID uint64, dsAddr string) error {
	cp := append([]uint64(nil), players...)
	f.calls <- refreshCall{players: cp, matchID: matchID, dsAddr: dsAddr}
	return nil
}

// TestHeartbeatRefreshesBattleLocation 验证:running 心跳后异步给在场玩家续期 BATTLE 位置,
// 携带正确的 match_id / ds_addr / 玩家列表(让登录侧能在整局内识别重连)。
func TestHeartbeatRefreshesBattleLocation(t *testing.T) {
	ctx := context.Background()
	uc, repo, _ := newUsecaseWithAlloc(t, NewMockGameServerAllocator(testCfg()))
	matchID := uint64(555)
	players := []uint64{1, 2, 3}
	res := allocateReady(t, uc, repo, matchID, players, 10, "5v5_ranked")

	fr := newFakeRefresher()
	uc.SetLocationRefresher(fr)

	b, found, err := repo.GetBattle(ctx, matchID)
	if err != nil || !found {
		t.Fatalf("get battle: err=%v found=%v", err, found)
	}
	if _, err := uc.Heartbeat(ctx, matchID, b.DsPodName, int32(len(players)), "running", time.Now().UnixMilli()); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	select {
	case c := <-fr.calls:
		if c.matchID != matchID {
			t.Errorf("refresh matchID = %d, want %d", c.matchID, matchID)
		}
		if c.dsAddr != res.DSAddr {
			t.Errorf("refresh dsAddr = %q, want %q", c.dsAddr, res.DSAddr)
		}
		if len(c.players) != len(players) {
			t.Errorf("refresh players = %v, want %v", c.players, players)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("running heartbeat did not trigger battle location refresh")
	}
}

// TestHeartbeatNoRefreshWhenNilRefresher 验证:未注入 refresher(dev / 未配 locator_addr)时,
// running 心跳不 panic、正常返回(弱依赖降级)。
func TestHeartbeatNoRefreshWhenNilRefresher(t *testing.T) {
	ctx := context.Background()
	uc, repo, _ := newUsecaseWithAlloc(t, NewMockGameServerAllocator(testCfg()))
	matchID := uint64(556)
	players := []uint64{1, 2}
	allocateReady(t, uc, repo, matchID, players, 10, "5v5_ranked") // 未 SetLocationRefresher

	b, _, _ := repo.GetBattle(ctx, matchID)
	if _, err := uc.Heartbeat(ctx, matchID, b.DsPodName, int32(len(players)), "running", time.Now().UnixMilli()); err != nil {
		t.Fatalf("heartbeat with nil refresher should not fail: %v", err)
	}
}
