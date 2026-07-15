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
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/cellroute"
	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/passwd"
	"github.com/luyuancpp/pandora/pkg/placement"
	"github.com/luyuancpp/pandora/pkg/snowflake"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
	loginv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/login/v1"
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
	Resume         ResumeContextResult
}

type ResumeContextResult struct {
	Route            loginv1.ResumeRoute
	MatchID          uint64
	MatchStage       loginv1.ResumeMatchStage
	PlacementVersion uint64
	OperationID      string
	PlacementState   loginv1.ResumePlacementState
	Target           placement.Target
	GameMode         string
	// battleDSAddr/battleTicket 只在 Login biz 内消费，不会映射进公共
	// ResumeContext。它们由 matchmaker 从 canonical READY match 的 exact
	// target + per-player placement binding 现签，禁止再用 roster projection
	// 在 Login 本地拼一张缺 placement 的票。
	battleDSAddr string
	battleTicket string
}

// BattleTicketIssuer 把所有 login 侧 Battle 票据签发统一到带 roster 权威门的入口。
// TicketUsecase 实现此接口；测试可注入严格 fake 验证 fail-closed 行为。
type BattleTicketIssuer interface {
	IssueBattleDSTicketAtCell(context.Context, uint64, uint64, uint32, uint32) (*DSTicketResult, error)
	// InspectBattleRoute 是 Hub 签票门的显式三态权威判定(零副作用,不签票):
	//   data.BattleRouteActive   = 玩家确属 live 对局 → 拒绝 Hub;
	//   data.BattleRouteTerminal = 权威记录显式终态(ended/abandoned) → 唯一允许 Hub 的证明;
	//   data.BattleRouteUnknown  = 其余一切(roster 漂移/非成员/记录缺失/stale/错误) → fail-closed。
	// P0 修复(2026-07-15,Codex 复审):不得用 AuthorizeBattleTicket 的通用 ErrPermissionDeny 充当终态证明。
	InspectBattleRoute(ctx context.Context, playerID, matchID uint64) (data.BattleRouteState, error)
	InspectBattleRouteProof(ctx context.Context, playerID, matchID uint64) (data.BattleRouteState, placement.BattleExitProof, error)
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
	// v2Verifier 独立验证 Hub allocator 返回的 DSTicket v2(RS256)。非 nil 也机械激活
	// 玩家 DSTicket 的 RS256-only profile；玩家 Session 仍走独立 HS256 verifier。
	v2Verifier *auth.DSTicketVerifier
	// battleTicketIssuer 必须在监听前注入。nil 或 roster 权威失败时不签重连票，
	// locator 已明确 InBattle 时返回 Unavailable；绝不回退到 signer 直签或继续 Hub 链。
	battleTicketIssuer BattleTicketIssuer
	// requireHubAssignmentBinding 激活后禁止 login 自签无归属绑定的 hub 票，也禁止在
	// hub_allocator 故障/旧版本返回无绑定票时回退；所有 hub 入场票必须由 allocator 权威签发。
	requireHubAssignmentBinding bool
	placementMode               placement.Mode
	placementChecker            data.PlacementAdmissionChecker
	placementProofSigner        *placement.ProofSigner
	matchResumeReader           data.MatchResumeReader

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
// sf 用 svc.BaseContext.Snowflake;hubDSAddr / hubRegion 从 conf 读;signer / legacy verifier /
// v2 verifier 由 main 层按独立信任域构造后传进来。
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
	v2Verifier *auth.DSTicketVerifier,
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
		v2Verifier:      v2Verifier,
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

func (u *LoginUsecase) SetPlacementPolicy(mode placement.Mode, checker data.PlacementAdmissionChecker) {
	u.placementMode, u.placementChecker = mode, checker
}

func (u *LoginUsecase) SetPlacementProofSigner(signer *placement.ProofSigner) {
	u.placementProofSigner = signer
}

func (u *LoginUsecase) SetMatchResumeReader(reader data.MatchResumeReader) {
	u.matchResumeReader = reader
}

func (u *LoginUsecase) rs256DSTicketProfileEnabled() bool {
	return u != nil && u.v2Verifier != nil
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
	newlyCreated := false

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
		newlyCreated = true
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
	if u.placementMode != placement.ModeOff {
		if err := u.ensurePlacementForLogin(ctx, playerID, newlyCreated); err != nil {
			if u.placementMode == placement.ModeEnforce {
				return nil, err
			}
			h.Warnw("msg", "placement_login_shadow_failed", "player_id", playerID, "err", err)
		}
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
	// local/off 保留 locator 弱依赖；B1 归属绑定激活后，必须先由 locator 证明玩家
	// !InBattle 才能分配 Hub，未配置或查询失败都 fail-closed。
	if u.placementMode == placement.ModeEnforce {
		resume, resumeErr := u.authoritativeLoginResume(ctx, playerID)
		if resumeErr != nil || resume.Route == loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN {
			return nil, errcode.NewCause(errcode.ErrUnavailable, resumeErr,
				"cannot resolve authoritative login route")
		}
		if resume.Route == loginv1.ResumeRoute_RESUME_ROUTE_BATTLE {
			return u.buildPlacementBattleLogin(ctx, playerID, deviceID, sessionToken, sessExpMs,
				regionID, cellID, resume)
		}
		// 在任何 Hub assignment/seat/ticket 副作用前再做一次 canonical
		// match + placement 合并门。QUEUED/CONFIRMING 只有仍稳定归属 Hub
		// 才能继续；ALLOCATING/READY/RUNNING/UNKNOWN 全部 fail-closed。
		if _, gateErr := u.authorizeHubEntry(ctx, playerID, 0, false); gateErr != nil {
			return nil, gateErr
		}
	} else if u.notifier == nil {
		if u.requireHubAssignmentBinding {
			return nil, errcode.New(errcode.ErrUnavailable,
				"player locator is required before B1 hub assignment")
		}
	} else {
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

	// B1 先建立 LOGIN_PENDING 权威位置，再调用 Hub allocator。写入失败时既不分配
	// Hub，也不会产生/交付 Hub 票；local/off 保留历史上的分配后 best-effort 通知顺序。
	pendingNotified := false
	if u.requireHubAssignmentBinding && u.placementMode != placement.ModeEnforce {
		if u.notifier == nil {
			return nil, errcode.New(errcode.ErrUnavailable,
				"player locator is required before B1 hub assignment")
		}
		if err := u.notifier.NotifyLoginPending(ctx, playerID, deviceID); err != nil {
			h.Warnw("msg", "locator_notify_failed", "err", err, "player_id", playerID)
			return nil, errcode.NewCause(errcode.ErrUnavailable, err,
				"player locator refused LOGIN_PENDING; hub was not assigned")
		}
		pendingNotified = true
	}

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

	// local/off 在 Hub 解析后 best-effort 通知；B1 已在分配前成功写入，不能重复写。
	if !pendingNotified && u.notifier != nil && u.placementMode != placement.ModeEnforce {
		if err := u.notifier.NotifyLoginPending(ctx, playerID, deviceID); err != nil {
			h.Warnw("msg", "locator_notify_failed", "err", err, "player_id", playerID)
		}
	}

	// 确定性 region/cell 路由已在上方一次算好(regionID/cellID),这里直接复用。
	h.Infow("msg", "login_ok", "player_id", playerID, "device_id", deviceID,
		"session_exp_ms", sessExpMs, "hub_ticket_exp_ms", hubExpMs,
		"region_id", regionID, "cell_id", cellID)

	resume, resumeErr := u.resumeContextForPlayer(ctx, playerID)
	if u.placementMode == placement.ModeEnforce {
		if resumeErr != nil || resume.Route == loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN {
			// The terminal outbox may have released the canonical match between
			// the pre-assignment read and this final fence. Recover only through
			// the exact durable signed exit proof, then re-read both authorities.
			resume, resumeErr = u.authoritativeLoginResume(ctx, playerID)
		}
		if resumeErr != nil || resume.Route == loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN {
			return nil, errcode.NewCause(errcode.ErrUnavailable, resumeErr,
				"authoritative route changed while assigning Hub")
		}
		if resume.Route == loginv1.ResumeRoute_RESUME_ROUTE_BATTLE {
			// The match saga won the race after the initial route read. Never
			// return the stale Hub ticket/address together with a Battle route.
			return u.buildPlacementBattleLogin(ctx, playerID, deviceID, sessionToken, sessExpMs,
				regionID, cellID, resume)
		}
	}
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
		Resume:         resume,
	}, nil
}

func (u *LoginUsecase) buildPlacementBattleLogin(
	ctx context.Context,
	playerID uint64,
	deviceID, sessionToken string,
	sessExpMs int64,
	regionID, cellID uint32,
	resume ResumeContextResult,
) (*LoginResult, error) {
	if resume.MatchID == 0 {
		return nil, errcode.New(errcode.ErrUnavailable, "Battle resume match identity unavailable")
	}
	result := &LoginResult{PlayerID: playerID, SessionToken: sessionToken, SessionExpMs: sessExpMs,
		MatchID: resume.MatchID, RegionID: regionID, CellID: cellID, Resume: resume}
	switch resume.MatchStage {
	case loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_ALLOCATING:
		// The durable worker is still progressing. Returning no Hub route is
		// intentional: the coordinator polls GetResumeContext and cannot create
		// a second Admission while allocation is unresolved.
	case loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_READY,
		loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_RUNNING:
		addr, ticket, expMs, err := u.verifyCanonicalBattleResumeTicket(playerID, resume)
		if err != nil {
			return nil, err
		}
		result.BattleDSAddr = addr
		result.BattleTicket = ticket
		result.BattleTicketExpMs = expMs
	default:
		return nil, errcode.New(errcode.ErrUnavailable, "Battle resume stage is not routable")
	}
	if err := u.repo.TouchDevice(ctx, playerID, deviceID); err != nil {
		plog.With(ctx).Warnw("msg", "touch_device_failed", "err", err,
			"player_id", playerID, "device_id", deviceID)
	}
	return result, nil
}

// verifyCanonicalBattleResumeTicket 验证 matchmaker 内部 reader 返回的现签票。
// 签名、玩家、match 和 placement binding 必须与同一 ResumeContext 精确一致；
// 任一字段漂移都返回可重试错误，绝不回退 roster projection 自签。
func (u *LoginUsecase) verifyCanonicalBattleResumeTicket(
	playerID uint64,
	resume ResumeContextResult,
) (addr, ticket string, expMs int64, err error) {
	if u.v2Verifier == nil || resume.battleDSAddr == "" || resume.battleTicket == "" ||
		resume.MatchID == 0 || resume.PlacementVersion == 0 || !placement.ValidOperationID(resume.OperationID) {
		return "", "", 0, errcode.New(errcode.ErrUnavailable,
			"canonical Battle resume credential incomplete")
	}
	claims, verifyErr := u.v2Verifier.Verify(resume.battleTicket)
	if verifyErr != nil {
		return "", "", 0, errcode.NewCause(errcode.ErrUnavailable, verifyErr,
			"verify canonical Battle resume ticket")
	}
	if claims.PlayerID() != playerID || claims.DSType != string(auth.DSTypeBattle) ||
		claims.MatchID != resume.MatchID || claims.PlacementVersion != resume.PlacementVersion ||
		claims.PlacementOperationID != resume.OperationID {
		return "", "", 0, errcode.New(errcode.ErrUnavailable,
			"canonical Battle resume ticket identity mismatch")
	}
	return resume.battleDSAddr, resume.battleTicket, claims.ExpiresAt.Time.UnixMilli(), nil
}

// ResolveBattleEndpoint 为 authenticated IssueDSTicket(battle) 与完整 Login
// 复用同一条恢复权威链。enforce 下只接受 canonical READY/RUNNING match 的
// exact target + per-player placement binding 现签票；local/off 才保留 roster
// projection 兼容路径。
func (u *LoginUsecase) ResolveBattleEndpoint(
	ctx context.Context,
	playerID, matchID uint64,
) (addr, ticket string, expMs int64, err error) {
	if playerID == 0 || matchID == 0 {
		return "", "", 0, errcode.New(errcode.ErrInvalidArg,
			"Battle endpoint requires player_id and match_id")
	}
	if u.placementMode == placement.ModeEnforce {
		resume, resumeErr := u.authoritativeLoginResume(ctx, playerID)
		if resumeErr != nil || resume.Route == loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN {
			return "", "", 0, errcode.NewCause(errcode.ErrUnavailable, resumeErr,
				"cannot resolve authoritative Battle route")
		}
		if resume.Route != loginv1.ResumeRoute_RESUME_ROUTE_BATTLE || resume.MatchID != matchID ||
			(resume.MatchStage != loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_READY &&
				resume.MatchStage != loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_RUNNING) {
			return "", "", 0, errcode.New(errcode.ErrInvalidState,
				"requested match is not the authoritative routable Battle")
		}
		return u.verifyCanonicalBattleResumeTicket(playerID, resume)
	}
	if u.battleTicketIssuer == nil {
		return "", "", 0, errcode.New(errcode.ErrUnavailable,
			"battle reconnect ticket authority unavailable")
	}
	regionID, cellID := u.routeRegionCell(ctx, playerID)
	result, issueErr := u.battleTicketIssuer.IssueBattleDSTicketAtCell(
		ctx, playerID, matchID, regionID, cellID)
	if issueErr != nil || result == nil || result.BattleDSAddr == "" || result.Ticket == "" {
		return "", "", 0, errcode.NewCause(errcode.ErrUnavailable, issueErr,
			"battle reconnect ticket authority unavailable")
	}
	return result.BattleDSAddr, result.Ticket, result.ExpiresAtMs, nil
}

// battleLocationQueryRetries / battleLocationQueryBackoff:BATTLE 位置查询的有界重试
// (docs/design/battle-reconnect.md §2.3)。local/off 下 locator 是弱依赖；B1 下它是
// Hub 分配前的权威门。偶发抖动/超时不该让
// "正在战斗的玩家"被误判成"不在战斗"从而错进大厅——重试把可恢复失败救回来,拿到
// InBattle 就照常跳回 battle。重试全失败时 local/off 才降级走 Hub，B1 则返回
// Unavailable。重试只发生在错误路径(罕见),不加正常登录延迟。
const (
	battleLocationQueryRetries = 3
	battleLocationQueryBackoff = 50 * time.Millisecond
)

// queryBattleLocation 查玩家 BATTLE 位置,对可恢复的查询失败做有界重试(§2.3)。
// 重试期间 ctx 被取消则立刻返回；重试全失败返回最后一次错误，由调用方按 profile
// 决定 local/off 降级或 B1 fail-closed。
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
// local/off 查询失败按既有 §2.3 弱依赖策略返回 (nil,nil) 走 Hub；B1 查询失败返回
// Unavailable。只有明确 !InBattle 才允许 B1 继续走 Hub。
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
		h.Warnw("msg", "battle_location_query_failed", "err", err, "player_id", playerID)
		if u.requireHubAssignmentBinding {
			return nil, errcode.NewCause(errcode.ErrUnavailable, err,
				"cannot prove player is outside battle before B1 hub assignment")
		}
		// local/off 保留历史弱依赖降级。
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

	resume, _ := u.resumeContextForPlayer(ctx, playerID)
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
		Resume:            resume,
	}, nil
}

