// Package service 是 player_locator 的 RPC 入口层。
//
// 职责:
//   - 实现 locatorv1.PlayerLocatorServiceServer
//   - proto Location / LocationState 与 biz.LocationInput/Output 互转
//   - errcode → proto.ErrCode 翻译(跟 login 服务一致,不抛 grpc error)
package service

import (
	"context"

	"github.com/luyuancpp/pandora/pkg/errcode"

	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"

	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/biz"
)

// LocatorService 实现 locatorv1.PlayerLocatorServiceServer。
type LocatorService struct {
	locatorv1.UnimplementedPlayerLocatorServiceServer

	uc *biz.LocatorUsecase
}

// NewLocatorService 注入 LocatorUsecase。
func NewLocatorService(uc *biz.LocatorUsecase) *LocatorService {
	return &LocatorService{uc: uc}
}

func (s *LocatorService) SetLocation(ctx context.Context, req *locatorv1.SetLocationRequest) (*locatorv1.SetLocationResponse, error) {
	loc := req.GetLocation()
	in := biz.LocationInput{
		PlayerID:  req.GetPlayerId(),
		State:     int32(loc.GetState()),
		HubPod:    loc.GetHubPod(),
		ShardID:   loc.GetShardId(),
		MatchID:   loc.GetMatchId(),
		BattlePod: loc.GetBattlePod(),
	}
	if err := s.uc.SetLocation(ctx, in); err != nil {
		return &locatorv1.SetLocationResponse{Code: toProtoCode(err)}, nil
	}
	return &locatorv1.SetLocationResponse{Code: commonv1.ErrCode_OK}, nil
}

func (s *LocatorService) GetLocation(ctx context.Context, req *locatorv1.GetLocationRequest) (*locatorv1.GetLocationResponse, error) {
	out, err := s.uc.GetLocation(ctx, req.GetPlayerId())
	if err != nil {
		return &locatorv1.GetLocationResponse{Code: toProtoCode(err)}, nil
	}
	return &locatorv1.GetLocationResponse{
		Code: commonv1.ErrCode_OK,
		Location: &locatorv1.Location{
			State:       locatorv1.LocationState(out.State),
			HubPod:      out.HubPod,
			ShardId:     out.ShardID,
			MatchId:     out.MatchID,
			BattlePod:   out.BattlePod,
			UpdatedAtMs: out.UpdatedAtMs,
		},
	}, nil
}

func (s *LocatorService) BatchGetLocation(ctx context.Context, req *locatorv1.BatchGetLocationRequest) (*locatorv1.BatchGetLocationResponse, error) {
	outs, err := s.uc.BatchGetLocation(ctx, req.GetPlayerIds())
	if err != nil {
		return &locatorv1.BatchGetLocationResponse{Code: toProtoCode(err)}, nil
	}
	locations := make(map[uint64]*locatorv1.Location, len(outs))
	for pid, out := range outs {
		locations[pid] = &locatorv1.Location{
			State:       locatorv1.LocationState(out.State),
			HubPod:      out.HubPod,
			ShardId:     out.ShardID,
			MatchId:     out.MatchID,
			BattlePod:   out.BattlePod,
			UpdatedAtMs: out.UpdatedAtMs,
		}
	}
	return &locatorv1.BatchGetLocationResponse{
		Code:      commonv1.ErrCode_OK,
		Locations: locations,
	}, nil
}

// SubscribePresence 客户端打开好友面板 → 订阅这批好友的在线态变更(§13.4.1)。
func (s *LocatorService) SubscribePresence(ctx context.Context, req *locatorv1.SubscribePresenceRequest) (*locatorv1.SubscribePresenceResponse, error) {
	if err := s.uc.SubscribePresence(req.GetSubscriberId(), req.GetWatchedPlayerIds()); err != nil {
		return &locatorv1.SubscribePresenceResponse{Code: toProtoCode(err)}, nil
	}
	return &locatorv1.SubscribePresenceResponse{Code: commonv1.ErrCode_OK}, nil
}

// UnsubscribePresence 关闭好友面板 → 退订(§13.4.1)。
func (s *LocatorService) UnsubscribePresence(ctx context.Context, req *locatorv1.UnsubscribePresenceRequest) (*locatorv1.UnsubscribePresenceResponse, error) {
	if err := s.uc.UnsubscribePresence(req.GetSubscriberId()); err != nil {
		return &locatorv1.UnsubscribePresenceResponse{Code: toProtoCode(err)}, nil
	}
	return &locatorv1.UnsubscribePresenceResponse{Code: commonv1.ErrCode_OK}, nil
}

func (s *LocatorService) ClearLocation(ctx context.Context, req *locatorv1.ClearLocationRequest) (*locatorv1.ClearLocationResponse, error) {
	if err := s.uc.ClearLocation(ctx, req.GetPlayerId()); err != nil {
		return &locatorv1.ClearLocationResponse{Code: toProtoCode(err)}, nil
	}
	return &locatorv1.ClearLocationResponse{Code: commonv1.ErrCode_OK}, nil
}

// toProtoCode 把 pkg/errcode 转成 proto enum(跟 login 一致)。
func toProtoCode(err error) commonv1.ErrCode {
	return commonv1.ErrCode(errcode.As(err))
}
