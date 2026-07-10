// Package service 是 data_service 的 gRPC service 层(2026-06-16)。
//
// 职责:
//   - 实现 datav1.DataServiceServer 接口
//   - proto Request/Response ↔ biz 入参/出参互转
//   - errcode.Code → commonv1.ErrCode 1:1 映射
//
// 身份约定:data_service 是内网服务-to-服务网关(不经 Envoy / 不直接暴露给玩家),
// player_id 取请求体字段,不从 JWT override。由内网 RPC 黑白名单限制调用方。
package service

import (
	"context"

	"github.com/luyuancpp/pandora/pkg/errcode"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	datav1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/data_service/v1"

	"github.com/luyuancpp/pandora/services/data/data_service/internal/biz"
)

// DataService 实现 datav1.DataServiceServer。
type DataService struct {
	datav1.UnimplementedDataServiceServer
	uc *biz.DataUsecase
}

// NewDataService 构造。
func NewDataService(uc *biz.DataUsecase) *DataService {
	return &DataService{uc: uc}
}

// ReadPlayer cache-aside 读玩家数据。无数据 → ERR_NOT_FOUND。
func (s *DataService) ReadPlayer(ctx context.Context, req *datav1.ReadPlayerRequest) (*datav1.ReadPlayerResponse, error) {
	if req.GetPlayerId() == 0 {
		return &datav1.ReadPlayerResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	pd, found, err := s.uc.ReadPlayer(ctx, req.GetPlayerId())
	if err != nil {
		return &datav1.ReadPlayerResponse{Code: toProtoCode(err)}, nil
	}
	if !found {
		return &datav1.ReadPlayerResponse{Code: commonv1.ErrCode_ERR_NOT_FOUND}, nil
	}
	return &datav1.ReadPlayerResponse{Code: commonv1.ErrCode_OK, Data: pd}, nil
}

// WritePlayer 乐观锁版本写。版本不匹配 → ERR_DATA_VERSION_MISMATCH。
// 更新(data.version>0)必须带非空 update_mask,只更新掩码内的业务列(滚动升级时旧副本不清零新列);
// 空掩码更新 → ERR_INVALID_ARG。新建(version==0)整条 INSERT,忽略掩码。
func (s *DataService) WritePlayer(ctx context.Context, req *datav1.WritePlayerRequest) (*datav1.WritePlayerResponse, error) {
	if req.GetData() == nil || req.GetData().GetPlayerId() == 0 {
		return &datav1.WritePlayerResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	newVersion, err := s.uc.WritePlayer(ctx, req.GetData(), req.GetUpdateMask().GetPaths())
	if err != nil {
		return &datav1.WritePlayerResponse{Code: toProtoCode(err)}, nil
	}
	return &datav1.WritePlayerResponse{Code: commonv1.ErrCode_OK, NewVersion: newVersion}, nil
}

// InvalidateCache 主动删缓存。
func (s *DataService) InvalidateCache(ctx context.Context, req *datav1.InvalidateCacheRequest) (*datav1.InvalidateCacheResponse, error) {
	if req.GetPlayerId() == 0 {
		return &datav1.InvalidateCacheResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	if err := s.uc.InvalidateCache(ctx, req.GetPlayerId()); err != nil {
		return &datav1.InvalidateCacheResponse{Code: toProtoCode(err)}, nil
	}
	return &datav1.InvalidateCacheResponse{Code: commonv1.ErrCode_OK}, nil
}

// toProtoCode 把 pkg/errcode 1:1 映射成 proto enum(数值相同)。
func toProtoCode(err error) commonv1.ErrCode {
	return commonv1.ErrCode(errcode.As(err))
}
