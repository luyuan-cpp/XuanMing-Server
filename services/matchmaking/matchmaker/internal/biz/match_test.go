// match_test.go — matchmaker biz 层撮合流水线测试(miniredis 真实跑通)。
package biz

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/cellroute"
	"github.com/luyuancpp/pandora/pkg/errcode"
	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"

	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/conf"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/data"
)

// ── 测试桩 ────────────────────────────────────────────────────────────────────

type mockPusher struct {
	mu     sync.Mutex
	events []*matchv1.MatchProgressEvent
}

func (m *mockPusher) PushMatchProgress(_ context.Context, _ uint64, to []uint64, payload []byte) (int, error) {
	var e matchv1.MatchProgressEvent
	if err := proto.Unmarshal(payload, &e); err == nil {
		m.mu.Lock()
		m.events = append(m.events, &e)
		m.mu.Unlock()
	}
	return len(to), nil
}

func (m *mockPusher) lastStageFor(playerID uint64) matchv1.MatchStage {
	m.mu.Lock()
	defer m.mu.Unlock()
	stage := matchv1.MatchStage_MATCH_STAGE_UNSPECIFIED
	for _, e := range m.events {
		if e.ToPlayerId == playerID && e.Progress != nil {
			stage = e.Progress.Stage
		}
	}
	return stage
}

// fakeIDGen 返回可预测的 match_id 序列。
type fakeIDGen struct {
	mu   sync.Mutex
	next uint64
}

func (f *fakeIDGen) Generate() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.next
	f.next++
	return id
}

// mockLocator 记录 matchmaker 上报的 MATCHING / BATTLE 状态，用于断言状态机串联。
type mockLocator struct {
	mu       sync.Mutex
	matching map[uint64]uint64 // playerID -> matchID
	battle   map[uint64]string // playerID -> battlePod
	inBattle map[uint64]bool   // playerID -> 强制 IsInBattle 返回值(拦截测试用)
	queryErr error             // 非 nil 时 IsInBattle 一律返回该错误(模拟 locator 抖动 / fail-closed 测试用)
	offline  map[uint64]bool   // playerID -> 强制 FindOfflinePlayers 判离线(成局前在线校验测试用)
}

func newMockLocator() *mockLocator {
	return &mockLocator{matching: map[uint64]uint64{}, battle: map[uint64]string{}, inBattle: map[uint64]bool{}, offline: map[uint64]bool{}}
}

func (m *mockLocator) NotifyMatching(_ context.Context, ids []uint64, matchID uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		m.matching[id] = matchID
	}
	return nil
}

func (m *mockLocator) NotifyBattle(_ context.Context, ids []uint64, matchID uint64, pod string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		m.battle[id] = pod
	}
	return nil
}

func (m *mockLocator) IsInBattle(_ context.Context, id uint64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.queryErr != nil {
		return false, m.queryErr
	}
	return m.inBattle[id], nil
}

func (m *mockLocator) FindOfflinePlayers(_ context.Context, ids []uint64) ([]uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.queryErr != nil {
		return nil, m.queryErr
	}
	var out []uint64
	for _, id := range ids {
		if m.offline[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

func (m *mockLocator) matchingOf(id uint64) (uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.matching[id]
	return v, ok
}

func (m *mockLocator) battleOf(id uint64) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.battle[id]
	return v, ok
}

// ── 测试夹具 ──────────────────────────────────────────────────────────────────

type fixture struct {
	repo    *data.RedisMatchRepo
	pusher  *mockPusher
	locator *mockLocator
	uc      *MatchUsecase
	cfg     conf.MatchConf
}

func newFixture(t *testing.T, firstMatchID uint64) *fixture {
	return newFixtureWith(t, firstMatchID, nil)
}

// newFixtureWith 与 newFixture 相同，但允许在构造 usecase 前修改 MatchConf（如打开 BattleGateFailOpen）。
func newFixtureWith(t *testing.T, firstMatchID uint64, mutate func(*conf.MatchConf)) *fixture {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis run: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	var c conf.Config
	c.Defaults()
	if mutate != nil {
		mutate(&c.Match)
	}
	repo := data.NewRedisMatchRepo(rdb, "")
	pusher := &mockPusher{}
	locator := newMockLocator()
	idGen := &fakeIDGen{next: firstMatchID}
	uc := NewMatchUsecase(repo, nil, pusher, NewStubDSAllocator("127.0.0.1:7777"), idGen, locator, c.Match)
	return &fixture{repo: repo, pusher: pusher, locator: locator, uc: uc, cfg: c.Match}
}

// seedTicket 写一张票据并声明其全体成员归属。
func (f *fixture) seedTicket(t *testing.T, ctx context.Context, ticketID uint64, playerIDs []uint64, avgMMR int32) {
	t.Helper()
	members := make([]*matchv1.MatchMemberStorageRecord, 0, len(playerIDs))
	for _, pid := range playerIDs {
		if _, ok, err := f.repo.ClaimPlayer(ctx, pid, ticketID, f.cfg.TicketTTL.Std()); err != nil || !ok {
			t.Fatalf("claim player %d: ok=%v err=%v", pid, ok, err)
		}
		members = append(members, &matchv1.MatchMemberStorageRecord{
			PlayerId: pid,
			TeamId:   ticketID,
			Mmr:      avgMMR,
			Confirm:  confirmPending,
		})
	}
	ticket := &matchv1.MatchTicketStorageRecord{
		TicketId:     ticketID,
		TeamId:       ticketID,
		CaptainId:    playerIDs[0],
		Members:      members,
		AvgMmr:       avgMMR,
		EnqueuedAtMs: time.Now().UnixMilli(),
	}
	if err := f.repo.AddTicket(ctx, ticket, f.cfg.TicketTTL.Std()); err != nil {
		t.Fatalf("add ticket %d: %v", ticketID, err)
	}
}

// ── 用例 ──────────────────────────────────────────────────────────────────────

// TestStartMatch_RejectsPlayerInBattle 验证本提交核心：队伍成员正处于 BATTLE 时，
// StartMatch 返回 ErrMatchInBattle，且不写票据 / 不声明 claim（战斗中禁止重复匹配，不变量 §1）。
func TestStartMatch_RejectsPlayerInBattle(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)

	const captain = uint64(42)
	f.locator.mu.Lock()
	f.locator.inBattle[captain] = true
	f.locator.mu.Unlock()

	if _, err := f.uc.StartMatch(ctx, 7001, 7001, captain, 0); err == nil {
		t.Fatalf("StartMatch: expected error, got nil")
	} else if code := errcode.As(err); code != errcode.ErrMatchInBattle {
		t.Fatalf("StartMatch code = %d, want ErrMatchInBattle(%d)", code, errcode.ErrMatchInBattle)
	}

	// 拦截必须发生在写入之前：既无 player claim，也无 ticket。
	if _, found, _ := f.repo.GetPlayerTicket(ctx, captain); found {
		t.Fatalf("player %d claim written despite in-battle rejection", captain)
	}
	if _, found, _ := f.repo.GetTicket(ctx, 7001); found {
		t.Fatalf("ticket written despite in-battle rejection")
	}
}

// TestStartMatch_FailClosedWhenLocatorUnavailable 验证 fail-closed 生产路径：
// player_locator 查询失败时（默认 BattleGateFailOpen=false），StartMatch 拒绝入队并返回
// ErrUnavailable，且不写票据 / claim，避免 locator 抖动叠加旧 claim 过期时绕过保护。
func TestStartMatch_FailClosedWhenLocatorUnavailable(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)

	const captain = uint64(43)
	f.locator.mu.Lock()
	f.locator.queryErr = errors.New("locator down")
	f.locator.mu.Unlock()

	if _, err := f.uc.StartMatch(ctx, 7002, 7002, captain, 0); err == nil {
		t.Fatalf("StartMatch: expected fail-closed error, got nil")
	} else if code := errcode.As(err); code != errcode.ErrUnavailable {
		t.Fatalf("StartMatch code = %d, want ErrUnavailable(%d)", code, errcode.ErrUnavailable)
	}

	if _, found, _ := f.repo.GetPlayerTicket(ctx, captain); found {
		t.Fatalf("player %d claim written despite fail-closed rejection", captain)
	}
	if _, found, _ := f.repo.GetTicket(ctx, 7002); found {
		t.Fatalf("ticket written despite fail-closed rejection")
	}
}

