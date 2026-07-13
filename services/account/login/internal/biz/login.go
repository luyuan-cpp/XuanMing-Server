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
	"github.com/luyuancpp/pandora/pkg/cellroute"
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

	// 断线重连(docs/design/battle-reconnect.md §2.1):玩家在 battle DS 掉线重登时,
	// login 查 player_locator 发现其处于 BATTLE 态,直接下发原对局 battle DS 直连信息。
	// 三字段"要么全空、要么全填":非空时客户端直连 battle DS 重连;为空则走 hub 进大厅。
	BattleDSAddr      string
	BattleTicket      string // battle DS JWT(新 jti)
	BattleTicketExpMs int64
	MatchID           uint64 // 重连对局 ID(Snowflake uint64)

	// RegionID / CellID 是玩家的确定性路由落点(docs/design/scale-cellular-20m.md §3.2/§4.2)。
	// 由 cellroute.Router 按 player_id 算出;未配 Router(单 Cell / dev)时为 0。
	// 客户端 / 边缘网关据此连到正确 Region 的正确 Cell 接入入口。
	RegionID uint32
	CellID   uint32

	// SelectedRoleID 是玩家当前已选角色(player_roles 表,选角权威化 2026-07-08)。
	// 0 = 从未选过角。客户端登录后进选角界面用此值预选中;确认后调 SelectRole。
	SelectedRoleID uint32
}

// BattleTicketIssuer 把所有 login 侧 Battle 票据签发统一到带 roster 权威门的入口。
// TicketUsecase 实现此接口；测试可注入严格 fake 验证 fail-closed 行为。
type BattleTicketIssuer interface {
	IssueBattleDSTicketAtCell(context.Context, uint64, uint64, uint32, uint32) (*DSTicketResult, error)
}

// LoginUsecase 是 Login / Logout 用例。
type LoginUsecase struct {
	repo        data.AccountRepo
	sessions    data.SessionRepo
	notifier    data.LocationNotifier
	hubAssigner data.HubAssigner    // W4 ⑥:hub_allocator 客户端,可为 nil(回退自签)
	roleRepo    data.PlayerRoleRepo // 选角权威化(2026-07-08):player_roles 仓储,可为 nil(降级无选角)
	sf          *snowflake.Node
	hubDSAddr   string // 回退用静态 hub DS 地址(hub_allocator 未配 / 调用失败时)
	hubRegion   string // 传给 hub_allocator.AssignHub 的 region(空=allocator 选最空分片)
	signer      *auth.Signer
	verifier    *auth.Verifier
	// battleTicketIssuer 必须在监听前注入。nil 或 roster 权威失败时不签重连票，
	// locator 已明确 InBattle 时返回 Unavailable；绝不回退到 signer 直签或继续 Hub 链。
	battleTicketIssuer BattleTicketIssuer
	// requireHubAssignmentBinding 激活后禁止 login 自签无归属绑定的 hub 票，也禁止在
	// hub_allocator 故障/旧版本返回无绑定票时回退；所有 hub 入场票必须由 allocator 权威签发。
	requireHubAssignmentBinding bool

	// router 是确定性 region/cell 路由器(scale-cellular-20m.md 三层化地基)。
	// 可为 nil:单 Cell / dev 部署不路由,登录返回 region/cell = 0。多 Cell 部署由 main
	// 经 SetCellRouter 注入(配静态表或 etcdtable 热更新表)。nil-safe,不阻断登录。
	router *cellroute.Router

	// devSkipPassword 开发期免密登录(conf.LoginConf.DevSkipPassword)。
	// 为 true 时跳过密码校验。
	devSkipPassword bool

	// devAutoRegister 开发期“假注册”(conf.LoginConf.DevAutoRegister)。
	// 为 true 时账号不存在则首登自动注册(存入本次密码 bcrypt 哈希)。
	devAutoRegister bool

	// allowedRoleIDs 是选角白名单(conf.LoginConf.AllowedRoleIDs,对齐客户端 CfgMisc.DefaultRoleIDs)。
	// 非空 = 严格白名单;空 = fail-closed 拒绝 SelectRole(除非 devAllowAnyRole=true)。
	allowedRoleIDs map[uint32]struct{}

	// devAllowAnyRole 开发期选角宽松开关(conf.LoginConf.DevAllowAnyRole)。
	// 为 true 且白名单为空时,SelectRole 只校验 roleID 非 0(配合客户端配置表快速迭代)。
	// 默认 false:白名单为空 → SelectRole 一律拒绝(fail-closed,防改包客户端签任意 role_id 进 hub 票据)。
	devAllowAnyRole bool
}

