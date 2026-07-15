// match_test.go — matchmaker data 层 Redis 实现测试(miniredis)。
package data

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"
)

func TestDurableStartOperationAndDiscoveryIndexDoNotExpireAtTicketTTL(t *testing.T) {
	ctx := context.Background()
	repo, mr := newRepo(t)
	op := &matchv1.MatchStartOperationStorageRecord{
		OperationId: "9849ab5b-2ecf-4fc3-983d-2d8df53cc009",
		TicketId:    701, TeamId: 701, CaptainId: 42,
		Members: []*matchv1.MatchMemberStorageRecord{{PlayerId: 42, TeamId: 701}},
		Phase:   matchv1.MatchStartPhase_MATCH_START_PHASE_ACCEPTED,
	}
	if err := repo.CreateStartOperation(ctx, op, time.Second); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(2 * time.Second)
	if _, found, err := repo.GetStartOperation(ctx, 701); err != nil || !found {
		t.Fatalf("nonterminal start operation expired: found=%v err=%v", found, err)
	}
	if ticketID, found, err := repo.GetStartPlayerOperation(ctx, 42); err != nil || !found || ticketID != 701 {
		t.Fatalf("start discovery index expired: ticket=%d found=%v err=%v", ticketID, found, err)
	}
}

func TestCreateStartOperationNeverReturnsBusinessRejectAfterCanonicalAccept(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	if existing, claimed, err := repo.ClaimStartPlayer(ctx, 42, 700, time.Minute); err != nil || !claimed || existing != 700 {
		t.Fatalf("seed competing start claim: existing=%d claimed=%v err=%v", existing, claimed, err)
	}
	op := &matchv1.MatchStartOperationStorageRecord{
		OperationId: "9849ab5b-2ecf-4fc3-983d-2d8df53cc010",
		TicketId:    701, TeamId: 701, CaptainId: 42,
		Members:         []*matchv1.MatchMemberStorageRecord{{PlayerId: 42, TeamId: 701}},
		Phase:           matchv1.MatchStartPhase_MATCH_START_PHASE_ACCEPTED,
		NextAttemptAtMs: time.Now().UnixMilli(),
	}
	if err := repo.CreateStartOperation(ctx, op, time.Minute); err != nil {
		t.Fatalf("canonical ACCEPTED was reported as rejected: %v", err)
	}
	stored, found, err := repo.GetStartOperation(ctx, 701)
	if err != nil || !found || stored.GetPhase() != matchv1.MatchStartPhase_MATCH_START_PHASE_ACCEPTED {
		t.Fatalf("accepted operation missing/drifted: found=%v op=%+v err=%v", found, stored, err)
	}
	if existing, found, err := repo.GetStartPlayerOperation(ctx, 42); err != nil || !found || existing != 700 {
		t.Fatalf("competing owner was overwritten: existing=%d found=%v err=%v", existing, found, err)
	}
	active, err := repo.RangeDueStartOperations(ctx, time.Now().Add(time.Minute).UnixMilli())
	if err != nil || len(active) != 1 || active[0] != 701 {
		t.Fatalf("accepted conflicting operation not scheduled for compensation: active=%v err=%v", active, err)
	}
}

func TestCanonicalMatchDoesNotExpireUntilExplicitTerminalCleanup(t *testing.T) {
	ctx := context.Background()
	repo, mr := newRepo(t)
	m := &matchv1.MatchStorageRecord{
		MatchId: 801, Stage: matchv1.MatchStage_MATCH_STAGE_CONFIRM,
		ConfirmDeadlineMs: time.Now().Add(time.Minute).UnixMilli(),
		Members:           []*matchv1.MatchMemberStorageRecord{{PlayerId: 42}},
	}
	if err := repo.CreateMatch(ctx, m, time.Second); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(2 * time.Second)
	if _, found, err := repo.GetMatch(ctx, 801); err != nil || !found {
		t.Fatalf("nonterminal match expired: found=%v err=%v", found, err)
	}
	if err := repo.UpdateMatchWithLock(ctx, 801, 1, func(rec *matchv1.MatchStorageRecord) error {
		rec.Stage = matchv1.MatchStage_MATCH_STAGE_READY
		return nil
	}, time.Second); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(2 * time.Second)
	if _, found, err := repo.GetMatch(ctx, 801); err != nil || !found {
		t.Fatalf("READY match expired: found=%v err=%v", found, err)
	}
	if err := repo.ExpireMatch(ctx, 801, time.Second); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(2 * time.Second)
	if _, found, err := repo.GetMatch(ctx, 801); err != nil || found {
		t.Fatalf("explicitly expired terminal match remains: found=%v err=%v", found, err)
	}
}