// TestStartMatch_FailOpenWhenLocatorUnavailable 验证 dev 弱依赖开关：
// 显式打开 BattleGateFailOpen 后，locator 查询失败仅告警并放行，票据 / claim 正常写入。
func TestStartMatch_FailOpenWhenLocatorUnavailable(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, 999, func(m *conf.MatchConf) { m.BattleGateFailOpen = true })

	const captain = uint64(44)
	f.locator.mu.Lock()
	f.locator.queryErr = errors.New("locator down")
	f.locator.mu.Unlock()

	id, err := f.uc.StartMatch(ctx, 7003, 7003, captain, 0)
	if err != nil {
		t.Fatalf("StartMatch fail-open: unexpected error: %v", err)
	}
	if id != 7003 {
		t.Fatalf("StartMatch returned ticket %d, want 7003", id)
	}
	if _, found, _ := f.repo.GetTicket(ctx, 7003); !found {
		t.Fatalf("ticket not written under fail-open")
	}
	if got, found, _ := f.repo.GetPlayerTicket(ctx, captain); !found || got != 7003 {
		t.Fatalf("player claim = %d found=%v, want ticket 7003", got, found)
	}
}

// 10 张单人票据 → matchOnce 凑成一场 5+5,进确认期。
func TestMatchOnce_FormsMatch(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)

	for i := uint64(1); i <= 10; i++ {
		f.seedTicket(t, ctx, 100+i, []uint64{i}, 1000)
	}

	if err := f.uc.matchOnce(ctx); err != nil {
		t.Fatalf("matchOnce: %v", err)
	}

	m, found, err := f.repo.GetMatch(ctx, 999)
	if err != nil || !found {
		t.Fatalf("get match 999: found=%v err=%v", found, err)
	}
	if m.Stage != stageConfirm {
		t.Fatalf("stage = %v, want CONFIRM", m.Stage)
	}
	if len(m.Members) != 10 {
		t.Fatalf("members = %d, want 10", len(m.Members))
	}
	var sideA, sideB int
	for _, mb := range m.Members {
		if mb.Side == 0 {
			sideA++
		} else {
			sideB++
		}
	}
	if sideA != 5 || sideB != 5 {
		t.Fatalf("sides = %d/%d, want 5/5", sideA, sideB)
	}
	// 队列票据应已预留(移出 queue)
	left, _ := f.repo.RangeQueueTickets(ctx)
	if len(left) != 0 {
		t.Fatalf("queue left = %d, want 0", len(left))
	}
}

// 全员确认 → match READY,带 ds 地址。
func TestConfirmMatch_AllAccept_Ready(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)
	for i := uint64(1); i <= 10; i++ {
		f.seedTicket(t, ctx, 100+i, []uint64{i}, 1000)
	}
	if err := f.uc.matchOnce(ctx); err != nil {
		t.Fatalf("matchOnce: %v", err)
	}

	for i := uint64(1); i <= 10; i++ {
		if err := f.uc.ConfirmMatch(ctx, i, 999, true); err != nil {
			t.Fatalf("confirm player %d: %v", i, err)
		}
	}

	m, found, _ := f.repo.GetMatch(ctx, 999)
	if !found {
		t.Fatal("match 999 gone")
	}
	if m.Stage != stageReady {
		t.Fatalf("stage = %v, want READY", m.Stage)
	}
	if m.BattleDsAddr == "" {
		t.Fatal("battle_ds_addr empty")
	}
	if got := f.pusher.lastStageFor(1); got != stageReady {
		t.Fatalf("player 1 last push stage = %v, want READY", got)
	}
}

func TestReleaseMatch_CleansReadyMatchState(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)
	for i := uint64(1); i <= 10; i++ {
		f.seedTicket(t, ctx, 100+i, []uint64{i}, 1000)
	}
	if err := f.uc.matchOnce(ctx); err != nil {
		t.Fatalf("matchOnce: %v", err)
	}
	for i := uint64(1); i <= 10; i++ {
		if err := f.uc.ConfirmMatch(ctx, i, 999, true); err != nil {
			t.Fatalf("confirm player %d: %v", i, err)
		}
	}

	if err := f.uc.ReleaseMatch(ctx, 999, nil); err != nil {
		t.Fatalf("ReleaseMatch: %v", err)
	}
	if _, found, err := f.repo.GetMatch(ctx, 999); err != nil || found {
		t.Fatalf("match after release: found=%v err=%v, want gone", found, err)
	}
	for i := uint64(1); i <= 10; i++ {
		ticketID := 100 + i
		if _, found, err := f.repo.GetTicket(ctx, ticketID); err != nil || found {
			t.Fatalf("ticket %d after release: found=%v err=%v, want gone", ticketID, found, err)
		}
		if got, found, err := f.repo.GetPlayerTicket(ctx, i); err != nil || found {
			t.Fatalf("player %d claim after release: ticket=%d found=%v err=%v, want gone", i, got, found, err)
		}
	}
}