// SetBattleTicketIssuer 在服务启动、对外监听前注入统一的 Battle 票据签发入口。
func (u *LoginUsecase) SetBattleTicketIssuer(issuer BattleTicketIssuer) {
	u.battleTicketIssuer = issuer
}

// NewLoginUsecase 构造 LoginUsecase。
//
// repo / sessions 必填;notifier / hubAssigner 可为 nil(弱依赖,nil 时降级)。
// sf 用 svc.BaseContext.Snowflake;hubDSAddr / hubRegion 从 conf 读;signer/verifier 由 main 层构造后传进来。
//
// W4 ⑥:新增 hubAssigner + hubRegion。hubAssigner 非 nil 时,Login 调 hub_allocator.AssignHub
// 拿真实 hub_ds_addr + hub_ticket;nil 或调用失败时回退到自签票据 + 静态 hubDSAddr。
//
// 选角权威化(2026-07-08):新增 roleRepo(可 nil,降级无选角) + allowedRoleIDs(选角白名单)
// + devAllowAnyRole(dev 宽松开关)。白名单空且未开 devAllowAnyRole → SelectRole fail-closed 拒绝。
// Login 读已选角透传给 AssignHub / 自签票;SelectRole 落库后重发 hub 票。
func NewLoginUsecase(
	repo data.AccountRepo,
	sessions data.SessionRepo,
	notifier data.LocationNotifier,
	hubAssigner data.HubAssigner,
	roleRepo data.PlayerRoleRepo,
	sf *snowflake.Node,
	hubDSAddr string,
	hubRegion string,
	signer *auth.Signer,
	verifier *auth.Verifier,
	devSkipPassword bool,
	devAutoRegister bool,
	allowedRoleIDs []uint32,
	devAllowAnyRole bool,
) *LoginUsecase {
	var allowed map[uint32]struct{}
	if len(allowedRoleIDs) > 0 {
		allowed = make(map[uint32]struct{}, len(allowedRoleIDs))
		for _, id := range allowedRoleIDs {
			allowed[id] = struct{}{}
		}
	}
	return &LoginUsecase{
		repo:            repo,
		sessions:        sessions,
		notifier:        notifier,
		hubAssigner:     hubAssigner,
		roleRepo:        roleRepo,
		sf:              sf,
		hubDSAddr:       hubDSAddr,
		hubRegion:       hubRegion,
		signer:          signer,
		verifier:        verifier,
		devSkipPassword: devSkipPassword,
		devAutoRegister: devAutoRegister,
		allowedRoleIDs:  allowed,
		devAllowAnyRole: devAllowAnyRole,
	}
}

// SetCellRouter 注入确定性 region/cell 路由器(可选,多 Cell 部署用)。
//
// nil-safe:不调用 / 传 nil 时,Login 返回的 RegionID/CellID 为 0(单 Cell / dev 语义)。
// 用 setter 而非构造参数,避免单 Cell 阶段所有调用点被迫改签名;多 Cell 部署在 main
// 装配阶段调一次即可。Router 内部读路径无锁(AtomicTable),并发安全。
func (u *LoginUsecase) SetCellRouter(r *cellroute.Router) {
	u.router = r
}

