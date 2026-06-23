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
	"time"

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
	repo        data.AccountRepo
	sessions    data.SessionRepo
	notifier    data.LocationNotifier
	hubAssigner data.HubAssigner // W4 ⑥:hub_allocator 客户端,可为 nil(回退自签)
	sf          *snowflake.Node
	hubDSAddr   string // 回退用静态 hub DS 地址(hub_allocator 未配 / 调用失败时)
	hubRegion   string // 传给 hub_allocator.AssignHub 的 region(空=allocator 选最空分片)
	signer      *auth.Signer
	verifier    *auth.Verifier

	// devSkipPassword 开发期免密登录(conf.LoginConf.DevSkipPassword)。
	// 为 true 时跳过密码校验。
	devSkipPassword bool

	// devAutoRegister 开发期“假注册”(conf.LoginConf.DevAutoRegister)。
	// 为 true 时账号不存在则首登自动注册(存入本次密码 bcrypt 哈希)。
	devAutoRegister bool
}

// NewLoginUsecase 构造 LoginUsecase。
//
// repo / sessions 必填;notifier / hubAssigner 可为 nil(弱依赖,nil 时降级)。
// sf 用 svc.BaseContext.Snowflake;hubDSAddr / hubRegion 从 conf 读;signer/verifier 由 main 层构造后传进来。
//
// W4 ⑥:新增 hubAssigner + hubRegion。hubAssigner 非 nil 时,Login 调 hub_allocator.AssignHub
// 拿真实 hub_ds_addr + hub_ticket;nil 或调用失败时回退到自签票据 + 静态 hubDSAddr。
func NewLoginUsecase(
	repo data.AccountRepo,
	sessions data.SessionRepo,
	notifier data.LocationNotifier,
	hubAssigner data.HubAssigner,
	sf *snowflake.Node,
	hubDSAddr string,
	hubRegion string,
	signer *auth.Signer,
	verifier *auth.Verifier,
	devSkipPassword bool,
	devAutoRegister bool,
) *LoginUsecase {
	return &LoginUsecase{
		repo:            repo,
		sessions:        sessions,
		notifier:        notifier,
		hubAssigner:     hubAssigner,
		sf:              sf,
		hubDSAddr:       hubDSAddr,
		hubRegion:       hubRegion,
		signer:          signer,
		verifier:        verifier,
		devSkipPassword: devSkipPassword,
		devAutoRegister: devAutoRegister,
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
		// 账号不存在:开发期“假注册” / 免密任一开关打开 → 首登自动注册(不阻断登录)。
		if errcode.As(err) != errcode.ErrLoginAccountNotFound || !(u.devAutoRegister || u.devSkipPassword) {
			h.Warnw("msg", "login_account_not_found", "account", account)
			return nil, err
		}
		playerID, err = u.ensureAccount(ctx, account, passwordHash)
		if err != nil {
			h.Errorw("msg", "login_auto_register_failed", "err", err, "account", account)
			return nil, err
		}
		// 刚注册:密码即客户端本次所发,无需再校验。
		h.Warnw("msg", "login_dev_auto_registered", "account", account, "player_id", playerID)
	} else if u.devSkipPassword {
		// 账号已存在 + 免密模式 → 跳过密码校验。
		h.Warnw("msg", "login_dev_skip_password", "account", account, "player_id", playerID)
	} else if verr := passwd.Verify(expected, passwordHash); verr != nil {
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

	// 写 session:同 player_id 多端登录直接覆盖前一份(顶号语义跟 push.ConnectionManager 一致)
	sessTTL := u.signer.SessionTTL()
	if u.sessions != nil {
		if err := u.sessions.Set(ctx, playerID, sessionToken, sessJTI, deviceID, sessTTL); err != nil {
			h.Errorw("msg", "session_set_failed", "err", err, "player_id", playerID)
			return nil, err
		}
	}

	// 解析 hub 分片 + hub 票据(W4 ⑥):
	// hub_allocator 是 hub 票据权威,优先调 AssignHub 拿真实地址 + 票据;
	// 未配 / 调用失败 → 回退自签票据 + 静态 hubDSAddr(弱依赖,不阻断登录)。
	hubDSAddr, hubTicket, hubExpMs, err := u.resolveHub(ctx, playerID)
	if err != nil {
		h.Errorw("msg", "resolve_hub_failed", "err", err, "player_id", playerID)
		return nil, err
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
		HubDSAddr:      hubDSAddr,
		HubTicket:      hubTicket,
		HubTicketExpMs: hubExpMs,
	}, nil
}

// ensureAccount 在开发期假注册 / 免密模式下为不存在的账号首登注册一条记录,返回稳定 player_id。
//
// snowflake 分配新 player_id 写入 accounts(uk_account 唯一),密码存入本次客户端所发
// passwordHash 的 bcrypt 哈希 → 后续用同密码可走正常 bcrypt 校验(真实“首登即注”)。
// 并发下若已被别的请求建好,CreateAccount 返回 ErrAlreadyExists,回查拿已存在的
// player_id(保证同 account 名稳定)。
func (u *LoginUsecase) ensureAccount(ctx context.Context, account, passwordHash string) (uint64, error) {
	bcryptHash, err := passwd.Hash(passwordHash, passwd.DevCost)
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "hash password for auto-register: %v", err)
	}
	newID := u.sf.Generate()
	if err := u.repo.CreateAccount(ctx, newID, account, bcryptHash); err != nil {
		if errcode.As(err) == errcode.ErrAlreadyExists {
			id, _, ferr := u.repo.FindByAccount(ctx, account)
			if ferr != nil {
				return 0, ferr
			}
			return id, nil
		}
		return 0, err
	}
	return newID, nil
}

