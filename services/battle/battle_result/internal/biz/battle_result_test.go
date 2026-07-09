// battle_result_test.go — biz 层单测(W4 ③,2026-06-06)。
//
// 覆盖:
//   - Elo:等分对称(+K/2 / -K/2)、强队赢得少、平局对称、K 守恒
//   - ReportResult:MMR 赋值 + 幂等命中
//   - HandleAbandoned:补偿记录 outcome=ABANDONED + delta 全 0 + 幂等
//   - 输入校验
package biz

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	battlev1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/battle/v1"

	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/data"
)

// ── 测试替身 ──────────────────────────────────────────────────────────────────

// fakeRepo 是内存版 data.BattleRepo,按 match_id 唯一(模拟 unique 幂等)+内存出箱。
type fakeRepo struct {
	store      map[uint64]*battlev1.BattleResult
	saveErr    error
	saveCnt    int
	outbox     []data.OutboxRecord     // player.update 待发布,按 ID 升序
	dropOutbox []data.DropOutboxRecord // 装备掉落待发放,按 ID 升序(W5 ④)
	nextID     int64
	nextDropID int64
}

func newFakeRepo() *fakeRepo { return &fakeRepo{store: map[uint64]*battlev1.BattleResult{}} }

func (r *fakeRepo) SaveResult(_ context.Context, result *battlev1.BattleResult, outbox []data.OutboxRecord, dropOutbox []data.DropOutboxRecord) (bool, error) {
	r.saveCnt++
	if r.saveErr != nil {
		return false, r.saveErr
	}
	if _, ok := r.store[result.GetMatchId()]; ok {
		return true, nil // 幂等命中(两路出箱都不写)
	}
	r.store[result.GetMatchId()] = proto.Clone(result).(*battlev1.BattleResult)
	for _, o := range outbox {
		r.nextID++
		r.outbox = append(r.outbox, data.OutboxRecord{ID: r.nextID, PlayerID: o.PlayerID, Payload: o.Payload})
	}
	for _, d := range dropOutbox {
		if len(d.ItemConfigIDs) == 0 {
			continue
		}
		r.nextDropID++
		r.dropOutbox = append(r.dropOutbox, data.DropOutboxRecord{
			ID: r.nextDropID, MatchID: result.GetMatchId(), PlayerID: d.PlayerID,
			ItemConfigIDs: append([]uint32(nil), d.ItemConfigIDs...),
		})
	}
	return false, nil
}

func (r *fakeRepo) GetResult(_ context.Context, matchID uint64) (*battlev1.BattleResult, bool, error) {
	res, ok := r.store[matchID]
	if !ok {
		return nil, false, nil
	}
	return res, true, nil
}

func (r *fakeRepo) ListPlayerHistory(_ context.Context, _ uint64, _ int, _ int64) ([]*battlev1.BattleResult, error) {
	out := make([]*battlev1.BattleResult, 0, len(r.store))
	for _, v := range r.store {
		out = append(out, v)
	}
	return out, nil
}

func (r *fakeRepo) FetchOutbox(_ context.Context, limit int) ([]data.OutboxRecord, error) {
	if limit <= 0 || limit > len(r.outbox) {
		limit = len(r.outbox)
	}
	out := make([]data.OutboxRecord, limit)
	copy(out, r.outbox[:limit])
	return out, nil
}

func (r *fakeRepo) DeleteOutbox(_ context.Context, id int64) error {
	for i, o := range r.outbox {
		if o.ID == id {
			r.outbox = append(r.outbox[:i], r.outbox[i+1:]...)
			return nil
		}
	}
	return nil
}

func (r *fakeRepo) FetchDropOutbox(_ context.Context, limit int) ([]data.DropOutboxRecord, error) {
	if limit <= 0 || limit > len(r.dropOutbox) {
		limit = len(r.dropOutbox)
	}
	out := make([]data.DropOutboxRecord, limit)
	copy(out, r.dropOutbox[:limit])
	return out, nil
}

func (r *fakeRepo) DeleteDropOutbox(_ context.Context, id int64) error {
	for i, d := range r.dropOutbox {
		if d.ID == id {
			r.dropOutbox = append(r.dropOutbox[:i], r.dropOutbox[i+1:]...)
			return nil
		}
	}
	return nil
}

// fakePusher 捕获 player.update 事件;failFirst>0 时前 failFirst 次推送返错(模拟 Kafka 不可用),
// failAt>0 时第 failAt 次调用单次返错(模拟一批中途失败)。
type fakePusher struct {
	events    []capturedPush
	failFirst int
	failAt    int
	calls     int
}

type capturedPush struct {
	playerID uint64
	payload  []byte
}

func (p *fakePusher) PushPlayerUpdate(_ context.Context, playerID uint64, payload []byte) error {
	p.calls++
	if p.calls <= p.failFirst || p.calls == p.failAt {
		return simpleErr("kafka down")
	}
	p.events = append(p.events, capturedPush{playerID: playerID, payload: payload})
	return nil
}

// fakeGranter 捕获 GrantInstances 调用;failPlayer!=0 时对该玩家恒返错(模拟背包满,验证不阻塞其他玩家)。
// capacityFull=true 时所有玩家返 ErrInventoryCapacityFull(验证背包满转邮件路径)。
type fakeGranter struct {
	calls        []grantCall
	failPlayer   uint64
	capacityFull bool
}