func TestReleaseMatch_DoesNotDeleteNewClaim(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)
	for i := uint64(1); i <= 10; i++ {
		f.seedTicket(t, ctx, 100+i, []uint64{i}, 1000)
	}
	if err := f.uc.matchOnce(ctx); err != nil {
		t.Fatalf("matchOnce: %v", err)
	}
	for i := uint64(1); i <= 10; i++ {
		if err := f.uc.ConfirmMatch(ctx, i, 999, true); err != nil {
			t.Fatalf("confirm player %d: %v", i, err)
		}
	}

	// 模拟旧局释放与新一局入队竞态:player 1 已经拥有一张不属于旧 match 的新票据。
	if err := f.repo.DeletePlayerIndex(ctx, 1); err != nil {
		t.Fatalf("delete old player index: %v", err)
	}
	const newTicketID uint64 = 9001
	if _, ok, err := f.repo.ClaimPlayer(ctx, 1, newTicketID, f.cfg.TicketTTL.Std()); err != nil || !ok {
		t.Fatalf("claim new ticket: ok=%v err=%v", ok, err)
	}
	if err := f.repo.AddTicket(ctx, &matchv1.MatchTicketStorageRecord{
		TicketId:     newTicketID,
		TeamId:       newTicketID,
		CaptainId:    1,
		Members:      []*matchv1.MatchMemberStorageRecord{{PlayerId: 1, TeamId: newTicketID, Confirm: confirmPending}},
		AvgMmr:       1000,
		EnqueuedAtMs: time.Now().UnixMilli(),
	}, f.cfg.TicketTTL.Std()); err != nil {
		t.Fatalf("add new ticket: %v", err)
	}

	if err := f.uc.ReleaseMatch(ctx, 999, nil); err != nil {
		t.Fatalf("ReleaseMatch: %v", err)
	}
	got, found, err := f.repo.GetPlayerTicket(ctx, 1)
	if err != nil || !found || got != newTicketID {
		t.Fatalf("player 1 new claim after old release: ticket=%d found=%v err=%v, want %d", got, found, err, newTicketID)
	}
}

// 僵尸 claim 清理 / 回滚必须 CAS:claim 已指向新一局票据时,旧票据的清理路径不准误删。
func TestRollbackClaims_DoesNotDeleteNewClaim(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)

	// player 1 的 claim 指向新票据 300
	if _, ok, err := f.repo.ClaimPlayer(ctx, 1, 300, f.cfg.TicketTTL.Std()); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	// 旧票据 100 的回滚(模拟「读到旧 claim → 过期 → 新 claim 写入 → 回滚执行」竞态)
	f.uc.rollbackClaims(ctx, 100, []uint64{1})
	if got, found, _ := f.repo.GetPlayerTicket(ctx, 1); !found || got != 300 {
		t.Fatalf("new claim after stale rollback: ticket=%d found=%v, want 300", got, found)
	}
	// 本票据 300 的回滚 → 正常删除
	f.uc.rollbackClaims(ctx, 300, []uint64{1})
	if _, found, _ := f.repo.GetPlayerTicket(ctx, 1); found {
		t.Fatal("claim should be gone after matching rollback")
	}
}

// 撮合成局 → locator 上报全员 MATCHING(带 match_id);全员确认就绪 → 上报 BATTLE(带 ds_addr)。
func TestLocatorState_MatchingThenBattle(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)
	for i := uint64(1); i <= 10; i++ {
		f.seedTicket(t, ctx, 100+i, []uint64{i}, 1000)
	}

	// 成局:进确认期,全员应被标记 MATCHING(match_id=999)
	if err := f.uc.matchOnce(ctx); err != nil {
		t.Fatalf("matchOnce: %v", err)
	}
	for i := uint64(1); i <= 10; i++ {
		got, ok := f.locator.matchingOf(i)
		if !ok || got != 999 {
			t.Fatalf("player %d MATCHING match_id = %d ok=%v, want 999", i, got, ok)
		}
		// 此阶段尚未进战斗,不应有 BATTLE 上报
		if _, ok := f.locator.battleOf(i); ok {
			t.Fatalf("player %d unexpectedly BATTLE before confirm", i)
		}
	}

	// 全员确认 → READY,全员应被标记 BATTLE(battle_pod = ds_addr)
	for i := uint64(1); i <= 10; i++ {
		if err := f.uc.ConfirmMatch(ctx, i, 999, true); err != nil {
			t.Fatalf("confirm player %d: %v", i, err)
		}
	}
	m, _, _ := f.repo.GetMatch(ctx, 999)
	for i := uint64(1); i <= 10; i++ {
		pod, ok := f.locator.battleOf(i)
		if !ok || pod == "" {
			t.Fatalf("player %d BATTLE pod = %q ok=%v, want non-empty", i, pod, ok)
		}
		if pod != m.BattleDsAddr {
			t.Fatalf("player %d BATTLE pod = %q, want ds_addr %q", i, pod, m.BattleDsAddr)
		}
	}
}

// 任一玩家拒绝 → match FAILED,其余整队退回队列,拒绝者票据删除。
func TestConfirmMatch_Reject_FailsAndRequeues(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)
	// 两张五人票:ticket 100(player 1-5)、ticket 200(player 6-10)
	f.seedTicket(t, ctx, 100, []uint64{1, 2, 3, 4, 5}, 1000)
	f.seedTicket(t, ctx, 200, []uint64{6, 7, 8, 9, 10}, 1000)
	if err := f.uc.matchOnce(ctx); err != nil {
		t.Fatalf("matchOnce: %v", err)
	}

	// player 1(属 ticket 100)拒绝
	if err := f.uc.ConfirmMatch(ctx, 1, 999, false); err != nil {
		t.Fatalf("reject: %v", err)
	}

	m, found, _ := f.repo.GetMatch(ctx, 999)
	if !found || m.Stage != stageFailed {
		t.Fatalf("match stage = %v found=%v, want FAILED", m.GetStage(), found)
	}

	// ticket 200 应退回队列,ticket 100 应被删除
	left, _ := f.repo.RangeQueueTickets(ctx)
	if len(left) != 1 || left[0] != 200 {
		t.Fatalf("queue = %v, want [200]", left)
	}
	if _, found, _ := f.repo.GetTicket(ctx, 100); found {
		t.Fatal("rejecter ticket 100 should be deleted")
	}
	// 退回的票据保留排队时长(enqueued_at_ms 不为 0)且 match_id 清零
	rq, found, _ := f.repo.GetTicket(ctx, 200)
	if !found || rq.MatchId != 0 || rq.EnqueuedAtMs == 0 {
		t.Fatalf("requeued ticket bad: found=%v match_id=%d enq=%d", found, rq.GetMatchId(), rq.GetEnqueuedAtMs())
	}
}