func (u *LoginUsecase) GetResumeContext(ctx context.Context, sessionToken string) (ResumeContextResult, error) {
	if u.verifier == nil {
		return ResumeContextResult{}, errcode.New(errcode.ErrUnavailable, "session verifier unavailable")
	}
	claims, err := u.verifier.VerifySession(sessionToken)
	if err != nil || claims.PlayerID() == 0 {
		return ResumeContextResult{}, errcode.New(errcode.ErrUnauthorized, "invalid session")
	}
	// ResumeContext is the foreground/cold-recovery authority used when the
	// client still has a valid session and therefore does not perform a full
	// Login.  It must consume the same exact durable terminal proof as Login;
	// otherwise match release followed by a disconnected foreground resume
	// would observe STABLE BATTLE + match NONE as UNKNOWN forever.
	return u.authoritativeLoginResume(ctx, claims.PlayerID())
}

// authoritativeLoginResume is the only recovery mutation allowed on the login
// authority service (full Login and authenticated GetResumeContext).
// battle_result deliberately releases the canonical match graph after writing
// its durable per-player exit proof. A process killed in that window restarts
// with match=NONE and placement=STABLE BATTLE; treating that exact combination
// as permanent UNKNOWN would strand the player unless the client happened to
// retain and call a separate ReturnHub operation. We therefore consume only the
// signed, version-bound proof and then re-read the merged authorities. A missing
// proof, an active/unknown match, transport error, or any other placement shape
// remains fail-closed and has zero routing side effects.
func (u *LoginUsecase) authoritativeLoginResume(ctx context.Context, playerID uint64) (ResumeContextResult, error) {
	resume, resumeErr := u.resumeContextForPlayer(ctx, playerID)
	if resumeErr == nil && resume.Route != loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN {
		if u.placementMode == placement.ModeEnforce && resume.Route == loginv1.ResumeRoute_RESUME_ROUTE_HUB &&
			u.placementChecker != nil {
			// A terminal BATTLE->HUB Begin response may have been lost and its
			// first lease may have expired during a long app/server outage. The
			// merged route is still correctly HUB, but AssignHub cannot bind an
			// expired target. Re-present the exact durable proof/op before any Hub
			// assignment side effect. Account bootstrap is renewed below with its
			// exact bootstrap proof/op; Hub->Hub remains owned by hub_allocator.
			snapshot, err := u.placementChecker.GetPlacement(ctx, playerID)
			if err != nil {
				return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN}, err
			}
			pendingHub := snapshot.Found &&
				snapshot.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
				snapshot.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB
			if pendingHub && snapshot.SourceMatchID != 0 {
				if err := u.prepareHubPlacement(ctx, playerID, snapshot.SourceMatchID); err != nil {
					return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN}, err
				}
				return u.resumeContextForPlayer(ctx, playerID)
			}
			if pendingHub && isAccountBootstrapHubPending(snapshot) {
				if err := u.renewAccountBootstrapHubPlacement(ctx, playerID, snapshot); err != nil {
					return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN}, err
				}
				return u.resumeContextForPlayer(ctx, playerID)
			}
		}
		return resume, nil
	}
	if u.placementMode != placement.ModeEnforce || u.placementChecker == nil || u.matchResumeReader == nil {
		return resume, resumeErr
	}

	snapshot, err := u.placementChecker.GetPlacement(ctx, playerID)
	if err != nil {
		return resume, err
	}
	stableBattle := snapshot.Found &&
		snapshot.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE &&
		snapshot.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE && snapshot.MatchID != 0
	pendingBattle := snapshot.Found &&
		snapshot.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
		snapshot.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB && snapshot.MatchID == 0 &&
		snapshot.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE && snapshot.TargetMatchID != 0
	if !stableBattle && !pendingBattle {
		return resume, resumeErr
	}
	matchContext, err := u.matchResumeReader.ResolvePlayerMatchContext(ctx, playerID)
	if err != nil || matchContext.State == data.MatchContextUnknown {
		return resume, errcode.NewCause(errcode.ErrUnavailable, err,
			"cannot prove canonical match is released for terminal Login recovery")
	}
	if matchContext.State != data.MatchContextNone {
		return resume, resumeErr
	}
	sourceMatchID := snapshot.MatchID
	if pendingBattle {
		sourceMatchID = snapshot.TargetMatchID
	}
	if err := u.prepareHubPlacement(ctx, playerID, sourceMatchID); err != nil {
		return resume, err
	}
	return u.resumeContextForPlayer(ctx, playerID)
}

