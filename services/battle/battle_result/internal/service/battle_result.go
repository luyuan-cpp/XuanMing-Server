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

	"github.com/luyuancpp/pandora/pkg/errcode"
	battlev1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/battle/v1"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"

	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/biz"
)

// BattleResultService 实现 battlev1.BattleResultServiceServer。
type BattleResultService struct {
	battlev1.UnimplementedBattleResultServiceServer
	uc *biz.BattleResultUsecase
}

// NewBattleResultService 构造。
func NewBattleResultService(uc *biz.BattleResultUsecase) *BattleResultService {
	return &BattleResultService{uc: uc}
}

// ReportResult 同步上报一场对局结算(幂等)。
func (s *BattleResultService) ReportResult(ctx context.Context, req *battlev1.ReportResultRequest) (*battlev1.ReportResultResponse, error) {
	if req.GetResult() == nil || req.GetResult().GetMatchId() == 0 {
		return &battlev1.ReportResultResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	already, err := s.uc.ReportResult(ctx, req.GetResult())
	if err != nil {
		return &battlev1.ReportResultResponse{Code: toProtoCode(err)}, nil
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
