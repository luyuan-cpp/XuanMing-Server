// Package service 是 login 服务的 RPC 入口层。
//
// 职责:
//   - 实现 loginv1.LoginServiceServer 接口
//   - proto Request/Response 与 biz 入参/出参互转
//   - errcode.*Error 翻译成 proto.LoginResponse.code(不抛 grpc error,客户端永远看 code 字段)
//
// 不变量(docs/design/protocol-ordering-rules.md 原则 1):
//   - "立即完成型 RPC" 的 response 必须包含完整业务数据,客户端不等任何后续 push
package service

import (
	"context"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/middleware"

	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	loginv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/login/v1"

	"github.com/luyuancpp/pandora/services/account/login/internal/biz"
	"github.com/luyuancpp/pandora/services/account/login/internal/data"
)

// LoginService 实现 loginv1.LoginServiceServer。
//
// 内嵌 UnimplementedLoginServiceServer 以满足 grpc 向前兼容约束。
//
// W3 ①(2026-06-05):IssueDSTicket / VerifyDSTicket 接 pkg/auth 真实化。
// Login() 返回的 session_token / hub_ticket 也都是 HS256 JWT(由 LoginUsecase 内部签)。
type LoginService struct {
	loginv1.UnimplementedLoginServiceServer

	loginUC  *biz.LoginUsecase
	ticketUC *biz.TicketUsecase

	// redisDSAdmission 仅由 authority_mode=redis + mode=enforce 的 main 开启。
	// guard/checker 任一缺失都 fail-closed，绝不回退 legacy Verify。
	redisDSAdmission bool
	dsGuard          *middleware.DSCallbackGuard
	admissionChecker data.DSAdmissionChecker
}

// NewLoginService 注入 LoginUsecase + TicketUsecase。
func NewLoginService(loginUC *biz.LoginUsecase, ticketUC *biz.TicketUsecase) *LoginService {
	return &LoginService{loginUC: loginUC, ticketUC: ticketUC}
}

// SetRedisDSAdmissionAuthority 启用 VerifyDSTicket 的 DS 在线 active 权威门。
func (s *LoginService) SetRedisDSAdmissionAuthority(guard *middleware.DSCallbackGuard, checker data.DSAdmissionChecker) {
	s.redisDSAdmission = true
	s.dsGuard = guard
	s.admissionChecker = checker
}

// Login 立即完成型(参考 proto/pandora/login/v1/login.proto 注释)。
func (s *LoginService) Login(ctx context.Context, req *loginv1.LoginRequest) (*loginv1.LoginResponse, error) {
	res, err := s.loginUC.Login(ctx, req.GetAccount(), req.GetPasswordHash(), req.GetDeviceId())
	if err != nil {
		return &loginv1.LoginResponse{
			Code: toProtoCode(err),
		}, nil
	}
	return &loginv1.LoginResponse{
		Code:         commonv1.ErrCode_OK,
		PlayerId:     res.PlayerID,
		SessionToken: res.SessionToken,
		HubDsAddr:    res.HubDSAddr,
		HubTicket:    res.HubTicket,
		RegionId:     res.RegionID,
		CellId:       res.CellID,
		// 断线重连(docs/design/battle-reconnect.md):命中时非空,客户端直连 battle DS 重连;
		// 未命中时为空(零值),客户端走 hub_ds_addr / hub_ticket 进大厅。
		BattleDsAddr: res.BattleDSAddr,
		BattleTicket: res.BattleTicket,
		MatchId:      res.MatchID,
		// 选角权威化(2026-07-08):玩家当前已选角色(0=从未选过),客户端选角界面预选中用。
		SelectedRoleId: res.SelectedRoleID,
		ResumeContext:  resumeContextToProto(res.Resume),
	}, nil
}

func (s *LoginService) GetResumeContext(ctx context.Context, req *loginv1.GetResumeContextRequest) (*loginv1.GetResumeContextResponse, error) {
	out, err := s.loginUC.GetResumeContext(ctx, req.GetSessionToken())
	if err != nil {
		return &loginv1.GetResumeContextResponse{Code: toProtoCode(err)}, nil
	}
	return &loginv1.GetResumeContextResponse{Code: commonv1.ErrCode_OK, Context: resumeContextToProto(out)}, nil
}

func resumeContextToProto(in biz.ResumeContextResult) *loginv1.ResumeContext {
	return &loginv1.ResumeContext{Route: in.Route, MatchId: in.MatchID,
		MatchStage: in.MatchStage, GameMode: in.GameMode}
}

