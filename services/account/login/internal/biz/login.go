// Package biz 是 login 服务的业务逻辑层(usecase)。
//
// 职责分层(Kratos 风格 + 大厂惯例):
//
//	service/  RPC 入口,只做 proto 与 biz 类型互转、错误码映射
//	biz/      用例,纯业务逻辑(不依赖 redis/mysql/grpc 直接 API)
//	data/     仓储,提供 mysql/redis/外部 grpc 访问的接口实现
//
// W2 mock:LoginUsecase.Login 只校验 account / password_hash 等于 conf 里固定值,
// 通过则签发一个 session_token(uuid)+ 返回 hub_ds_addr。
package biz

import (
	"context"

	"github.com/google/uuid"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/snowflake"
	"github.com/luyuancpp/pandora/services/account/login/internal/data"
)

// LoginResult 是 LoginUsecase.Login 的产出。service 层再翻译成 proto。
type LoginResult struct {
	PlayerID     int64
	SessionToken string
	HubDSAddr    string
	HubTicket    string
}

// LoginUsecase 是 Login / Logout 用例。
type LoginUsecase struct {
	repo      data.AccountRepo
	sf        *snowflake.Node
	hubDSAddr string
}

// NewLoginUsecase 构造 LoginUsecase。
//
// repo 由 data 层注入;sf 用 svc.BaseContext.Snowflake;hubDSAddr 从 conf 读。
func NewLoginUsecase(repo data.AccountRepo, sf *snowflake.Node, hubDSAddr string) *LoginUsecase {
	return &LoginUsecase{repo: repo, sf: sf, hubDSAddr: hubDSAddr}
}

// Login 走 mock 流程:
//  1. repo.FindByAccount → 拿 password_hash
//  2. 对比客户端传的 password_hash
//  3. repo.CheckBanned → 必须 false
//  4. 签发 session_token(uuid v4)
//  5. 返回 hub_ds_addr + hub_ticket(W2 是固定字符串,W3 调 hub_allocator + 签 JWT)
//
// 任何步骤失败返回 *errcode.Error,由 service 层翻译。
func (u *LoginUsecase) Login(ctx context.Context, account, passwordHash, deviceID string) (*LoginResult, error) {
	h := plog.With(ctx)

	playerID, expected, err := u.repo.FindByAccount(ctx, account)
	if err != nil {
		h.Warnw("msg", "login_account_not_found", "account", account)
		return nil, err
	}

	if expected != passwordHash {
		h.Warnw("msg", "login_password_mismatch", "account", account)
		return nil, errcode.New(errcode.ErrLoginPasswordMismatch, "password mismatch")
	}

	banned, err := u.repo.CheckBanned(ctx, playerID, deviceID)
	if err != nil {
		return nil, err
	}
	if banned {
		return nil, errcode.New(errcode.ErrLoginAccountBanned, "account banned player_id=%d", playerID)
	}

	sessionToken := uuid.NewString()
	// W2 mock 票据:固定字符串前缀 + uuid,便于 DS 端日志区分
	hubTicket := "mock-hub-ticket-" + uuid.NewString()

	h.Infow("msg", "login_ok", "player_id", playerID, "device_id", deviceID)

	return &LoginResult{
		PlayerID:     playerID,
		SessionToken: sessionToken,
		HubDSAddr:    u.hubDSAddr,
		HubTicket:    hubTicket,
	}, nil
}

// Logout W2 mock:不维护 session 状态,直接返 OK。
// W3 真实化:redis DEL session:<token>。
func (u *LoginUsecase) Logout(ctx context.Context, sessionToken string) error {
	plog.With(ctx).Infow("msg", "logout_ok")
	return nil
}