type grantCall struct {
	playerID uint64
	items    []uint32
	key      string
}

func (g *fakeGranter) GrantInstances(_ context.Context, playerID uint64, itemConfigIDs []uint32, key string) error {
	if g.capacityFull {
		return errcode.New(errcode.ErrInventoryCapacityFull, "bag full")
	}
	if g.failPlayer != 0 && playerID == g.failPlayer {
		return simpleErr("bag full")
	}
	g.calls = append(g.calls, grantCall{playerID: playerID, items: append([]uint32(nil), itemConfigIDs...), key: key})
	return nil
}

// fakeMailSender 捕获 SendOverflowMail 调用;failAll=true 时恒返错(验证转邮件失败保留出箱行)。
type fakeMailSender struct {
	calls   []grantCall
	failAll bool
}

func (m *fakeMailSender) SendOverflowMail(_ context.Context, playerID uint64, itemConfigIDs []uint32, key string) error {
	if m.failAll {
		return simpleErr("mail down")
	}
	m.calls = append(m.calls, grantCall{playerID: playerID, items: append([]uint32(nil), itemConfigIDs...), key: key})
	return nil
}

// simpleErr 是测试用轻量 error(避免多引一个包)。
type simpleErr string

func (e simpleErr) Error() string { return string(e) }

func newTestUsecase(repo *fakeRepo, pusher PlayerUpdatePusher) *BattleResultUsecase {
	cfg := conf.BattleConf{EloKFactor: 32, BaseMMR: 1500}
	return NewBattleResultUsecase(repo, NewStaticMMRReader(cfg.BaseMMR), pusher, nil, cfg)
}

// ── Elo ───────────────────────────────────────────────────────────────────────

func TestEloDeltasEqualSymmetric(t *testing.T) {
	dA, dB := eloDeltas(1500, 1500, 32, winnerTeamA)
	if dA != 16 || dB != -16 {
		t.Fatalf("equal MMR A win: got (%d,%d) want (16,-16)", dA, dB)
	}
	dA, dB = eloDeltas(1500, 1500, 32, winnerTeamB)
	if dA != -16 || dB != 16 {
		t.Fatalf("equal MMR B win: got (%d,%d) want (-16,16)", dA, dB)
	}
}

func TestEloDeltasDrawSymmetric(t *testing.T) {
	dA, dB := eloDeltas(1500, 1500, 32, winnerTeamDraw)
	if dA != 0 || dB != 0 {
		t.Fatalf("equal MMR draw: got (%d,%d) want (0,0)", dA, dB)
	}
}

func TestEloDeltasFavoriteWinsLess(t *testing.T) {
	// A 队远强(1900 vs 1500),A 赢应远小于 K/2;B 若爆冷赢应远大于 K/2。
	dStrongWin, _ := eloDeltas(1900, 1500, 32, winnerTeamA)
	dWeakWinA, dWeakWinB := eloDeltas(1900, 1500, 32, winnerTeamB)
	if dStrongWin >= 16 {
		t.Fatalf("favorite win delta should be < 16, got %d", dStrongWin)
	}
	if dWeakWinB <= 16 {
		t.Fatalf("underdog win delta should be > 16, got %d", dWeakWinB)
	}
	// K 守恒(K 相等时两队 delta 互为相反数)
	if dWeakWinA != -dWeakWinB {
		t.Fatalf("K conservation broken: dA=%d dB=%d", dWeakWinA, dWeakWinB)
	}
}

// ── ReportResult ──────────────────────────────────────────────────────────────

func TestReportResultAssignsMMRAndIdempotent(t *testing.T) {
	repo := newFakeRepo()
	pusher := &fakePusher{}
	uc := newTestUsecase(repo, pusher)

	result := &battlev1.BattleResult{
		MatchId:    100,
		WinnerTeam: winnerTeamA,
		EndedAtMs:  1234,
		Stats: []*battlev1.PlayerStats{
			{PlayerId: 1, Team: 0, MmrDelta: 999}, // DS 上报的脏值,应被覆盖
			{PlayerId: 2, Team: 0},
			{PlayerId: 3, Team: 1},
			{PlayerId: 4, Team: 1},
		},
	}

	already, err := uc.ReportResult(context.Background(), result)
	if err != nil {
		t.Fatalf("ReportResult err: %v", err)
	}
	if already {
		t.Fatal("first report should not be alreadyRecorded")
	}
	// outcome 缺省补 NORMAL
	if result.GetOutcome() != battlev1.BattleOutcome_BATTLE_OUTCOME_NORMAL {
		t.Fatalf("outcome got %v want NORMAL", result.GetOutcome())
	}
	// 等分队伍:A 队 +16,B 队 -16(覆盖 DS 脏值)
	for _, s := range result.GetStats() {
		want := int32(16)
		if s.GetTeam() == 1 {
			want = -16
		}
		if s.GetMmrDelta() != want {
			t.Fatalf("player %d mmr_delta got %d want %d", s.GetPlayerId(), s.GetMmrDelta(), want)
		}
	}
	// 出箱象驱动发布后才推 player.update(W4 ⑨ 事务出箱)
	n, err := uc.publishOutboxBatch(context.Background())
	if err != nil {
		t.Fatalf("publishOutboxBatch err: %v", err)
	}
	if n != 4 || len(pusher.events) != 4 {
		t.Fatalf("expected 4 player.update pushes, got published=%d events=%d", n, len(pusher.events))
	}
	if len(repo.outbox) != 0 {
		t.Fatalf("outbox should be drained, got %d", len(repo.outbox))
	}

	// 幂等:再报一次同 match_id → alreadyRecorded
	already2, err := uc.ReportResult(context.Background(), result)
	if err != nil {
		t.Fatalf("second ReportResult err: %v", err)
	}
	if !already2 {
		t.Fatal("second report should be alreadyRecorded")
	}
}

