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
)

// ── 测试替身 ──────────────────────────────────────────────────────────────────

// fakeRepo 是内存版 data.BattleRepo,按 match_id 唯一(模拟 unique 幂等)。
type fakeRepo struct {
	store   map[uint64]*battlev1.BattleResult
	saveErr error
	saveCnt int
}

func newFakeRepo() *fakeRepo { return &fakeRepo{store: map[uint64]*battlev1.BattleResult{}} }

func (r *fakeRepo) SaveResult(_ context.Context, result *battlev1.BattleResult) (bool, error) {
	r.saveCnt++
	if r.saveErr != nil {
		return false, r.saveErr
	}
	if _, ok := r.store[result.GetMatchId()]; ok {
		return true, nil // 幂等命中
	}
	r.store[result.GetMatchId()] = proto.Clone(result).(*battlev1.BattleResult)
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

// fakePusher 捕获 player.update 事件。
type fakePusher struct {
	events []capturedPush
}

type capturedPush struct {
	playerID uint64
	payload  []byte
}

func (p *fakePusher) PushPlayerUpdate(_ context.Context, playerID uint64, payload []byte) error {
	p.events = append(p.events, capturedPush{playerID: playerID, payload: payload})
	return nil
}

func newTestUsecase(repo *fakeRepo, pusher PlayerUpdatePusher) *BattleResultUsecase {
	cfg := conf.BattleConf{EloKFactor: 32, BaseMMR: 1500}
	return NewBattleResultUsecase(repo, NewStaticMMRReader(cfg.BaseMMR), pusher, cfg)
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
	if len(pusher.events) != 4 {
		t.Fatalf("expected 4 player.update pushes, got %d", len(pusher.events))
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
	if len(pusher.events) != 3 {
		t.Fatalf("expected 3 abandon pushes, got %d", len(pusher.events))
	}

	// 幂等:重复 abandoned 不再推
	pusher.events = nil
	if err := uc.HandleAbandoned(context.Background(), 200, players, 5, "ranked_5v5", 0); err != nil {
		t.Fatalf("second HandleAbandoned err: %v", err)
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