// SetRequireHubAssignmentBinding 在服务监听前设置 Hub DSTicket 归属绑定激活栅栏。
func (u *LoginUsecase) SetRequireHubAssignmentBinding(require bool) {
	u.requireHubAssignmentBinding = require
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

	// 确定性 region/cell 路由落点(scale-cellular-20m.md §3.2/§3.3):多 Cell 部署时算出玩家落点,
	// 一处算好,既供客户端 / 边缘网关连到正确 Cell,又盖进自签 hub 票据(§3.3 防跨单元串号)。
	// router 为 nil(单 Cell / dev)或 Route 报错 → 降级 0/0(同单 Cell 行为),不阻断登录。
	regionID, cellID := u.routeRegionCell(ctx, playerID)

	// 断线重连(docs/design/battle-reconnect.md §2.1):玩家在 battle DS 中掉线重登时,
	// 查 player_locator 若发现其仍处于 BATTLE 态,直接下发原对局的 battle DS 直连信息,
	// 而非把玩家丢回大厅。命中重连时:跳过 hub 分配 + 跳过 NotifyLoginPending
	// (避免把 BATTLE 位置顶成 LOGIN_PENDING / HUB,把玩家从战斗里拉出来)。
	// locator 查询失败仍沿用既有弱依赖策略；但一旦 locator 已明确 InBattle，后续 roster/Redis/
	// 签票任一失败都必须阻断本次路由，不能再给同一玩家分配第二个 Hub 归属。
	if u.notifier != nil {
		res, reconnectErr := u.tryBattleReconnect(ctx, playerID, deviceID, sessionToken, sessExpMs, regionID, cellID)
		if reconnectErr != nil {
			return nil, reconnectErr
		}
		if res != nil {
			return res, nil
		}
	}

	// 读玩家已选角色(选角权威化 2026-07-08):弱依赖,读失败按 0(未选角)处理不阻断登录。
	// 透传给 resolveHub → hub 票据 claim;同时回给客户端选角界面预选中。
	selectedRoleID := u.loadSelectedRole(ctx, playerID)

	// 解析 hub 分片 + hub 票据(W4 ⑥):
	// hub_allocator 是 hub 票据权威,优先调 AssignHub 拿真实地址 + 票据;
	// 未配 / 调用失败 → 回退自签票据(盖 region/cell 戳) + 静态 hubDSAddr(弱依赖,不阻断登录)。
	hubDSAddr, hubTicket, hubExpMs, err := u.resolveHub(ctx, playerID, regionID, cellID, selectedRoleID)
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

	// 确定性 region/cell 路由已在上方一次算好(regionID/cellID),这里直接复用。
	h.Infow("msg", "login_ok", "player_id", playerID, "device_id", deviceID,
		"session_exp_ms", sessExpMs, "hub_ticket_exp_ms", hubExpMs,
		"region_id", regionID, "cell_id", cellID)

	return &LoginResult{
		PlayerID:       playerID,
		SessionToken:   sessionToken,
		SessionExpMs:   sessExpMs,
		HubDSAddr:      hubDSAddr,
		HubTicket:      hubTicket,
		HubTicketExpMs: hubExpMs,
		RegionID:       regionID,
		CellID:         cellID,
		SelectedRoleID: selectedRoleID,
	}, nil
}

// battleLocationQueryRetries / battleLocationQueryBackoff:BATTLE 位置查询的有界重试
// (docs/design/battle-reconnect.md §2.3)。locator 是核心弱依赖,偶发抖动/超时不该让
// "正在战斗的玩家"被误判成"不在战斗"从而错进大厅——重试把可恢复失败救回来,拿到
// InBattle 就照常跳回 battle。仅当重试全失败(locator 真的挂了)才降级走 hub,残余情况
// 由 hub 入口对账兜底(§2.3)。重试只发生在错误路径(罕见),不加正常登录延迟。
const (
	battleLocationQueryRetries = 3
	battleLocationQueryBackoff = 50 * time.Millisecond
)

// queryBattleLocation 查玩家 BATTLE 位置,对可恢复的查询失败做有界重试(§2.3)。
// 重试期间 ctx 被取消则立刻返回;重试全失败返回最后一次错误,由调用方降级走 hub。
func (u *LoginUsecase) queryBattleLocation(ctx context.Context, playerID uint64) (data.BattleLocation, error) {
	h := plog.With(ctx)
	var lastErr error
	for attempt := 0; attempt < battleLocationQueryRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return data.BattleLocation{}, ctx.Err()
			case <-time.After(battleLocationQueryBackoff):
			}
		}
		bl, err := u.notifier.GetBattleLocation(ctx, playerID)
		if err == nil {
			return bl, nil
		}
		lastErr = err
		h.Warnw("msg", "battle_location_query_retry", "err", err,
			"player_id", playerID, "attempt", attempt+1, "max", battleLocationQueryRetries)
	}
	return data.BattleLocation{}, lastErr
}

