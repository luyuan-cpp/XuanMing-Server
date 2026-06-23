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

	battlev1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/battle/v1"

	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/data"
)

// ── 测试替身 ──────────────────────────────────────────────────────────────────

// fakeRepo 是内存版 data.BattleRepo,按 match_id 唯一(模拟 unique 幂等)+内存出箱。
type fakeRepo struct {
	store   map[uint64]*battlev1.BattleResult
	saveErr error
	saveCnt int
	outbox  []data.OutboxRecord // 待发布,按 ID 升序
	nextID  int64
}

func newFakeRepo() *fakeRepo { return &fakeRepo{store: map[uint64]*battlev1.BattleResult{}} }

func (r *fakeRepo) SaveResult(_ context.Context, result *battlev1.BattleResult, outbox []data.OutboxRecord) (bool, error) {
	r.saveCnt++
	if r.saveErr != nil {
		return false, r.saveErr
	}
	if _, ok := r.store[result.GetMatchId()]; ok {
		return true, nil // 幂等命中(出箱不写)
	}
	r.store[result.GetMatchId()] = proto.Clone(result).(*battlev1.BattleResult)
	for _, o := range outbox {
		r.nextID++
		r.outbox = append(r.outbox, data.OutboxRecord{ID: r.nextID, PlayerID: o.PlayerID, Payload: o.Payload})
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
