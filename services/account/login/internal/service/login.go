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

	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	loginv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/login/v1"

	"github.com/luyuancpp/pandora/services/account/login/internal/biz"
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
}

// NewLoginService 注入 LoginUsecase + TicketUsecase。
func NewLoginService(loginUC *biz.LoginUsecase, ticketUC *biz.TicketUsecase) *LoginService {
	return &LoginService{loginUC: loginUC, ticketUC: ticketUC}
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
		addr, ticket, _, err := s.loginUC.ResolveHubEndpoint(ctx, playerID)
		if err != nil {
			return &loginv1.IssueDSTicketResponse{Code: toProtoCode(err)}, nil
		}
		return &loginv1.IssueDSTicketResponse{
			Code:      commonv1.ErrCode_OK,
			Ticket:    ticket,
			HubDsAddr: addr,
		}, nil
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
// ⚠️ Envoy 应该用 ext_authz / route 限制本 path 只允许内网(DS 调,不暴露给玩家客户端)。
// 不变量 §3:本方法返回的 claims.exp 必须严格短时效。
func (s *LoginService) VerifyDSTicket(ctx context.Context, req *loginv1.VerifyDSTicketRequest) (*loginv1.VerifyDSTicketResponse, error) {
	claims, err := s.ticketUC.VerifyDSTicket(ctx, req.GetTicket(), req.GetDsPodName())
	if err != nil {
		return &loginv1.VerifyDSTicketResponse{Code: toProtoCode(err)}, nil
	}
	return &loginv1.VerifyDSTicketResponse{
		Code: commonv1.ErrCode_OK,
		Claims: &loginv1.DSTicket{
			PlayerId:    claims.PlayerID,
			MatchId:     claims.MatchID,
			IssuedAtMs:  claims.IssuedAtMs,
			ExpiresAtMs: claims.ExpiresAtMs,
			DsType:      claims.DSType,
			Jti:         claims.JTI,
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