// 成局最终门:全员确认后、分配 DS 前发现有人掉线(locator 判 OFFLINE)→
// match FAILED,掉线者所在票据删除,其余票据退回队列,不上报 BATTLE。
func TestConfirmMatch_OfflineMember_FailsAndRequeues(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, 999, func(c *conf.MatchConf) { c.LivenessGateEnabled = true })
	f.seedTicket(t, ctx, 100, []uint64{1, 2, 3, 4, 5}, 1000)
	f.seedTicket(t, ctx, 200, []uint64{6, 7, 8, 9, 10}, 1000)
	if err := f.uc.matchOnce(ctx); err != nil {
		t.Fatalf("matchOnce: %v", err)
	}

	// player 6(属 ticket 200)在确认期内掉线(断报 ≥30s,locator key 过期)
	f.locator.mu.Lock()
	f.locator.offline[6] = true
	f.locator.mu.Unlock()

	for i := uint64(1); i <= 10; i++ {
		if err := f.uc.ConfirmMatch(ctx, i, 999, true); err != nil {
			t.Fatalf("confirm player %d: %v", i, err)
		}
	}

	m, found, _ := f.repo.GetMatch(ctx, 999)
	if !found || m.Stage != stageFailed {
		t.Fatalf("match stage = %v found=%v, want FAILED", m.GetStage(), found)
	}
	// 掉线者所在 ticket 200 删除,无辜 ticket 100 退回队列
	if _, found, _ := f.repo.GetTicket(ctx, 200); found {
		t.Fatal("offline member ticket 200 should be deleted")
	}
	left, _ := f.repo.RangeQueueTickets(ctx)
	if len(left) != 1 || left[0] != 100 {
		t.Fatalf("queue = %v, want [100]", left)
	}
	// 不应有任何 BATTLE 上报(未进分配)
	for i := uint64(1); i <= 10; i++ {
		if _, ok := f.locator.battleOf(i); ok {
			t.Fatalf("player %d unexpectedly notified BATTLE for liveness-failed match", i)
		}
	}
}

// 成局最终门弱依赖:locator 查询失败 → 跳过在线校验,照常成局(不误杀正常对局)。
func TestConfirmMatch_LivenessQueryError_ProceedsReady(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, 999, func(c *conf.MatchConf) { c.LivenessGateEnabled = true })
	for i := uint64(1); i <= 10; i++ {
		f.seedTicket(t, ctx, 100+i, []uint64{i}, 1000)
	}
	if err := f.uc.matchOnce(ctx); err != nil {
		t.Fatalf("matchOnce: %v", err)
	}

	// locator 抖动:FindOfflinePlayers 一律报错 → 应跳过校验继续成局
	f.locator.mu.Lock()
	f.locator.queryErr = errcode.New(errcode.ErrInternal, "locator down")
	f.locator.mu.Unlock()

	for i := uint64(1); i <= 10; i++ {
		if err := f.uc.ConfirmMatch(ctx, i, 999, true); err != nil {
			t.Fatalf("confirm player %d: %v", i, err)
		}
	}
	m, found, _ := f.repo.GetMatch(ctx, 999)
	if !found || m.Stage != stageReady {
		t.Fatalf("match stage = %v found=%v, want READY (liveness check skipped on error)", m.GetStage(), found)
	}
}

// 队列在线扫除:掉线玩家的死票被主动删除并释放归属,在线票据不受影响。
func TestLivenessSweep_ReapsOfflineTickets(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, 999, func(c *conf.MatchConf) { c.LivenessGateEnabled = true })
	// ticket 100(player 1-5 组队,player 3 掉线)、ticket 200(player 6 单排,在线)
	f.seedTicket(t, ctx, 100, []uint64{1, 2, 3, 4, 5}, 1000)
	f.seedTicket(t, ctx, 200, []uint64{6}, 1000)

	f.locator.mu.Lock()
	f.locator.offline[3] = true
	f.locator.mu.Unlock()

	if err := f.uc.livenessSweepOnce(ctx); err != nil {
		t.Fatalf("livenessSweepOnce: %v", err)
	}

	// 死票 100 删除 + 全体成员归属释放(可立刻再排)
	if _, found, _ := f.repo.GetTicket(ctx, 100); found {
		t.Fatal("dead ticket 100 should be reaped")
	}
	for i := uint64(1); i <= 5; i++ {
		if _, found, _ := f.repo.GetPlayerTicket(ctx, i); found {
			t.Fatalf("player %d claim should be released after reap", i)
		}
	}
	// 在线票据 200 原样保留在队列
	left, _ := f.repo.RangeQueueTickets(ctx)
	if len(left) != 1 || left[0] != 200 {
		t.Fatalf("queue = %v, want [200]", left)
	}
	// 同票在线队友(player 1)收到 FAILED 推送
	if got := f.pusher.lastStageFor(1); got != stageFailed {
		t.Fatalf("player 1 last push stage = %v, want FAILED", got)
	}
}

// 队列在线扫除弱依赖:locator 查询失败 → 本轮不删任何票。
func TestLivenessSweep_QueryError_NoReap(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, 999, func(c *conf.MatchConf) { c.LivenessGateEnabled = true })
	f.seedTicket(t, ctx, 100, []uint64{1}, 1000)

	f.locator.mu.Lock()
	f.locator.offline[1] = true
	f.locator.queryErr = errcode.New(errcode.ErrInternal, "locator down")
	f.locator.mu.Unlock()

	if err := f.uc.livenessSweepOnce(ctx); err != nil {
		t.Fatalf("livenessSweepOnce: %v", err)
	}
	if _, found, _ := f.repo.GetTicket(ctx, 100); !found {
		t.Fatal("ticket must survive when locator query fails (weak dependency)")
	}
}

// 开关默认关闭(LivenessGateEnabled=false):Hub DS player_ids 心跳未联发前,
// locator 判离线不生效——队列扫除不删票,成局最终门放行照常 READY
// (否则旧 Hub DS 发空 player_ids 时在线玩家 30s 后会被误判离线、票据被扫)。
func TestLivenessGate_DisabledByDefault_NoOfflineJudgement(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999) // 默认配置:开关关闭
	f.seedTicket(t, ctx, 100, []uint64{1, 2, 3, 4, 5}, 1000)
	f.seedTicket(t, ctx, 200, []uint64{6, 7, 8, 9, 10}, 1000)

	// locator 把全员判离线(模拟旧 Hub DS 发空 player_ids → 位置过期)
	f.locator.mu.Lock()
	for i := uint64(1); i <= 10; i++ {
		f.locator.offline[i] = true
	}
	f.locator.mu.Unlock()

	// 队列扫除:不删任何票
	if err := f.uc.livenessSweepOnce(ctx); err != nil {
		t.Fatalf("livenessSweepOnce: %v", err)
	}
	if left, _ := f.repo.RangeQueueTickets(ctx); len(left) != 2 {
		t.Fatalf("queue = %v, want both tickets intact when gate disabled", left)
	}

	// 成局最终门:照常成局 READY
	if err := f.uc.matchOnce(ctx); err != nil {
		t.Fatalf("matchOnce: %v", err)
	}
	for i := uint64(1); i <= 10; i++ {
		if err := f.uc.ConfirmMatch(ctx, i, 999, true); err != nil {
			t.Fatalf("confirm player %d: %v", i, err)
		}
	}
	m, found, _ := f.repo.GetMatch(ctx, 999)
	if !found || m.Stage != stageReady {
		t.Fatalf("match stage = %v found=%v, want READY (liveness gate disabled)", m.GetStage(), found)
	}
}

