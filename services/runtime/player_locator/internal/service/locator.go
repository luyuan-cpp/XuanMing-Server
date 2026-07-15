// Package service 是 player_locator 的 RPC 入口层。
//
// 职责:
//   - 实现 locatorv1.PlayerLocatorServiceServer
//   - proto Location / LocationState 与 biz.LocationInput/Output 互转
//   - errcode → proto.ErrCode 翻译(跟 login 服务一致,不抛 grpc error)
package service

import (
	"context"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/middleware"
	"github.com/luyuancpp/pandora/pkg/placement"

	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"

	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/biz"
)

// LocatorService 实现 locatorv1.PlayerLocatorServiceServer。
type LocatorService struct {
	locatorv1.UnimplementedPlayerLocatorServiceServer

	uc          *biz.LocatorUsecase
	placementUC *biz.PlacementUsecase

	// dsGuard DS 回调令牌守卫(审核 P1 #1);nil = 未启用(mode=off)。
	dsGuard *middleware.DSCallbackGuard

	// hubCredentialChecker 仅在 ds_auth.authority_mode=redis + enforce 时注入。
	// nil 表示 legacy/off/permissive，不改变既有行为。
	hubCredentialChecker HubCredentialStateChecker
}

// NewLocatorService 注入 LocatorUsecase。
func NewLocatorService(uc *biz.LocatorUsecase) *LocatorService {
	return &LocatorService{uc: uc}
}

func (s *LocatorService) SetPlacementUsecase(uc *biz.PlacementUsecase) { s.placementUC = uc }

// SetDSCallbackGuard 注入 DS 回调令牌守卫(main 按 ds_auth 配置构建;nil 表示 off)。
func (s *LocatorService) SetDSCallbackGuard(g *middleware.DSCallbackGuard) { s.dsGuard = g }

// SetHubCredentialStateChecker 注入 Model B Redis active credential 终态门。
func (s *LocatorService) SetHubCredentialStateChecker(c HubCredentialStateChecker) {
	s.hubCredentialChecker = c
}