// resolveHub 解析玩家进大厅需要的 hub_ds_addr + hub_ticket(+ 票据过期 unix ms)。
//
// 优先级(W4 ⑥):
//  1. hubAssigner 非 nil → 调 hub_allocator.AssignHub。成功则用其返回的 hub_ds_addr + hub_ticket
//     (hub_allocator 是 hub 票据权威,不变量 §1 一人一 DS 由其落地);票据 exp 用 verifier 解析,
//     解析失败则按 DSTicketTTL 估算。
//  2. hubAssigner 为 nil 或 AssignHub 失败 → 回退自签 hub 票据 + 静态 hubDSAddr(仅 Warn,不阻断登录)。
//
// 回退分支保证 login 可独立联调(本机不起 hub_allocator 也能拿到可连 hub 的票据,
// 因为 login 与 hub_allocator 共享同一 JWT secret/issuer/audience)。
func (u *LoginUsecase) resolveHub(ctx context.Context, playerID uint64) (addr, ticket string, expMs int64, err error) {
	h := plog.With(ctx)

	if u.hubAssigner != nil {
		assign, aerr := u.hubAssigner.AssignHub(ctx, playerID, u.hubRegion, 0)
		if aerr == nil {
			expMs = u.hubTicketExpMs(assign.HubTicket)
			h.Infow("msg", "hub_assigned", "player_id", playerID,
				"hub_pod", assign.HubPodName, "shard_id", assign.ShardID, "hub_ds_addr", assign.HubDSAddr)
			return assign.HubDSAddr, assign.HubTicket, expMs, nil
		}
		// hub_allocator 不可用 → 回退自签,不阻断登录(玩家仍可凭票据连静态 hub DS)
		h.Warnw("msg", "hub_assign_failed_fallback_self_sign", "err", aerr, "player_id", playerID)
	}

	ticket, expMs, err = u.signer.SignDSTicket(playerID, auth.DSTypeHub, 0, uuid.NewString())
	if err != nil {
		return "", "", 0, errcode.New(errcode.ErrInternal, "sign hub ticket failed: %v", err)
	}
	return u.hubDSAddr, ticket, expMs, nil
}

// ResolveHubEndpoint 复用登录时的 hub 分配链路(resolveHub → hub_allocator.AssignHub),
// 返回"当前有效"的大厅 DS 地址 + 一张全新的一次性 hub 票据。
//
// 用途(结算返回大厅):客户端不能复用登录时缓存的 hub_ds_addr / hub_ticket。
//   - 旧 Hub DS 可能已被 Agones 判 Unhealthy/Deleted/换端口,缓存地址已失效;
//   - 旧 hub 票据的 jti 已在首次进大厅时被消费,复用会被 DS 判 ticket replay。
//
// AssignHub 幂等且自愈:玩家原分片仍 ready → 重签票返回同地址;原分片下线 → 自动改派到
// 健康分片并返回新地址。两种情况都返回新签的票据(新 jti),不破坏 DS ticket 一次性语义。
//
// hubAssigner 未配 / 调用失败时,resolveHub 回退自签票据 + 静态 hubDSAddr(与登录一致,不阻断)。
func (u *LoginUsecase) ResolveHubEndpoint(ctx context.Context, playerID uint64) (addr, ticket string, expMs int64, err error) {
	if playerID == 0 {
		return "", "", 0, errcode.New(errcode.ErrInvalidArg, "playerID must be > 0")
	}
	return u.resolveHub(ctx, playerID)
}

// hubTicketExpMs 解析 hub_allocator 签发的 hub 票据,取其 exp(unix ms)给客户端展示。
//
// login 与 hub_allocator 共享 JWT secret/issuer/audience,故 verifier 可直接验签。
// 解析失败(理论上不应发生)兜底为 now + DSTicketTTL,避免返回 0 让客户端误判已过期。
func (u *LoginUsecase) hubTicketExpMs(ticket string) int64 {
	if u.verifier != nil {
		if claims, err := u.verifier.VerifyDSTicket(ticket); err == nil && claims.ExpiresAt != nil {
			return claims.ExpiresAt.UnixMilli()
		}
	}
	return time.Now().Add(u.signer.DSTicketTTL()).UnixMilli()
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