func TestReportResultValidation(t *testing.T) {
	uc := newTestUsecase(newFakeRepo(), &fakePusher{})
	if _, err := uc.ReportResult(context.Background(), &battlev1.BattleResult{MatchId: 0}); err == nil {
		t.Fatal("expected error for match_id=0")
	}
	if _, err := uc.ReportResult(context.Background(), &battlev1.BattleResult{MatchId: 1}); err == nil {
		t.Fatal("expected error for empty stats")
	}
}

// TestReportResultAbandonedForcesZeroDelta 守住风险入口:battle.result 路径若误报 / 伪造
// Outcome=ABANDONED,ReportResult 必须强制 mmr_delta 全 0(不走 assignMMR),
// 防 DS 不可信地通过 abandoned 改玩家段位(不变量 §4/§6)。
func TestReportResultAbandonedForcesZeroDelta(t *testing.T) {
	repo := newFakeRepo()
	pusher := &fakePusher{}
	uc := newTestUsecase(repo, pusher)

	result := &battlev1.BattleResult{
		MatchId:    300,
		WinnerTeam: winnerTeamA, // 即便伪造了胜方,abandoned 也不许据此加分
		Outcome:    battlev1.BattleOutcome_BATTLE_OUTCOME_ABANDONED,
		EndedAtMs:  4321,
		Stats: []*battlev1.PlayerStats{
			{PlayerId: 1, Team: 0, MmrDelta: 50}, // DS 上报脏值,应被清零
			{PlayerId: 2, Team: 0, MmrDelta: 50},
			{PlayerId: 3, Team: 1, MmrDelta: -50},
			{PlayerId: 4, Team: 1, MmrDelta: -50},
		},
	}

	already, err := uc.ReportResult(context.Background(), result)
	if err != nil {
		t.Fatalf("ReportResult abandoned err: %v", err)
	}
	if already {
		t.Fatal("first abandoned report should not be alreadyRecorded")
	}
	// outcome 保持 ABANDONED(不被改写成 NORMAL)
	if result.GetOutcome() != battlev1.BattleOutcome_BATTLE_OUTCOME_ABANDONED {
		t.Fatalf("outcome got %v want ABANDONED", result.GetOutcome())
	}
	// 所有玩家 delta 必须被强制清零
	for _, s := range result.GetStats() {
		if s.GetMmrDelta() != 0 {
			t.Fatalf("abandoned-via-ReportResult player %d mmr_delta got %d want 0", s.GetPlayerId(), s.GetMmrDelta())
		}
	}
	// 落库记录里也应是 delta 全 0
	rec, ok, _ := repo.GetResult(context.Background(), 300)
	if !ok {
		t.Fatal("abandoned record not saved")
	}
	for _, s := range rec.GetStats() {
		if s.GetMmrDelta() != 0 {
			t.Fatalf("saved abandoned player %d mmr_delta got %d want 0", s.GetPlayerId(), s.GetMmrDelta())
		}
	}
}

