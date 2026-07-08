// Package service 是 hub_allocator 服务的 gRPC service 层(W4 ⑤,2026-06-06)。
//
// 职责:
//   - 实现 hubv1.HubAllocatorServiceServer 接口
//   - proto Request/Response ↔ biz 入参/出参互转
//   - errcode.Code → commonv1.ErrCode 1:1 映射
//
// 说明:调用方是后端内部(login 调 AssignHub、Hub DS 调 Heartbeat),不是玩家客户端,
// 因此不从 ctx 取 player_id;player_id 由 login 等上游服务在请求里显式传入。
//
// 例外:ListHubLines / TransferToLine 是玩家侧 RPC(经 Envoy :8443 客户端面,
// jwt_authn 注入 x-pandora-player-id),player_id 一律从 ctx 取(JWT sub 权威),不信请求体。
package service

import (
	"context"

	"github.com/luyuancpp/pandora/pkg/errcode"
	pmw "github.com/luyuancpp/pandora/pkg/middleware"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"

	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/biz"
)

// HubService 实现 hubv1.HubAllocatorServiceServer。
type HubService struct {
	hubv1.UnimplementedHubAllocatorServiceServer
	uc *biz.HubUsecase
}

// NewHubService 构造 HubService。
func NewHubService(uc *biz.HubUsecase) *HubService {
	return &HubService{uc: uc}
}

