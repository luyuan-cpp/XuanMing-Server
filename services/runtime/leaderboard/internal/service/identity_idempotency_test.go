package service

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	plog "github.com/luyuancpp/pandora/pkg/log"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	leaderboardv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/leaderboard/v1"
	"github.com/luyuancpp/pandora/services/runtime/leaderboard/internal/biz"
	"github.com/luyuancpp/pandora/services/runtime/leaderboard/internal/conf"
	"github.com/luyuancpp/pandora/services/runtime/leaderboard/internal/data"
)

func serviceTestBoard() *leaderboardv1.BoardKey {
	return &leaderboardv1.BoardKey{
		BoardType: 1,
		Scope:     leaderboardv1.LeaderboardScope_LEADERBOARD_SCOPE_GLOBAL,
		Period:    "service-test",
	}
}

// TestLeaderboardServiceRejectsPlayerIdentityOnEverySystemWrite 固化系统写边界：
// 带玩家 JWT 的调用必须在触达 usecase 前被拒绝，读接口不受此规则影响。
func TestLeaderboardServiceRejectsPlayerIdentityOnEverySystemWrite(t *testing.T) {
	svc := NewLeaderboardService(nil)
	ctx := context.WithValue(context.Background(), plog.CtxKeyPlayerID, uint64(7))
	board := serviceTestBoard()

	tests := []struct {
		name string
		call func() (commonv1.ErrCode, error)
	}{
		{"submit", func() (commonv1.ErrCode, error) {
			resp, err := svc.SubmitScore(ctx, &leaderboardv1.SubmitScoreRequest{Board: board, EntityId: 7})
			return resp.GetCode(), err
		}},
		{"remove", func() (commonv1.ErrCode, error) {
			resp, err := svc.RemoveEntry(ctx, &leaderboardv1.RemoveEntryRequest{Board: board, EntityId: 7})
			return resp.GetCode(), err
		}},
		{"settle", func() (commonv1.ErrCode, error) {
			resp, err := svc.SettleBoard(ctx, &leaderboardv1.SettleBoardRequest{Board: board})
			return resp.GetCode(), err
		}},
		{"delete", func() (commonv1.ErrCode, error) {
			resp, err := svc.DeleteBoard(ctx, &leaderboardv1.DeleteBoardRequest{Board: board})
			return resp.GetCode(), err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, err := tt.call()
			if err != nil || code != commonv1.ErrCode_ERR_PERMISSION_DENY {
				t.Fatalf("system write code=%s err=%v", code, err)
			}
		})
	}
}

type serviceSettlementRepo struct {
	settlements map[string]*data.SettlementRecord
	snapshots   map[uint64][]data.SnapshotRow
}

func newServiceSettlementRepo() *serviceSettlementRepo {
	return &serviceSettlementRepo{
		settlements: make(map[string]*data.SettlementRecord),
		snapshots:   make(map[uint64][]data.SnapshotRow),
	}
}

func (r *serviceSettlementRepo) ClaimSettlement(_ context.Context, rec *data.SettlementRecord) (*data.SettlementRecord, bool, error) {
	if existing, ok := r.settlements[rec.SettleIdemKey]; ok {
		cp := *existing
		return &cp, true, nil
	}
	cp := *rec
	r.settlements[rec.SettleIdemKey] = &cp
	return &cp, false, nil
}

func (r *serviceSettlementRepo) SaveSnapshot(_ context.Context, settlementID uint64, rows []data.SnapshotRow) error {
	r.snapshots[settlementID] = append([]data.SnapshotRow(nil), rows...)
	return nil
}

func (r *serviceSettlementRepo) LoadSnapshot(_ context.Context, settlementID uint64) ([]data.SnapshotRow, error) {
	return append([]data.SnapshotRow(nil), r.snapshots[settlementID]...), nil
}

func (*serviceSettlementRepo) ClaimReward(context.Context, *data.RewardLogRecord) (bool, error) {
	return false, nil
}

func (*serviceSettlementRepo) MarkReward(context.Context, string, int8, int64) error { return nil }

func (*serviceSettlementRepo) ListUngrantedRewards(context.Context, int64, int) ([]data.RewardLogRecord, error) {
	return nil, nil
}

type serviceSettlementIDGen struct{ next uint64 }

func (g *serviceSettlementIDGen) Generate() uint64 {
	g.next++
	return g.next
}

func newLeaderboardServiceHarness(t *testing.T) *LeaderboardService {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	uc := biz.NewLeaderboardUsecase(
		newServiceSettlementRepo(),
		data.NewRedisBoardStore(rdb),
		nil,
		nil,
		&serviceSettlementIDGen{next: 9000},
		conf.LeaderboardConf{DefaultListLimit: 10, MaxListLimit: 20},
	)
	return NewLeaderboardService(uc)
}