// TestReportResultDoesNotReclaimDS 守住 2026-07-03 根因修复:battle_result 结算落库后
// **绝不主动回收战斗 DS**(不在 ReportResult 同步响应路径 taskkill/DELETE DS)。
//
// 背景:DS 收到 ReportResult OK 后才 ended 心跳 → 通知客户端回大厅 → 自身 Agones Shutdown。
// 曾经 battle_result 在响应路径同步调 ds_allocator.ReleaseBattle(=taskkill/DELETE),抢在 DS
// 通知客户端之前把 DS 杀掉 → 客户端永远收不到回大厅通知,卡战斗态。修复:移除该调用,DS 生命周期
// 归 ds_allocator(ended 心跳 → killStrandedDS / Agones 自停)+ 15s 心跳超时 sweep 兜底。
//
// 本测试是架构回归护栏:battle_result 已无 DSReleaser 依赖(编译期保证),此处进一步断言正常 /
// abandoned 结算都能落库成功,证明 DS 回收已与结算响应路径解耦。若有人重新引入同步 DS 回收,
// 应先删除本测试并复审此根因,而非绕过。
func TestReportResultDoesNotReclaimDS(t *testing.T) {
	mkResult := func(matchID uint64, outcome battlev1.BattleOutcome) *battlev1.BattleResult {
		return &battlev1.BattleResult{
			MatchId:    matchID,
			WinnerTeam: winnerTeamA,
			Outcome:    outcome,
			EndedAtMs:  1000,
			Stats: []*battlev1.PlayerStats{
				{PlayerId: 1, Team: 0},
				{PlayerId: 2, Team: 1},
			},
		}
	}

	// 1) 正常结算:落库成功、返回 !alreadyRecorded;不依赖任何 DS 回收器(构造签名已无 DSReleaser)
	t.Run("normal_settle_persists_without_ds_reclaim", func(t *testing.T) {
		repo := newFakeRepo()
		uc := newTestUsecase(repo, &fakePusher{})
		already, err := uc.ReportResult(context.Background(), mkResult(500, battlev1.BattleOutcome_BATTLE_OUTCOME_UNSPECIFIED))
		if err != nil {
			t.Fatalf("ReportResult err: %v", err)
		}
		if already {
			t.Fatal("first report should not be alreadyRecorded")
		}
		if _, ok, _ := repo.GetResult(context.Background(), 500); !ok {
			t.Fatal("normal settlement must be persisted")
		}
		// 幂等命中(同 match_id 再报)仍成功,不产生任何 DS 副作用
		if already2, err := uc.ReportResult(context.Background(), mkResult(500, battlev1.BattleOutcome_BATTLE_OUTCOME_UNSPECIFIED)); err != nil {
			t.Fatalf("second ReportResult err: %v", err)
		} else if !already2 {
			t.Fatal("second report of same match should be alreadyRecorded")
		}
	})

	// 2) abandoned(防伪兜底 / sweep 补偿)同样落库成功,不涉及 DS 回收
	t.Run("abandoned_settle_persists_without_ds_reclaim", func(t *testing.T) {
		repo := newFakeRepo()
		uc := newTestUsecase(repo, &fakePusher{})
		if _, err := uc.ReportResult(context.Background(), mkResult(501, battlev1.BattleOutcome_BATTLE_OUTCOME_ABANDONED)); err != nil {
			t.Fatalf("ReportResult abandoned err: %v", err)
		}
		if _, ok, _ := repo.GetResult(context.Background(), 501); !ok {
			t.Fatal("abandoned settlement must be persisted")
		}
		if err := uc.HandleAbandoned(context.Background(), 502, []uint64{1, 2}, 5, "ranked_5v5", 0); err != nil {
			t.Fatalf("HandleAbandoned err: %v", err)
		}
		if _, ok, _ := repo.GetResult(context.Background(), 502); !ok {
			t.Fatal("HandleAbandoned compensation must be persisted")
		}
	})
}

// ── HandleAbandoned ───────────────────────────────────────────────────────────

func TestHandleAbandonedZeroDeltaIdempotent(t *testing.T) {
	repo := newFakeRepo()
	pusher := &fakePusher{}
	uc := newTestUsecase(repo, pusher)

	players := []uint64{10, 11, 12}
	if err := uc.HandleAbandoned(context.Background(), 200, players, 5, "ranked_5v5", 0); err != nil {
		t.Fatalf("HandleAbandoned err: %v", err)
	}

	rec, ok, _ := repo.GetResult(context.Background(), 200)
	if !ok {
		t.Fatal("abandoned record not saved")
	}
	if rec.GetOutcome() != battlev1.BattleOutcome_BATTLE_OUTCOME_ABANDONED {
		t.Fatalf("outcome got %v want ABANDONED", rec.GetOutcome())
	}
	if rec.GetWinnerTeam() != winnerTeamDraw {
		t.Fatalf("winner_team got %d want draw(%d)", rec.GetWinnerTeam(), winnerTeamDraw)
	}
	for _, s := range rec.GetStats() {
		if s.GetMmrDelta() != 0 {
			t.Fatalf("abandoned player %d mmr_delta got %d want 0", s.GetPlayerId(), s.GetMmrDelta())
		}
	}
	// 出箱驱动发布后应有 3 条 abandon 推送
	if _, perr := uc.publishOutboxBatch(context.Background()); perr != nil {
		t.Fatalf("publishOutboxBatch err: %v", perr)
	}
	if len(pusher.events) != 3 {
		t.Fatalf("expected 3 abandon pushes, got %d", len(pusher.events))
	}

	// 幂等:重复 abandoned 不再入箱 → 发布不再推
	pusher.events = nil
	if err := uc.HandleAbandoned(context.Background(), 200, players, 5, "ranked_5v5", 0); err != nil {
		t.Fatalf("second HandleAbandoned err: %v", err)
	}
	if _, perr := uc.publishOutboxBatch(context.Background()); perr != nil {
		t.Fatalf("publishOutboxBatch err: %v", perr)
	}
	if len(pusher.events) != 0 {
		t.Fatalf("idempotent abandoned should not push, got %d", len(pusher.events))
	}
}

func TestHandleAbandonedValidation(t *testing.T) {
	uc := newTestUsecase(newFakeRepo(), &fakePusher{})
	if err := uc.HandleAbandoned(context.Background(), 0, nil, 0, "", 0); err == nil {
		t.Fatal("expected error for match_id=0")
	}
}

// ── 出箱可靠发布(W4 ⑨,不变量 §4)──────────────────────────────────────────────