func (u *LoginUsecase) resumeContextForPlayer(ctx context.Context, playerID uint64) (ResumeContextResult, error) {
	if u.placementChecker == nil {
		return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN}, errcode.New(errcode.ErrUnavailable, "placement authority unavailable")
	}
	s, err := u.placementChecker.GetPlacement(ctx, playerID)
	if err != nil {
		return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN}, err
	}
	out := ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN,
		PlacementVersion: s.Version, OperationID: s.OperationID, Target: s.Target}
	if !s.Found {
		return out, errcode.New(errcode.ErrUnavailable, "placement is UNKNOWN")
	}
	switch s.TransitionState {
	case locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING:
		out.PlacementState = loginv1.ResumePlacementState_RESUME_PLACEMENT_STATE_PENDING
	case locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE:
		out.PlacementState = loginv1.ResumePlacementState_RESUME_PLACEMENT_STATE_STABLE
	default:
		return out, errcode.New(errcode.ErrUnavailable, "placement transition state is UNKNOWN")
	}

	// Matchmaker's durable start/claim/ticket/match graph is the authority for
	// pre-Battle stages. Presence is never consulted here. In enforce mode a
	// missing reader or any UNKNOWN/drift makes the whole route UNKNOWN.
	var mc data.MatchResumeContext
	if u.matchResumeReader != nil {
		mc, err = u.matchResumeReader.ResolvePlayerMatchContext(ctx, playerID)
		if err != nil || mc.State == data.MatchContextUnknown {
			return out, errcode.NewCause(errcode.ErrUnavailable, err, "match resume context is UNKNOWN")
		}
	} else if u.placementMode == placement.ModeEnforce {
		return out, errcode.New(errcode.ErrUnavailable, "match resume authority unavailable")
	} else {
		mc.State = data.MatchContextNone
	}
	out.GameMode = mc.GameMode

	if mc.State == data.MatchContextActive {
		switch mc.Stage {
		case data.MatchStageStarting, data.MatchStageQueued:
			// Before a match is formed, ticket_id is the durable progress/cancel
			// handle exposed as match_id by the existing client contract.
			out.MatchID = mc.TicketID
			out.Route = loginv1.ResumeRoute_RESUME_ROUTE_HUB
			out.MatchStage = loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_QUEUED
		case data.MatchStageConfirming:
			out.MatchID = mc.MatchID
			out.Route = loginv1.ResumeRoute_RESUME_ROUTE_HUB
			out.MatchStage = loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_CONFIRMING
		case data.MatchStageAllocating:
			out.MatchID = mc.MatchID
			out.Route = loginv1.ResumeRoute_RESUME_ROUTE_BATTLE
			out.MatchStage = loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_ALLOCATING
		case data.MatchStageReady:
			out.MatchID = mc.MatchID
			out.Route = loginv1.ResumeRoute_RESUME_ROUTE_BATTLE
			out.MatchStage = loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_READY
		default:
			return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN},
				errcode.New(errcode.ErrUnavailable, "match resume stage is UNKNOWN")
		}
	}

	stableHub := s.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE &&
		s.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB
	stableBattle := s.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE &&
		s.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE && s.MatchID != 0
	pendingHub := s.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
		s.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB
	pendingBattle := s.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
		s.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE && s.TargetMatchID != 0

	if mc.State == data.MatchContextNone {
		switch {
		case stableHub:
			out.Route = loginv1.ResumeRoute_RESUME_ROUTE_HUB
			out.MatchStage = loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_NONE
			return out, nil
		case pendingHub:
			out.Route, out.MatchID = loginv1.ResumeRoute_RESUME_ROUTE_HUB, s.SourceMatchID
			out.MatchStage = loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_NONE
			return out, nil
		default:
			return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN,
					PlacementVersion: s.Version, OperationID: s.OperationID},
				errcode.New(errcode.ErrUnavailable, "match/placement authority drift")
		}
	}

	// ACTIVE match must agree with placement. Early stages remain physically in
	// Hub. READY is only visible after the match worker has bound BATTLE_PENDING;
	// STABLE BATTLE + READY means Admission already committed and is RUNNING.
	switch mc.Stage {
	case data.MatchStageStarting, data.MatchStageQueued, data.MatchStageConfirming:
		if (!stableHub && !pendingHub) || (mc.MatchID != 0 && s.MatchID != 0 && s.MatchID != mc.MatchID) {
			return out, errcode.New(errcode.ErrUnavailable, "early match stage conflicts with placement")
		}
	case data.MatchStageAllocating:
		if stableBattle && s.MatchID == mc.MatchID {
			out.MatchStage = loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_RUNNING
		} else if pendingBattle && s.TargetMatchID == mc.MatchID {
			// keep ALLOCATING, even if the target became bound before the durable
			// match job published READY.
		} else if !stableHub {
			return out, errcode.New(errcode.ErrUnavailable, "allocating match conflicts with placement")
		}
	case data.MatchStageReady:
		if mc.PlacementVersion == 0 || mc.PlacementOperationID == "" ||
			mc.PlacementVersion != s.Version || mc.PlacementOperationID != s.OperationID {
			return out, errcode.New(errcode.ErrUnavailable, "READY match placement binding drift")
		}
		if stableBattle && s.MatchID == mc.MatchID {
			out.MatchStage = loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_RUNNING
		} else if !pendingBattle || s.TargetMatchID != mc.MatchID || !s.TargetBound {
			return out, errcode.New(errcode.ErrUnavailable, "READY match lacks exact Battle placement target")
		}
		out.battleDSAddr = mc.DSAddr
		out.battleTicket = mc.BattleTicket
	}
	return out, nil
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
//  1. hubAssigner 非 nil → 调 hub_allocator.AssignHub。local/off 按 JOSE alg 验 legacy；
//     RS256 profile 只接受 v2，校验 player / Hub 类型 / 目标实例绑定并读取已验签 exp。
//     解析、验签或绑定任一失败均 fail-closed，不回退估算 exp，也不把坏票返回客户端。
//  2. 仅完全未配置 v2 且 assignment binding 关闭的 local/off，Hub 不可用时才可回退
//     自签 HS256 hub 票据 + 静态 hubDSAddr。
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
	placementBinding, placementErr := u.hubPlacementBinding(ctx, playerID)
	if placementErr != nil {
		if u.placementMode == placement.ModeEnforce {
			return "", "", 0, placementErr
		}
		if u.placementMode == placement.ModeShadow {
			h.Warnw("msg", "hub_placement_shadow_rejected", "player_id", playerID, "err", placementErr)
		}
	}

	if u.hubAssigner != nil {
		var assign *data.HubAssignment
		var aerr error
		if u.placementMode != placement.ModeOff && placementBinding.Complete() {
			if pa, ok := u.hubAssigner.(data.PlacementHubAssigner); ok {
				assign, aerr = pa.AssignHubWithPlacement(ctx, playerID, u.hubRegion, 0, roleID, placementBinding)
			} else {
				aerr = errcode.New(errcode.ErrUnavailable, "hub allocator does not support placement binding")
			}
		} else {
			assign, aerr = u.hubAssigner.AssignHub(ctx, playerID, u.hubRegion, 0, roleID)
		}
		if aerr == nil && assign == nil {
			aerr = errcode.New(errcode.ErrUnavailable, "hub allocator returned an empty assignment")
		}
		if aerr == nil {
			if u.placementMode == placement.ModeEnforce && !assign.Placement.Equal(placementBinding) {
				return "", "", 0, errcode.New(errcode.ErrUnavailable, "hub allocator placement echo mismatch")
			}
			expMs, verr := u.verifyHubAssignmentTicket(playerID, assign)
			if verr != nil {
				h.Errorw("msg", "hub_assigner_returned_invalid_ticket", "err", verr,
					"player_id", playerID, "hub_pod", assign.HubPodName)
				return "", "", 0, errcode.New(errcode.ErrUnavailable,
					"hub allocator returned an invalid ticket: %v", verr)
			}
			h.Infow("msg", "hub_assigned", "player_id", playerID,
				"hub_pod", assign.HubPodName, "shard_id", assign.ShardID, "hub_ds_addr", assign.HubDSAddr)
			return assign.HubDSAddr, assign.HubTicket, expMs, nil
		}
		if u.requireHubAssignmentBinding || u.rs256DSTicketProfileEnabled() || u.placementMode == placement.ModeEnforce {
			return "", "", 0, errcode.New(errcode.ErrUnavailable,
				"hub allocator required for RS256/assignment-bound ticket: %v", aerr)
		}
		// hub_allocator 不可用 → 回退自签,不阻断登录(玩家仍可凭票据连静态 hub DS)
		h.Warnw("msg", "hub_assign_failed_fallback_self_sign", "err", aerr, "player_id", playerID)
	}
	if u.requireHubAssignmentBinding || u.rs256DSTicketProfileEnabled() || u.placementMode == placement.ModeEnforce {
		return "", "", 0, errcode.New(errcode.ErrUnavailable,
			"hub allocator is required by the RS256/assignment-bound ticket profile")
	}

	ticket, expMs, err = u.signer.SignDSTicketFull(playerID, auth.DSTypeHub, 0, regionID, cellID, roleID, uuid.NewString())
	if err != nil {
		return "", "", 0, errcode.New(errcode.ErrInternal, "sign hub ticket failed: %v", err)
	}
	return u.hubDSAddr, ticket, expMs, nil
}

