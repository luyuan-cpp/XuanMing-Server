// Package service 是 mail 服务的 gRPC service 层(2026-06-29)。
//
// 职责:
//   - 实现 mailv1.MailServiceServer
//   - 玩家 RPC 从 ctx 取 JWT player_id(R5:override request 字段)
//   - 运营 RPC(SendSystemMail/SendGuildMail/SendPersonalMail)内网调用,不经 Envoy;
//     systemOnly 兜底拒绝客户端(Envoy 侧还有精确 path 403,双保险)
//   - errcode.Code → commonv1.ErrCode 1:1 映射
package service

import (
	"context"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	mailv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/mail/v1"

	"github.com/luyuancpp/pandora/services/social/mail/internal/biz"
)

// snowflakeGen 是 snowflake.Node 的最小接口。
type snowflakeGen interface {
	Generate() uint64
}

// MailService 实现 mailv1.MailServiceServer。
type MailService struct {
	mailv1.UnimplementedMailServiceServer
	uc *biz.MailUsecase
	sf snowflakeGen
}

// NewMailService 构造。
func NewMailService(uc *biz.MailUsecase, sf snowflakeGen) *MailService {
	return &MailService{uc: uc, sf: sf}
}

func nowMs() int64 { return time.Now().UnixMilli() }

// ListMail 拉取收件箱。player_id 以 JWT ctx 为准(R5)。
func (s *MailService) ListMail(ctx context.Context, req *mailv1.ListMailRequest) (*mailv1.ListMailResponse, error) {
	pid := callerID(ctx)
	if pid == 0 {
		return &mailv1.ListMailResponse{Code: commonv1.ErrCode_ERR_UNAUTHORIZED}, nil
	}
	mails, next, err := s.uc.ListMail(ctx, pid, nowMs(), req.GetCursor(), int(req.GetLimit()))
	if err != nil {
		return &mailv1.ListMailResponse{Code: toProtoCode(err)}, nil
	}
	return &mailv1.ListMailResponse{Code: commonv1.ErrCode_OK, Mails: mails, NextCursor: next}, nil
}

// ReadMail 置已读。
func (s *MailService) ReadMail(ctx context.Context, req *mailv1.ReadMailRequest) (*mailv1.ReadMailResponse, error) {
	pid := callerID(ctx)
	if pid == 0 {
		return &mailv1.ReadMailResponse{Code: commonv1.ErrCode_ERR_UNAUTHORIZED}, nil
	}
	if err := s.uc.ReadMail(ctx, pid, req.GetMailId()); err != nil {
		return &mailv1.ReadMailResponse{Code: toProtoCode(err)}, nil
	}
	return &mailv1.ReadMailResponse{Code: commonv1.ErrCode_OK}, nil
}

// ClaimMail 领附件(幂等)。
func (s *MailService) ClaimMail(ctx context.Context, req *mailv1.ClaimMailRequest) (*mailv1.ClaimMailResponse, error) {
	pid := callerID(ctx)
	if pid == 0 {
		return &mailv1.ClaimMailResponse{Code: commonv1.ErrCode_ERR_UNAUTHORIZED}, nil
	}
	atts, err := s.uc.ClaimMail(ctx, pid, req.GetMailId(), nowMs())
	if err != nil {
		return &mailv1.ClaimMailResponse{Code: toProtoCode(err), Attachments: atts}, nil
	}
	return &mailv1.ClaimMailResponse{Code: commonv1.ErrCode_OK, Attachments: atts}, nil
}

// DeleteMail 删个人邮件。
func (s *MailService) DeleteMail(ctx context.Context, req *mailv1.DeleteMailRequest) (*mailv1.DeleteMailResponse, error) {
	pid := callerID(ctx)
	if pid == 0 {
		return &mailv1.DeleteMailResponse{Code: commonv1.ErrCode_ERR_UNAUTHORIZED}, nil
	}
	if err := s.uc.DeleteMail(ctx, pid, req.GetMailId()); err != nil {
		return &mailv1.DeleteMailResponse{Code: toProtoCode(err)}, nil
	}
	return &mailv1.DeleteMailResponse{Code: commonv1.ErrCode_OK}, nil
}