// reportFour 落一场 4 人正常结算,返回 usecase / repo / pusher。
func reportFour(t *testing.T, pusher PlayerUpdatePusher) (*BattleResultUsecase, *fakeRepo) {
	t.Helper()
	repo := newFakeRepo()
	uc := newTestUsecase(repo, pusher)
	result := &battlev1.BattleResult{
		MatchId:    700,
		WinnerTeam: winnerTeamA,
		EndedAtMs:  9999,
		Stats: []*battlev1.PlayerStats{
			{PlayerId: 1, Team: 0}, {PlayerId: 2, Team: 0},
			{PlayerId: 3, Team: 1}, {PlayerId: 4, Team: 1},
		},
	}
	if _, err := uc.ReportResult(context.Background(), result); err != nil {
		t.Fatalf("ReportResult err: %v", err)
	}
	return uc, repo
}

// TestOutboxWrittenAtomicallyOnSave 落库即入箱:ReportResult 后出箱有 4 条待发布(尚未投递)。
func TestOutboxWrittenAtomicallyOnSave(t *testing.T) {
	pusher := &fakePusher{}
	_, repo := reportFour(t, pusher)
	if len(repo.outbox) != 4 {
		t.Fatalf("expected 4 outbox rows after save, got %d", len(repo.outbox))
	}
	if len(pusher.events) != 0 {
		t.Fatalf("nothing should be pushed before publisher runs, got %d", len(pusher.events))
	}
}

// TestOutboxReliablePublish_RetryUntilDelivered 模拟 Kafka 临时不可用:
// 前 2 轮发布全失败,出箱行保留;Kafka 恢复后第 3 轮全部投递并清空出箱(at-least-once 闭环)。
func TestOutboxReliablePublish_RetryUntilDelivered(t *testing.T) {
	// 每个失败批只发生 1 次推送调用(首条即失败立即中断),故 failFirst=2 = 前 2 轮失败。
	pusher := &fakePusher{failFirst: 2}
	uc, repo := reportFour(t, pusher)

	// 第 1 轮:首条即失败 → 0 投递,出箱仍 4 条
	if n, err := uc.publishOutboxBatch(context.Background()); err == nil || n != 0 {
		t.Fatalf("round1 expect fail n=0, got n=%d err=%v", n, err)
	}
	if len(repo.outbox) != 4 {
		t.Fatalf("round1 outbox should stay 4, got %d", len(repo.outbox))
	}
	if len(pusher.events) != 0 {
		t.Fatalf("round1 should deliver 0, got %d", len(pusher.events))
	}

	// 第 2 轮:仍在失败窗口内 → 继续 0 投递、出箱不减
	if n, _ := uc.publishOutboxBatch(context.Background()); n != 0 {
		t.Fatalf("round2 expect 0 published, got %d", n)
	}
	if len(repo.outbox) != 4 {
		t.Fatalf("round2 outbox should stay 4, got %d", len(repo.outbox))
	}

	// 第 3 轮:Kafka 恢复(calls 已过 failFirst)→ 全投递、出箱清空
	if n, err := uc.publishOutboxBatch(context.Background()); err != nil || n != 4 {
		t.Fatalf("round3 expect 4 published, got n=%d err=%v", n, err)
	}
	if len(repo.outbox) != 0 {
		t.Fatalf("round3 outbox should be drained, got %d", len(repo.outbox))
	}
	if len(pusher.events) != 4 {
		t.Fatalf("round3 should deliver 4, got %d", len(pusher.events))
	}

	// 第 4 轮:出箱已空 → 0 投递、无副作用
	if n, err := uc.publishOutboxBatch(context.Background()); err != nil || n != 0 {
		t.Fatalf("round4 expect 0 published, got n=%d err=%v", n, err)
	}
}

// TestOutboxPublishMidBatchFailureKeepsOrder 一批中途失败:前 k 条成功删除,失败处中断,
// 剩余行保留(下轮从失败处续传),保证同玩家事件按 id 顺序投递(不变量 §9)。
func TestOutboxPublishMidBatchFailureKeepsOrder(t *testing.T) {
	// 第 3 次推送单次失败:前 2 条成功删,第 3 条起保留。
	pusher := &fakePusher{failAt: 3}
	uc, repo := reportFour(t, pusher)

	n, err := uc.publishOutboxBatch(context.Background())
	if err == nil {
		t.Fatal("expected mid-batch failure")
	}
	if n != 2 {
		t.Fatalf("expected 2 published before failure, got %d", n)
	}
	if len(repo.outbox) != 2 {
		t.Fatalf("expected 2 outbox rows retained, got %d", len(repo.outbox))
	}
	// 保留的应是后 2 个玩家(id 顺序:player 3、4)
	if repo.outbox[0].PlayerID != 3 || repo.outbox[1].PlayerID != 4 {
		t.Fatalf("retained order wrong: %d,%d", repo.outbox[0].PlayerID, repo.outbox[1].PlayerID)
	}
}

// TestOutboxNilPusherNoLoss pusher 为 nil(kafka 未配置)时发布器不投递,但出箱行不丢。
func TestOutboxNilPusherNoLoss(t *testing.T) {
	uc, repo := reportFour(t, nil)
	if n, err := uc.publishOutboxBatch(context.Background()); err != nil || n != 0 {
		t.Fatalf("nil pusher expect 0 published no error, got n=%d err=%v", n, err)
	}
	if len(repo.outbox) != 4 {
		t.Fatalf("nil pusher must not lose outbox, got %d", len(repo.outbox))
	}
}