// SelectRole 立即完成型(选角权威化 2026-07-08,见 login.proto SelectRole 注释)。
//
// player_id 从 ctx 读(Envoy jwt_authn 验 session 后注入 x-pandora-player-id,
// middleware/auth 提进 ctx,与 IssueDSTicket 同纪律),请求体不信任自报 player_id。
func (s *LoginService) SelectRole(ctx context.Context, req *loginv1.SelectRoleRequest) (*loginv1.SelectRoleResponse, error) {
	playerID, _ := ctx.Value(plog.CtxKeyPlayerID).(uint64)
	if playerID == 0 {
		plog.With(ctx).Warnw("msg", "select_role_no_player_id")
		return &loginv1.SelectRoleResponse{Code: commonv1.ErrCode_ERR_UNAUTHORIZED}, nil
	}
	addr, ticket, _, err := s.loginUC.SelectRole(ctx, playerID, req.GetRoleId())
	if err != nil {
		return &loginv1.SelectRoleResponse{Code: toProtoCode(err)}, nil
	}
	return &loginv1.SelectRoleResponse{
		Code:      commonv1.ErrCode_OK,
		HubDsAddr: addr,
		HubTicket: ticket,
	}, nil
}

// Logout 立即完成型。
func (s *LoginService) Logout(ctx context.Context, req *loginv1.LogoutRequest) (*loginv1.LogoutResponse, error) {
	if err := s.loginUC.Logout(ctx, req.GetSessionToken()); err != nil {
		return &loginv1.LogoutResponse{Code: toProtoCode(err)}, nil
	}
	return &loginv1.LogoutResponse{Code: commonv1.ErrCode_OK}, nil
}

// IssueDSTicket 立即完成型,W3 ① 真实化:
//   - 校验 req.SessionToken(委托给 TicketUsecase 内部走 verifier;此处直接信任 Envoy 已校验)
//   - 用 Signer 签 ds 票据,exp 默认 5min
//
// W2 阶段调用方传 session_token,W3 ① 暂不二次解 session(Envoy jwt_authn 已校验过),
// player_id 直接从 ctx 的 player_id(由 middleware/auth 从 x-pandora-player-id 头注入)读。
//
// W3 ②:加 jti SETNX EX 5min 防重放,加 session 在线检查。
func (s *LoginService) IssueDSTicket(ctx context.Context, req *loginv1.IssueDSTicketRequest) (*loginv1.IssueDSTicketResponse, error) {
	playerID, _ := ctx.Value(plog.CtxKeyPlayerID).(uint64)
	if playerID == 0 {
		plog.With(ctx).Warnw("msg", "ds_ticket_issue_no_player_id")
		return &loginv1.IssueDSTicketResponse{Code: commonv1.ErrCode_ERR_UNAUTHORIZED}, nil
	}

	// ds_type=hub:复用登录的 hub 分配链路(hub_allocator.AssignHub),返回"当前有效"的大厅地址
	// + 全新一次性票据。结算返回大厅必须走这条路,以应对 Hub DS 被 Agones 重建/换端口/换分片
	// (客户端登录时缓存的旧地址会失效)。battle 票据仍由 ticketUC 仅签发(地址来自 matchmaker)。
	if req.GetDsType() == "hub" {
		// target_id 历史上携带来源 match;现在仅作日志参考,路由权威是
		// locator 租约 + match 三态门(biz.ResolveHubEndpointFromMatch)。
		addr, ticket, _, err := s.loginUC.ResolveHubEndpointFromMatch(ctx, playerID, req.GetTargetId())
		if err != nil {
			return &loginv1.IssueDSTicketResponse{Code: toProtoCode(err)}, nil
		}
		return &loginv1.IssueDSTicketResponse{
			Code:      commonv1.ErrCode_OK,
			Ticket:    ticket,
			HubDsAddr: addr,
		}, nil
	}

	if req.GetDsType() == "battle" {
		_, ticket, _, err := s.loginUC.ResolveBattleEndpoint(ctx, playerID, req.GetTargetId())
		if err != nil {
			return &loginv1.IssueDSTicketResponse{Code: toProtoCode(err)}, nil
		}
		return &loginv1.IssueDSTicketResponse{Code: commonv1.ErrCode_OK, Ticket: ticket}, nil
	}

	res, err := s.ticketUC.IssueDSTicket(ctx, playerID, req.GetDsType(), req.GetTargetId())
	if err != nil {
		return &loginv1.IssueDSTicketResponse{Code: toProtoCode(err)}, nil
	}
	return &loginv1.IssueDSTicketResponse{
		Code:   commonv1.ErrCode_OK,
		Ticket: res.Ticket,
	}, nil
}