// tryBattleReconnect 检测玩家是否在 battle DS 中掉线,是则组装"直连 battle DS 重连"的
// LoginResult(docs/design/battle-reconnect.md §2.1)。返回 nil 表示未命中重连 → 调用方继续
// 走正常 hub 登录流程。
//
// 查询失败仍按既有 §2.3 弱依赖策略返回 (nil,nil) 走 Hub；明确 !InBattle 也走 Hub。
// 一旦 locator 已明确 InBattle，issuer/roster/Redis/签名失败返回 Unavailable，调用方不得继续
// AssignHub 或 NotifyLoginPending。Generic PermissionDeny 可能代表 roster 漂移而不是已终态，不能据此
// 猜测玩家已经可以回 Hub。命中重连时:
//   - 经统一 roster 权威门现签一张新 jti 的 battle 票(sub=playerID,盖 region/cell 落点);
//   - best-effort 记录登录设备;
//   - 不调 NotifyLoginPending / 不分配 hub(避免顶掉 BATTLE 位置把玩家拉出战斗)。
func (u *LoginUsecase) tryBattleReconnect(
	ctx context.Context, playerID uint64, deviceID, sessionToken string, sessExpMs int64, regionID, cellID uint32,
) (*LoginResult, error) {
	h := plog.With(ctx)

	bl, err := u.queryBattleLocation(ctx, playerID)
	if err != nil {
		// 重试仍失败:locator 不可用,无法确认玩家是否在战斗 → 降级走 hub(不阻断登录),
		// "战斗中误进 hub" 的兜底交给 hub 入口对账(§2.3)。
		h.Warnw("msg", "battle_location_query_failed", "err", err, "player_id", playerID)
		return nil, nil
	}
	if !bl.InBattle {
		return nil, nil
	}

	if u.battleTicketIssuer == nil {
		h.Errorw("msg", "battle_reconnect_ticket_issuer_unavailable",
			"player_id", playerID, "match_id", bl.MatchID)
		return nil, errcode.New(errcode.ErrUnavailable, "battle reconnect ticket authority unavailable")
	}
	battleResult, terr := u.battleTicketIssuer.IssueBattleDSTicketAtCell(
		ctx, playerID, bl.MatchID, regionID, cellID)
	if terr != nil {
		// roster/Redis/签票任一失败 → 本次路由 fail-closed，绝不直签或继续分配 Hub。
		h.Errorw("msg", "authorize_battle_reconnect_ticket_failed", "err", terr,
			"player_id", playerID, "match_id", bl.MatchID)
		return nil, errcode.NewCause(errcode.ErrUnavailable, terr,
			"battle reconnect ticket authority unavailable")
	}
	battleTicket, battleExpMs := battleResult.Ticket, battleResult.ExpiresAtMs
	if battleResult.BattleDSAddr == "" {
		return nil, errcode.New(errcode.ErrUnavailable, "battle reconnect target address unavailable")
	}

	// 记录最近登录设备(失败不阻塞登录,只日志告警)。
	if err := u.repo.TouchDevice(ctx, playerID, deviceID); err != nil {
		h.Warnw("msg", "touch_device_failed", "err", err, "player_id", playerID, "device_id", deviceID)
	}

	h.Infow("msg", "login_battle_reconnect", "player_id", playerID, "device_id", deviceID,
		"match_id", bl.MatchID, "battle_ds_addr", battleResult.BattleDSAddr,
		"battle_ticket_exp_ms", battleExpMs, "region_id", regionID, "cell_id", cellID)

	return &LoginResult{
		PlayerID:          playerID,
		SessionToken:      sessionToken,
		SessionExpMs:      sessExpMs,
		BattleDSAddr:      battleResult.BattleDSAddr,
		BattleTicket:      battleTicket,
		BattleTicketExpMs: battleExpMs,
		MatchID:           bl.MatchID,
		RegionID:          regionID,
		CellID:            cellID,
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
//
// regionID / cellID 是玩家确定性路由落点(由 Login / ResolveHubEndpoint 一次算好传入)。
// 回退自签分支把落点盖进 hub 票据(scale-cellular-20m.md §3.3 防跨单元串号);单 Cell / dev 为 0。
// hub_allocator 路径的票据由其自身签发(其内部落点绑定属 Codex/hub_allocator 职责)。
//
// roleID(选角权威化 2026-07-08):玩家已选角色。两条路径都把它盖进 hub 票据 claim:
// AssignHub 透传给 allocator 签;回退自签用 SignDSTicketFull。0 = 未选角(claim 不序列化)。
func (u *LoginUsecase) resolveHub(ctx context.Context, playerID uint64, regionID, cellID, roleID uint32) (addr, ticket string, expMs int64, err error) {
	h := plog.With(ctx)

	if u.hubAssigner != nil {
		assign, aerr := u.hubAssigner.AssignHub(ctx, playerID, u.hubRegion, 0, roleID)
		if aerr == nil && assign == nil {
			aerr = errcode.New(errcode.ErrUnavailable, "hub allocator returned an empty assignment")
		}
		if aerr == nil {
			if u.requireHubAssignmentBinding {
				if u.verifier == nil {
					return "", "", 0, errcode.New(errcode.ErrUnavailable,
						"hub ticket verifier unavailable while assignment binding is required")
				}
				claims, verr := u.verifier.VerifyDSTicket(assign.HubTicket)
				if verr != nil || claims.DSType != string(auth.DSTypeHub) ||
					claims.HubAssignmentID == "" || claims.DSPodName == "" ||
					claims.DSPodName != assign.HubPodName ||
					claims.DSWriterEpoch != auth.DSAuthWriterEpochV2 {
					h.Errorw("msg", "hub_assigner_returned_unbound_ticket", "err", verr,
						"player_id", playerID, "hub_pod", assign.HubPodName)
					return "", "", 0, errcode.New(errcode.ErrUnavailable,
						"hub allocator did not return a valid assignment-bound ticket")
				}
			}
			expMs = u.hubTicketExpMs(assign.HubTicket)
			h.Infow("msg", "hub_assigned", "player_id", playerID,
				"hub_pod", assign.HubPodName, "shard_id", assign.ShardID, "hub_ds_addr", assign.HubDSAddr)
			return assign.HubDSAddr, assign.HubTicket, expMs, nil
		}
		if u.requireHubAssignmentBinding {
			return "", "", 0, errcode.New(errcode.ErrUnavailable,
				"hub allocator required for assignment-bound ticket: %v", aerr)
		}
		// hub_allocator 不可用 → 回退自签,不阻断登录(玩家仍可凭票据连静态 hub DS)
		h.Warnw("msg", "hub_assign_failed_fallback_self_sign", "err", aerr, "player_id", playerID)
	}
	if u.requireHubAssignmentBinding {
		return "", "", 0, errcode.New(errcode.ErrUnavailable,
			"hub allocator is required while assignment binding is enabled")
	}

	ticket, expMs, err = u.signer.SignDSTicketFull(playerID, auth.DSTypeHub, 0, regionID, cellID, roleID, uuid.NewString())
	if err != nil {
		return "", "", 0, errcode.New(errcode.ErrInternal, "sign hub ticket failed: %v", err)
	}
	return u.hubDSAddr, ticket, expMs, nil
}

// routeRegionCell 算玩家确定性路由落点(scale-cellular-20m.md §3.2/§3.3)。
//
// router 为 nil(单 Cell / dev)或 Route 报错(配置缺口)→ 降级为 0/0(同单 Cell 行为),
// 仅告警不阻断登录。
func (u *LoginUsecase) routeRegionCell(ctx context.Context, playerID uint64) (regionID, cellID uint32) {
	if u.router == nil {
		return 0, 0
	}
	loc, err := u.router.Route(playerID)
	if err != nil {
		plog.With(ctx).Warnw("msg", "cellroute_failed", "err", err, "player_id", playerID)
		return 0, 0
	}
	return loc.RegionID, loc.CellID
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
	regionID, cellID := u.routeRegionCell(ctx, playerID)
	// 选角权威化:返回大厅路径也把已选角盖进新票(与登录同语义,DS 重入时同样能 spawn 对角色)。
	return u.resolveHub(ctx, playerID, regionID, cellID, u.loadSelectedRole(ctx, playerID))
}

// loadSelectedRole 读玩家已选角色(player_roles)。弱依赖:roleRepo 未配 / 读失败 → 0
// (未选角语义,仅告警不阻断)。只用于读路径;写路径(SelectRole)失败必须报错。
func (u *LoginUsecase) loadSelectedRole(ctx context.Context, playerID uint64) uint32 {
	if u.roleRepo == nil {
		return 0
	}
	roleID, err := u.roleRepo.GetRole(ctx, playerID)
	if err != nil {
		plog.With(ctx).Warnw("msg", "load_selected_role_failed", "err", err, "player_id", playerID)
		return 0
	}
	return roleID
}

// SelectRole 选角用例(选角权威化 2026-07-08,docs 综述见 login.proto SelectRole 注释)。
//
// 流程:
//  1. 校验 roleID:必须 >0;allowedRoleIDs 非空时必须在白名单;白名单为空时 fail-closed
//     一律拒绝(防改包客户端签任意 role_id 进 hub 票据),仅 devAllowAnyRole=true 放宽为只校非 0。
//  2. roleRepo.SetRole 落库(权威数据,失败必须报错——没落库就不能发票,否则重登后角色回退)。
//  3. resolveHub(带 roleID) → hub_allocator 把 role_id 签进全新 hub 票据 + 返回当前有效地址。
//
// 幂等:重复选同角 / 换角重选都是覆盖式 upsert + 重签新票(新 jti),不破坏票据一次性语义。
// roleRepo 未配(dev 裸跑)时跳过落库只签票,Warn 提示。
func (u *LoginUsecase) SelectRole(ctx context.Context, playerID uint64, roleID uint32) (addr, ticket string, expMs int64, err error) {
	h := plog.With(ctx)
	if playerID == 0 {
		return "", "", 0, errcode.New(errcode.ErrInvalidArg, "playerID must be > 0")
	}
	if roleID == 0 {
		return "", "", 0, errcode.New(errcode.ErrInvalidArg, "roleID must be > 0")
	}
	if len(u.allowedRoleIDs) > 0 {
		if _, ok := u.allowedRoleIDs[roleID]; !ok {
			h.Warnw("msg", "select_role_not_allowed", "player_id", playerID, "role_id", roleID)
			return "", "", 0, errcode.New(errcode.ErrInvalidArg, "role_id=%d not allowed", roleID)
		}
	} else if !u.devAllowAnyRole {
		// fail-closed:白名单没配就放行任意 role_id = 改包客户端可把任意角色配置 ID 签进 hub 票据
		// (hub_allocator 无二次校验)。生产必须配 allowed_role_ids;dev 宽松需显式开 dev_allow_any_role。
		h.Errorw("msg", "select_role_rejected_no_whitelist", "player_id", playerID, "role_id", roleID,
			"hint", "configure login.allowed_role_ids (prod) or enable login.dev_allow_any_role (dev only)")
		return "", "", 0, errcode.New(errcode.ErrInvalidState, "role selection disabled: allowed_role_ids not configured")
	}

	if u.roleRepo != nil {
		if serr := u.roleRepo.SetRole(ctx, playerID, roleID); serr != nil {
			h.Errorw("msg", "select_role_persist_failed", "err", serr, "player_id", playerID, "role_id", roleID)
			return "", "", 0, serr
		}
	} else {
		h.Warnw("msg", "select_role_repo_nil_skip_persist", "player_id", playerID, "role_id", roleID)
	}

	regionID, cellID := u.routeRegionCell(ctx, playerID)
	addr, ticket, expMs, err = u.resolveHub(ctx, playerID, regionID, cellID, roleID)
	if err != nil {
		h.Errorw("msg", "select_role_resolve_hub_failed", "err", err, "player_id", playerID, "role_id", roleID)
		return "", "", 0, err
	}
	h.Infow("msg", "select_role_ok", "player_id", playerID, "role_id", roleID, "hub_ds_addr", addr)
	return addr, ticket, expMs, nil
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