// ── 战斗装备掉落回写(W5 ④,drop 白名单过滤 + 事务出箱 + GrantInstances 幂等)──────────

// newDropUsecase 构造带 drop 白名单 + granter 的 usecase。whitelist 决定哪些 item_config_id 可落库。
func newDropUsecase(repo *fakeRepo, granter InstanceGranter, whitelist []uint32) *BattleResultUsecase {
	cfg := conf.BattleConf{EloKFactor: 32, BaseMMR: 1500, DropWhitelist: whitelist}
	uc := NewBattleResultUsecase(repo, NewStaticMMRReader(cfg.BaseMMR), &fakePusher{}, nil, cfg)
	if granter != nil {
		uc.SetInstanceGranter(granter)
	}
	return uc
}

// dropResult 组一场 2 人正常结算,player 1 掉落 drop1,player 2 掉落 drop2。
func dropResult(matchID uint64, drop1, drop2 []uint32) *battlev1.BattleResult {
	return &battlev1.BattleResult{
		MatchId:    matchID,
		WinnerTeam: winnerTeamA,
		EndedAtMs:  9999,
		Stats: []*battlev1.PlayerStats{
			{PlayerId: 1, Team: 0, DroppedItemConfigIds: drop1},
			{PlayerId: 2, Team: 1, DroppedItemConfigIds: drop2},
		},
	}
}

// TestDropWhitelistFilter DS 上报的掉落只有白名单内 ID 入 drop 出箱(DS 不可信)。
func TestDropWhitelistFilter(t *testing.T) {
	repo := newFakeRepo()
	uc := newDropUsecase(repo, &fakeGranter{}, []uint32{5001, 5002})
	// player 1 报 [5001(白), 9999(非白)];player 2 报 [8888(非白)]。
	if _, err := uc.ReportResult(context.Background(), dropResult(600, []uint32{5001, 9999}, []uint32{8888})); err != nil {
		t.Fatalf("ReportResult err: %v", err)
	}
	// 只 player 1 有白名单内掉落 → drop 出箱 1 行,内容仅 [5001]。
	if len(repo.dropOutbox) != 1 {
		t.Fatalf("expected 1 drop outbox row, got %d", len(repo.dropOutbox))
	}
	d := repo.dropOutbox[0]
	if d.PlayerID != 1 || len(d.ItemConfigIDs) != 1 || d.ItemConfigIDs[0] != 5001 {
		t.Fatalf("drop outbox filtered wrong: player=%d items=%v", d.PlayerID, d.ItemConfigIDs)
	}
	if d.MatchID != 600 {
		t.Fatalf("drop outbox match_id got %d want 600", d.MatchID)
	}
}

// TestDropPerPlayerCap 恶意/异常 DS 重复上报海量白名单 ID → 每玩家按上限截断,
// 结算正常落库不回滚(防撑爆 battle_drop_outbox.item_config_ids VARCHAR(512))。
func TestDropPerPlayerCap(t *testing.T) {
	repo := newFakeRepo()
	cfg := conf.BattleConf{EloKFactor: 32, BaseMMR: 1500, DropWhitelist: []uint32{5001}, MaxDropPerPlayer: 3}
	uc := NewBattleResultUsecase(repo, NewStaticMMRReader(cfg.BaseMMR), &fakePusher{}, nil, cfg)
	uc.SetInstanceGranter(&fakeGranter{})
	flood := make([]uint32, 500)
	for i := range flood {
		flood[i] = 5001
	}
	if _, err := uc.ReportResult(context.Background(), dropResult(610, flood, nil)); err != nil {
		t.Fatalf("ReportResult err: %v", err)
	}
	if len(repo.dropOutbox) != 1 {
		t.Fatalf("expected 1 drop outbox row, got %d", len(repo.dropOutbox))
	}
	if got := len(repo.dropOutbox[0].ItemConfigIDs); got != 3 {
		t.Fatalf("per-player cap 3 not enforced, kept %d", got)
	}
}

// TestDropCapDefaults 未配置 → 默认 32;配置超硬上限 → 钳制到 46(VARCHAR(512) 安全上限)。
func TestDropCapDefaults(t *testing.T) {
	b := conf.BattleConf{}
	if got := b.MaxDropsPerPlayer(); got != 32 {
		t.Fatalf("default cap got %d want 32", got)
	}
	b.MaxDropPerPlayer = 100
	if got := b.MaxDropsPerPlayer(); got != 46 {
		t.Fatalf("hard cap got %d want 46", got)
	}
}

// TestDropEmptyWhitelistBlocksAll 白名单为空 → 任何掉落都不入库(安全默认)。
func TestDropEmptyWhitelistBlocksAll(t *testing.T) {
	repo := newFakeRepo()
	uc := newDropUsecase(repo, &fakeGranter{}, nil)
	if _, err := uc.ReportResult(context.Background(), dropResult(601, []uint32{5001}, []uint32{5002})); err != nil {
		t.Fatalf("ReportResult err: %v", err)
	}
	if len(repo.dropOutbox) != 0 {
		t.Fatalf("empty whitelist must block all drops, got %d rows", len(repo.dropOutbox))
	}
}