func (u *LoginUsecase) hubPlacementBinding(ctx context.Context, playerID uint64) (placement.Binding, error) {
	if u.placementMode == placement.ModeOff {
		return placement.Binding{}, nil
	}
	if u.placementChecker == nil {
		return placement.Binding{}, errcode.New(errcode.ErrUnavailable, "placement authority unavailable")
	}
	snapshot, err := u.placementChecker.GetPlacement(ctx, playerID)
	if err != nil {
		return placement.Binding{}, err
	}
	binding, ok := snapshot.HubBinding()
	if !ok {
		return placement.Binding{}, errcode.New(errcode.ErrLocatorNotFound, "placement is UNKNOWN or does not route to Hub")
	}
	return binding, nil
}

func isAccountBootstrapHubPending(s data.PlacementSnapshot) bool {
	return s.Found && s.Version == 1 && placement.ValidOperationID(s.OperationID) &&
		s.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
		s.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB &&
		s.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_UNSPECIFIED &&
		s.MatchID == 0 && s.TargetMatchID == 0 && s.SourceMatchID == 0 &&
		s.ProofType == locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP &&
		s.ProofID != ""
}

// renewAccountBootstrapHubPlacement is the sole renewal path for a bootstrap
// lease. It never manufactures a replacement operation: every signed field is
// copied from the durable locator record and the response must echo that exact
// version/op. Hub-transfer and Battle-exit leases have different proof owners
// and are deliberately rejected here.
func (u *LoginUsecase) renewAccountBootstrapHubPlacement(
	ctx context.Context, playerID uint64, snapshot data.PlacementSnapshot,
) error {
	if u.placementChecker == nil || u.placementProofSigner == nil {
		return errcode.New(errcode.ErrUnavailable, "placement bootstrap authority unavailable")
	}
	if playerID == 0 || !isAccountBootstrapHubPending(snapshot) {
		return errcode.New(errcode.ErrLocatorConflict,
			"placement is not an exact pending account bootstrap")
	}
	expected := placement.Binding{Version: snapshot.Version, OperationID: snapshot.OperationID}
	proof := placement.Proof{PlayerID: playerID, TargetRoute: placement.RouteHub,
		ProofType: placement.ProofAccountBootstrap, ProofID: snapshot.ProofID,
		OperationID: snapshot.OperationID}
	confirmed, err := u.placementChecker.BootstrapHub(ctx, playerID, snapshot.OperationID, snapshot.ProofID,
		u.placementProofSigner.Sign(proof), time.Now().Add(10*time.Minute).UnixMilli())
	if err != nil {
		return err
	}
	if !confirmed.Equal(expected) {
		return errcode.New(errcode.ErrLocatorConflict,
			"placement bootstrap renewal response identity mismatch")
	}
	return nil
}