func (s *LocatorService) SetLocation(ctx context.Context, req *locatorv1.SetLocationRequest) (*locatorv1.SetLocationResponse, error) {
	loc := req.GetLocation()
	// DS 回调范围绑定:Hub DS 只能写 HUB 状态且 pod 必须与令牌 sub 一致;
	// 其余状态(MATCHING/BATTLE/OFFLINE 等)只允许内部服务写(matchmaker/ds_allocator/login),
	// 来自 DS 网关或带 DS 令牌的请求一律拒(DenyDS)。
	// 全仓确认:写 HUB 状态的唯一合法调用者是 Hub DS(经回调令牌),无任何内部 Go 服务写 HUB
	// (login→LOGIN_PENDING、matchmaker→MATCHING/BATTLE、ds_allocator→BATTLE),故 HUB 分支置
	// RequireToken:enforce 下无令牌直连(绕过 Envoy)一律拒(fail-closed,审核 P1)。
	scope := middleware.DSScope{DenyDS: true}
	if loc.GetState() == locatorv1.LocationState_LOCATION_STATE_HUB {
		scope = middleware.DSScope{Type: auth.DSTypeHub, Pod: loc.GetHubPod(), RequireToken: true}
	}
	_, cred, err := s.dsGuard.CheckHubCredential(ctx, scope)
	if err != nil {
		return &locatorv1.SetLocationResponse{Code: toProtoCode(err)}, nil
	}
	if loc.GetState() == locatorv1.LocationState_LOCATION_STATE_HUB && s.hubCredentialChecker != nil {
		if err := s.hubCredentialChecker.CheckActive(ctx, loc.GetHubPod(), cred); err != nil {
			return &locatorv1.SetLocationResponse{Code: toProtoCode(err)}, nil
		}
	}
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

// RefreshHubLocations Hub DS 心跳捎带的在线保活:批量续期 HUB 位置 TTL
// (hub_allocator 转发,只续 state==HUB 且 hub_pod 匹配的记录)。
func (s *LocatorService) RefreshHubLocations(ctx context.Context, req *locatorv1.RefreshHubLocationsRequest) (*locatorv1.RefreshHubLocationsResponse, error) {
	_, cred, err := s.dsGuard.CheckHubCredential(ctx, middleware.DSScope{
		Type: auth.DSTypeHub, Pod: req.GetHubPod(), RequireToken: true,
	})
	if err != nil {
		return &locatorv1.RefreshHubLocationsResponse{Code: toProtoCode(err)}, nil
	}
	if s.hubCredentialChecker != nil {
		if err := s.hubCredentialChecker.CheckActive(ctx, req.GetHubPod(), cred); err != nil {
			return &locatorv1.RefreshHubLocationsResponse{Code: toProtoCode(err)}, nil
		}
	}
	refreshed, err := s.uc.RefreshHubLocations(ctx, req.GetHubPod(), req.GetPlayerIds())
	if err != nil {
		return &locatorv1.RefreshHubLocationsResponse{Code: toProtoCode(err)}, nil
	}
	return &locatorv1.RefreshHubLocationsResponse{
		Code:      commonv1.ErrCode_OK,
		Refreshed: int32(refreshed),
	}, nil
}

// ReportDisconnect 快速断线上报:Hub DS 在玩家 Logout / 连接超时时调用,
// 把 HUB 位置 TTL 缩到 grace(只缩 state==HUB 且 hub_pod 匹配的记录,只缩不涨)。
func (s *LocatorService) ReportDisconnect(ctx context.Context, req *locatorv1.ReportDisconnectRequest) (*locatorv1.ReportDisconnectResponse, error) {
	// DS 回调范围绑定:hub 令牌 sub 必须等于 req.hub_pod(防伪造别的 pod 缩别人 TTL)。
	// 全仓确认:ReportDisconnect 唯一合法调用者是 Hub DS,无任何内部 Go 服务调用,故置 RequireToken
	// —— enforce 下无令牌直连(绕过 Envoy)一律拒(fail-closed,审核 P1)。
	_, cred, err := s.dsGuard.CheckHubCredential(ctx, middleware.DSScope{Type: auth.DSTypeHub, Pod: req.GetHubPod(), RequireToken: true})
	if err != nil {
		return &locatorv1.ReportDisconnectResponse{Code: toProtoCode(err)}, nil
	}
	if s.hubCredentialChecker != nil {
		if err := s.hubCredentialChecker.CheckActive(ctx, req.GetHubPod(), cred); err != nil {
			return &locatorv1.ReportDisconnectResponse{Code: toProtoCode(err)}, nil
		}
	}
	shrunk, err := s.uc.ReportDisconnect(ctx, req.GetHubPod(), req.GetPlayerId())
	if err != nil {
		return &locatorv1.ReportDisconnectResponse{Code: toProtoCode(err)}, nil
	}
	return &locatorv1.ReportDisconnectResponse{
		Code:   commonv1.ErrCode_OK,
		Shrunk: shrunk,
	}, nil
}

func (s *LocatorService) ClearLocation(ctx context.Context, req *locatorv1.ClearLocationRequest) (*locatorv1.ClearLocationResponse, error) {
	if err := s.uc.ClearLocation(ctx, req.GetPlayerId()); err != nil {
		return &locatorv1.ClearLocationResponse{Code: toProtoCode(err)}, nil
	}
	return &locatorv1.ClearLocationResponse{Code: commonv1.ErrCode_OK}, nil
}

func (s *LocatorService) GetPlacement(ctx context.Context, req *locatorv1.GetPlacementRequest) (*locatorv1.GetPlacementResponse, error) {
	if s.placementUC == nil {
		return &locatorv1.GetPlacementResponse{Code: commonv1.ErrCode_ERR_UNAVAILABLE}, nil
	}
	rec, found, err := s.placementUC.Get(ctx, req.GetPlayerId())
	if err != nil {
		return &locatorv1.GetPlacementResponse{Code: toProtoCode(err)}, nil
	}
	return &locatorv1.GetPlacementResponse{Code: commonv1.ErrCode_OK, Found: found, Placement: rec}, nil
}

func (s *LocatorService) BeginPlacementTransition(ctx context.Context, req *locatorv1.BeginPlacementTransitionRequest) (*locatorv1.BeginPlacementTransitionResponse, error) {
	if s.placementUC == nil {
		return &locatorv1.BeginPlacementTransitionResponse{Code: commonv1.ErrCode_ERR_UNAVAILABLE}, nil
	}
	rec, err := s.placementUC.Begin(ctx, biz.BeginPlacementInput{
		PlayerID: req.GetPlayerId(), ExpectedVersion: req.GetExpectedVersion(), TargetRoute: req.GetTargetRoute(),
		OperationID: req.GetOperationId(), SourceMatchID: req.GetSourceMatchId(), ProofType: req.GetProofType(),
		ProofID: req.GetProofId(), LeaseDeadlineMs: req.GetLeaseDeadlineMs(), TargetMatchID: req.GetTargetMatchId(),
		ProofSignature: req.GetProofSignature(),
	})
	if err != nil {
		return &locatorv1.BeginPlacementTransitionResponse{Code: toProtoCode(err)}, nil
	}
	return &locatorv1.BeginPlacementTransitionResponse{Code: commonv1.ErrCode_OK, Placement: rec}, nil
}

func (s *LocatorService) BindPlacementTarget(ctx context.Context, req *locatorv1.BindPlacementTargetRequest) (*locatorv1.BindPlacementTargetResponse, error) {
	if s.placementUC == nil {
		return &locatorv1.BindPlacementTargetResponse{Code: commonv1.ErrCode_ERR_UNAVAILABLE}, nil
	}
	rec, err := s.placementUC.Bind(ctx, biz.BindPlacementInput{
		PlayerID: req.GetPlayerId(), Version: req.GetPlacementVersion(), OperationID: req.GetOperationId(),
		TargetRoute: req.GetTargetRoute(), PodName: req.GetDsPodName(), InstanceUID: req.GetDsInstanceUid(),
		AssignmentID: req.GetHubAssignmentId(), TargetMatchID: req.GetTargetMatchId(),
		InstanceEpoch: req.GetDsInstanceEpoch(), AllocationID: req.GetAllocationId(), ReleaseTrack: req.GetReleaseTrack(),
		LeaseDeadlineMs: req.GetLeaseDeadlineMs(),
	})
	if err != nil {
		return &locatorv1.BindPlacementTargetResponse{Code: toProtoCode(err)}, nil
	}
	return &locatorv1.BindPlacementTargetResponse{Code: commonv1.ErrCode_OK, Placement: rec}, nil
}

func (s *LocatorService) RetargetPlacementTarget(ctx context.Context, req *locatorv1.RetargetPlacementTargetRequest) (*locatorv1.RetargetPlacementTargetResponse, error) {
	if s.placementUC == nil {
		return &locatorv1.RetargetPlacementTargetResponse{Code: commonv1.ErrCode_ERR_UNAVAILABLE}, nil
	}
	expected, replacement := req.GetExpectedTarget(), req.GetReplacementTarget()
	rec, err := s.placementUC.Retarget(ctx, biz.RetargetPlacementInput{
		PlayerID: req.GetPlayerId(), Version: req.GetPlacementVersion(), OperationID: req.GetOperationId(),
		TargetRoute: req.GetTargetRoute(), TargetMatchID: req.GetTargetMatchId(),
		ExpectedTarget: placementTarget(expected), ReplacementVersion: req.GetReplacementVersion(),
		ReplacementOperationID: req.GetReplacementOperationId(), ReplacementTarget: placementTarget(replacement),
		ProofType: req.GetProofType(), Reason: req.GetReason(), ProofID: req.GetProofId(),
		ProofSignature: req.GetProofSignature(), LeaseDeadlineMs: req.GetLeaseDeadlineMs(),
	})
	if err != nil {
		return &locatorv1.RetargetPlacementTargetResponse{Code: toProtoCode(err)}, nil
	}
	return &locatorv1.RetargetPlacementTargetResponse{Code: commonv1.ErrCode_OK, Placement: rec}, nil
}

func placementTarget(in *locatorv1.PlacementTargetIdentity) placement.Target {
	if in == nil {
		return placement.Target{}
	}
	return placement.Target{PodName: in.GetDsPodName(), InstanceUID: in.GetDsInstanceUid(),
		InstanceEpoch: in.GetDsInstanceEpoch(), AssignmentID: in.GetHubAssignmentId(),
		AllocationID: in.GetAllocationId(), ReleaseTrack: in.GetReleaseTrack()}
}

func (s *LocatorService) CommitPlacementAdmission(ctx context.Context, req *locatorv1.CommitPlacementAdmissionRequest) (*locatorv1.CommitPlacementAdmissionResponse, error) {
	if s.placementUC == nil {
		return &locatorv1.CommitPlacementAdmissionResponse{Code: commonv1.ErrCode_ERR_UNAVAILABLE}, nil
	}
	rec, err := s.placementUC.Commit(ctx, biz.CommitPlacementInput{
		BindPlacementInput: biz.BindPlacementInput{
			PlayerID: req.GetPlayerId(), Version: req.GetPlacementVersion(), OperationID: req.GetOperationId(),
			TargetRoute: req.GetTargetRoute(), PodName: req.GetDsPodName(), InstanceUID: req.GetDsInstanceUid(),
			AssignmentID: req.GetHubAssignmentId(), TargetMatchID: req.GetTargetMatchId(),
			InstanceEpoch: req.GetDsInstanceEpoch(), AllocationID: req.GetAllocationId(), ReleaseTrack: req.GetReleaseTrack(),
		}, AdmissionID: req.GetAdmissionId(),
	})
	if err != nil {
		return &locatorv1.CommitPlacementAdmissionResponse{Code: toProtoCode(err)}, nil
	}
	return &locatorv1.CommitPlacementAdmissionResponse{Code: commonv1.ErrCode_OK, Committed: true, Placement: rec}, nil
}

func (s *LocatorService) BootstrapPlacement(ctx context.Context, req *locatorv1.BootstrapPlacementRequest) (*locatorv1.BootstrapPlacementResponse, error) {
	if s.placementUC == nil {
		return &locatorv1.BootstrapPlacementResponse{Code: commonv1.ErrCode_ERR_UNAVAILABLE}, nil
	}
	rec, err := s.placementUC.Bootstrap(ctx, biz.BootstrapPlacementInput{
		PlayerID: req.GetPlayerId(), OperationID: req.GetOperationId(), ProofID: req.GetProofId(),
		ProofSignature: req.GetProofSignature(), LeaseDeadlineMs: req.GetLeaseDeadlineMs(),
	})
	if err != nil {
		return &locatorv1.BootstrapPlacementResponse{Code: toProtoCode(err)}, nil
	}
	return &locatorv1.BootstrapPlacementResponse{Code: commonv1.ErrCode_OK, Placement: rec}, nil
}

// toProtoCode 把 pkg/errcode 转成 proto enum(跟 login 一致)。
func toProtoCode(err error) commonv1.ErrCode {
	return commonv1.ErrCode(errcode.As(err))
}
