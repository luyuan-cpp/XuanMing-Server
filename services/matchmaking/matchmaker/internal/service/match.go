// Package service 是 matchmaker 服务的 gRPC service 层(W4 ①,2026-06-06)。
//
// 职责:
//   - 实现 matchv1.MatchServiceServer 接口
//   - 从 ctx 取 JWT player_id(防伪造他人身份),覆盖 request 中的 player_id
//   - proto Request/Response ↔ biz 入参/出参互转
//   - errcode.Code → commonv1.ErrCode 1:1 映射
//
// 4 个 RPC 全"已受理型"(协议原则 3):返回 code/match_id 后,客户端 UI 状态机
// 由 pandora.match.progress push 驱动。
package service

import (
	"context"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"

	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/biz"
)

// MatchService 实现 matchv1.MatchServiceServer。
type MatchService struct {
	matchv1.UnimplementedMatchServiceServer
	uc *biz.MatchUsecase
	sf snowflakeGen
}

// snowflakeGen 是 snowflake.Node 的最小接口。
type snowflakeGen interface {
	Generate() uint64
}

// NewMatchService 构造 MatchService。
func NewMatchService(uc *biz.MatchUsecase, sf snowflakeGen) *MatchService {
	return &MatchService{uc: uc, sf: sf}
}

// StartMatch 把队伍加入撮合队列。captain_id 以 JWT ctx 为准。
func (s *MatchService) StartMatch(ctx context.Context, req *matchv1.StartMatchRequest) (*matchv1.StartMatchResponse, error) {
	captainID := callerID(ctx)
	if captainID == 0 {
		return &matchv1.StartMatchResponse{Code: commonv1.ErrCode_ERR_UNAUTHORIZED}, nil
	}
	if req.GetTeamId() == 0 {
		return &matchv1.StartMatchResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}

	ticketID := s.sf.Generate()
	id, err := s.uc.StartMatch(ctx, ticketID, req.GetTeamId(), captainID)
	if err != nil {
		return &matchv1.StartMatchResponse{Code: toProtoCode(err)}, nil
	}
	return &matchv1.StartMatchResponse{Code: commonv1.ErrCode_OK, MatchId: id}, nil
}

// CancelMatch 取消匹配。player_id 以 JWT ctx 为准。
func (s *MatchService) CancelMatch(ctx context.Context, _ *matchv1.CancelMatchRequest) (*matchv1.CancelMatchResponse, error) {
	playerID := callerID(ctx)
	if playerID == 0 {
		return &matchv1.CancelMatchResponse{Code: commonv1.ErrCode_ERR_UNAUTHORIZED}, nil
	}
	if err := s.uc.CancelMatch(ctx, playerID); err != nil {
		return &matchv1.CancelMatchResponse{Code: toProtoCode(err)}, nil
	}
	return &matchv1.CancelMatchResponse{Code: commonv1.ErrCode_OK}, nil
}

// ConfirmMatch 确认/拒绝匹配。player_id 以 JWT ctx 为准。
func (s *MatchService) ConfirmMatch(ctx context.Context, req *matchv1.ConfirmMatchRequest) (*matchv1.ConfirmMatchResponse, error) {
	playerID := callerID(ctx)
	if playerID == 0 {
		return &matchv1.ConfirmMatchResponse{Code: commonv1.ErrCode_ERR_UNAUTHORIZED}, nil
	}
	if req.GetMatchId() == 0 {
		return &matchv1.ConfirmMatchResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	if err := s.uc.ConfirmMatch(ctx, playerID, req.GetMatchId(), req.GetAccept()); err != nil {
		return &matchv1.ConfirmMatchResponse{Code: toProtoCode(err)}, nil
	}
	return &matchv1.ConfirmMatchResponse{Code: commonv1.ErrCode_OK}, nil
}

// GetMatchProgress 查询匹配进度。match_id 即授权；READY 阶段额外按 callerID 给本人现签新 battle 票（重连用）。
func (s *MatchService) GetMatchProgress(ctx context.Context, req *matchv1.GetMatchProgressRequest) (*matchv1.GetMatchProgressResponse, error) {
	if req.GetMatchId() == 0 {
		return &matchv1.GetMatchProgressResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	prog, err := s.uc.GetMatchProgress(ctx, callerID(ctx), req.GetMatchId())
	if err != nil {
		return &matchv1.GetMatchProgressResponse{Code: toProtoCode(err)}, nil
	}
	return &matchv1.GetMatchProgressResponse{Code: commonv1.ErrCode_OK, Progress: prog}, nil
}

// ── 辅助 ──────────────────────────────────────────────────────────────────────

// callerID 从 ctx 取 JWT 注入的 player_id。
func callerID(ctx context.Context) uint64 {
	id, _ := ctx.Value(plog.CtxKeyPlayerID).(uint64)
	return id
}

// toProtoCode 把 pkg/errcode 1:1 映射成 proto enum(数值相同)。
func toProtoCode(err error) commonv1.ErrCode {
	return commonv1.ErrCode(errcode.As(err))
}
