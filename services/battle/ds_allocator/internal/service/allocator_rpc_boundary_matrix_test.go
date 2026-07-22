package service

import (
	"context"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/biz"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/data"
)

type allocatorRPCProbe func() (commonv1.ErrCode, error)

// TestAllocatorPublicRPCBoundaryMatrix 覆盖公共 RPC 在任何 usecase/Redis/Agones 副作用前
// 的参数门、停用接口与权威不可用映射。
func TestAllocatorPublicRPCBoundaryMatrix(t *testing.T) {
	svc := NewAllocatorService(nil)
	ctx := context.Background()
	tests := []struct {
		name string
		want commonv1.ErrCode
		call allocatorRPCProbe
	}{
		{"AllocateBattle/match_id=0", commonv1.ErrCode_ERR_INVALID_ARG, func() (commonv1.ErrCode, error) {
			r, e := svc.AllocateBattle(ctx, &dsv1.AllocateBattleRequest{})
			return r.GetCode(), e
		}},
		{"AllocateBattle/duplicate_combat_faction", commonv1.ErrCode_ERR_INVALID_ARG, func() (commonv1.ErrCode, error) {
			r, e := svc.AllocateBattle(ctx, &dsv1.AllocateBattleRequest{MatchId: 1,
				PlayerCombatFactions: []*dsv1.BattlePlayerCombatFaction{{PlayerId: 7}, {PlayerId: 7}}})
			return r.GetCode(), e
		}},
		{"ResolveBattleTarget/missing_match", commonv1.ErrCode_ERR_INVALID_ARG, func() (commonv1.ErrCode, error) {
			r, e := svc.ResolveBattleTarget(ctx, &dsv1.ResolveBattleTargetRequest{PlayerId: 7})
			return r.GetCode(), e
		}},
		{"ResolveBattleTarget/missing_player", commonv1.ErrCode_ERR_INVALID_ARG, func() (commonv1.ErrCode, error) {
			r, e := svc.ResolveBattleTarget(ctx, &dsv1.ResolveBattleTargetRequest{MatchId: 1})
			return r.GetCode(), e
		}},
		{"ResolveBattleTarget/authority_unknown", commonv1.ErrCode_ERR_UNAVAILABLE, func() (commonv1.ErrCode, error) {
			r, e := svc.ResolveBattleTarget(ctx, &dsv1.ResolveBattleTargetRequest{MatchId: 1, PlayerId: 7})
			return r.GetCode(), e
		}},
		{"ReleaseBattle/match_id=0", commonv1.ErrCode_ERR_INVALID_ARG, func() (commonv1.ErrCode, error) {
			r, e := svc.ReleaseBattle(ctx, &dsv1.ReleaseBattleRequest{})
			return r.GetCode(), e
		}},
		{"AbortPreactiveBattle/incomplete_identity", commonv1.ErrCode_ERR_INVALID_ARG, func() (commonv1.ErrCode, error) {
			r, e := svc.AbortPreactiveBattle(ctx, &dsv1.AbortPreactiveBattleRequest{MatchId: 1})
			return r.GetCode(), e
		}},
		{"EnsurePlayerDeparture/removed_surface", commonv1.ErrCode_ERR_SERVICE_DISABLED, func() (commonv1.ErrCode, error) {
			r, e := svc.EnsurePlayerDeparture(ctx, &dsv1.EnsurePlayerDepartureRequest{})
			return r.GetCode(), e
		}},
		{"Heartbeat/match_id=0", commonv1.ErrCode_ERR_INVALID_ARG, func() (commonv1.ErrCode, error) {
			r, e := svc.Heartbeat(ctx, &dsv1.HeartbeatRequest{})
			return r.GetCode(), e
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.call()
			if err != nil || got != tt.want {
				t.Fatalf("code=%v err=%v, want code=%v", got, err, tt.want)
			}
		})
	}
}

type allocatorListBoundaryRepo struct {
	data.BattleRepo
	matchIDs []uint64
	battles  map[uint64]*dsv1.BattleStorageRecord
	err      error
}

func (r *allocatorListBoundaryRepo) RangeActiveBattles(context.Context) ([]uint64, error) {
	if r.err != nil {
		return nil, r.err
	}
	return append([]uint64(nil), r.matchIDs...), nil
}

func (r *allocatorListBoundaryRepo) GetBattle(_ context.Context, matchID uint64) (*dsv1.BattleStorageRecord, bool, error) {
	battle, ok := r.battles[matchID]
	return battle, ok, nil
}

// TestAllocatorListBattlesServiceMapping 补齐无参数门的运维 RPC：既验证成功响应
// 透传筛选结果，也验证业务错误只进入 response code、transport error 始终为 nil。
func TestAllocatorListBattlesServiceMapping(t *testing.T) {
	repo := &allocatorListBoundaryRepo{
		matchIDs: []uint64{11, 12},
		battles: map[uint64]*dsv1.BattleStorageRecord{
			11: {MatchId: 11, DsPodName: "battle-11", State: "running"},
			12: {MatchId: 12, DsPodName: "battle-12", State: "ready"},
		},
	}
	svc := NewAllocatorService(biz.NewAllocatorUsecase(repo, nil, conf.AllocatorConf{}))

	resp, transportErr := svc.ListBattles(context.Background(), &dsv1.ListBattlesRequest{StateFilter: "running"})
	if transportErr != nil {
		t.Fatalf("ListBattles success returned transport error: %v", transportErr)
	}
	if resp.GetCode() != commonv1.ErrCode_OK || len(resp.GetBattles()) != 1 || resp.GetBattles()[0].GetMatchId() != 11 {
		t.Fatalf("ListBattles success mapping: code=%v battles=%+v", resp.GetCode(), resp.GetBattles())
	}

	repo.err = errcode.New(errcode.ErrUnavailable, "active index unavailable")
	resp, transportErr = svc.ListBattles(context.Background(), &dsv1.ListBattlesRequest{})
	if transportErr != nil {
		t.Fatalf("ListBattles business failure returned transport error: %v", transportErr)
	}
	if resp.GetCode() != commonv1.ErrCode_ERR_UNAVAILABLE || len(resp.GetBattles()) != 0 {
		t.Fatalf("ListBattles error mapping: code=%v battles=%+v", resp.GetCode(), resp.GetBattles())
	}
}
