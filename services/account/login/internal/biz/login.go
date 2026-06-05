// Package biz 是 login 服务的业务逻辑层(usecase)。
//
// 职责分层(Kratos 风格 + 大厂惯例):
//
//	service/  RPC 入口,只做 proto 与 biz 类型互转、错误码映射
//	biz/      用例,纯业务逻辑(不依赖 redis/mysql/grpc 直接 API)
//	data/     仓储,提供 mysql/redis/外部 grpc 访问的接口实现
//
// W3 ①(2026-06-05):session_token 从 uuid 改为由 pkg/auth.Signer 签发的 HS256 JWT。
// Envoy jwt_authn filter 会验证该 JWT 并把 sub 提到 x-pandora-player-id 头。
//
// W3 ②(2026-06-05):
//   - 密码改 bcrypt 校验(pkg/passwd)
//   - 登录成功写 redis session(覆盖式,顶号靠 push.ConnectionManager + 新 session 覆盖)
//   - TouchDevice 写 account_devices(失败只日志,不阻塞登录)
//   - Logout 真实 DEL pandora:sess:<player_id>
package biz

import (
	"context"

	"github.com/google/uuid"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/passwd"
	"github.com/luyuancpp/pandora/pkg/snowflake"
	"github.com/luyuancpp/pandora/services/account/login/internal/data"
)

// LoginResult 是 LoginUsecase.Login 的产出。service 层再翻译成 proto。
type LoginResult struct {
	PlayerID       uint64
	SessionToken   string // JWT(W3 ①)
	SessionExpMs   int64  // session_token exp(unix ms),客户端展示 / 提前别未过期
	HubDSAddr      string
	HubTicket      string // hub DS JWT(W3 ①)
	HubTicketExpMs int64
}

// LoginUsecase 是 Login / Logout 用例。
type LoginUsecase struct {
	repo      data.AccountRepo
	sessions  data.SessionRepo
	notifier  data.LocationNotifier
	sf        *snowflake.Node
	hubDSAddr string
	signer    *auth.Signer
	verifier  *auth.Verifier
}

// NewLoginUsecase 构造 LoginUsecase。
//
// repo / sessions / notifier 由 data 层注入(notifier 可为 nil);sf 用 svc.BaseContext.Snowflake;
// hubDSAddr 从 conf 读;signer/verifier 由 main 层构造后传进来。
func NewLoginUsecase(
	repo data.AccountRepo,
	sessions data.SessionRepo,
	notifier data.LocationNotifier,
	sf *snowflake.Node,
	hubDSAddr string,
	signer *auth.Signer,
	verifier *auth.Verifier,
) *LoginUsecase {
	return &LoginUsecase{
		repo:      repo,
		sessions:  sessions,
		notifier:  notifier,
		sf:        sf,
		hubDSAddr: hubDSAddr,
		signer:    signer,
		verifier:  verifier,
	}
}