func TestReservedTicketAndClaimDoNotExpireDuringLongBattle(t *testing.T) {
	ctx := context.Background()
	repo, mr := newRepo(t)
	ticket := &matchv1.MatchTicketStorageRecord{TicketId: 901, MatchId: 9901,
		Members: []*matchv1.MatchMemberStorageRecord{{PlayerId: 77}}}
	queued := proto.Clone(ticket).(*matchv1.MatchTicketStorageRecord)
	queued.MatchId = 0
	if err := repo.AddTicket(ctx, queued, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := repo.ClaimPlayer(ctx, 77, 901, time.Second); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	if err := repo.ReserveTicket(ctx, ticket, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := repo.PersistPlayerClaim(ctx, 77, 901); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(2 * time.Second)
	if got, found, err := repo.GetTicket(ctx, 901); err != nil || !found || got.GetMatchId() != 9901 {
		t.Fatalf("reserved ticket expired: found=%v ticket=%+v err=%v", found, got, err)
	}
	if got, found, err := repo.GetPlayerTicket(ctx, 77); err != nil || !found || got != 901 {
		t.Fatalf("reserved claim expired: found=%v ticket=%d err=%v", found, got, err)
	}
}

func TestQueuedTicketAndClaimDoNotSilentlyExpire(t *testing.T) {
	ctx := context.Background()
	repo, mr := newRepo(t)
	ticket := &matchv1.MatchTicketStorageRecord{TicketId: 902,
		Members: []*matchv1.MatchMemberStorageRecord{{PlayerId: 78}}}
	if err := repo.AddTicket(ctx, ticket, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := repo.ClaimPlayer(ctx, 78, 902, time.Second); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	mr.FastForward(24 * time.Hour)
	if got, found, err := repo.GetTicket(ctx, 902); err != nil || !found || got.GetTicketId() != 902 {
		t.Fatalf("queued ticket expired: found=%v ticket=%+v err=%v", found, got, err)
	}
	if got, found, err := repo.GetPlayerTicket(ctx, 78); err != nil || !found || got != 902 {
		t.Fatalf("queued claim expired: found=%v ticket=%d err=%v", found, got, err)
	}
}

const testTTL = 30 * time.Minute

func newRepo(t *testing.T) (*RedisMatchRepo, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis run: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisMatchRepo(rdb, ""), mr
}

func TestTicketRoundtripAndQueueOrder(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	// 入队三张票,avg_mmr 乱序
	mmrs := map[uint64]int32{10: 1500, 20: 900, 30: 1200}
	for id, mmr := range mmrs {
		ticket := &matchv1.MatchTicketStorageRecord{
			TicketId: id, TeamId: id, AvgMmr: mmr, EnqueuedAtMs: 1,
			Members: []*matchv1.MatchMemberStorageRecord{{PlayerId: id, Mmr: mmr}},
		}
		if err := repo.AddTicket(ctx, ticket, testTTL); err != nil {
			t.Fatalf("add ticket %d: %v", id, err)
		}
	}

	// 读回校验
	got, found, err := repo.GetTicket(ctx, 30)
	if err != nil || !found {
		t.Fatalf("get ticket 30: found=%v err=%v", found, err)
	}
	if got.AvgMmr != 1200 || got.Members[0].PlayerId != 30 {
		t.Fatalf("ticket 30 mismatch: %+v", got)
	}

	// 队列按 avg_mmr 升序:20(900) < 30(1200) < 10(1500)
	order, err := repo.RangeQueueTickets(ctx)
	if err != nil {
		t.Fatalf("range queue: %v", err)
	}
	want := []uint64{20, 30, 10}
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Fatalf("queue order = %v, want %v", order, want)
	}

	// DeleteTicket 移出队列 + 删 record
	if err := repo.DeleteTicket(ctx, 20); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := repo.GetTicket(ctx, 20); found {
		t.Fatal("ticket 20 should be deleted")
	}
	order, _ = repo.RangeQueueTickets(ctx)
	if len(order) != 2 {
		t.Fatalf("queue len = %d, want 2", len(order))
	}
}

func TestClaimPlayerSETNX(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	if _, ok, err := repo.ClaimPlayer(ctx, 1, 100, testTTL); err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	// 同一玩家被另一票据声明 → 冲突,返回已存在票据
	existing, ok, err := repo.ClaimPlayer(ctx, 1, 200, testTTL)
	if err != nil {
		t.Fatalf("second claim err: %v", err)
	}
	if ok {
		t.Fatal("second claim should fail")
	}
	if existing != 100 {
		t.Fatalf("existing = %d, want 100", existing)
	}

	tid, found, err := repo.GetPlayerTicket(ctx, 1)
	if err != nil || !found || tid != 100 {
		t.Fatalf("get player ticket: tid=%d found=%v err=%v", tid, found, err)
	}

	if err := repo.DeletePlayerIndex(ctx, 1); err != nil {
		t.Fatalf("delete index: %v", err)
	}
	if _, found, _ := repo.GetPlayerTicket(ctx, 1); found {
		t.Fatal("player index should be gone")
	}
}

func TestDeletePlayerIndexIfMatches(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	if _, ok, err := repo.ClaimPlayer(ctx, 1, 100, testTTL); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	// 值不匹配(claim 已被新一局 200 替换的场景)→ 不删除
	if err := repo.DeletePlayerIndexIfMatches(ctx, 1, 999); err != nil {
		t.Fatalf("cas delete mismatch err: %v", err)
	}
	if tid, found, _ := repo.GetPlayerTicket(ctx, 1); !found || tid != 100 {
		t.Fatalf("claim should survive mismatch delete: tid=%d found=%v", tid, found)
	}

	// 值匹配 → 删除
	if err := repo.DeletePlayerIndexIfMatches(ctx, 1, 100); err != nil {
		t.Fatalf("cas delete match err: %v", err)
	}
	if _, found, _ := repo.GetPlayerTicket(ctx, 1); found {
		t.Fatal("claim should be deleted when ticketID matches")
	}

	// key 不存在时幂等不报错
	if err := repo.DeletePlayerIndexIfMatches(ctx, 1, 100); err != nil {
		t.Fatalf("cas delete on missing key err: %v", err)
	}
}

func TestUpdateMatchWithLock(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	match := &matchv1.MatchStorageRecord{
		MatchId: 999, Stage: matchv1.MatchStage_MATCH_STAGE_CONFIRM,
		ConfirmDeadlineMs: 5000,
		Members: []*matchv1.MatchMemberStorageRecord{
			{PlayerId: 1, Confirm: matchv1.MatchConfirmStatus_MATCH_CONFIRM_STATUS_PENDING},
		},
	}
	if err := repo.CreateMatch(ctx, match, testTTL); err != nil {
		t.Fatalf("create match: %v", err)
	}

	err := repo.UpdateMatchWithLock(ctx, 999, 3, func(m *matchv1.MatchStorageRecord) error {
		m.Members[0].Confirm = matchv1.MatchConfirmStatus_MATCH_CONFIRM_STATUS_ACCEPTED
		m.Stage = matchv1.MatchStage_MATCH_STAGE_ALLOCATING
		return nil
	}, testTTL)
	if err != nil {
		t.Fatalf("update with lock: %v", err)
	}

	got, found, _ := repo.GetMatch(ctx, 999)
	if !found {
		t.Fatal("match gone")
	}
	if got.Stage != matchv1.MatchStage_MATCH_STAGE_ALLOCATING {
		t.Fatalf("stage = %v, want ALLOCATING", got.Stage)
	}
	if got.Members[0].Confirm != matchv1.MatchConfirmStatus_MATCH_CONFIRM_STATUS_ACCEPTED {
		t.Fatalf("confirm not persisted: %v", got.Members[0].Confirm)
	}

	// fn 返回业务错误 → 透传,不写回
	wantErr := errcode.New(errcode.ErrMatchDeclined, "boom")
	if err := repo.UpdateMatchWithLock(ctx, 999, 3, func(m *matchv1.MatchStorageRecord) error {
		m.Stage = matchv1.MatchStage_MATCH_STAGE_READY
		return wantErr
	}, testTTL); err == nil {
		t.Fatal("expected error passthrough")
	}
	got, _, _ = repo.GetMatch(ctx, 999)
	if got.Stage != matchv1.MatchStage_MATCH_STAGE_ALLOCATING {
		t.Fatalf("stage changed despite error: %v", got.Stage)
	}
}

func TestActiveRangeAndExpire(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	// 两场:一场 deadline 在过去,一场在未来
	now := time.Now().UnixMilli()
	past := &matchv1.MatchStorageRecord{MatchId: 1, Stage: matchv1.MatchStage_MATCH_STAGE_CONFIRM, ConfirmDeadlineMs: now - 1000}
	future := &matchv1.MatchStorageRecord{MatchId: 2, Stage: matchv1.MatchStage_MATCH_STAGE_CONFIRM, ConfirmDeadlineMs: now + 60000}
	if err := repo.CreateMatch(ctx, past, testTTL); err != nil {
		t.Fatalf("create past: %v", err)
	}
	if err := repo.CreateMatch(ctx, future, testTTL); err != nil {
		t.Fatalf("create future: %v", err)
	}

	expired, err := repo.RangeExpiredMatches(ctx, now)
	if err != nil {
		t.Fatalf("range expired: %v", err)
	}
	if len(expired) != 1 || expired[0] != 1 {
		t.Fatalf("expired = %v, want [1]", expired)
	}

	// RemoveActive 后不再出现在超时扫描
	if err := repo.RemoveActive(ctx, 1); err != nil {
		t.Fatalf("remove active: %v", err)
	}
	expired, _ = repo.RangeExpiredMatches(ctx, now)
	if len(expired) != 0 {
		t.Fatalf("expired after remove = %v, want []", expired)
	}

	// ExpireMatch:match record 仍可查(终态保留),但移出 active
	if err := repo.ExpireMatch(ctx, 2, testTTL); err != nil {
		t.Fatalf("expire match 2: %v", err)
	}
	if _, found, _ := repo.GetMatch(ctx, 2); !found {
		t.Fatal("match 2 record should remain queryable")
	}
}

// TestGameModeNamespaceIsolatesQueueAndActive 验证:同一套 Redis 下,不同 game_mode 的
// queue / active 索引互相隔离(见 decision-revisit-matchmaker-single-writer.md §3.2),
// 而 ticket / match 记录本体由全局唯一 ID 定址、跨模式可见(不混入对方扫描)。
func TestGameModeNamespaceIsolatesQueueAndActive(t *testing.T) {
	ctx := context.Background()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis run: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	repoA := NewRedisMatchRepo(rdb, "5v5_ranked")
	repoB := NewRedisMatchRepo(rdb, "3v3_casual")

	// A 模式入队一张票
	ticket := &matchv1.MatchTicketStorageRecord{
		TicketId: 1001, TeamId: 1001, AvgMmr: 1000, EnqueuedAtMs: 1,
		Members: []*matchv1.MatchMemberStorageRecord{{PlayerId: 1001, Mmr: 1000}},
	}
	if err := repoA.AddTicket(ctx, ticket, testTTL); err != nil {
		t.Fatalf("A add ticket: %v", err)
	}

	// A 的 queue 能扫到,B 的 queue 扫不到(索引隔离)
	if order, _ := repoA.RangeQueueTickets(ctx); len(order) != 1 || order[0] != 1001 {
		t.Fatalf("A queue = %v, want [1001]", order)
	}
	if order, _ := repoB.RangeQueueTickets(ctx); len(order) != 0 {
		t.Fatalf("B queue = %v, want [] (isolated)", order)
	}

	// 记录本体全局可见:B 也能按全局 ticketID 读到 record
	if _, found, _ := repoB.GetTicket(ctx, 1001); !found {
		t.Fatal("ticket record should be globally addressable across modes")
	}

	// active 同理隔离
	now := time.Now().UnixMilli()
	match := &matchv1.MatchStorageRecord{MatchId: 2002, ConfirmDeadlineMs: now - 1}
	if err := repoA.CreateMatch(ctx, match, testTTL); err != nil {
		t.Fatalf("A create match: %v", err)
	}
	if expired, _ := repoA.RangeExpiredMatches(ctx, now); len(expired) != 1 || expired[0] != 2002 {
		t.Fatalf("A expired = %v, want [2002]", expired)
	}
	if expired, _ := repoB.RangeExpiredMatches(ctx, now); len(expired) != 0 {
		t.Fatalf("B expired = %v, want [] (isolated)", expired)
	}

	// 空 namespace 走旧全局 key,与带 namespace 的池互不干扰
	repoLegacy := NewRedisMatchRepo(rdb, "")
	if order, _ := repoLegacy.RangeQueueTickets(ctx); len(order) != 0 {
		t.Fatalf("legacy queue = %v, want [] (separate from namespaced)", order)
	}
}

func TestDeleteMatchRemovesRecordAndActive(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	now := time.Now().UnixMilli()
	match := &matchv1.MatchStorageRecord{
		MatchId:           3,
		Stage:             matchv1.MatchStage_MATCH_STAGE_CONFIRM,
		ConfirmDeadlineMs: now - 1000,
	}
	if err := repo.CreateMatch(ctx, match, testTTL); err != nil {
		t.Fatalf("create match: %v", err)
	}

	if err := repo.DeleteMatch(ctx, 3); err != nil {
		t.Fatalf("delete match: %v", err)
	}
	if _, found, _ := repo.GetMatch(ctx, 3); found {
		t.Fatal("match record should be deleted")
	}
	expired, err := repo.RangeExpiredMatches(ctx, now)
	if err != nil {
		t.Fatalf("range expired: %v", err)
	}
	if len(expired) != 0 {
		t.Fatalf("expired after delete = %v, want []", expired)
	}
}

// TestDeleteTicketIfUnmatched_CAS 验证 CancelMatch 与撮合循环 ReserveTicket 的竞态防护:
// 未撮合可删;已被预留(match_id!=0)拒删并返回其 match_id;不存在返回 (false, 0)。
func TestDeleteTicketIfUnmatched_CAS(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	seed := func(id, matchID uint64) {
		ticket := &matchv1.MatchTicketStorageRecord{
			TicketId: id, TeamId: id, MatchId: matchID, EnqueuedAtMs: 1,
			Members: []*matchv1.MatchMemberStorageRecord{{PlayerId: id}},
		}
		if err := repo.AddTicket(ctx, ticket, testTTL); err != nil {
			t.Fatalf("add ticket %d: %v", id, err)
		}
	}

	// 未撮合 → 删除成功,record + queue 均清
	seed(10, 0)
	deleted, mid, err := repo.DeleteTicketIfUnmatched(ctx, 10)
	if err != nil || !deleted || mid != 0 {
		t.Fatalf("unmatched delete: deleted=%v mid=%d err=%v", deleted, mid, err)
	}
	if _, found, _ := repo.GetTicket(ctx, 10); found {
		t.Fatal("ticket 10 should be deleted")
	}
	if order, _ := repo.RangeQueueTickets(ctx); len(order) != 0 {
		t.Fatalf("queue should be empty, got %v", order)
	}

	// 已被撮合(match_id=99)→ 拒删,返回占用 match
	seed(20, 99)
	deleted, mid, err = repo.DeleteTicketIfUnmatched(ctx, 20)
	if err != nil || deleted || mid != 99 {
		t.Fatalf("reserved delete: deleted=%v mid=%d err=%v", deleted, mid, err)
	}
	if _, found, _ := repo.GetTicket(ctx, 20); !found {
		t.Fatal("reserved ticket 20 must survive")
	}

	// 不存在 → (false, 0, nil)
	deleted, mid, err = repo.DeleteTicketIfUnmatched(ctx, 404)
	if err != nil || deleted || mid != 0 {
		t.Fatalf("missing delete: deleted=%v mid=%d err=%v", deleted, mid, err)
	}
}

// TestReserveTicket_CAS 验证预留 CAS:票据已被取消删除 → ErrMatchNotFound;
// 已被并发预留进其他 match → ErrMatchConcurrent;幂等重预留同一 match 放行。
func TestReserveTicket_CAS(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	ticket := &matchv1.MatchTicketStorageRecord{
		TicketId: 1, TeamId: 1, EnqueuedAtMs: 1,
		Members: []*matchv1.MatchMemberStorageRecord{{PlayerId: 1}},
	}
	if err := repo.AddTicket(ctx, ticket, testTTL); err != nil {
		t.Fatalf("add ticket: %v", err)
	}

	// 正常预留
	ticket.MatchId = 100
	if err := repo.ReserveTicket(ctx, ticket, testTTL); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if order, _ := repo.RangeQueueTickets(ctx); len(order) != 0 {
		t.Fatalf("queue should be empty after reserve, got %v", order)
	}
	// 幂等重预留同一 match → 放行
	if err := repo.ReserveTicket(ctx, ticket, testTTL); err != nil {
		t.Fatalf("idempotent re-reserve: %v", err)
	}
	// 另一 match 抢同一张票(leader 交接重叠)→ ErrMatchConcurrent
	other := &matchv1.MatchTicketStorageRecord{TicketId: 1, TeamId: 1, MatchId: 200,
		Members: []*matchv1.MatchMemberStorageRecord{{PlayerId: 1}}}
	if err := repo.ReserveTicket(ctx, other, testTTL); errcode.As(err) != errcode.ErrMatchConcurrent {
		t.Fatalf("conflict reserve err = %v, want ErrMatchConcurrent", err)
	}

	// 票据已被取消删除 → ErrMatchNotFound
	if err := repo.DeleteTicket(ctx, 1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := repo.ReserveTicket(ctx, ticket, testTTL); errcode.As(err) != errcode.ErrMatchNotFound {
		t.Fatalf("gone reserve err = %v, want ErrMatchNotFound", err)
	}
}

// TestRefreshPlayerClaim 验证滚动升级 claim 仅在仍指向本票据时移除 TTL。
func TestRefreshPlayerClaim(t *testing.T) {
	ctx := context.Background()
	repo, mr := newRepo(t)

	// Simulate a rolling-upgrade key created by the old TTL-backed writer.
	if err := repo.rdb.Set(ctx, playerKey(1), "100", time.Minute).Err(); err != nil {
		t.Fatalf("seed legacy claim: %v", err)
	}
	// 指向本票据 → durable，无 TTL。
	if err := repo.RefreshPlayerClaim(ctx, 1, 100, testTTL); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if ttl := mr.TTL(playerKey(1)); ttl != 0 {
		t.Fatalf("ttl = %v, want persistent", ttl)
	}
	// 指向别的票据 → 不动(防误续玩家新一局 claim)
	if err := repo.RefreshPlayerClaim(ctx, 1, 999, time.Hour); err != nil {
		t.Fatalf("refresh other: %v", err)
	}
	if ttl := mr.TTL(playerKey(1)); ttl != 0 {
		t.Fatalf("ttl = %v, wrong ticket mutated durable claim", ttl)
	}
}

func TestPersistPlayerClaimRejectsLostOwnership(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	if _, ok, err := repo.ClaimPlayer(ctx, 1, 100, time.Minute); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := repo.PersistPlayerClaim(ctx, 1, 999); errcode.As(err) != errcode.ErrMatchConcurrent {
		t.Fatalf("lost owner code=%v err=%v", errcode.As(err), err)
	}
}