func (u *LoginUsecase) bootstrapHubPlacement(ctx context.Context, playerID uint64, proofID string) error {
	if u.placementChecker == nil || u.placementProofSigner == nil {
		return errcode.New(errcode.ErrUnavailable, "placement bootstrap authority unavailable")
	}
	if playerID == 0 || proofID == "" {
		return errcode.New(errcode.ErrInvalidArg, "placement bootstrap identity required")
	}
	snapshot, err := u.placementChecker.GetPlacement(ctx, playerID)
	if err != nil {
		return err
	}
	if snapshot.Found {
		if snapshot.ProofID != proofID {
			return errcode.New(errcode.ErrLocatorConflict,
				"existing placement bootstrap proof does not match account authority")
		}
		return u.renewAccountBootstrapHubPlacement(ctx, playerID, snapshot)
	}
	opID := uuid.NewString()
	proof := placement.Proof{PlayerID: playerID, TargetRoute: placement.RouteHub,
		ProofType: placement.ProofAccountBootstrap, ProofID: proofID, OperationID: opID}
	confirmed, err := u.placementChecker.BootstrapHub(ctx, playerID, opID, proofID,
		u.placementProofSigner.Sign(proof), time.Now().Add(10*time.Minute).UnixMilli())
	if err != nil {
		return err
	}
	if !confirmed.Equal(placement.Binding{Version: 1, OperationID: opID}) {
		return errcode.New(errcode.ErrLocatorConflict,
			"placement bootstrap response identity mismatch")
	}
	return nil
}