// TestDropAbandonedNoDrops ABANDONED(DS 崩溃补偿)不产出任何掉落,即使 DS 上报了白名单内 ID。
func TestDropAbandonedNoDrops(t *testing.T) {
	repo := newFakeRepo()
	uc := newDropUsecase(repo, &fakeGranter{}, []uint32{5001})
	res := dropResult(602, []uint32{5001}, []uint32{5001})
	res.Outcome = battlev1.BattleOutcome_BATTLE_OUTCOME_ABANDONED
	if _, err := uc.ReportResult(context.Background(), res); err != nil {
		t.Fatalf("ReportResult err: %v", err)
	}
	if len(repo.dropOutbox) != 0 {
		t.Fatalf("abandoned must produce no drops, got %d rows", len(repo.dropOutbox))
	}
}

// TestDropPublisherGrantsAndDrains 掉落出箱经发布器发放:调 GrantInstances(幂等键正确)并清空出箱。
func TestDropPublisherGrantsAndDrains(t *testing.T) {
	repo := newFakeRepo()
	granter := &fakeGranter{}
	uc := newDropUsecase(repo, granter, []uint32{5001, 5002})
	if _, err := uc.ReportResult(context.Background(), dropResult(603, []uint32{5001}, []uint32{5002})); err != nil {
		t.Fatalf("ReportResult err: %v", err)
	}
	if len(repo.dropOutbox) != 2 {
		t.Fatalf("expected 2 drop outbox rows, got %d", len(repo.dropOutbox))
	}
	n, err := uc.publishDropBatch(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("publishDropBatch expect 2 granted, got n=%d err=%v", n, err)
	}
	if len(repo.dropOutbox) != 0 {
		t.Fatalf("drop outbox should drain, got %d", len(repo.dropOutbox))
	}
	if len(granter.calls) != 2 {
		t.Fatalf("expected 2 grant calls, got %d", len(granter.calls))
	}
	// 幂等键 = battle_drop:{match_id}:{player_id}
	if granter.calls[0].key != "battle_drop:603:1" {
		t.Fatalf("idempotency key wrong: %s", granter.calls[0].key)
	}
}

// TestDropPublisherPerRowRetry 单玩家背包满(granter 恒返错)不阻塞其他玩家:失败行保留,成功行清空。
func TestDropPublisherPerRowRetry(t *testing.T) {
	repo := newFakeRepo()
	granter := &fakeGranter{failPlayer: 2} // player 2 背包满
	uc := newDropUsecase(repo, granter, []uint32{5001, 5002})
	if _, err := uc.ReportResult(context.Background(), dropResult(604, []uint32{5001}, []uint32{5002})); err != nil {
		t.Fatalf("ReportResult err: %v", err)
	}
	n, err := uc.publishDropBatch(context.Background())
	if err != nil {
		t.Fatalf("publishDropBatch err: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 granted (player 1), got %d", n)
	}
	// player 2 失败行保留下轮重试;player 1 已发放清空。
	if len(repo.dropOutbox) != 1 || repo.dropOutbox[0].PlayerID != 2 {
		t.Fatalf("failed row for player 2 must be retained, got %+v", repo.dropOutbox)
	}
}

// TestDropIdempotentReplay 幂等命中(同 match 再报)不重复写 drop 出箱。
func TestDropIdempotentReplay(t *testing.T) {
	repo := newFakeRepo()
	uc := newDropUsecase(repo, &fakeGranter{}, []uint32{5001})
	res := dropResult(605, []uint32{5001}, nil)
	if _, err := uc.ReportResult(context.Background(), res); err != nil {
		t.Fatalf("first ReportResult err: %v", err)
	}
	if already, err := uc.ReportResult(context.Background(), dropResult(605, []uint32{5001}, nil)); err != nil || !already {
		t.Fatalf("second report expect alreadyRecorded, got already=%v err=%v", already, err)
	}
	if len(repo.dropOutbox) != 1 {
		t.Fatalf("idempotent replay must not duplicate drop outbox, got %d rows", len(repo.dropOutbox))
	}
}

// TestDropNilGranterNoLoss granter 为 nil(inventory_addr 未配)→ 发布器不发放,但出箱行不丢。
func TestDropNilGranterNoLoss(t *testing.T) {
	repo := newFakeRepo()
	uc := newDropUsecase(repo, nil, []uint32{5001})
	if _, err := uc.ReportResult(context.Background(), dropResult(606, []uint32{5001}, nil)); err != nil {
		t.Fatalf("ReportResult err: %v", err)
	}
	if n, err := uc.publishDropBatch(context.Background()); err != nil || n != 0 {
		t.Fatalf("nil granter expect 0 granted no error, got n=%d err=%v", n, err)
	}
	if len(repo.dropOutbox) != 1 {
		t.Fatalf("nil granter must not lose drop outbox, got %d", len(repo.dropOutbox))
	}
}

// ── 背包满溢出转邮件(W5 ④+,ErrInventoryCapacityFull → mail.SendOverflowMail)──────