// SendSystemMail 运营群发(内网,系统接口:客户端一律拒)。
func (s *MailService) SendSystemMail(ctx context.Context, req *mailv1.SendSystemMailRequest) (*mailv1.SendSystemMailResponse, error) {
	if code := systemOnly(ctx); code != commonv1.ErrCode_OK {
		return &mailv1.SendSystemMailResponse{Code: code}, nil
	}
	id, err := s.uc.SendSystemMail(ctx, s.sf.Generate(), req.GetTitle(), req.GetBody(), req.GetAttachments(), req.GetStartMs(), req.GetEndMs(), nowMs())
	if err != nil {
		return &mailv1.SendSystemMailResponse{Code: toProtoCode(err)}, nil
	}
	return &mailv1.SendSystemMailResponse{Code: commonv1.ErrCode_OK, MailId: id}, nil
}

// SendGuildMail 公会群发(内网,系统接口:客户端一律拒)。
func (s *MailService) SendGuildMail(ctx context.Context, req *mailv1.SendGuildMailRequest) (*mailv1.SendGuildMailResponse, error) {
	if code := systemOnly(ctx); code != commonv1.ErrCode_OK {
		return &mailv1.SendGuildMailResponse{Code: code}, nil
	}
	id, err := s.uc.SendGuildMail(ctx, s.sf.Generate(), req.GetGuildId(), req.GetTitle(), req.GetBody(), req.GetAttachments(), req.GetStartMs(), req.GetEndMs(), nowMs())
	if err != nil {
		return &mailv1.SendGuildMailResponse{Code: toProtoCode(err)}, nil
	}
	return &mailv1.SendGuildMailResponse{Code: commonv1.ErrCode_OK, MailId: id}, nil
}

// SendPersonalMail 定点发个人邮件(离线可达;内网系统接口:客户端一律拒,防自助发带附件邮件)。
func (s *MailService) SendPersonalMail(ctx context.Context, req *mailv1.SendPersonalMailRequest) (*mailv1.SendPersonalMailResponse, error) {
	if code := systemOnly(ctx); code != commonv1.ErrCode_OK {
		return &mailv1.SendPersonalMailResponse{Code: code}, nil
	}
	id, err := s.uc.SendPersonalMail(ctx, s.sf.Generate(), req.GetToPlayerId(), req.GetTitle(), req.GetBody(), req.GetAttachments(), req.GetExpireMs(), nowMs(), req.GetInstanceGrantKey())
	if err != nil {
		return &mailv1.SendPersonalMailResponse{Code: toProtoCode(err)}, nil
	}
	return &mailv1.SendPersonalMailResponse{Code: commonv1.ErrCode_OK, MailId: id}, nil
}

// ── 辅助 ──────────────────────────────────────────────────────────────────────

func callerID(ctx context.Context) uint64 {
	id, _ := ctx.Value(plog.CtxKeyPlayerID).(uint64)
	return id
}

// systemOnly 系统接口鉴权:经 Envoy 的客户端(callerID>0)一律拒,合法调用者是后端内部直连
// (运营工具 / battle_result 背包满转邮件,无 JWT 注入 → callerID==0)。
// Envoy 侧对三个 Send* 精确 path 直接 403,此处是服务层兜底(双保险,对齐 player 服务模式)。
func systemOnly(ctx context.Context) commonv1.ErrCode {
	if callerID(ctx) != 0 {
		return commonv1.ErrCode_ERR_PERMISSION_DENY
	}
	return commonv1.ErrCode_OK
}

func toProtoCode(err error) commonv1.ErrCode {
	return commonv1.ErrCode(errcode.As(err))
}