// 确认期超时 → expireOnce 标记 FAILED;含未确认(AFK)成员的票据被删除并释放归属,
// 不退回队列(否则同一批人 + 同一挂机者会无限重凑同一场 → 再超时)。
func TestExpireOnce_Timeout_Fails(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)
	f.seedTicket(t, ctx, 100, []uint64{1, 2, 3, 4, 5}, 1000)
	f.seedTicket(t, ctx, 200, []uint64{6, 7, 8, 9, 10}, 1000)

	// 手动建一场确认期已超时的 match(deadline 在过去)
	ta, _, _ := f.repo.GetTicket(ctx, 100)
	tb, _, _ := f.repo.GetTicket(ctx, 200)
	members := make([]*matchv1.MatchMemberStorageRecord, 0, 10)
	for _, t := range ta.Members {
		members = append(members, &matchv1.MatchMemberStorageRecord{PlayerId: t.PlayerId, TeamId: t.TeamId, Side: 0, Confirm: confirmPending})
	}
	for _, t := range tb.Members {
		members = append(members, &matchv1.MatchMemberStorageRecord{PlayerId: t.PlayerId, TeamId: t.TeamId, Side: 1, Confirm: confirmPending})
	}
	now := time.Now().UnixMilli()
	match := &matchv1.MatchStorageRecord{
		MatchId:           999,
		Stage:             stageConfirm,
		Members:           members,
		TicketIds:         []uint64{100, 200},
		CreatedAtMs:       now - 60000,
		ConfirmDeadlineMs: now - 1000, // 已超时
	}
	// reserve 票据(写 match_id,移出 queue),模拟 formMatch 后状态
	ta.MatchId = 999
	tb.MatchId = 999
	_ = f.repo.ReserveTicket(ctx, ta, f.cfg.TicketTTL.Std())
	_ = f.repo.ReserveTicket(ctx, tb, f.cfg.TicketTTL.Std())
	if err := f.repo.CreateMatch(ctx, match, f.cfg.MatchTTL.Std()); err != nil {
		t.Fatalf("create match: %v", err)
	}

	if err := f.uc.expireOnce(ctx); err != nil {
		t.Fatalf("expireOnce: %v", err)
	}

	m, found, _ := f.repo.GetMatch(ctx, 999)
	if !found || m.Stage != stageFailed {
		t.Fatalf("stage = %v found=%v, want FAILED", m.GetStage(), found)
	}
	// 超时且全员未确认(PENDING)→ 两张票均判责:删票、释放归属、不退回队列
	left, _ := f.repo.RangeQueueTickets(ctx)
	if len(left) != 0 {
		t.Fatalf("queue = %v, want empty (AFK tickets dropped, not requeued)", left)
	}
	for _, tid := range []uint64{100, 200} {
		if _, found, _ := f.repo.GetTicket(ctx, tid); found {
			t.Fatalf("ticket %d should be deleted", tid)
		}
	}
	for pid := uint64(1); pid <= 10; pid++ {
		if _, ok, _ := f.repo.GetPlayerTicket(ctx, pid); ok {
			t.Fatalf("player %d claim should be released", pid)
		}
		if got := f.pusher.lastStageFor(pid); got != stageFailed {
			t.Fatalf("player %d last stage = %v, want FAILED", pid, got)
		}
	}
}

// 超时时已全员接受的票据退回队列(不能连坐),含未确认成员的票据判责删除。
func TestExpireOnce_Timeout_MixedConfirm_RequeuesInnocentDropsAFK(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)
	f.seedTicket(t, ctx, 100, []uint64{1, 2, 3, 4, 5}, 1000)
	f.seedTicket(t, ctx, 200, []uint64{6, 7, 8, 9, 10}, 1000)

	ta, _, _ := f.repo.GetTicket(ctx, 100)
	tb, _, _ := f.repo.GetTicket(ctx, 200)
	members := make([]*matchv1.MatchMemberStorageRecord, 0, 10)
	for _, m := range ta.Members { // ticket 100 全员已接受
		members = append(members, &matchv1.MatchMemberStorageRecord{PlayerId: m.PlayerId, TeamId: m.TeamId, Side: 0, Confirm: confirmAccepted})
	}
	for _, m := range tb.Members { // ticket 200 全员未确认(AFK)
		members = append(members, &matchv1.MatchMemberStorageRecord{PlayerId: m.PlayerId, TeamId: m.TeamId, Side: 1, Confirm: confirmPending})
	}
	now := time.Now().UnixMilli()
	match := &matchv1.MatchStorageRecord{
		MatchId:           999,
		Stage:             stageConfirm,
		Members:           members,
		TicketIds:         []uint64{100, 200},
		CreatedAtMs:       now - 60000,
		ConfirmDeadlineMs: now - 1000,
	}
	ta.MatchId = 999
	tb.MatchId = 999
	_ = f.repo.ReserveTicket(ctx, ta, f.cfg.TicketTTL.Std())
	_ = f.repo.ReserveTicket(ctx, tb, f.cfg.TicketTTL.Std())
	if err := f.repo.CreateMatch(ctx, match, f.cfg.MatchTTL.Std()); err != nil {
		t.Fatalf("create match: %v", err)
	}

	if err := f.uc.expireOnce(ctx); err != nil {
		t.Fatalf("expireOnce: %v", err)
	}

	// ticket 100(无过错)退回队列且 match_id 清零;ticket 200(AFK)被删除
	left, _ := f.repo.RangeQueueTickets(ctx)
	if len(left) != 1 || left[0] != 100 {
		t.Fatalf("queue = %v, want [100]", left)
	}
	rq, found, _ := f.repo.GetTicket(ctx, 100)
	if !found || rq.MatchId != 0 {
		t.Fatalf("requeued ticket bad: found=%v match_id=%d", found, rq.GetMatchId())
	}
	if _, found, _ := f.repo.GetTicket(ctx, 200); found {
		t.Fatal("AFK ticket 200 should be deleted")
	}
	for pid := uint64(6); pid <= 10; pid++ {
		if _, ok, _ := f.repo.GetPlayerTicket(ctx, pid); ok {
			t.Fatalf("AFK player %d claim should be released", pid)
		}
	}
	// 无过错方收到“已回到队列”补推
	if got := f.pusher.lastStageFor(1); got != stageQueueing {
		t.Fatalf("innocent player last stage = %v, want QUEUEING", got)
	}
}

