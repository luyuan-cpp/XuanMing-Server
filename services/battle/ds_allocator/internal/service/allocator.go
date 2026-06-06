// Package service 是 ds_allocator 服务的 gRPC service 层(W4 ②,2026-06-06)。
//
// 职责:
//   - 实现 dsv1.DSAllocatorServiceServer 接口
//   - proto Request/Response ↔ biz 入参/出参互转
//   - errcode.Code → commonv1.ErrCode 1:1 映射
//
// 说明:本服务调用方是后端内部(matchmaker 调 AllocateBattle/ReleaseBattle、
// 战斗 DS 调 Heartbeat),不是玩家客户端,因此不从 ctx 取 player_id。
package service

import (
	"context"

	"github.com/luyuancpp/pandora/pkg/errcode"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"

	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/biz"
)

// AllocatorService 实现 dsv1.DSAllocatorServiceServer。
type AllocatorService struct {
	dsv1.UnimplementedDSAllocatorServiceServer
	uc *biz.AllocatorUsecase
}

// NewAllocatorService 构造 AllocatorService。
func NewAllocatorService(uc *biz.AllocatorUsecase) *AllocatorService {
	return &AllocatorService{uc: uc}
}

// AllocateBattle 为 match 申请战斗 DS(matchmaker 全员确认后调)。
func (s *AllocatorService) AllocateBattle(ctx context.Context, req *dsv1.AllocateBattleRequest) (*dsv1.AllocateBattleResponse, error) {
	if req.GetMatchId() == 0 {
		return &dsv1.AllocateBattleResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	res, err := s.uc.AllocateBattle(ctx, req.GetMatchId(), req.GetPlayerIds(), req.GetMapId(), req.GetGameMode())
	if err != nil {
		return &dsv1.AllocateBattleResponse{Code: toProtoCode(err)}, nil
	}
	return &dsv1.AllocateBattleResponse{
		Code:          commonv1.ErrCode_OK,
		DsAddr:        res.DSAddr,
		DsPodName:     res.DSPodName,
		AllocatedAtMs: res.AllocatedAtMs,
	}, nil
}

// ReleaseBattle 回收战斗 DS(对局结束/异常)。
func (s *AllocatorService) ReleaseBattle(ctx context.Context, req *dsv1.ReleaseBattleRequest) (*dsv1.ReleaseBattleResponse, error) {
	if req.GetMatchId() == 0 {
		return &dsv1.ReleaseBattleResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	if err := s.uc.ReleaseBattle(ctx, req.GetMatchId(), req.GetReason()); err != nil {
		return &dsv1.ReleaseBattleResponse{Code: toProtoCode(err)}, nil
	}
	return &dsv1.ReleaseBattleResponse{Code: commonv1.ErrCode_OK}, nil
}

// Heartbeat 处理战斗 DS 心跳上报(DS 每 5s 调)。
func (s *AllocatorService) Heartbeat(ctx context.Context, req *dsv1.HeartbeatRequest) (*dsv1.HeartbeatResponse, error) {
	if req.GetMatchId() == 0 {
		return &dsv1.HeartbeatResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	res, err := s.uc.Heartbeat(ctx, req.GetMatchId(), req.GetDsPodName(), req.GetPlayerCount(), req.GetState(), req.GetTsMs())
	if err != nil {
		return &dsv1.HeartbeatResponse{Code: toProtoCode(err)}, nil
	}
	return &dsv1.HeartbeatResponse{Code: commonv1.ErrCode_OK, Command: res.Command}, nil
}

// ListBattles 列出当前战斗实例(运维/调试)。
func (s *AllocatorService) ListBattles(ctx context.Context, req *dsv1.ListBattlesRequest) (*dsv1.ListBattlesResponse, error) {
	battles, err := s.uc.ListBattles(ctx, req.GetStateFilter())
	if err != nil {
		return &dsv1.ListBattlesResponse{Code: toProtoCode(err)}, nil
	}
	return &dsv1.ListBattlesResponse{Code: commonv1.ErrCode_OK, Battles: battles}, nil
}

// ── 辅助 ──────────────────────────────────────────────────────────────────────

// toProtoCode 把 pkg/errcode 1:1 映射成 proto enum(数值相同)。
func toProtoCode(err error) commonv1.ErrCode {
	return commonv1.ErrCode(errcode.As(err))
}
