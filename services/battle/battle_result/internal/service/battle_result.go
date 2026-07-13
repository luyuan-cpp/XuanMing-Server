// Package service 是 battle_result 服务的 gRPC service 层(W4 ③,2026-06-06)。
//
// 职责:
//   - 实现 battlev1.BattleResultServiceServer 接口
//   - proto Request/Response ↔ biz 入参/出参互转
//   - errcode.Code → commonv1.ErrCode 1:1 映射
//
// 说明:ReportResult 同步上报(测试 / 兼容用),正常链路走 kafka 消费;
// 调用方为后端内部 / 运维,不从 ctx 取 player_id。
package service

import (
	"context"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/middleware"
	battlev1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/battle/v1"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"

	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/biz"
)

// BattleResultService 实现 battlev1.BattleResultServiceServer。
type BattleResultService struct {
	battlev1.UnimplementedBattleResultServiceServer
	uc *biz.BattleResultUsecase

	// dsGuard DS 回调令牌守卫(审核 P1 #1);nil = 未启用(mode=off)。
	dsGuard                 *middleware.DSCallbackGuard
	battleCredentialChecker BattleCredentialStateChecker
}

// NewBattleResultService 构造。
func NewBattleResultService(uc *biz.BattleResultUsecase) *BattleResultService {
	return &BattleResultService{uc: uc}
}

// SetDSCallbackGuard 注入 DS 回调令牌守卫(main 按 ds_auth 配置构建;nil 表示 off)。
func (s *BattleResultService) SetDSCallbackGuard(g *middleware.DSCallbackGuard) { s.dsGuard = g }

// SetBattleCredentialStateChecker 启用 Redis active credential 终态门。
func (s *BattleResultService) SetBattleCredentialStateChecker(checker BattleCredentialStateChecker) {
	s.battleCredentialChecker = checker
}

// ReportResult 同步上报一场对局结算(幂等)。
func (s *BattleResultService) ReportResult(ctx context.Context, req *battlev1.ReportResultRequest) (*battlev1.ReportResultResponse, error) {
	if req.GetResult() == nil || req.GetResult().GetMatchId() == 0 {
		return &battlev1.ReportResultResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	// DS 回调范围绑定:battle 令牌 match_id 必须等于上报的 match_id
	// (防拿 A 局令牌伪造 B 局结算;不变量 §9.2 结算幂等 + §9.6 DS 不可信)。
	// RequireToken:纯 DS 回调,enforce 下无令牌直连一律拒(堵绕过 Envoy 的东西向旁路,审核 P1)。
	_, credential, err := s.dsGuard.CheckBattleCredential(ctx, middleware.DSScope{Type: auth.DSTypeBattle, MatchID: req.GetResult().GetMatchId(), RequireToken: true})
	if err != nil {
		return &battlev1.ReportResultResponse{Code: toProtoCode(err)}, nil
	}
	if s.battleCredentialChecker != nil {
		if credential == nil || req.GetResult().GetDsPodName() == "" || req.GetResult().GetDsPodName() != credential.Pod {
			return &battlev1.ReportResultResponse{Code: commonv1.ErrCode_ERR_UNAUTHORIZED}, nil
		}
		if err := s.battleCredentialChecker.CheckActive(ctx, req.GetResult().GetMatchId(), credential); err != nil {
			return &battlev1.ReportResultResponse{Code: toProtoCode(err)}, nil
		}
	}
	already, err := s.uc.ReportResult(ctx, req.GetResult())
	if err != nil {
		return &battlev1.ReportResultResponse{Code: toProtoCode(err)}, nil
	}
	if s.battleCredentialChecker != nil {
		// DB 幂等落库成功后，必须再把完整 active tuple 写成同槽 result receipt。
		// receipt 失败时不回 OK；DS 用同一结果重试会命中 DB 幂等，再次完成 receipt。
		if err := s.battleCredentialChecker.MarkResultRecorded(
			ctx, req.GetResult().GetMatchId(), credential); err != nil {
			return &battlev1.ReportResultResponse{Code: toProtoCode(err)}, nil
		}
	}
	return &battlev1.ReportResultResponse{Code: commonv1.ErrCode_OK, AlreadyRecorded: already}, nil
}

// GetMatchResult 查询一场对局结算。
func (s *BattleResultService) GetMatchResult(ctx context.Context, req *battlev1.GetMatchResultRequest) (*battlev1.GetMatchResultResponse, error) {
	if req.GetMatchId() == 0 {
		return &battlev1.GetMatchResultResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	res, found, err := s.uc.GetMatchResult(ctx, req.GetMatchId())
	if err != nil {
		return &battlev1.GetMatchResultResponse{Code: toProtoCode(err)}, nil
	}
	if !found {
		return &battlev1.GetMatchResultResponse{Code: commonv1.ErrCode_ERR_NOT_FOUND}, nil
	}
	return &battlev1.GetMatchResultResponse{Code: commonv1.ErrCode_OK, Result: res}, nil
}

// ListPlayerHistory 倒序列出玩家战绩历史。
func (s *BattleResultService) ListPlayerHistory(ctx context.Context, req *battlev1.ListPlayerHistoryRequest) (*battlev1.ListPlayerHistoryResponse, error) {
	if req.GetPlayerId() == 0 {
		return &battlev1.ListPlayerHistoryResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	results, err := s.uc.ListPlayerHistory(ctx, req.GetPlayerId(), int(req.GetLimit()), req.GetBeforeMs())
	if err != nil {
		return &battlev1.ListPlayerHistoryResponse{Code: toProtoCode(err)}, nil
	}
	return &battlev1.ListPlayerHistoryResponse{Code: commonv1.ErrCode_OK, Results: results}, nil
}

// toProtoCode 把 pkg/errcode 1:1 映射成 proto enum(数值相同)。
func toProtoCode(err error) commonv1.ErrCode {
	return commonv1.ErrCode(errcode.As(err))
}