// newDropUsecaseWithMail 在 newDropUsecase 基础上再注入 mailSender。
func newDropUsecaseWithMail(repo *fakeRepo, granter InstanceGranter, mail MailSender, whitelist []uint32) *BattleResultUsecase {
	uc := newDropUsecase(repo, granter, whitelist)
	if mail != nil {
		uc.SetMailSender(mail)
	}
	return uc
}

// TestDropOverflowToMailOnCapacityFull 背包满 + 已配 mail:掉落转个人邮件(源键传递正确),出箱行清空。
func TestDropOverflowToMailOnCapacityFull(t *testing.T) {
	repo := newFakeRepo()
	granter := &fakeGranter{capacityFull: true}
	mail := &fakeMailSender{}
	uc := newDropUsecaseWithMail(repo, granter, mail, []uint32{5001, 5002})
	if _, err := uc.ReportResult(context.Background(), dropResult(700, []uint32{5001}, []uint32{5002})); err != nil {
		t.Fatalf("ReportResult err: %v", err)
	}
	n, err := uc.publishDropBatch(context.Background())
	if err != nil {
		t.Fatalf("publishDropBatch err: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 rows overflow-mailed+drained, got %d", n)
	}
	if len(repo.dropOutbox) != 0 {
		t.Fatalf("drop outbox should drain after overflow-mail, got %d", len(repo.dropOutbox))
	}
	if len(mail.calls) != 2 {
		t.Fatalf("expected 2 overflow mail calls, got %d", len(mail.calls))
	}
	// 直发 granter 应无成功入账(全 capacity-full),掉落全部走邮件。
	if len(granter.calls) != 0 {
		t.Fatalf("expected 0 direct grant calls on capacity-full, got %d", len(granter.calls))
	}
	// 溢出邮件必须传与直发相同的源键 battle_drop:{match}:{player}(领取时同键去重)。
	if mail.calls[0].key != "battle_drop:700:1" {
		t.Fatalf("overflow mail key wrong: %s", mail.calls[0].key)
	}
	if len(mail.calls[0].items) != 1 || mail.calls[0].items[0] != 5001 {
		t.Fatalf("overflow mail items wrong: %v", mail.calls[0].items)
	}
}

// TestDropOverflowMailFailureKeepsRow 转邮件失败 → 出箱行保留下轮重试(不丢),granted=0。
func TestDropOverflowMailFailureKeepsRow(t *testing.T) {
	repo := newFakeRepo()
	granter := &fakeGranter{capacityFull: true}
	mail := &fakeMailSender{failAll: true}
	uc := newDropUsecaseWithMail(repo, granter, mail, []uint32{5001})
	if _, err := uc.ReportResult(context.Background(), dropResult(701, []uint32{5001}, nil)); err != nil {
		t.Fatalf("ReportResult err: %v", err)
	}
	n, err := uc.publishDropBatch(context.Background())
	if err != nil {
		t.Fatalf("publishDropBatch err: %v", err)
	}
	if n != 0 {
		t.Fatalf("mail failure must not drain, got granted=%d", n)
	}
	if len(repo.dropOutbox) != 1 {
		t.Fatalf("mail failure must retain drop outbox row, got %d", len(repo.dropOutbox))
	}
}

// TestDropCapacityFullNoMailSenderKeepsRow 背包满但未配 mail → 退化为历史行为:保留出箱行重试(不丢)。
func TestDropCapacityFullNoMailSenderKeepsRow(t *testing.T) {
	repo := newFakeRepo()
	granter := &fakeGranter{capacityFull: true}
	uc := newDropUsecase(repo, granter, []uint32{5001}) // 无 mailSender
	if _, err := uc.ReportResult(context.Background(), dropResult(702, []uint32{5001}, nil)); err != nil {
		t.Fatalf("ReportResult err: %v", err)
	}
	n, err := uc.publishDropBatch(context.Background())
	if err != nil {
		t.Fatalf("publishDropBatch err: %v", err)
	}
	if n != 0 {
		t.Fatalf("no mail sender: capacity-full must not drain, got granted=%d", n)
	}
	if len(repo.dropOutbox) != 1 {
		t.Fatalf("no mail sender: capacity-full must retain row, got %d", len(repo.dropOutbox))
	}
}

// TestDropTransientErrNoMailOverflow 非背包满错误(inventory 临时不可用)不触发转邮件,保留出箱行重试。
func TestDropTransientErrNoMailOverflow(t *testing.T) {
	repo := newFakeRepo()
	granter := &fakeGranter{failPlayer: 1} // 返回普通 error(非 capacity-full)
	mail := &fakeMailSender{}
	uc := newDropUsecaseWithMail(repo, granter, mail, []uint32{5001})
	if _, err := uc.ReportResult(context.Background(), dropResult(703, []uint32{5001}, nil)); err != nil {
		t.Fatalf("ReportResult err: %v", err)
	}
	n, err := uc.publishDropBatch(context.Background())
	if err != nil {
		t.Fatalf("publishDropBatch err: %v", err)
	}
	if n != 0 {
		t.Fatalf("transient err must not drain, got granted=%d", n)
	}
	if len(mail.calls) != 0 {
		t.Fatalf("transient err must NOT trigger overflow mail, got %d calls", len(mail.calls))
	}
	if len(repo.dropOutbox) != 1 {
		t.Fatalf("transient err must retain row, got %d", len(repo.dropOutbox))
	}
}