// seedAllocatingMatch 造一场 ALLOCATING 阶段的 match(票据已预留、deadline 可指定)。
func seedAllocatingMatch(t *testing.T, ctx context.Context, f *fixture, matchID uint64, deadlineMs int64) {
	t.Helper()
	f.seedTicket(t, ctx, 100, []uint64{1, 2, 3, 4, 5}, 1000)
	f.seedTicket(t, ctx, 200, []uint64{6, 7, 8, 9, 10}, 1000)
	ta, _, _ := f.repo.GetTicket(ctx, 100)
	tb, _, _ := f.repo.GetTicket(ctx, 200)
	members := make([]*matchv1.MatchMemberStorageRecord, 0, 10)
	for _, m := range ta.Members {
		members = append(members, &matchv1.MatchMemberStorageRecord{PlayerId: m.PlayerId, TeamId: m.TeamId, Side: 0, Confirm: confirmAccepted})
	}
	for _, m := range tb.Members {
		members = append(members, &matchv1.MatchMemberStorageRecord{PlayerId: m.PlayerId, TeamId: m.TeamId, Side: 1, Confirm: confirmAccepted})
	}
	match := &matchv1.MatchStorageRecord{
		MatchId:           matchID,
		Stage:             stageAllocating,
		Members:           members,
		TicketIds:         []uint64{100, 200},
		CreatedAtMs:       deadlineMs - 15000,
		ConfirmDeadlineMs: deadlineMs,
	}
	ta.MatchId = matchID
	tb.MatchId = matchID
	_ = f.repo.ReserveTicket(ctx, ta, f.cfg.TicketTTL.Std())
	_ = f.repo.ReserveTicket(ctx, tb, f.cfg.TicketTTL.Std())
	if err := f.repo.CreateMatch(ctx, match, f.cfg.MatchTTL.Std()); err != nil {
		t.Fatalf("create match: %v", err)
	}
}

// ALLOCATING 在宽限期内 → expireOnce 不动它(留在 active 继续观察,不判失败)。
func TestExpireOnce_AllocatingWithinGrace_Kept(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)
	// deadline 刚过 1s,仍在 allocatingGrace(60s)内
	seedAllocatingMatch(t, ctx, f, 999, time.Now().UnixMilli()-1000)

	if err := f.uc.expireOnce(ctx); err != nil {
		t.Fatalf("expireOnce: %v", err)
	}
	m, found, _ := f.repo.GetMatch(ctx, 999)
	if !found || m.Stage != stageAllocating {
		t.Fatalf("stage = %v found=%v, want ALLOCATING kept", m.GetStage(), found)
	}
	// 仍在 active(留观),下一轮还能扫到
	expired, _ := f.repo.RangeExpiredMatches(ctx, time.Now().UnixMilli())
	if len(expired) != 1 || expired[0] != 999 {
		t.Fatalf("active = %v, want [999] kept for observation", expired)
	}
}

// ALLOCATING 超宽限期(分配副本崩溃)→ 判失败,票据退回队列,玩家不再卡死。
func TestExpireOnce_AllocatingBeyondGrace_Fails(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)
	// deadline 已过 61s > allocatingGrace(60s)
	seedAllocatingMatch(t, ctx, f, 999, time.Now().UnixMilli()-61_000)

	if err := f.uc.expireOnce(ctx); err != nil {
		t.Fatalf("expireOnce: %v", err)
	}
	m, found, _ := f.repo.GetMatch(ctx, 999)
	if !found || m.Stage != stageFailed {
		t.Fatalf("stage = %v found=%v, want FAILED", m.GetStage(), found)
	}
	left, _ := f.repo.RangeQueueTickets(ctx)
	if len(left) != 2 {
		t.Fatalf("queue = %v, want 2 tickets requeued", left)
	}
}

// 卡死回归:超宽限判失败后 set-ready 迟到(分配副本没死只是慢)→ stage 守卫拒绝
// FAILED→READY 回流,已退队票据不被"拉进战斗"。
func TestOnAllConfirmed_LateReady_DoesNotOverrideFailed(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)
	seedAllocatingMatch(t, ctx, f, 999, time.Now().UnixMilli()-61_000)
	if err := f.uc.expireOnce(ctx); err != nil {
		t.Fatalf("expireOnce: %v", err)
	}
	m, _, _ := f.repo.GetMatch(ctx, 999)

	// 迟到的 onAllConfirmed(拿的是失败前的 ALLOCATING 快照)
	stale := cloneMatch(m)
	stale.Stage = stageAllocating
	f.uc.onAllConfirmed(ctx, stale)

	got, found, _ := f.repo.GetMatch(ctx, 999)
	if !found || got.Stage != stageFailed {
		t.Fatalf("stage = %v, want FAILED preserved (no READY override)", got.GetStage())
	}
}

// CancelMatch 与撮合循环竞态:票据已被 ReserveTicket 抢先(match_id 已写)时,
// 排队取消路径必须转"拒绝确认"(match 失败),绝不盲删已进 match 的票据。
func TestCancelMatch_TicketJustReserved_TurnsIntoReject(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)
	f.seedTicket(t, ctx, 100, []uint64{1, 2, 3, 4, 5}, 1000)
	f.seedTicket(t, ctx, 200, []uint64{6, 7, 8, 9, 10}, 1000)
	if err := f.uc.matchOnce(ctx); err != nil {
		t.Fatalf("matchOnce: %v", err)
	}

	// player 1 取消:其票据已被撮进 match 999 → 应等价拒绝,match 失败、对方票退回队列
	if err := f.uc.CancelMatch(ctx, 1); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	m, found, _ := f.repo.GetMatch(ctx, 999)
	if !found || m.Stage != stageFailed {
		t.Fatalf("stage = %v found=%v, want FAILED", m.GetStage(), found)
	}
	left, _ := f.repo.RangeQueueTickets(ctx)
	if len(left) != 1 || left[0] != 200 {
		t.Fatalf("queue = %v, want [200]", left)
	}
}

// 排队路径取消(未撮合)后必须给票据全体成员补推 FAILED:取消可能不是本人发起
// (队长取消 / team 离队联动撤票),其余队友的客户端不能一直停在 QUEUEING。
func TestCancelMatch_QueuePath_PushesFailedToAllMembers(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)
	f.seedTicket(t, ctx, 100, []uint64{1, 2, 3, 4, 5}, 1000)

	if err := f.uc.CancelMatch(ctx, 1); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// 票据已删、队列已空
	if _, found, _ := f.repo.GetTicket(ctx, 100); found {
		t.Fatal("ticket should be deleted")
	}
	left, _ := f.repo.RangeQueueTickets(ctx)
	if len(left) != 0 {
		t.Fatalf("queue = %v, want empty", left)
	}
	// 全体成员(含发起者)收到 FAILED
	for pid := uint64(1); pid <= 5; pid++ {
		if got := f.pusher.lastStageFor(pid); got != stageFailed {
			t.Errorf("player %d last stage = %v, want FAILED", pid, got)
		}
	}
	// claim 已释放:全员可立即重新排队
	for pid := uint64(1); pid <= 5; pid++ {
		if _, ok, err := f.repo.ClaimPlayer(ctx, pid, 300, f.cfg.TicketTTL.Std()); err != nil || !ok {
			t.Fatalf("player %d should be claimable again: ok=%v err=%v", pid, ok, err)
		}
	}
}

