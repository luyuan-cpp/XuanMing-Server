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
}

func newMockLocator() *mockLocator {
	return &mockLocator{matching: map[uint64]uint64{}, battle: map[uint64]string{}, inBattle: map[uint64]bool{}}
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

	if _, err := f.uc.StartMatch(ctx, 7001, 7001, captain); err == nil {
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

	if _, err := f.uc.StartMatch(ctx, 7002, 7002, captain); err == nil {
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

	id, err := f.uc.StartMatch(ctx, 7003, 7003, captain)
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

// 确认期超时 → expireOnce 标记 FAILED,票据退回队列。
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
	// 无明确拒绝者(rejecterID=0)→ 两张票均退回队列
	left, _ := f.repo.RangeQueueTickets(ctx)
	if len(left) != 2 {
		t.Fatalf("queue = %v, want 2 tickets requeued", left)
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

	// match 不应被创建(预留失败发生在 CreateMatch 之前)
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