func seedServiceBoard(t *testing.T, svc *LeaderboardService, board *leaderboardv1.BoardKey) {
	t.Helper()
	for entityID, score := range map[uint64]int64{1: 10, 2: 30, 3: 20} {
		resp, err := svc.SubmitScore(context.Background(), &leaderboardv1.SubmitScoreRequest{
			Board:    board,
			EntityId: entityID,
			Score:    score,
			Mode:     leaderboardv1.SubmitMode_SUBMIT_MODE_SET,
		})
		if err != nil || resp.GetCode() != commonv1.ErrCode_OK {
			t.Fatalf("seed entity=%d code=%s err=%v", entityID, resp.GetCode(), err)
		}
	}
}

// TestLeaderboardServicePlayerReadsButCannotWrite 证明身份边界不是“有 JWT 全拒绝”：
// 玩家可读榜，但同一 ctx 的系统写必须拒绝且不能改变榜内容。
func TestLeaderboardServicePlayerReadsButCannotWrite(t *testing.T) {
	svc := newLeaderboardServiceHarness(t)
	board := serviceTestBoard()
	seedServiceBoard(t, svc, board)
	playerCtx := context.WithValue(context.Background(), plog.CtxKeyPlayerID, uint64(7))

	read, err := svc.GetRange(playerCtx, &leaderboardv1.GetRangeRequest{Board: board, Limit: 10})
	if err != nil || read.GetCode() != commonv1.ErrCode_OK || len(read.GetEntries()) != 3 {
		t.Fatalf("player read code=%s entries=%d err=%v", read.GetCode(), len(read.GetEntries()), err)
	}
	write, err := svc.SubmitScore(playerCtx, &leaderboardv1.SubmitScoreRequest{
		Board: board, EntityId: 4, Score: 99, Mode: leaderboardv1.SubmitMode_SUBMIT_MODE_SET,
	})
	if err != nil || write.GetCode() != commonv1.ErrCode_ERR_PERMISSION_DENY {
		t.Fatalf("player write code=%s err=%v", write.GetCode(), err)
	}
	after, err := svc.GetRange(playerCtx, &leaderboardv1.GetRangeRequest{Board: board, Limit: 10})
	if err != nil || len(after.GetEntries()) != 3 {
		t.Fatalf("denied write changed board: entries=%d err=%v", len(after.GetEntries()), err)
	}
}

// TestLeaderboardServiceSettleRetryReplaysExactResponse 模拟首次响应丢失后的同键重试：
// 必须返回原 settlement_id、原 winners，并标记 already_settled，不能生成第二批次。
func TestLeaderboardServiceSettleRetryReplaysExactResponse(t *testing.T) {
	svc := newLeaderboardServiceHarness(t)
	board := serviceTestBoard()
	seedServiceBoard(t, svc, board)
	req := &leaderboardv1.SettleBoardRequest{
		Board:                board,
		TopN:                 2,
		SettleIdempotencyKey: "response-lost-retry",
	}

	first, err := svc.SettleBoard(context.Background(), req)
	if err != nil || first.GetCode() != commonv1.ErrCode_OK || first.GetAlreadySettled() {
		t.Fatalf("first settle code=%s already=%v err=%v", first.GetCode(), first.GetAlreadySettled(), err)
	}
	second, err := svc.SettleBoard(context.Background(), req)
	if err != nil || second.GetCode() != commonv1.ErrCode_OK || !second.GetAlreadySettled() {
		t.Fatalf("retry settle code=%s already=%v err=%v", second.GetCode(), second.GetAlreadySettled(), err)
	}
	if second.GetSettlementId() != first.GetSettlementId() || second.GetSettledCount() != first.GetSettledCount() {
		t.Fatalf("retry identity/count=%d/%d want=%d/%d",
			second.GetSettlementId(), second.GetSettledCount(), first.GetSettlementId(), first.GetSettledCount())
	}
	if len(second.GetWinners()) != len(first.GetWinners()) || len(second.GetWinners()) != 2 {
		t.Fatalf("retry winners=%d want=%d", len(second.GetWinners()), len(first.GetWinners()))
	}
	for i := range first.GetWinners() {
		got, want := second.GetWinners()[i], first.GetWinners()[i]
		if got.GetEntityId() != want.GetEntityId() || got.GetScore() != want.GetScore() || got.GetRank() != want.GetRank() {
			t.Fatalf("retry winner[%d]=%+v want=%+v", i, got, want)
		}
	}
}