// claim 指向已消失票据(崩溃残留)→ StartMatch 自愈清理后正常入队,不再卡 4002 半小时。
func TestStartMatch_HealsStaleClaim(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)

	const captain = uint64(50)
	// 残留形态:claim 活着但票据 8888 不存在(onMatchFailed 删票后、释放 claim 前崩溃)
	if _, ok, err := f.repo.ClaimPlayer(ctx, captain, 8888, f.cfg.TicketTTL.Std()); err != nil || !ok {
		t.Fatalf("seed stale claim: ok=%v err=%v", ok, err)
	}

	id, err := f.uc.StartMatch(ctx, 7010, 7010, captain, 0)
	if err != nil {
		t.Fatalf("StartMatch should heal stale claim: %v", err)
	}
	if id != 7010 {
		t.Fatalf("ticket = %d, want 7010", id)
	}
	if got, found, _ := f.repo.GetPlayerTicket(ctx, captain); !found || got != 7010 {
		t.Fatalf("claim = %d found=%v, want 7010", got, found)
	}
}

// claim 指向仍存活的票据 → StartMatch 绝不自愈误删,照常拒绝 4002(真占用)。
func TestStartMatch_LiveClaimStillRejected(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)
	f.seedTicket(t, ctx, 100, []uint64{60}, 1000)

	_, err := f.uc.StartMatch(ctx, 7011, 7011, 60, 0)
	if code := errcode.As(err); code != errcode.ErrMatchAlreadyMatching {
		t.Fatalf("code = %d, want ErrMatchAlreadyMatching(%d)", code, errcode.ErrMatchAlreadyMatching)
	}
	// 原票据与 claim 原样保留
	if got, found, _ := f.repo.GetPlayerTicket(ctx, 60); !found || got != 100 {
		t.Fatalf("claim = %d found=%v, want 100", got, found)
	}
	if _, found, _ := f.repo.GetTicket(ctx, 100); !found {
		t.Fatal("live ticket 100 must survive")
	}
}

// 票据 match_id 指向已不存在的 match(崩溃残留孤儿)→ CancelMatch 收割:删票 + 释放归属 + 推 FAILED。
func TestCancelMatch_OrphanTicket_CleansUp(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)
	f.seedTicket(t, ctx, 100, []uint64{1, 2, 3, 4, 5}, 1000)

	tk, _, _ := f.repo.GetTicket(ctx, 100)
	tk.MatchId = 4242 // match 4242 从未创建(回滚中途崩溃的残留形态)
	if err := f.repo.ReserveTicket(ctx, tk, f.cfg.TicketTTL.Std()); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	if err := f.uc.CancelMatch(ctx, 1); err != nil {
		t.Fatalf("cancel orphan ticket: %v", err)
	}

	if _, found, _ := f.repo.GetTicket(ctx, 100); found {
		t.Fatal("orphan ticket should be deleted")
	}
	for pid := uint64(1); pid <= 5; pid++ {
		if _, ok, _ := f.repo.GetPlayerTicket(ctx, pid); ok {
			t.Fatalf("player %d claim should be released", pid)
		}
		if got := f.pusher.lastStageFor(pid); got != stageFailed {
			t.Errorf("player %d last stage = %v, want FAILED", pid, got)
		}
	}
}

// ALLOCATING(全员已确认、分配中)阶段拒绝 → 诚实报错 ErrInvalidState,match 不受影响;
// accept 仍幂等成功。防止"取消成功却被拉进战斗"的假成功。
func TestConfirmMatch_RejectWhileAllocating_ReturnsError(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)
	seedAllocatingMatch(t, ctx, f, 999, time.Now().UnixMilli()+15000)

	err := f.uc.ConfirmMatch(ctx, 1, 999, false)
	if code := errcode.As(err); code != errcode.ErrInvalidState {
		t.Fatalf("reject while allocating: code = %d err=%v, want ErrInvalidState(%d)", code, err, errcode.ErrInvalidState)
	}
	m, found, _ := f.repo.GetMatch(ctx, 999)
	if !found || m.Stage != stageAllocating {
		t.Fatalf("stage = %v found=%v, want ALLOCATING unchanged", m.GetStage(), found)
	}
	if err := f.uc.ConfirmMatch(ctx, 1, 999, true); err != nil {
		t.Fatalf("accept while allocating should stay idempotent-success: %v", err)
	}
}

// ── ReserveTicket 失败一致性 ──────────────────────────────────────────────────

// faultyReserveRepo 包装真实 repo,在第 failOnCall 次 ReserveTicket 调用上注入失败,
// 用于验证 formMatch 中途预留失败时的补偿(退回队列、不留残缺 match)。
type faultyReserveRepo struct {
	data.MatchRepo
	calls      int
	failOnCall int // 第几次 ReserveTicket 调用返回错误(1-based);0 表示全部失败
}

func (r *faultyReserveRepo) ReserveTicket(ctx context.Context, ticket *matchv1.MatchTicketStorageRecord, ttl time.Duration) error {
	r.calls++
	if r.failOnCall == 0 || r.calls == r.failOnCall {
		return errors.New("injected reserve failure")
	}
	return r.MatchRepo.ReserveTicket(ctx, ticket, ttl)
}

// formMatch 预留到一半失败 → 已预留票据全部退回队列,不建 match(无悬空残留)。
func TestFormMatch_ReserveFailsMidway_RollsBackNoMatch(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)
	for i := uint64(1); i <= 10; i++ {
		f.seedTicket(t, ctx, 100+i, []uint64{i}, 1000)
	}

	// 第 2 次 ReserveTicket 失败:第 1 张已预留,应被回滚退回队列
	faulty := &faultyReserveRepo{MatchRepo: f.repo, failOnCall: 2}
	uc := NewMatchUsecase(faulty, nil, f.pusher, NewStubDSAllocator("127.0.0.1:7777"), &fakeIDGen{next: 999}, nil, f.cfg)

	sideA := make([]*matchv1.MatchTicketStorageRecord, 0, 5)
	sideB := make([]*matchv1.MatchTicketStorageRecord, 0, 5)
	for i := uint64(1); i <= 10; i++ {
		ticket, found, err := f.repo.GetTicket(ctx, 100+i)
		if err != nil || !found {
			t.Fatalf("get ticket %d: found=%v err=%v", 100+i, found, err)
		}
		if i <= 5 {
			sideA = append(sideA, ticket)
		} else {
			sideB = append(sideB, ticket)
		}
	}

	if err := uc.formMatch(ctx, sideA, sideB); err == nil {
		t.Fatal("formMatch should fail when ReserveTicket fails")
	}

	// match 已先建后回滚:预留失败时必须把 match 删干净(否则 expireOnce 会把
	// 已退回队列的票据当成本局成员重复处理)
	if _, found, _ := f.repo.GetMatch(ctx, 999); found {
		t.Fatal("match 999 should not exist after reserve failure")
	}
	// 全部 10 张票据应仍在队列(第 1 张回滚退回 + 其余从未预留)
	left, _ := f.repo.RangeQueueTickets(ctx)
	if len(left) != 10 {
		t.Fatalf("queue = %d tickets, want 10 (consistent, no orphan)", len(left))
	}
	// 每张票据 match_id 必须清零,否则下一轮会被当作已撮合跳过/或重复处理
	for i := uint64(1); i <= 10; i++ {
		ticket, found, _ := f.repo.GetTicket(ctx, 100+i)
		if !found {
			t.Fatalf("ticket %d gone", 100+i)
		}
		if ticket.MatchId != 0 {
			t.Fatalf("ticket %d match_id = %d, want 0", 100+i, ticket.MatchId)
		}
	}
}