// VerifyDSTicket 立即完成型,W3 ① 真实化(验签 + exp + iss + aud)。
//
// Envoy 客户端面 :8443 对本 path 精确 403；唯一网关入口是 :8444 exact route。
// Redis authority 下还必须通过 DS Bearer + active/projection，网络位置本身不构成身份。
// 不变量 §3:本方法返回的 claims.exp 必须严格短时效。
func (s *LoginService) VerifyDSTicket(ctx context.Context, req *loginv1.VerifyDSTicketRequest) (*loginv1.VerifyDSTicketResponse, error) {
	var (
		claims *biz.DSTicketClaims
		err    error
	)
	if s.redisDSAdmission {
		// ds_pod_name 是 Guard 的范围输入；空值不能退化成“不校验 pod”。
		if req.GetDsPodName() == "" {
			return &loginv1.VerifyDSTicketResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
		}
		if s.dsGuard == nil || s.admissionChecker == nil {
			return &loginv1.VerifyDSTicketResponse{Code: commonv1.ErrCode_ERR_UNAVAILABLE}, nil
		}
		// 固定线性顺序：① Bearer 验签+请求 pod scope；② Redis active；
		// ③ TicketUsecase 比对玩家票 binding/assignment；④ 原子 MarkUsedByAdmission。
		_, credential, guardErr := s.dsGuard.CheckCredential(ctx, middleware.DSScope{
			Pod: req.GetDsPodName(), RequireToken: true,
		})
		if guardErr != nil {
			return &loginv1.VerifyDSTicketResponse{Code: toProtoCode(guardErr)}, nil
		}
		if credential == nil {
			return &loginv1.VerifyDSTicketResponse{Code: commonv1.ErrCode_ERR_UNAUTHORIZED}, nil
		}
		admission, activeErr := s.admissionChecker.CheckActive(ctx, req.GetDsPodName(), credential)
		if activeErr != nil {
			return &loginv1.VerifyDSTicketResponse{Code: toProtoCode(activeErr)}, nil
		}
		claims, err = s.ticketUC.VerifyDSTicketForAdmission(
			ctx, req.GetTicket(), req.GetDsPodName(), req.GetAdmissionId(), admission)
	} else {
		// off/legacy 完整保留既有内部 Verify 语义与单次 JTI SETNX。
		claims, err = s.ticketUC.VerifyDSTicket(ctx, req.GetTicket(), req.GetDsPodName())
	}
	if err != nil {
		return &loginv1.VerifyDSTicketResponse{Code: toProtoCode(err)}, nil
	}
	return &loginv1.VerifyDSTicketResponse{
		Code: commonv1.ErrCode_OK,
		Claims: &loginv1.DSTicket{
			PlayerId:             claims.PlayerID,
			MatchId:              claims.MatchID,
			IssuedAtMs:           claims.IssuedAtMs,
			ExpiresAtMs:          claims.ExpiresAtMs,
			DsType:               claims.DSType,
			Jti:                  claims.JTI,
			RegionId:             claims.RegionID,
			CellId:               claims.CellID,
			RoleId:               claims.RoleID,
			DsPodName:            claims.DSPodName,
			DsInstanceUid:        claims.DSInstanceUID,
			DsProtocolEpoch:      claims.DSProtocolEpoch,
			DsCredentialGen:      claims.DSCredentialGen,
			DsCredentialJti:      claims.DSCredentialJTI,
			HubAssignmentId:      claims.HubAssignmentID,
			DsWriterEpoch:        claims.DSWriterEpoch,
			DstVer:          uint32(claims.Version),
			DsInstanceEpoch: claims.DSInstanceEpoch,
			AllocationId:    claims.AllocationID,
			ReleaseTrack:    claims.ReleaseTrack,
		},
	}, nil
}

// toProtoCode 把 pkg/errcode 转成 proto enum。
//
// pkg/errcode.Code 是 int32,proto enum 数值跟它 1:1 对齐
// (见 proto/pandora/common/v1/errcode.proto 上的"errcode 双向同步纪律"注释)。
func toProtoCode(err error) commonv1.ErrCode {
	c := errcode.As(err)
	return commonv1.ErrCode(c)
}
