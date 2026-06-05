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
type LoginService struct {
	loginv1.UnimplementedLoginServiceServer

	uc *biz.LoginUsecase
}

// NewLoginService 注入 LoginUsecase。
func NewLoginService(uc *biz.LoginUsecase) *LoginService {
	return &LoginService{uc: uc}
}

// Login 立即完成型(参考 proto/pandora/login/v1/login.proto 注释)。
func (s *LoginService) Login(ctx context.Context, req *loginv1.LoginRequest) (*loginv1.LoginResponse, error) {
	res, err := s.uc.Login(ctx, req.GetAccount(), req.GetPasswordHash(), req.GetDeviceId())
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
	if err := s.uc.Logout(ctx, req.GetSessionToken()); err != nil {
		return &loginv1.LogoutResponse{Code: toProtoCode(err)}, nil
	}
	return &loginv1.LogoutResponse{Code: commonv1.ErrCode_OK}, nil
}

// IssueDSTicket W2 阶段未实现(W3 接 JWT + hub_allocator)。
//
// 设计上不返 grpc error,而是 code 字段返 ErrUnknown,客户端可识别"未实现"语义。
func (s *LoginService) IssueDSTicket(ctx context.Context, req *loginv1.IssueDSTicketRequest) (*loginv1.IssueDSTicketResponse, error) {
	plog.With(ctx).Warnw("msg", "ds_ticket_issue_not_implemented_w2")
	return &loginv1.IssueDSTicketResponse{
		Code: commonv1.ErrCode_ERR_UNKNOWN,
	}, nil
}

// VerifyDSTicket W2 阶段未实现(W3 接 JWT 验证 + jti 黑名单)。
func (s *LoginService) VerifyDSTicket(ctx context.Context, req *loginv1.VerifyDSTicketRequest) (*loginv1.VerifyDSTicketResponse, error) {
	plog.With(ctx).Warnw("msg", "ds_ticket_verify_not_implemented_w2")
	return &loginv1.VerifyDSTicketResponse{
		Code: commonv1.ErrCode_ERR_UNKNOWN,
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