// Login 走真实流程(W3 ②):
//  1. repo.FindByAccount → 拿 bcrypt 哈希
//  2. passwd.Verify(stored, clientDigest) 比对
//  3. repo.CheckBanned → 必须 false
//  4. 用 signer 签 session(24h) + hub_ticket(5min)
//  5. sessions.Set 写入 redis(顶号策略:同 key 覆盖)
//  6. repo.TouchDevice 异步语义(同步调,失败仅日志)
//  7. 返回 hub_ds_addr + 两份 JWT
//
// 任何步骤失败返回 *errcode.Error,由 service 层翻译。
func (u *LoginUsecase) Login(ctx context.Context, account, passwordHash, deviceID string) (*LoginResult, error) {
	h := plog.With(ctx)

	playerID, expected, err := u.repo.FindByAccount(ctx, account)
	if err != nil {
		h.Warnw("msg", "login_account_not_found", "account", account)
		return nil, err
	}

	if verr := passwd.Verify(expected, passwordHash); verr != nil {
		h.Warnw("msg", "login_password_mismatch", "account", account, "player_id", playerID)
		return nil, errcode.New(errcode.ErrLoginPasswordMismatch, "password mismatch")
	}

	banned, err := u.repo.CheckBanned(ctx, playerID, deviceID)
	if err != nil {
		return nil, err
	}
	if banned {
		return nil, errcode.New(errcode.ErrLoginAccountBanned, "account banned player_id=%d", playerID)
	}

	sessJTI := uuid.NewString()
	sessionToken, sessExpMs, err := u.signer.SignSession(playerID, sessJTI)
	if err != nil {
		h.Errorw("msg", "sign_session_failed", "err", err, "player_id", playerID)
		return nil, errcode.New(errcode.ErrInternal, "sign session failed: %v", err)
	}
	hubTicket, hubExpMs, err := u.signer.SignDSTicket(playerID, auth.DSTypeHub, 0, uuid.NewString())
	if err != nil {
		h.Errorw("msg", "sign_hub_ticket_failed", "err", err, "player_id", playerID)
		return nil, errcode.New(errcode.ErrInternal, "sign hub ticket failed: %v", err)
	}

	// 写 session:同 player_id 多端登录直接覆盖前一份(顶号语义跟 push.ConnectionManager 一致)
	sessTTL := u.signer.SessionTTL()
	if u.sessions != nil {
		if err := u.sessions.Set(ctx, playerID, sessionToken, sessJTI, deviceID, sessTTL); err != nil {
			h.Errorw("msg", "session_set_failed", "err", err, "player_id", playerID)
			return nil, err
		}
	}

	// 记录最近登录设备(失败不阻塞登录,只日志告警)
	if err := u.repo.TouchDevice(ctx, playerID, deviceID); err != nil {
		h.Warnw("msg", "touch_device_failed", "err", err, "player_id", playerID, "device_id", deviceID)
	}

	// 通知 locator:玩家进入 LOGIN_PENDING(W3 ⑤,不变量 §1 入口)。
	// locator 不可用 → 仅 Warn,不阻断登录(hub DS 接入后会重新刷此 key)。
	if u.notifier != nil {
		if err := u.notifier.NotifyLoginPending(ctx, playerID, deviceID); err != nil {
			h.Warnw("msg", "locator_notify_failed", "err", err, "player_id", playerID)
		}
	}

	h.Infow("msg", "login_ok", "player_id", playerID, "device_id", deviceID,
		"session_exp_ms", sessExpMs, "hub_ticket_exp_ms", hubExpMs)

	return &LoginResult{
		PlayerID:       playerID,
		SessionToken:   sessionToken,
		SessionExpMs:   sessExpMs,
		HubDSAddr:      u.hubDSAddr,
		HubTicket:      hubTicket,
		HubTicketExpMs: hubExpMs,
	}, nil
}

// Logout 真实化(W3 ②):验 session_token 拿 player_id,DEL redis session。
//
// 客户端实际很少调 Logout(直接关进程),所以本路径不要求强一致:
// token 验签失败 → 也返回 OK(让客户端能 fire-and-forget,清理本地状态);只记日志。
func (u *LoginUsecase) Logout(ctx context.Context, sessionToken string) error {
	h := plog.With(ctx)
	if u.verifier == nil || u.sessions == nil {
		h.Infow("msg", "logout_ok_noop")
		return nil
	}
	claims, err := u.verifier.VerifySession(sessionToken)
	if err != nil {
		// token 不合法不算业务错(可能客户端 token 过期了),直接返 OK
		h.Warnw("msg", "logout_verify_session_failed", "err", err)
		return nil
	}
	playerID := claims.PlayerID()
	if playerID == 0 {
		h.Warnw("msg", "logout_session_no_player")
		return nil
	}
	if err := u.sessions.Delete(ctx, playerID); err != nil {
		h.Errorw("msg", "logout_session_del_failed", "err", err, "player_id", playerID)
		return err
	}
	h.Infow("msg", "logout_ok", "player_id", playerID)
	return nil
}