// ensurePlacementForLogin supports two auditable creation paths only:
//  1. this request durably created the account;
//  2. a pre-placement account is backfilled after both canonical match state
//     and presence explicitly prove it is not MATCHING/BATTLE.
//
// Missing/conflicting/unreadable authority is UNKNOWN and never bootstraps.
func (u *LoginUsecase) ensurePlacementForLogin(ctx context.Context, playerID uint64, newlyCreated bool) error {
	if u.placementChecker == nil || u.placementProofSigner == nil {
		return errcode.New(errcode.ErrUnavailable, "placement bootstrap authority unavailable")
	}
	s, err := u.placementChecker.GetPlacement(ctx, playerID)
	if err != nil {
		return err
	}
	if s.Found {
		// Recover an expired/lost-response account bootstrap using its exact
		// durable op/proof identity. Other transitions have their own authority.
		if s.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
			s.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB && s.Version == 1 &&
			s.ProofType == locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP &&
			s.ProofID != "" {
			return u.bootstrapHubPlacement(ctx, playerID, s.ProofID)
		}
		return nil
	}
	proofID := "account-created:" + strconv.FormatUint(playerID, 10)
	if !newlyCreated {
		if u.matchResumeReader == nil || u.notifier == nil {
			return errcode.New(errcode.ErrUnavailable, "placement backfill authorities unavailable")
		}
		mc, matchErr := u.matchResumeReader.ResolvePlayerMatchContext(ctx, playerID)
		if matchErr != nil || mc.State != data.MatchContextNone {
			return errcode.NewCause(errcode.ErrUnavailable, matchErr,
				"placement backfill cannot prove canonical NOT_BATTLE")
		}
		presence, presenceErr := u.queryBattleLocation(ctx, playerID)
		if presenceErr != nil {
			return errcode.NewCause(errcode.ErrUnavailable, presenceErr,
				"placement backfill presence is UNKNOWN")
		}
		switch presence.PresenceState {
		case locatorv1.LocationState_LOCATION_STATE_OFFLINE,
			locatorv1.LocationState_LOCATION_STATE_LOGIN_PENDING,
			locatorv1.LocationState_LOCATION_STATE_HUB:
			// explicit non-match presence
		default:
			return errcode.New(errcode.ErrUnavailable,
				"placement backfill presence is not explicitly outside MATCHING/BATTLE")
		}
		proofID = "account-backfill-not-battle:" + strconv.FormatUint(playerID, 10)
		plog.With(ctx).Infow("msg", "placement_account_backfill_authorized", "player_id", playerID,
			"proof_id", proofID, "presence_state", int32(presence.PresenceState))
	}
	if err := u.bootstrapHubPlacement(ctx, playerID, proofID); err != nil {
		return err
	}
	plog.With(ctx).Infow("msg", "placement_account_bootstrapped", "player_id", playerID,
		"proof_id", proofID, "newly_created", newlyCreated)
	return nil
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
//
// P0 止血(2026-07-14,docs §7.16.3 候选 A 下沉):本入口是客户端直连 battle 失败/重连超时
// 回大厅的旁路,必须先过 active-BATTLE 三态权威门;BATTLE_ACTIVE/UNKNOWN 时零副作用拒绝,
// 绝不先 AssignHub 再补偿。
func (u *LoginUsecase) ResolveHubEndpoint(ctx context.Context, playerID uint64) (addr, ticket string, expMs int64, err error) {
	return u.ResolveHubEndpointFromMatch(ctx, playerID, 0)
}

// authorizeHubEntry 是所有 Hub 物理副作用（assignment/seat/role/ticket）的
// 最终前置门。enforce 下它合并 canonical match graph 与 placement，并对刚读
// identity 再做一次精确 placement fence：
//   - ALLOCATING/READY/RUNNING/UNKNOWN 一律拒绝；
//   - QUEUED/CONFIRMING 仅在 STABLE HUB 时允许；
//   - BATTLE->HUB 仅 ResolveHubEndpointFromMatch 可携 exact source_match_id
//     触发 durable terminal/leave proof 恢复。
//
// local/off/shadow 保留旧三态 presence 门，供滚动迁移使用。
func (u *LoginUsecase) authorizeHubEntry(
	ctx context.Context,
	playerID, sourceMatchID uint64,
	allowTerminalTransition bool,
) (ResumeContextResult, error) {
	if u.placementMode != placement.ModeEnforce {
		if err := u.guardHubRouteAgainstActiveBattle(ctx, playerID); err != nil {
			return ResumeContextResult{}, err
		}
		return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_HUB}, nil
	}
	if u.placementChecker == nil || u.matchResumeReader == nil {
		return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN},
			errcode.New(errcode.ErrUnavailable, "Hub route authorities unavailable")
	}

	resume, resumeErr := u.resumeContextForPlayer(ctx, playerID)
	if resumeErr != nil || resume.Route == loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN {
		if !allowTerminalTransition {
			return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN},
				errcode.NewCause(errcode.ErrUnavailable, resumeErr,
					"cannot prove authoritative Hub route")
		}
		// 先验证客户端 fence 与当前 stable Battle 精确一致，再允许
		// authoritativeLoginResume 消费 durable terminal/leave proof。canonical
		// match 仍 active/unknown 时 authoritativeLoginResume 自身保持零 mutation。
		before, err := u.placementChecker.GetPlacement(ctx, playerID)
		if err != nil {
			return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN}, err
		}
		stableBattle := before.Found &&
			before.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE &&
			before.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE && before.MatchID != 0
		pendingBattle := before.Found &&
			before.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
			before.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB && before.MatchID == 0 &&
			before.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE && before.TargetMatchID != 0
		if !stableBattle && !pendingBattle {
			return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN},
				errcode.NewCause(errcode.ErrUnavailable, resumeErr,
					"cannot prove authoritative Hub route")
		}
		authoritativeMatchID := before.MatchID
		if pendingBattle {
			authoritativeMatchID = before.TargetMatchID
		}
		if sourceMatchID == 0 || sourceMatchID != authoritativeMatchID {
			return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN},
				errcode.New(errcode.ErrInvalidState,
					"source match is required and must equal authoritative Battle placement")
		}
		resume, resumeErr = u.authoritativeLoginResume(ctx, playerID)
	} else if resume.Route == loginv1.ResumeRoute_RESUME_ROUTE_HUB {
		// Lost-response/expired pending Hub operations must renew their exact
		// durable op before allocator target bind. Stable Hub is a read-only no-op.
		resume, resumeErr = u.authoritativeLoginResume(ctx, playerID)
	}
	if resumeErr != nil || resume.Route == loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN {
		return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN},
			errcode.NewCause(errcode.ErrUnavailable, resumeErr,
				"cannot prove authoritative Hub route")
	}
	if resume.Route != loginv1.ResumeRoute_RESUME_ROUTE_HUB {
		return resume, errcode.New(errcode.ErrInvalidState,
			"authoritative route is Battle (stage=%s)", resume.MatchStage.String())
	}

	// 最后一次 placement fence 必须仍与合并结果同 version/op；否则 match
	// worker 已赢得并发切换，旧 Hub 决策不得产生 assignment/seat/ticket。
	snapshot, err := u.placementChecker.GetPlacement(ctx, playerID)
	if err != nil {
		return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN}, err
	}
	if !snapshot.Found || snapshot.Version != resume.PlacementVersion ||
		snapshot.OperationID != resume.OperationID {
		return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN},
			errcode.New(errcode.ErrUnavailable, "Hub route placement changed during authorization")
	}
	if _, ok := snapshot.HubBinding(); !ok {
		return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN},
			errcode.New(errcode.ErrUnavailable, "placement does not authorize Hub")
	}
	stableHub := snapshot.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE &&
		snapshot.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB
	switch resume.MatchStage {
	case loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_NONE:
		// NONE may be a proof-authorized HUB_PENDING that AssignHub must bind.
	case loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_QUEUED,
		loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_CONFIRMING:
		if !stableHub {
			return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN},
				errcode.New(errcode.ErrUnavailable,
					"early match stage requires stable Hub placement")
		}
	default:
		return resume, errcode.New(errcode.ErrInvalidState,
			"match stage does not authorize Hub side effects")
	}
	if sourceMatchID != 0 && snapshot.SourceMatchID != sourceMatchID {
		return ResumeContextResult{Route: loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN},
			errcode.New(errcode.ErrLocatorConflict, "Hub source match fence mismatch")
	}
	return resume, nil
}