// AssignHub 为玩家分配大厅 DS 分片(login 登录成功后调)。
func (s *HubService) AssignHub(ctx context.Context, req *hubv1.AssignHubRequest) (*hubv1.AssignHubResponse, error) {
	if req.GetPlayerId() == 0 {
		return &hubv1.AssignHubResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	res, err := s.uc.AssignHub(ctx, req.GetPlayerId(), req.GetRegion(), req.GetTeamId(), req.GetRoleId())
	if err != nil {
		return &hubv1.AssignHubResponse{Code: toProtoCode(err)}, nil
	}
	return &hubv1.AssignHubResponse{
		Code:       commonv1.ErrCode_OK,
		HubDsAddr:  res.HubDSAddr,
		HubTicket:  res.HubTicket,
		HubPodName: res.HubPodName,
		ShardId:    res.ShardID,
	}, nil
}

// ReleaseHub 玩家离开大厅(登出/进战斗)。
func (s *HubService) ReleaseHub(ctx context.Context, req *hubv1.ReleaseHubRequest) (*hubv1.ReleaseHubResponse, error) {
	if req.GetPlayerId() == 0 {
		return &hubv1.ReleaseHubResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	if err := s.uc.ReleaseHub(ctx, req.GetPlayerId()); err != nil {
		return &hubv1.ReleaseHubResponse{Code: toProtoCode(err)}, nil
	}
	return &hubv1.ReleaseHubResponse{Code: commonv1.ErrCode_OK}, nil
}

// TransferHub 跨分片传送(玩家点传送点)。
func (s *HubService) TransferHub(ctx context.Context, req *hubv1.TransferHubRequest) (*hubv1.TransferHubResponse, error) {
	if req.GetPlayerId() == 0 {
		return &hubv1.TransferHubResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	res, err := s.uc.TransferHub(ctx, req.GetPlayerId(), req.GetTargetHubId())
	if err != nil {
		return &hubv1.TransferHubResponse{Code: toProtoCode(err)}, nil
	}
	return &hubv1.TransferHubResponse{
		Code:         commonv1.ErrCode_OK,
		NewHubDsAddr: res.NewHubDSAddr,
		NewHubTicket: res.NewHubTicket,
	}, nil
}

// ListHubs 列出分片负载(运维/调试)。
func (s *HubService) ListHubs(ctx context.Context, req *hubv1.ListHubsRequest) (*hubv1.ListHubsResponse, error) {
	hubs, err := s.uc.ListHubs(ctx, req.GetRegion())
	if err != nil {
		return &hubv1.ListHubsResponse{Code: toProtoCode(err)}, nil
	}
	return &hubv1.ListHubsResponse{Code: commonv1.ErrCode_OK, Hubs: hubs}, nil
}

// Heartbeat 处理大厅 DS 心跳上报(Hub DS 每 5s 调)。
func (s *HubService) Heartbeat(ctx context.Context, req *hubv1.HeartbeatRequest) (*hubv1.HeartbeatResponse, error) {
	if req.GetHubPodName() == "" {
		return &hubv1.HeartbeatResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	res, err := s.uc.Heartbeat(ctx, req.GetHubPodName(), req.GetPlayerCount(), req.GetState(), req.GetTsMs())
	if err != nil {
		return &hubv1.HeartbeatResponse{Code: toProtoCode(err)}, nil
	}
	// 在线保活:把心跳捎带的在场 player_ids 转发 locator 续 HUB 位置 TTL
	// (biz 内 goroutine 异步 + 独立超时,locator 抖动不拖慢心跳响应)。
	s.uc.RefreshHubPresence(ctx, req.GetHubPodName(), req.GetPlayerIds())
	return &hubv1.HeartbeatResponse{
		Code:         commonv1.ErrCode_OK,
		Command:      res.Command,
		GraceSeconds: res.GraceSeconds,
	}, nil
}

// ListHubLines 列出玩家当前 region 可切换的大厅线路(玩家侧,player_id 取自 JWT sub)。
func (s *HubService) ListHubLines(ctx context.Context, req *hubv1.ListHubLinesRequest) (*hubv1.ListHubLinesResponse, error) {
	playerID := pmw.PlayerIDFromContext(ctx)
	if playerID == 0 {
		return &hubv1.ListHubLinesResponse{Code: commonv1.ErrCode_ERR_UNAUTHORIZED}, nil
	}
	views, err := s.uc.ListHubLinesForPlayer(ctx, playerID, req.GetRegion())
	if err != nil {
		return &hubv1.ListHubLinesResponse{Code: toProtoCode(err)}, nil
	}
	lines := make([]*hubv1.HubLine, 0, len(views))
	for _, v := range views {
		lines = append(lines, &hubv1.HubLine{
			LineNo:      v.LineNo,
			ShardId:     v.ShardID,
			PlayerCount: v.PlayerCount,
			Capacity:    v.Capacity,
			IsFull:      v.IsFull,
			IsCurrent:   v.IsCurrent,
		})
	}
	return &hubv1.ListHubLinesResponse{Code: commonv1.ErrCode_OK, Lines: lines}, nil
}

// TransferToLine 玩家主动切换到指定线路(换实例,player_id 取自 JWT sub)。
func (s *HubService) TransferToLine(ctx context.Context, req *hubv1.TransferToLineRequest) (*hubv1.TransferToLineResponse, error) {
	playerID := pmw.PlayerIDFromContext(ctx)
	if playerID == 0 {
		return &hubv1.TransferToLineResponse{Code: commonv1.ErrCode_ERR_UNAUTHORIZED}, nil
	}
	res, err := s.uc.TransferToLineForPlayer(ctx, playerID, req.GetTargetShardId())
	if err != nil {
		return &hubv1.TransferToLineResponse{Code: toProtoCode(err)}, nil
	}
	return &hubv1.TransferToLineResponse{
		Code:         commonv1.ErrCode_OK,
		NewHubDsAddr: res.NewHubDSAddr,
		NewHubTicket: res.NewHubTicket,
		NewShardId:   res.NewShardID,
		LineNo:       res.LineNo,
	}, nil
}

// ── 辅助 ──────────────────────────────────────────────────────────────────────

// toProtoCode 把 pkg/errcode 1:1 映射成 proto enum(数值相同)。
func toProtoCode(err error) commonv1.ErrCode {
	return commonv1.ErrCode(errcode.As(err))
}