// matchOnce 在 ReserveTicket 持续失败时不留"已建 match + 票据仍在队列"的不一致(防重复撮合)。
func TestMatchOnce_ReserveFails_NoOrphanMatch(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)
	for i := uint64(1); i <= 10; i++ {
		f.seedTicket(t, ctx, 100+i, []uint64{i}, 1000)
	}

	faulty := &faultyReserveRepo{MatchRepo: f.repo, failOnCall: 0} // 全部失败
	uc := NewMatchUsecase(faulty, nil, f.pusher, NewStubDSAllocator("127.0.0.1:7777"), &fakeIDGen{next: 999}, nil, f.cfg)

	if err := uc.matchOnce(ctx); err != nil {
		t.Fatalf("matchOnce should swallow form errors and continue: %v", err)
	}

	// 没有任何 match 被建出来
	if _, found, _ := f.repo.GetMatch(ctx, 999); found {
		t.Fatal("no match should be created when all reserves fail")
	}
	// 全部票据仍在队列,可被后续轮次正常重试
	left, _ := f.repo.RangeQueueTickets(ctx)
	if len(left) != 10 {
		t.Fatalf("queue = %d tickets, want 10 (all retryable)", len(left))
	}
}

// ── 两级撮合(region 感知)接线 ───────────────────────────────────────────────

// singleRegionRouter 构造一张把所有 logical_cell 都指向 (region, cell) 的路由器,
// 用于验证 region 感知主循环在"全员同 region"时与单桶行为一致(非回归)。
func singleRegionRouter(t *testing.T, region, cell uint32) *cellroute.Router {
	t.Helper()
	entries, regionOfCell, err := cellroute.BuildBalancedEntries([]cellroute.CellSpec{{RegionID: region, CellID: cell}})
	if err != nil {
		t.Fatalf("BuildBalancedEntries: %v", err)
	}
	tbl, err := cellroute.NewStaticTable(entries, regionOfCell)
	if err != nil {
		t.Fatalf("NewStaticTable: %v", err)
	}
	r, err := cellroute.NewRouter(tbl)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return r
}

// 设了 Router 且全员同 region 时,matchOnce 仍正常凑成一场 5+5(region 感知主循环非回归)。
func TestMatchOnce_RegionAware_SingleRegionFormsMatch(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 999)
	f.uc.SetCellRouter(singleRegionRouter(t, 3, 30)) // 所有玩家 → region 3

	for i := uint64(1); i <= 10; i++ {
		f.seedTicket(t, ctx, 100+i, []uint64{i}, 1000)
	}
	if err := f.uc.matchOnce(ctx); err != nil {
		t.Fatalf("matchOnce: %v", err)
	}

	m, found, err := f.repo.GetMatch(ctx, 999)
	if err != nil || !found {
		t.Fatalf("get match 999: found=%v err=%v", found, err)
	}
	if m.Stage != stageConfirm || len(m.Members) != 10 {
		t.Fatalf("stage=%v members=%d, want CONFIRM/10", m.Stage, len(m.Members))
	}
	left, _ := f.repo.RangeQueueTickets(ctx)
	if len(left) != 0 {
		t.Fatalf("queue left = %d, want 0", len(left))
	}
}

// ticketRegion 在 router 为 nil 时恒返回 0(单 Cell / dev 语义),不阻断撮合。
func TestTicketRegion_NilRouterZero(t *testing.T) {
	f := newFixture(t, 999)
	tk := &matchv1.MatchTicketStorageRecord{TicketId: 1, CaptainId: 12345}
	if r := f.uc.ticketRegion(tk); r != 0 {
		t.Fatalf("nil router ticketRegion = %d, want 0", r)
	}
}

// battlePlacement 在 router 为 nil 时返回 ok=false(单 Cell / dev:不带放置提示)。
func TestBattlePlacement_NilRouterNotOk(t *testing.T) {
	f := newFixture(t, 999)
	if _, ok := f.uc.battlePlacement([]uint64{1, 2, 3}); ok {
		t.Fatal("nil router battlePlacement should return ok=false")
	}
}

// battlePlacement 在所有玩家落同一 (region, cell) 时返回该落点(单 region 路由非回归)。
func TestBattlePlacement_SingleRegionAllAgree(t *testing.T) {
	f := newFixture(t, 999)
	f.uc.SetCellRouter(singleRegionRouter(t, 7, 70)) // 所有玩家 → region 7 / cell 70
	got, ok := f.uc.battlePlacement([]uint64{11, 22, 33, 44, 55})
	if !ok {
		t.Fatal("expected ok with router set")
	}
	if got.RegionID != 7 || got.CellID != 70 {
		t.Fatalf("placement = %+v, want {7,70}", got)
	}
}

// ticketTier 经 regionPolicy.MmrTier 把票据 avg_mmr 映射到段位档(默认策略:普通段 0、高分段更高)。
func TestTicketTier_FollowsPolicy(t *testing.T) {
	f := newFixture(t, 999) // 默认 DefaultRegionMatchPolicy
	low := &matchv1.MatchTicketStorageRecord{TicketId: 1, AvgMmr: 1500}
	high := &matchv1.MatchTicketStorageRecord{TicketId: 2, AvgMmr: 3300}
	if tr := f.uc.ticketTier(low); tr != 0 {
		t.Fatalf("low mmr tier = %d, want 0", tr)
	}
	if tr := f.uc.ticketTier(high); tr != 3 {
		t.Fatalf("high mmr tier = %d, want 3", tr)
	}
	if tr := f.uc.ticketTier(nil); tr != 0 {
		t.Fatalf("nil ticket tier = %d, want 0", tr)
	}
}