// ResolveHubEndpointFromMatch is the settlement/leave return path. sourceMatchID
// is mandatory while durable placement is still BATTLE and is checked against
// both the placement record and the signed terminal/leave proof. A response
// loss retries the proof's durable operation id.
func (u *LoginUsecase) ResolveHubEndpointFromMatch(ctx context.Context, playerID, sourceMatchID uint64) (addr, ticket string, expMs int64, err error) {
	if playerID == 0 {
		return "", "", 0, errcode.New(errcode.ErrInvalidArg, "playerID must be > 0")
	}
	// enforce 必须先合并 canonical match + placement，再决定是否可以消费
	// terminal proof；active/unknown 路径在这里返回，尚未触碰 allocator。
	if _, gateErr := u.authorizeHubEntry(ctx, playerID, sourceMatchID, true); gateErr != nil {
		return "", "", 0, gateErr
	}
	if u.placementMode == placement.ModeShadow {
		perr := u.prepareHubPlacement(ctx, playerID, sourceMatchID)
		if perr != nil {
			plog.With(ctx).Warnw("msg", "hub_placement_transition_shadow_rejected",
				"player_id", playerID, "source_match_id", sourceMatchID, "err", perr)
		}
	}
	regionID, cellID := u.routeRegionCell(ctx, playerID)
	// 选角权威化:返回大厅路径也把已选角盖进新票(与登录同语义,DS 重入时同样能 spawn 对角色)。
	return u.resolveHub(ctx, playerID, regionID, cellID, u.loadSelectedRole(ctx, playerID))
}

// prepareHubPlacement performs the zero-side-effect route decision before
// AssignHub and, only with an exact Battle exit proof, begins BATTLE -> HUB.
// Missing placement is UNKNOWN. Pending operations are resumed, never replaced.
func (u *LoginUsecase) prepareHubPlacement(ctx context.Context, playerID, requestedSourceMatchID uint64) error {
	if u.placementChecker == nil {
		return errcode.New(errcode.ErrUnavailable, "placement authority unavailable")
	}
	s, err := u.placementChecker.GetPlacement(ctx, playerID)
	if err != nil {
		return err
	}
	if !s.Found {
		return errcode.New(errcode.ErrLocatorNotFound, "placement is UNKNOWN")
	}
	if _, ok := s.HubBinding(); ok {
		if requestedSourceMatchID != 0 && s.SourceMatchID != requestedSourceMatchID {
			return errcode.New(errcode.ErrLocatorConflict, "Hub placement source match mismatch")
		}
		if s.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING {
			// A target-unavailable retarget has its own allocator-signed lineage and
			// replacement operation. Replaying the original terminal/bootstrap Begin
			// would necessarily conflict. AssignHub owns exact retarget replay/lease
			// renewal before it can publish a ticket.
			if s.RetargetCount > 0 {
				if !s.Target.CompleteHub() || s.LastRetargetProofID == "" ||
					s.LastRetargetReason == locatorv1.PlacementTargetUnavailableReason_PLACEMENT_TARGET_UNAVAILABLE_REASON_UNSPECIFIED {
					return errcode.New(errcode.ErrUnavailable, "retargeted Hub placement lineage is incomplete")
				}
				return nil
			}
			// A PENDING lease can expire while login/allocator is down. Renew only
			// with the proof authority that owns this exact operation; Hub-transfer
			// source_match_id=0 remains hub_allocator-owned.
			if s.SourceMatchID != 0 {
				return u.renewBattleExitHubPlacement(ctx, playerID, s.SourceMatchID, s)
			}
			if isAccountBootstrapHubPending(s) {
				return u.renewAccountBootstrapHubPlacement(ctx, playerID, s)
			}
		}
		return nil
	}
	if s.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
		s.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB && s.MatchID == 0 &&
		s.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE && s.TargetMatchID != 0 {
		if requestedSourceMatchID == 0 || requestedSourceMatchID != s.TargetMatchID {
			return errcode.New(errcode.ErrInvalidState,
				"source match is required and must equal pending Battle target")
		}
		return u.renewBattleExitHubPlacement(ctx, playerID, s.TargetMatchID, s)
	}
	if s.TransitionState != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE ||
		s.CurrentRoute != locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE || s.MatchID == 0 {
		return errcode.New(errcode.ErrLocatorConflict, "placement does not authorize a Hub transition")
	}
	if requestedSourceMatchID == 0 || requestedSourceMatchID != s.MatchID {
		return errcode.New(errcode.ErrInvalidState,
			"source match is required and must equal authoritative Battle placement")
	}
	return u.renewBattleExitHubPlacement(ctx, playerID, s.MatchID, s)
}

func (u *LoginUsecase) renewBattleExitHubPlacement(
	ctx context.Context, playerID, matchID uint64, snapshot data.PlacementSnapshot,
) error {
	if u.battleTicketIssuer == nil {
		return errcode.New(errcode.ErrUnavailable, "battle exit proof authority unavailable")
	}
	state, proof, err := u.battleTicketIssuer.InspectBattleRouteProof(ctx, playerID, matchID)
	if err != nil || state != data.BattleRouteTerminal {
		return errcode.NewCause(errcode.ErrUnavailable, err,
			"cannot prove Battle terminal/leave transition")
	}
	stableBattle := snapshot.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE &&
		snapshot.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE && snapshot.MatchID == matchID &&
		proof.ExpectedVersion == snapshot.Version
	pendingHubRetry := snapshot.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
		snapshot.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB && snapshot.SourceMatchID == matchID &&
		proof.ExpectedVersion+1 == snapshot.Version && proof.OperationID == snapshot.OperationID
	pendingBattleCancellation := snapshot.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
		snapshot.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB && snapshot.MatchID == 0 &&
		snapshot.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE && snapshot.TargetMatchID == matchID &&
		proof.ExpectedVersion == snapshot.Version
	if proof.ExpectedVersion == 0 || proof.OperationID == "" ||
		(!stableBattle && !pendingHubRetry && !pendingBattleCancellation) {
		return errcode.New(errcode.ErrLocatorConflict, "battle exit proof no longer matches placement")
	}
	_, err = u.placementChecker.BeginHubFromBattle(ctx, playerID, matchID, proof,
		time.Now().Add(10*time.Minute).UnixMilli())
	return err
}

// guardHubRouteAgainstActiveBattle 是所有非 Login 主链 Hub 签票入口(IssueDSTicket(hub) /
// SelectRole)的 active-BATTLE 三态权威门(P0 止血,封 battle-reconnect.md §7.3 A 双归属漏洞):
//
//	ACTIVE  (locator BATTLE 且 roster 权威证明 live) → ErrInvalidState 拒绝,零副作用;
//	  客户端应重新 Login 走权威路由(会被 tryBattleReconnect 直连回原局)。
//	TERMINAL(locator BATTLE 但投影记录显式 ended/abandoned) → 放行。
//	  这是唯一允许的“BATTLE 残留”证明,覆盖正常结算回大厅(位置 TTL 尚未过期)。
//	UNKNOWN (locator 查询失败 / roster 漂移 / 记录缺失 / stale / 不可读) → fail-closed:
//	  locator 阳性 BATTLE 下不分 profile 一律 ErrUnavailable;仅“locator 查询本身失败”
//	  在 local/off 保留历史弱降级(dev 裸跑)。
//
// P0 修复(2026-07-15,Codex 复审):不再把通用 ErrPermissionDeny 当终态证明——它同时
// 覆盖 roster 漂移/非成员/记录缺失,那些必须 UNKNOWN 拒绝。只有投影记录显式终态才放行。
//
// 残余(phase 2,§7.3 J):locator key 因 DS 续期连续失败而正常过期时,本门会误判
// NOT_BATTLE。根治靠版本化 placement lease(候选 B,已拍板),不在本 P0 范围。
func (u *LoginUsecase) guardHubRouteAgainstActiveBattle(ctx context.Context, playerID uint64) error {
	h := plog.With(ctx)
	if u.notifier == nil {
		if u.requireHubAssignmentBinding {
			return errcode.New(errcode.ErrUnavailable,
				"player locator is required before hub ticket issuance")
		}
		return nil // local/off 无 locator:保留历史行为(dev 裸跑)。
	}
	bl, err := u.queryBattleLocation(ctx, playerID)
	if err != nil {
		if u.requireHubAssignmentBinding {
			return errcode.NewCause(errcode.ErrUnavailable, err,
				"cannot prove player is outside battle before hub ticket issuance")
		}
		h.Warnw("msg", "hub_route_gate_locator_degraded", "err", err, "player_id", playerID)
		return nil // local/off 保留历史弱降级。
	}
	if !bl.InBattle {
		return nil
	}
	// locator 明确 InBattle:必须由 roster 权威区分“仍在活局”与“显式终局后 TTL 残留”。
	// 不可判定时不分 profile 一律 fail-closed:阳性 BATTLE 信号下猜“已结束”就是双归属。
	if u.battleTicketIssuer == nil {
		return errcode.New(errcode.ErrUnavailable,
			"battle route authority unavailable while locator reports BATTLE")
	}
	state, rerr := u.battleTicketIssuer.InspectBattleRoute(ctx, playerID, bl.MatchID)
	switch state {
	case data.BattleRouteActive:
		h.Warnw("msg", "hub_route_rejected_active_battle",
			"player_id", playerID, "match_id", bl.MatchID)
		return errcode.New(errcode.ErrInvalidState,
			"player is in active battle (match_id=%d); reconnect via Login instead of hub ticket", bl.MatchID)
	case data.BattleRouteTerminal:
		// 权威记录显式终态(ended/abandoned) → locator BATTLE 仅为 TTL 残留,放行 Hub(正常结算回大厅)。
		return nil
	default:
		// UNKNOWN(含 roster 漂移/非成员/记录缺失/stale/错误):不得猜测,拒绝。
		h.Warnw("msg", "hub_route_rejected_unknown_battle_state",
			"player_id", playerID, "match_id", bl.MatchID, "err", rerr)
		return errcode.NewCause(errcode.ErrUnavailable, rerr,
			"cannot prove battle is over before hub ticket issuance")
	}
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
	// SelectRole 也是 Hub 物理副作用入口：先合并 canonical match + placement。
	// ALLOCATING/READY/RUNNING/UNKNOWN 时在 SetRole/AssignHub/签票前返回。
	if _, gerr := u.authorizeHubEntry(ctx, playerID, 0, false); gerr != nil {
		return "", "", 0, gerr
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

// verifyHubAssignmentTicket 验证 hub_allocator 返回的票据并读取已验签 exp。
//
// 迁移期只允许两条显式路径：HS256=legacy，RS256=v2。alg 仅用于选择 verifier，随后分支
// 必须完成各自签名/claims 校验；其它算法、缺 verifier、坏票和不完整绑定全部 fail-closed。
func (u *LoginUsecase) verifyHubAssignmentTicket(playerID uint64, assign *data.HubAssignment) (int64, error) {
	if assign == nil || assign.HubTicket == "" {
		return 0, errcode.New(errcode.ErrLoginTicketInvalid, "hub assignment ticket is empty")
	}
	alg, err := auth.DSTicketAlgorithm(assign.HubTicket)
	if err != nil {
		return 0, err
	}
	switch alg {
	case "HS256":
		// v2 verifier 非 nil 是 Login 主链的机械 RS256-only 开关；不能因为收到一张
		// HS256 票就退回 legacy verifier。SessionToken 的 HS256 验证不经过本函数。
		if u.rs256DSTicketProfileEnabled() {
			return 0, errcode.New(errcode.ErrLoginTicketInvalid,
				"legacy HS256 DSTicket is disabled by the RS256 profile")
		}
		if u == nil || u.verifier == nil {
			return 0, errcode.New(errcode.ErrUnavailable, "legacy DSTicket verifier unavailable")
		}
		claims, err := u.verifier.VerifyDSTicket(assign.HubTicket)
		if err != nil {
			return 0, err
		}
		if claims.PlayerID() != playerID || claims.DSType != string(auth.DSTypeHub) {
			return 0, errcode.New(errcode.ErrLoginTicketInvalid,
				"legacy hub ticket player or ds_type mismatch")
		}
		if claims.ExpiresAt == nil {
			return 0, errcode.New(errcode.ErrLoginTicketInvalid, "legacy hub ticket missing exp")
		}
		// 兼容窗内的旧票允许完全没有实例绑定；一旦携带 pod，就必须与 allocator 响应一致。
		if claims.DSPodName != "" && claims.DSPodName != assign.HubPodName {
			return 0, errcode.New(errcode.ErrLoginTicketInvalid, "legacy hub ticket target pod mismatch")
		}
		if u.requireHubAssignmentBinding &&
			(assign.HubPodName == "" || claims.DSPodName != assign.HubPodName ||
				claims.DSInstanceUID == "" || claims.DSProtocolEpoch == 0 ||
				claims.DSCredentialGen == 0 || claims.DSCredentialJTI == "" ||
				claims.HubAssignmentID == "" || claims.DSWriterEpoch != auth.DSAuthWriterEpochV2) {
			return 0, errcode.New(errcode.ErrLoginTicketInvalid,
				"legacy hub ticket assignment binding is incomplete")
		}
		return claims.ExpiresAt.UnixMilli(), nil

	case "RS256":
		if u == nil || u.v2Verifier == nil {
			return 0, errcode.New(errcode.ErrUnavailable, "DSTicket v2 verifier unavailable")
		}
		claims, err := u.v2Verifier.Verify(assign.HubTicket)
		if err != nil {
			return 0, err
		}
		if claims.PlayerID() != playerID || claims.DSType != string(auth.DSTypeHub) {
			return 0, errcode.New(errcode.ErrLoginTicketInvalid,
				"hub DSTicket v2 player or ds_type mismatch")
		}
		if assign.HubPodName == "" || claims.DSPodName != assign.HubPodName {
			return 0, errcode.New(errcode.ErrLoginTicketInvalid, "hub DSTicket v2 target pod mismatch")
		}
		if claims.DSInstanceUID == "" || claims.DSInstanceEpoch == 0 ||
			claims.HubAssignmentID == "" ||
			(claims.ReleaseTrack != auth.ReleaseTrackStable && claims.ReleaseTrack != auth.ReleaseTrackCanary) {
			return 0, errcode.New(errcode.ErrLoginTicketInvalid,
				"hub DSTicket v2 instance binding is incomplete")
		}
		if claims.ExpiresAt == nil {
			return 0, errcode.New(errcode.ErrLoginTicketInvalid, "hub DSTicket v2 missing exp")
		}
		return claims.ExpiresAt.UnixMilli(), nil

	default:
		// DSTicketAlgorithm 已先拒绝其它 alg；保留 default 作为未来改动的 fail-closed 保险。
		return 0, errcode.New(errcode.ErrLoginTicketInvalid, "DSTicket algorithm unsupported")
	}
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
