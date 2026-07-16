// Package biz 是 hub_allocator 服务的业务逻辑层(W4 ⑤,2026-06-06)。
//
// 职责(docs/design/go-services.md §2.12):大厅 DS 分片调度。
//   - AssignHub:玩家进大厅,按 region + 队友 + 最空分片选一个 hub DS,签 hub DSTicket
//   - ReleaseHub:玩家离开大厅,退分片占位
//   - TransferHub:跨分片传送,先占新分片再切归属,最后退旧分片,重签票据
//   - ListHubs:运维/调试查询分片负载
//   - Heartbeat:Hub DS 每 5s 主动上报(单向 unary),刷新在线数 + 心跳时刻
//   - RunHeartbeatSweep:后台扫描 active ZSET,心跳超时 → 标记 draining 停止分配(不变量 §4)
//
// 关键不变量:
//   - 玩家在线只在一个 hub(不变量 §1,GetAssignment 幂等;已分配 → 重签票不重复占位)
//   - hub DSTicket 短时效(不变量 §3,由 TicketSigner 经 pkg/auth 签 5min)
//
// 容量计数说明:player_count 由 hub_allocator 维护(Assign 自增 / Release 自减,容量判定基准);
// 真实 Hub DS Heartbeat 上报的在线数会回写对账(W4 ⑤ Mock 期无真实 DS,仅由分配计数维护)。
package biz

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/placement"
	"github.com/luyuancpp/pandora/pkg/releasetrack"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"

	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/data"
)

// 分片状态常量(对应 proto string state 字段)。
const (
	stateWarming  = "warming" // 分片已播种但尚未收到首个(鉴权)心跳:不可被 AssignHub 选中
	stateReady    = "ready"
	stateDraining = "draining"
	stateStopping = "stopping"
)

// Heartbeat 响应控制指令常量。
const (
	commandNone  = ""
	commandStop  = "stop"  // 通知孤儿 Hub DS(无对应分片镜像)自行停机
	commandDrain = "drain" // 通知 draining 分片上的 Hub DS 开始优雅迁移(下发 grace_seconds 倒计时)
)

// 迁移原因常量(HubMigrateEvent.reason)。
const migrateReasonConsolidation = "consolidation"

// presenceRefreshTimeout 是心跳后异步续期在场玩家 HUB 位置的独立 ctx 预算(在线保活,弱依赖)。
const presenceRefreshTimeout = 3 * time.Second

// TicketSigner 抽象 hub DSTicket 签发(biz 不依赖 pkg/auth 具体实现,便于测试)。
type TicketSigner interface {
	// SignHubTicket 给 playerID 签一张 hub DSTicket,返回 token + 过期毫秒。
	// roleID(选角权威化 2026-07-08):玩家已选角色,>0 时盖进票据 role_id claim
	// (DS 验签后直接用它 spawn);0 = 未选角,claim 不序列化(与旧票兼容)。
	SignHubTicket(playerID uint64, roleID uint32, binding HubTicketBinding) (token string, expiresAtMs int64, err error)
}

// HubTicketBinding 把 hub 入场票绑定到当前归属版本和目标 DS active 凭据。
// legacy 模式使用零值；Model B 必须六项完整。
type HubTicketBinding struct {
	PodName         string
	InstanceUID     string
	ProtocolEpoch   uint32
	CredentialGen   uint64
	CredentialJTI   string
	HubAssignmentID string
	WriterEpoch     uint32
	ReleaseTrack    string
	Placement       placement.Binding
}

// HubMigratePusher 抽象强制整合迁移通知推送(走 Kafka topic pandora.hub.migrate,key=player_id)。
// 弱依赖:nil 时跳过推送(整合仍做服务端权威搬迁,Hub DS drain 心跳指令兼底客户端重连)。
type HubMigratePusher interface {
	// PushMigrate 把 HubMigrateEvent 序列化后的 payload 推给单个玩家。
	PushMigrate(ctx context.Context, playerID uint64, payload []byte) error
}

// HubUsecase 是 hub_allocator 业务逻辑核心。
type HubUsecase struct {
	repo    data.HubRepo
	fleet   HubFleetProvider
	scaler  HubFleetScaler
	signer  TicketSigner
	migrate HubMigratePusher
	locator data.HubLocationChecker
	cfg     conf.HubConf
	// requireHeartbeatReady:播种分片镜像时先置 warming,等首个通过 Guard 的 Hub DS 心跳才转 ready
	// (审核 P1:agones PATCH/进程拉起成功 ≠ DS 已真正鉴权回调,不能直接当 ready 否则会把玩家路由到
	// 一个从未成功心跳的 Hub)。mode=agones 置 true;mock/local 不置(无真实心跳/保 dev 自测不坏)。
	requireHeartbeatReady bool
	// dsTokenGeneration:令牌代际绑定开关(审核 P1-6/P1-8;仅 agones + ds_auth.mode=enforce 置 true)。
	// 开启后拓扑对账把候选令牌代际(Redis INCR 单调值)写进镜像 CurrentTokenGen,重签(gen 递增)时复位分片 warming;
	// 心跳侧只有携带当前代际已验签令牌的心跳才能把 warming 翻回 ready(gen 精确相等)。off/permissive 不开:
	// 守卫不验签、心跳无可信 gen,开了会自锁。
	dsTokenGeneration bool
	// authRepo:Model B「Redis 唯一授权权威」授权记录仓(decision-revisit-ds-callback-auth §7)。
	// 仅 ds_auth.authority_mode=redis(agones+enforce)时由 main 注入;nil = legacy 代际门路径
	// (mock/local/off 及默认 legacy 权威模式)。装配后:①心跳走 ActivateHeartbeat 单事务线性化点
	// (授权 promote + 分片 warming→ready + 投影 active 元组同一 EXEC),stale 一律 fail-closed;
	// ②AssignHub/TransferHub 走 ReserveRoutableSeat/CheckRoutable 原子终态门。
	// 此时 dsTokenGeneration 关闭(Model B 授权记录取代 legacy 镜像代际门)。
	authRepo data.HubAuthRepo
	// authTTL:Model B 授权记录键 TTL(CE8:必须独立于 shardTTL,授权寿命远长于分片镜像 TTL,
	// 否则授权键被 shardTTL 提前过期会导致「有效凭据被判 stale」)。main 注入(默认 2×HubTokenTTL,floor 48h)。
	authTTL time.Duration
	// releasePolicy 只决定无 assignment 玩家首次尝试的轨；实际命中轨写入 assignment 后粘性。
	releasePolicy   releasetrack.Policy
	placementMode   placement.Mode
	placementClient data.HubPlacementClient
	// One Hub authority key, separate canonical HMAC domains for logical
	// HubTransfer/Retarget and physical HubDeparture.
	placementProofSigner *placement.ProofSigner
}

// HubCredential 是 service 层从**验签通过**的 Model B hub 令牌抽出的凭据身份(§7),
// 由 HeartbeatWithCredential 透传到 authRepo.ActivateHeartbeat 做 promote/validate 匹配。
type HubCredential struct {
	InstanceUID   string
	ProtocolEpoch uint32
	Gen           uint64
	JTI           string
	TokenSHA256   string
	Kid           string
	WriterEpoch   uint32
}

// NewHubUsecase 构造 HubUsecase。
func NewHubUsecase(repo data.HubRepo, fleet HubFleetProvider, signer TicketSigner, cfg conf.HubConf) *HubUsecase {
	var scaler HubFleetScaler
	if s, ok := fleet.(HubFleetScaler); ok {
		scaler = s
	}
	return &HubUsecase{repo: repo, fleet: fleet, scaler: scaler, signer: signer, cfg: cfg}
}

// SetMigratePusher 注入强制整合迁移通知推送器(弱依赖,不改 NewHubUsecase 签名以不破现有测试/调用方)。
func (u *HubUsecase) SetMigratePusher(p HubMigratePusher) { u.migrate = p }

// SetLocationChecker 注入 player_locator 位置检查器(弱依赖:玩家切线护栏,nil 时跳过战斗/匹配中检查)。
func (u *HubUsecase) SetLocationChecker(c data.HubLocationChecker) { u.locator = c }

func (u *HubUsecase) SetPlacementPolicy(mode placement.Mode, client data.HubPlacementClient) {
	u.placementMode, u.placementClient = mode, client
}

func (u *HubUsecase) SetPlacementProofSigner(signer *placement.ProofSigner) {
	u.placementProofSigner = signer
}

// SetRequireHeartbeatReady 开启「先 warming、首个鉴权心跳才 ready」(agones 真 DS 链路置 true)。
// off/mock/local 不置:无真实心跳的模式仍直接播种 ready,保持现有 dev/离线联调行为不变。
func (u *HubUsecase) SetRequireHeartbeatReady(b bool) { u.requireHeartbeatReady = b }

// SetDSTokenGeneration 开启令牌代际绑定(仅 agones + enforce;见字段注释)。
func (u *HubUsecase) SetDSTokenGeneration(b bool) { u.dsTokenGeneration = b }

// SetAuthRepo 注入 Model B 授权记录仓(§7;仅 ds_auth.authority_mode=redis 时装配)。
// 装配后心跳走 ActivateHeartbeat 单事务线性化点、AssignHub/TransferHub 走 ReserveRoutableSeat/
// CheckRoutable 原子终态门;nil 保持 legacy 代际门路径不变。
func (u *HubUsecase) SetAuthRepo(r data.HubAuthRepo) { u.authRepo = r }

// SetAuthTTL 注入 Model B 授权记录键 TTL(CE8;独立于 shardTTL,授权寿命更长)。
func (u *HubUsecase) SetAuthTTL(d time.Duration) { u.authTTL = d }

// SetReleaseTrackPolicy 注入 player_id 级确定性 cohort 策略。
func (u *HubUsecase) SetReleaseTrackPolicy(p releasetrack.Policy) { u.releasePolicy = p }

// authTTLDur 返回授权键 TTL:main 已注入用注入值;未注入(测试/兜底)回退 2×shardTTL,
// 绝不返回 0(0 = Redis 永不过期,授权键会泄漏)。
func (u *HubUsecase) authTTLDur() time.Duration {
	if u.authTTL > 0 {
		return u.authTTL
	}
	return u.shardTTL() * 2
}

// heartbeatMaxAgeMs 返回「分片心跳仍算新鲜」的最大毫秒(= 心跳超时阈值)。
// ReserveRoutableSeat/CheckRoutable 用它拒绝把玩家分到心跳已陈旧但镜像尚未被 sweep 标 draining 的分片。
func (u *HubUsecase) heartbeatMaxAgeMs() int64 {
	return u.cfg.HeartbeatTimeout.Std().Milliseconds()
}

// candidateTokenExp 返回播种/对账时写入镜像的令牌 exp 镜像值(仅调试/兼容,不再当代际):
// 仅代际绑定开启且候选带有效 exp 时记录,其余恒 0。代际识别已改用 candidateTokenGen。
func (u *HubUsecase) candidateTokenExp(expMs int64) uint64 {
	if u.dsTokenGeneration && expMs > 0 {
		return uint64(expMs)
	}
	return 0
}

// candidateTokenGen 返回播种/对账时写入镜像的令牌「代际」(Redis INCR 单调值):仅代际绑定
// 开启时记录候选 gen,其余恒 0(= 不启用;off/permissive 心跳无已验签 claims,开了会自锁)。
// 替代秒级 exp 代际:gen 来自 Redis INCR 单调,同秒多次重签也不碰撞(审核 P1-6),且可精确相等比较。
func (u *HubUsecase) candidateTokenGen(gen uint64) uint64 {
	if u.dsTokenGeneration {
		return gen
	}
	return 0
}

// initialShardState 返回播种新分片镜像的初始状态:需心跳确认时 warming,否则直接 ready。
func (u *HubUsecase) initialShardState() string {
	if u.requireHeartbeatReady {
		return stateWarming
	}
	return stateReady
}

func (u *HubUsecase) shardTTL() time.Duration         { return u.cfg.ShardTTL.Std() }
func (u *HubUsecase) assignTTL() time.Duration        { return u.cfg.AssignmentTTL.Std() }
func (u *HubUsecase) reservationTTL() time.Duration   { return u.cfg.ReservationTTL.Std() }
func (u *HubUsecase) transferCooldown() time.Duration { return u.cfg.TransferCooldown.Std() }
func (u *HubUsecase) retry() int                      { return u.cfg.OptimisticRetry }

// assignmentSagaTTL keeps the exact Hub owner durable in enforce mode, both
// while placement is PENDING and after STABLE Admission. Connected ownership
// deliberately has no TTL; expiring only its canonical assignment would make a
// later Login create a second Hub owner. Release/Departure/transfer cleanup are
// explicit exact operations, never inferred from time.
func (u *HubUsecase) assignmentSagaTTL() time.Duration {
	if u.placementMode == placement.ModeEnforce {
		return 0
	}
	return u.assignTTL()
}

// ── RPC 1:AssignHub ───────────────────────────────────────────────────────────

// AssignResult 是 AssignHub 的出参。
type AssignResult struct {
	HubDSAddr   string
	HubTicket   string
	HubPodName  string
	ShardID     uint32
	TicketExpMs int64
	Placement   placement.Binding
}

// AssignHub 为玩家分配一个大厅 DS 分片。幂等:已分配且分片可用 → 重签票返回。
//
// roleID(选角权威化 2026-07-08):玩家已选角色(login 从 player_roles 读出透传)。
// >0 时覆盖归属镜像里的 role_id(login 是角色数据权威,换角重选以新值为准);
// 0 = 调用方不知角色,保留已存镜像值(Transfer/重签路径不丢角色)。
// 最终生效的 role 盖进本次签发的 hub 票据 claim(票据单一签发权威在本服务)。
func (u *HubUsecase) AssignHub(ctx context.Context, playerID uint64, region string, teamID uint64, roleID uint32) (*AssignResult, error) {
	return u.AssignHubWithPlacement(ctx, playerID, region, teamID, roleID, placement.Binding{})
}

func (u *HubUsecase) AssignHubWithPlacement(ctx context.Context, playerID uint64, region string, teamID uint64, roleID uint32, placementBinding placement.Binding) (*AssignResult, error) {
	if playerID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if err := placementBinding.ValidateOptional(); err != nil {
		return nil, errcode.NewCause(errcode.ErrInvalidArg, err, "invalid placement binding")
	}
	if u.placementMode == placement.ModeEnforce && !placementBinding.Complete() {
		return nil, errcode.New(errcode.ErrLocatorNotFound, "placement binding required before Hub assignment")
	}
	if region == "" {
		region = u.cfg.DefaultRegion
	}

	for attempt := 0; attempt < 8; attempt++ {
		existing, found, err := u.repo.GetAssignment(ctx, playerID)
		if err != nil {
			return nil, err
		}
		if found && (existing.GetTransferCleanupPending() || existing.GetReleaseCleanupPending()) {
			var stillFound bool
			existing, stillFound, err = u.resumeAssignmentCleanup(ctx, playerID, existing.GetAssignmentId())
			if err != nil {
				return nil, err
			}
			if !stillFound {
				continue
			}
		}
		effectiveRole := roleID
		desiredTrack := u.releasePolicy.Select(playerID)
		if found {
			var trackErr error
			desiredTrack, trackErr = stickyReleaseTrack(existing.GetReleaseTrack())
			if trackErr != nil {
				return nil, trackErr
			}
			effectiveRole = effectiveRoleID(roleID, existing.RoleId)
			requiresNewPhysicalAdmission := u.placementMode == placement.ModeEnforce &&
				placementBinding.Complete() &&
				!placementBinding.Equal(placementBindingFromAssignment(existing))
			current, reusable, rerr := u.assignmentRoutable(ctx, playerID, existing)
			if rerr != nil {
				return nil, rerr // Redis/授权读取失败不能降级成另分配
			}
			if !requiresNewPhysicalAdmission && (reusable || assignmentSameInstance(existing, &current)) {
				// assignment 可以比 admission ledger 活得更久：clean Logout 会精确删除 session，
				// 未进场 reservation 也会绝对到期。重签票前必须在 {pod} 同槽事务里重新确保
				// “已有 session 或新鲜 reservation”；否则会返回一张必被 Admission 拒绝的票。
				// ReserveAssignment 对已有 session/reservation 幂等，不重复计数；这里也绝不能在
				// 后续跨槽 assignment CAS loser 时补偿删除，因为该 seat 可能是原连接的共享 session。
				ensured, ensureErr := u.ensureExistingAssignmentSeat(ctx, playerID, existing, &current)
				if ensureErr == nil {
					next := proto.Clone(existing).(*hubv1.HubAssignmentStorageRecord)
					next.HubAddr, next.ShardId, next.Region = ensured.HubAddr, ensured.ShardID, ensured.Region
					next.ReleaseTrack = ensured.ReleaseTrack
					next.RoleId = effectiveRole
					if next.AssignmentId == "" {
						next.AssignmentId = uuid.NewString()
					}
					bindAssignmentAuth(next, ensured)
					bindAssignmentPlacement(next, placementBinding)
					// 即使归属 bytes 完全相同也必须走 CAS SET 刷新 assignment TTL。CAS 仍以
					// 完整旧 bytes 为前置；失败时不清理 ensure 的共享 seat，交 winner 精确释放
					// 或让新建 reservation 的有界 TTL 回收。
					swapped, serr := u.repo.CompareAndSwapAssignment(ctx, playerID, existing, next, u.assignmentSagaTTL())
					if serr != nil {
						return nil, serr
					}
					if !swapped {
						continue
					}
					u.addShardMember(ctx, next.HubPodName, playerID)
					bound, err := u.bindPlacementTarget(ctx, playerID, next)
					if err != nil {
						return nil, err
					}
					if u.placementMode == placement.ModeEnforce ||
						(u.placementMode == placement.ModeShadow && placementBindingFromAssignment(bound).Complete()) {
						// Bind can turn a STABLE same-target placement into a new HubTransfer
						// operation.  The ticket must never escape while the handoff capability
						// is still bound to the pre-bind version.
						if _, ensureErr := u.ensureExistingAssignmentSeat(ctx, playerID, bound, ensured); ensureErr != nil {
							return nil, ensureErr
						}
					}
					return u.signResult(ctx, playerID, effectiveRole, bound)
				}
				if errcode.As(ensureErr) != errcode.ErrHubNoAvailable {
					return nil, ensureErr
				}
				// 旧 assignment 已无 seat 且原分片已满/漂移时，继续走新 assignment 选择；
				// 不能反复刷新旧归属后返回永远无法 Admission 的票。
			}
		}

		if err := u.ensureShards(ctx, region, desiredTrack); err != nil {
			return nil, err
		}
		assignmentID := uuid.NewString()
		target, seat, err := u.selectAndReserveShard(ctx, playerID, assignmentID, region, teamID, "", desiredTrack)
		// 只有“首次分配且 cohort=canary 且 canary 明确无可用容量”才允许回退 stable；
		// 已有 assignment 必须保持粘性，stable 也绝不反向进入 canary。
		if err != nil && !found && desiredTrack == releasetrack.Canary && errcode.As(err) == errcode.ErrHubNoAvailable {
			if ensureErr := u.ensureShards(ctx, region, releasetrack.Stable); ensureErr != nil {
				return nil, ensureErr
			}
			target, seat, err = u.selectAndReserveShard(ctx, playerID, assignmentID, region, teamID, "", releasetrack.Stable)
		}
		if err != nil {
			if errcode.As(err) == errcode.ErrHubNoAvailable {
				u.tryScaleOutOnNoCapacity(ctx, region)
			}
			return nil, err
		}
		assignment := &hubv1.HubAssignmentStorageRecord{}
		if found {
			assignment = proto.Clone(existing).(*hubv1.HubAssignmentStorageRecord)
		}
		assignment.PlayerId = playerID
		assignment.HubPodName = target.HubPodName
		assignment.HubAddr = target.HubAddr
		assignment.ShardId = target.ShardId
		assignment.Region = target.Region
		assignment.TeamId = teamID
		assignment.AssignedAtMs = time.Now().UnixMilli()
		assignment.RoleId = effectiveRole
		assignment.AssignmentId = assignmentID
		assignment.ReleaseTrack = target.ReleaseTrack
		bindAssignmentAuth(assignment, seat)
		bindAssignmentPlacement(assignment, placementBinding)
		cleanupRegistered := false
		if found && u.placementMode == placement.ModeEnforce {
			if cleanupErr := u.registerTransferCleanup(ctx, assignment, existing); cleanupErr != nil {
				u.compensateReservedSeat(ctx, target.HubPodName, playerID, assignmentID, seat)
				return nil, cleanupErr
			}
			cleanupRegistered = true
		}
		// placement=off 没有随后必须先落盘的跨服务状态，继续保持原有的“先签票、
		// 再发布 assignment”顺序。这样签名器失败时可以用 reservation identity
		// 精确补偿，既不暴露拿不到票的归属，也不泄漏容量。shadow/enforce 仍需先
		// 持久化可恢复的 assignment saga，再尝试 BindTarget。
		var signedResult *AssignResult
		if u.placementMode == placement.ModeOff {
			var signErr error
			signedResult, signErr = u.signResult(ctx, playerID, effectiveRole, assignment)
			if signErr != nil {
				u.compensateReservedSeat(ctx, target.HubPodName, playerID, assignmentID, seat)
				return nil, signErr
			}
		}
		var expected *hubv1.HubAssignmentStorageRecord
		if found {
			expected = existing
		}
		swapped, serr := u.repo.CompareAndSwapAssignment(ctx, playerID, expected, assignment, u.assignmentSagaTTL())
		if serr != nil {
			// In enforce mode the CAS result may be unknown. Keep the index-first
			// ref and exact reservation; restart reconciliation distinguishes a
			// committed saga from an orphan without risking the new owner.
			if !cleanupRegistered {
				u.compensateReservedSeat(ctx, target.HubPodName, playerID, assignmentID, seat)
			}
			return nil, serr
		}
		if !swapped {
			if cleanupRegistered {
				u.removeTransferCleanupRef(ctx, existing.GetHubPodName(), transferCleanupRef(assignment))
			}
			u.compensateReservedSeat(ctx, target.HubPodName, playerID, assignmentID, seat)
			continue
		}

		u.addShardMember(ctx, target.HubPodName, playerID)
		if teamID != 0 {
			if terr := u.repo.SetTeamShard(ctx, teamID, target.HubPodName, u.assignTTL()); terr != nil {
				plog.With(ctx).Warnw("msg", "set_team_shard_failed", "team_id", teamID, "err", terr)
			}
		}
		// Assignment（玩家槽）和 placement（{player} 槽）是显式 saga。先落可恢复的
		// assignment/reservation，再幂等 BindTarget；未知结果保留记录供同 operation 重试，
		// 绝不能补偿后改绑另一 DS。只有 Bind 确认后才签票。
		boundAssignment := assignment
		if cleanupRegistered {
			var stillFound bool
			boundAssignment, stillFound, err = u.resumeAssignmentCleanup(ctx, playerID, assignmentID)
			if err != nil {
				return nil, err
			}
			if !stillFound {
				return nil, errcode.New(errcode.ErrInvalidState,
					"Hub replacement assignment disappeared during cleanup")
			}
		} else {
			boundAssignment, err = u.bindPlacementTarget(ctx, playerID, assignment)
			if err != nil {
				return nil, err
			}
		}
		assignment = boundAssignment
		if signedResult == nil {
			var signErr error
			signedResult, signErr = u.signResult(ctx, playerID, effectiveRole, assignment)
			if signErr != nil {
				return nil, signErr
			}
		}
		// Bind + ticket creation are the publication gate for a replacement
		// assignment. Until both succeed retain the exact previous physical
		// owner; Login can resume the persisted new operation without leaving
		// the player ownerless during a locator/signer outage.
		if found && !cleanupRegistered {
			u.releaseAssignmentSeat(ctx, existing)
			u.removeShardMember(ctx, existing.HubPodName, playerID)
		}
		plog.With(ctx).Infow("msg", "hub_assigned",
			"player_id", playerID, "pod", target.HubPodName, "shard_id", target.ShardId,
			"region", target.Region, "release_track", target.ReleaseTrack)
		return signedResult, nil
	}
	return nil, errcode.New(errcode.ErrHubNoAvailable, "player %d assignment changed concurrently", playerID)
}

// ── RPC 2:ReleaseHub ──────────────────────────────────────────────────────────

// ReleaseHub 玩家离开大厅,退分片占位 + 删归属。幂等:无归属视为已离开。
func (u *HubUsecase) ReleaseHub(ctx context.Context, playerID uint64) error {
	if playerID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	for attempt := 0; attempt < 8; attempt++ {
		assignment, found, err := u.repo.GetAssignment(ctx, playerID)
		if err != nil {
			return err
		}
		if !found {
			return nil // 幂等
		}
		if assignment.GetTransferCleanupPending() || assignment.GetReleaseCleanupPending() {
			_, stillFound, cleanupErr := u.resumeAssignmentCleanup(ctx, playerID, assignment.GetAssignmentId())
			if cleanupErr != nil {
				return cleanupErr
			}
			if !stillFound {
				return nil
			}
			continue
		}
		if u.authRepo != nil {
			current, reusable, routeErr := u.assignmentRoutable(ctx, playerID, assignment)
			if routeErr != nil {
				return routeErr
			}
			if !reusable {
				if assignmentSameInstance(assignment, &current) {
					// 同实例普通凭据轮换：先把归属 CAS 到当前 active，再重新进入精确
					// Release；不占新座，也不会拿旧 tuple 删除归属后退座失败。
					next := proto.Clone(assignment).(*hubv1.HubAssignmentStorageRecord)
					bindAssignmentAuth(next, &current)
					swapped, swapErr := u.repo.CompareAndSwapAssignment(ctx, playerID, assignment, next, u.assignmentSagaTTL())
					if swapErr != nil {
						return swapErr
					}
					if !swapped {
						continue
					}
					continue
				}
				return errcode.New(errcode.ErrInvalidState,
					"hub assignment is not bound to the current active credential")
			}
			ref := transferCleanupRef(assignment)
			// Index-first: after this succeeds, a process crash before/after the
			// tombstone CAS remains enumerable. CAS losers remove only their exact
			// assignment ref.
			if indexErr := u.repo.RegisterTransferCleanup(ctx, assignment.GetHubPodName(), ref); indexErr != nil {
				return indexErr
			}
			next := proto.Clone(assignment).(*hubv1.HubAssignmentStorageRecord)
			next.ReleaseCleanupPending = true
			next.ReleaseCleanupMatchId = 0
			next.ReleaseCleanupPlacementVersion = 0
			next.ReleaseCleanupOperationId = ""
			marked, markErr := u.repo.CompareAndSwapAssignment(ctx, playerID, assignment, next, u.assignmentSagaTTL())
			if markErr != nil {
				// Unknown CAS result: retain the ref. Reconciler removes an orphan or
				// resumes the durable tombstone; never report success here.
				return markErr
			}
			if !marked {
				u.removeTransferCleanupRef(ctx, assignment.GetHubPodName(), ref)
				continue
			}
			_, stillFound, cleanupErr := u.resumeAssignmentCleanup(ctx, playerID, assignment.GetAssignmentId())
			if cleanupErr != nil {
				return cleanupErr
			}
			if stillFound {
				return errcode.New(errcode.ErrInvalidState, "Hub release cleanup did not delete assignment")
			}
			plog.With(ctx).Infow("msg", "hub_released", "player_id", playerID, "pod", assignment.HubPodName)
			return nil
		}
		// Legacy/off path has no exact Model-B owner. Preserve the historical
		// CAS/delete behavior; placement enforce cannot reach this branch because
		// strict Admission itself requires authRepo.
		deleted, derr := u.repo.CompareAndSwapAssignment(ctx, playerID, assignment, nil, 0)
		if derr != nil {
			return derr
		}
		if !deleted {
			continue
		}
		u.releaseAssignmentSeat(ctx, assignment)
		u.removeShardMember(ctx, assignment.HubPodName, playerID)
		plog.With(ctx).Infow("msg", "hub_released", "player_id", playerID, "pod", assignment.HubPodName)
		return nil
	}
	return errcode.New(errcode.ErrInternal, "player %d release CAS retry exhausted", playerID)
}

func exactPendingBattleDeparture(s data.HubPlacementSnapshot, matchID uint64,
	binding placement.Binding) bool {
	return s.Found && binding.Complete() && s.Binding.Equal(binding) &&
		s.SourceBinding.Complete() && s.SourceTarget.CompleteHub() &&
		releasetrack.Valid(s.SourceTarget.ReleaseTrack) && s.SourceTarget.AllocationID == "" &&
		s.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB && s.CurrentMatchID == 0 &&
		s.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE &&
		s.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
		s.TargetMatchID == matchID && s.Target.CompleteBattle()
}

func hubSourceDepartureProof(playerID uint64, snapshot data.HubPlacementSnapshot) (
	placement.SourceDepartureProof, error,
) {
	validTarget := snapshot.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB &&
		snapshot.TargetMatchID == 0 && snapshot.Target.CompleteHub() ||
		snapshot.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE &&
			snapshot.TargetMatchID > 0 && snapshot.Target.CompleteBattle()
	if playerID == 0 || !snapshot.Found || !snapshot.Binding.Complete() ||
		!snapshot.SourceBinding.Complete() || snapshot.SourceBinding.Version >= snapshot.Binding.Version ||
		snapshot.CurrentRoute != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB || snapshot.CurrentMatchID != 0 ||
		snapshot.TransitionState != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING ||
		!snapshot.SourceTarget.CompleteHub() || snapshot.SourceTarget.AllocationID != "" ||
		!releasetrack.Valid(snapshot.SourceTarget.ReleaseTrack) || !validTarget {
		return placement.SourceDepartureProof{}, errcode.New(errcode.ErrLocatorConflict,
			"locator has no exact pending Hub physical source")
	}
	material := fmt.Sprintf("%d\n%d\n%s\n%d\n%d\n%d\n%s\n%s\n%s\n%d\n%s\n%s",
		playerID, snapshot.Binding.Version, snapshot.Binding.OperationID,
		snapshot.TargetRoute, snapshot.TargetMatchID,
		snapshot.SourceBinding.Version, snapshot.SourceBinding.OperationID, snapshot.SourceTarget.PodName,
		snapshot.SourceTarget.InstanceUID, snapshot.SourceTarget.InstanceEpoch,
		snapshot.SourceTarget.AssignmentID, snapshot.SourceTarget.ReleaseTrack)
	digest := sha256.Sum256([]byte(material))
	return placement.SourceDepartureProof{
		PlayerID: playerID, PlacementVersion: snapshot.Binding.Version,
		OperationID: snapshot.Binding.OperationID, TargetRoute: int32(snapshot.TargetRoute),
		TargetMatchID:          snapshot.TargetMatchID,
		SourcePlacementVersion: snapshot.SourceBinding.Version,
		SourceOperationID:      snapshot.SourceBinding.OperationID,
		SourceRoute:            placement.RouteHub, SourceMatchID: 0,
		SourceTarget: snapshot.SourceTarget, ProofType: placement.ProofHubDeparture,
		ProofID: fmt.Sprintf("hub-departure:%x", digest),
	}, nil
}

func hubSourceDepartureConfirmationMatches(snapshot data.HubPlacementSnapshot,
	proof placement.SourceDepartureProof,
) bool {
	return snapshot.SourceDepartureConfirmed &&
		int32(snapshot.SourceDepartureProofType) == placement.ProofHubDeparture &&
		snapshot.SourceDepartureProofID == proof.ProofID
}

func sameHubPhysicalSource(expected, actual placement.Target) bool {
	return expected.PodName != "" && expected.PodName == actual.PodName &&
		expected.InstanceUID != "" && expected.InstanceUID == actual.InstanceUID &&
		expected.InstanceEpoch != 0 && expected.InstanceEpoch == actual.InstanceEpoch &&
		expected.AssignmentID != "" && expected.AssignmentID == actual.AssignmentID &&
		(expected.ReleaseTrack == "" || expected.ReleaseTrack == actual.ReleaseTrack)
}

func samePlacementLineage(expected, actual placement.Binding) bool {
	return expected.Version == actual.Version && expected.OperationID == actual.OperationID
}

// confirmHubSourceDeparture runs only after the exact source ledger is absent.
// The assignment cleanup record retains expectedSource until the locator ACK is
// known, so an RPC response loss replays the same proof id after a restart.
func (u *HubUsecase) confirmHubSourceDeparture(ctx context.Context, playerID uint64,
	targetBinding, expectedSourceBinding placement.Binding, expectedSource placement.Target,
) error {
	if u.placementMode != placement.ModeEnforce || u.placementClient == nil ||
		u.placementProofSigner == nil || u.authRepo == nil {
		return errcode.New(errcode.ErrUnavailable, "Hub source departure authority unavailable")
	}
	snapshot, err := u.placementClient.GetHubPlacement(ctx, playerID)
	if err != nil {
		return err
	}
	proof, err := hubSourceDepartureProof(playerID, snapshot)
	if err != nil {
		return err
	}
	if !snapshot.Binding.Equal(targetBinding) ||
		(expectedSourceBinding.Complete() && !samePlacementLineage(expectedSourceBinding, snapshot.SourceBinding)) ||
		!sameHubPhysicalSource(expectedSource, snapshot.SourceTarget) {
		return errcode.New(errcode.ErrLocatorConflict,
			"Hub source departure tuple changed after physical cleanup")
	}
	seat, err := u.authRepo.InspectAssignmentSeat(ctx, snapshot.SourceTarget.PodName,
		data.AssignmentInstanceIdentity{PlayerID: playerID, AssignmentID: snapshot.SourceTarget.AssignmentID,
			InstanceUID: snapshot.SourceTarget.InstanceUID, ProtocolEpoch: snapshot.SourceTarget.InstanceEpoch,
			WriterEpoch: auth.DSAuthWriterEpochV2, PlacementVersion: snapshot.SourceBinding.Version,
			PlacementOperationID: snapshot.SourceBinding.OperationID,
			SourceMatchID:        snapshot.SourceBinding.SourceMatchID})
	if err != nil {
		return err
	}
	if seat.Conflict {
		return errcode.New(errcode.ErrLocatorConflict,
			"Hub source departure census found a different physical owner")
	}
	if !seat.AlreadyAbsent || seat.Reserved || seat.Connected {
		return errcode.New(errcode.ErrUnavailable,
			"Hub source departure census has not proved exact absence")
	}
	return u.placementClient.ConfirmHubSourceDeparture(ctx, proof,
		u.placementProofSigner.SignSourceDeparture(proof))
}

func hubTicketPlacementReady(playerID uint64, snapshot data.HubPlacementSnapshot,
	binding placement.Binding, target placement.Target, nowMs int64,
) bool {
	if playerID == 0 || nowMs <= 0 || !snapshot.Found || !snapshot.Binding.Equal(binding) ||
		!snapshot.Target.Equal(target) || !target.CompleteHub() {
		return false
	}
	if snapshot.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE {
		return snapshot.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB &&
			snapshot.CurrentMatchID == 0 && snapshot.TargetMatchID == 0 &&
			snapshot.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_UNSPECIFIED &&
			snapshot.LeaseDeadlineMs == 0 && snapshot.SourceBinding.Empty() &&
			snapshot.SourceTarget.Equal(placement.Target{}) && !snapshot.SourceDepartureConfirmed &&
			snapshot.SourceDepartureProofType == locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED &&
			snapshot.SourceDepartureProofID == ""
	}
	if snapshot.TransitionState != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING ||
		snapshot.TargetRoute != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB || snapshot.TargetMatchID != 0 ||
		snapshot.LeaseDeadlineMs <= nowMs {
		return false
	}
	// Account creation has no physical source to evict.
	if snapshot.Binding.Version == 1 &&
		snapshot.ProofType == locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP &&
		snapshot.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_UNSPECIFIED &&
		snapshot.CurrentMatchID == 0 && binding.SourceMatchID == 0 && snapshot.SourceBinding.Empty() &&
		snapshot.SourceTarget.Equal(placement.Target{}) && !snapshot.SourceDepartureConfirmed &&
		snapshot.SourceDepartureProofType == locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED &&
		snapshot.SourceDepartureProofID == "" {
		return true
	}
	if !snapshot.SourceBinding.Complete() || !snapshot.SourceDepartureConfirmed ||
		snapshot.SourceBinding.Version >= snapshot.Binding.Version ||
		snapshot.SourceBinding.OperationID == snapshot.Binding.OperationID ||
		snapshot.SourceDepartureProofID == "" {
		return false
	}
	if snapshot.SourceTarget.CompleteHub() && snapshot.SourceTarget.AllocationID == "" &&
		snapshot.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB && snapshot.CurrentMatchID == 0 {
		proof, err := hubSourceDepartureProof(playerID, snapshot)
		return err == nil && hubSourceDepartureConfirmationMatches(snapshot, proof)
	}
	return snapshot.SourceTarget.CompleteBattle() && snapshot.SourceTarget.AssignmentID == "" &&
		snapshot.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE &&
		snapshot.CurrentMatchID > 0 && snapshot.Binding.SourceMatchID == snapshot.CurrentMatchID &&
		int32(snapshot.SourceDepartureProofType) == placement.ProofBattleDeparture
}

func (u *HubUsecase) requireHubTicketPlacementReady(ctx context.Context, playerID uint64,
	assignment *hubv1.HubAssignmentStorageRecord,
) error {
	if u.placementMode != placement.ModeEnforce {
		return nil
	}
	if u.placementClient == nil || assignment == nil {
		return errcode.New(errcode.ErrUnavailable, "Hub ticket placement authority unavailable")
	}
	snapshot, err := u.placementClient.GetHubPlacement(ctx, playerID)
	if err != nil {
		return err
	}
	if !hubTicketPlacementReady(playerID, snapshot, placementBindingFromAssignment(assignment),
		placementTargetFromAssignment(assignment), time.Now().UnixMilli()) {
		return errcode.New(errcode.ErrLocatorConflict,
			"Hub ticket requires exact committed or source-confirmed placement")
	}
	return nil
}

// assignmentMatchesBattleDepartureSource ties allocator state back to the
// immutable Hub source captured atomically by player_locator Begin.  The
// source record stores version/op but deliberately has no nested
// source_match_id field, so version+operation are the source binding identity;
// the complete physical target supplies the remaining exact tuple.
func assignmentMatchesBattleDepartureSource(a *hubv1.HubAssignmentStorageRecord, playerID uint64,
	s data.HubPlacementSnapshot) bool {
	return a != nil && a.GetPlayerId() == playerID &&
		a.GetPlacementVersion() == s.SourceBinding.Version &&
		a.GetPlacementOperationId() == s.SourceBinding.OperationID &&
		placementTargetFromAssignment(a).Equal(s.SourceTarget)
}

// battleDepartureSourceTombstone reconstructs only the immutable fields
// needed to inspect/evict an exact physical source.  It is used when the
// canonical assignment key was lost while the persistent per-pod session may
// still exist.  Treating a missing assignment as departed would strand a Pawn
// in Hub while publishing the Battle owner.
func battleDepartureSourceTombstone(playerID, matchID uint64, binding placement.Binding,
	s data.HubPlacementSnapshot) *hubv1.HubAssignmentStorageRecord {
	return &hubv1.HubAssignmentStorageRecord{
		PlayerId: playerID, HubPodName: s.SourceTarget.PodName,
		HubInstanceUid: s.SourceTarget.InstanceUID, AuthEpoch: s.SourceTarget.InstanceEpoch,
		AssignmentId: s.SourceTarget.AssignmentID, AuthWriterEpoch: auth.DSAuthWriterEpochV2,
		ReleaseTrack:     s.SourceTarget.ReleaseTrack,
		PlacementVersion: s.SourceBinding.Version, PlacementOperationId: s.SourceBinding.OperationID,
		ReleaseCleanupPending: true, ReleaseCleanupMatchId: matchID,
		ReleaseCleanupPlacementVersion: binding.Version,
		ReleaseCleanupOperationId:      binding.OperationID,
	}
}

// EnsureHubDepartureForBattle is the operation-bound physical source gate.
// It deliberately does not reuse ReleaseHub(player): an old ALLOCATING RPC may
// arrive after terminal recovery assigned a new Hub owner. The second locator
// read plus exact assignment CAS makes that ABA lose before an eviction order
// becomes visible.
func (u *HubUsecase) EnsureHubDepartureForBattle(ctx context.Context, playerID, matchID uint64,
	binding placement.Binding) error {
	if playerID == 0 || matchID == 0 || !binding.Complete() ||
		u.placementMode != placement.ModeEnforce || u.placementClient == nil || u.authRepo == nil {
		return errcode.New(errcode.ErrInvalidArg,
			"strict exact Battle placement identity required for Hub departure")
	}
	readExact := func() (data.HubPlacementSnapshot, error) {
		snapshot, err := u.placementClient.GetHubPlacement(ctx, playerID)
		if err != nil {
			return data.HubPlacementSnapshot{}, err
		}
		if !exactPendingBattleDeparture(snapshot, matchID, binding) {
			return data.HubPlacementSnapshot{}, errcode.New(errcode.ErrLocatorConflict,
				"Hub departure operation is no longer the authoritative Battle target")
		}
		return snapshot, nil
	}
	confirmedExact := func(snapshot data.HubPlacementSnapshot) bool {
		proof, proofErr := hubSourceDepartureProof(playerID, snapshot)
		return proofErr == nil && hubSourceDepartureConfirmationMatches(snapshot, proof)
	}
	requireConfirmed := func() error {
		snapshot, readErr := readExact()
		if readErr != nil {
			return readErr
		}
		if !confirmedExact(snapshot) {
			return errcode.New(errcode.ErrLocatorConflict,
				"Hub departure physical cleanup is not confirmed by locator")
		}
		return nil
	}
	if _, err := readExact(); err != nil {
		return err
	}
	for attempt := 0; attempt < 8; attempt++ {
		snapshot, readErr := readExact()
		if readErr != nil {
			return readErr
		}
		assignment, found, err := u.repo.GetAssignment(ctx, playerID)
		if err != nil {
			return err
		}
		if !found {
			if confirmedExact(snapshot) {
				return nil
			}
			// The assignment key is not a physical-absence proof. Recreate a
			// durable cleanup tombstone from locator's immutable source so the
			// per-pod connected ledger can either prove AlreadyAbsent, release an
			// unused reservation, or emit an exact eviction order.
			tombstone := battleDepartureSourceTombstone(playerID, matchID, binding, snapshot)
			ref := transferCleanupRef(tombstone)
			if indexErr := u.repo.RegisterTransferCleanup(ctx, tombstone.GetHubPodName(), ref); indexErr != nil {
				return indexErr
			}
			fresh, exactErr := readExact()
			if exactErr != nil || !fresh.SourceBinding.Equal(snapshot.SourceBinding) ||
				!fresh.SourceTarget.Equal(snapshot.SourceTarget) {
				u.removeTransferCleanupRef(ctx, tombstone.GetHubPodName(), ref)
				if exactErr != nil {
					return exactErr
				}
				return errcode.New(errcode.ErrLocatorConflict,
					"Hub departure physical source changed before cleanup publication")
			}
			marked, markErr := u.repo.CompareAndSwapAssignment(ctx, playerID, nil, tombstone,
				u.assignmentSagaTTL())
			if markErr != nil {
				// Unknown CAS result: retain the exact index so restart can resume
				// a committed tombstone or remove an orphan safely.
				return markErr
			}
			if !marked {
				u.removeTransferCleanupRef(ctx, tombstone.GetHubPodName(), ref)
				continue
			}
			_, stillFound, cleanupErr := u.resumeAssignmentCleanup(ctx, playerID,
				tombstone.GetAssignmentId())
			if cleanupErr != nil {
				return cleanupErr
			}
			if stillFound {
				return errcode.New(errcode.ErrInvalidState,
					"Hub Battle departure source tombstone survived completed cleanup")
			}
			return requireConfirmed()
		}
		if !assignmentMatchesBattleDepartureSource(assignment, playerID, snapshot) {
			return errcode.New(errcode.ErrLocatorConflict,
				"canonical Hub assignment does not match the exact placement source")
		}
		if assignment.GetTransferCleanupPending() {
			_, _, cleanupErr := u.resumeAssignmentCleanup(ctx, playerID, assignment.GetAssignmentId())
			if cleanupErr != nil {
				return cleanupErr
			}
			continue
		}
		if assignment.GetReleaseCleanupPending() {
			hasBoundOwner := assignment.GetReleaseCleanupMatchId() != 0 ||
				assignment.GetReleaseCleanupPlacementVersion() != 0 ||
				assignment.GetReleaseCleanupOperationId() != ""
			if hasBoundOwner && (assignment.GetReleaseCleanupMatchId() != matchID ||
				assignment.GetReleaseCleanupPlacementVersion() != binding.Version ||
				assignment.GetReleaseCleanupOperationId() != binding.OperationID) {
				return errcode.New(errcode.ErrLocatorConflict,
					"Hub departure cleanup belongs to another placement operation")
			}
			_, stillFound, cleanupErr := u.resumeAssignmentCleanup(ctx, playerID, assignment.GetAssignmentId())
			if cleanupErr != nil {
				return cleanupErr
			}
			if !stillFound {
				return requireConfirmed()
			}
			continue
		}

		ref := transferCleanupRef(assignment)
		if err := u.repo.RegisterTransferCleanup(ctx, assignment.GetHubPodName(), ref); err != nil {
			return err
		}
		next := proto.Clone(assignment).(*hubv1.HubAssignmentStorageRecord)
		next.ReleaseCleanupPending = true
		next.ReleaseCleanupMatchId = matchID
		next.ReleaseCleanupPlacementVersion = binding.Version
		next.ReleaseCleanupOperationId = binding.OperationID
		fresh, exactErr := readExact()
		if exactErr != nil || !assignmentMatchesBattleDepartureSource(assignment, playerID, fresh) {
			u.removeTransferCleanupRef(ctx, assignment.GetHubPodName(), ref)
			if exactErr != nil {
				return exactErr
			}
			return errcode.New(errcode.ErrLocatorConflict,
				"canonical Hub assignment changed from the exact placement source")
		}
		marked, markErr := u.repo.CompareAndSwapAssignment(ctx, playerID, assignment, next,
			u.assignmentSagaTTL())
		if markErr != nil {
			return markErr
		}
		if !marked {
			u.removeTransferCleanupRef(ctx, assignment.GetHubPodName(), ref)
			continue
		}
		_, stillFound, cleanupErr := u.resumeAssignmentCleanup(ctx, playerID, assignment.GetAssignmentId())
		if cleanupErr != nil {
			return cleanupErr
		}
		if !stillFound {
			return requireConfirmed()
		}
	}
	return errcode.New(errcode.ErrInternal, "Hub Battle departure CAS retry exhausted")
}

// ── RPC 3:TransferHub ─────────────────────────────────────────────────────────

// TransferResult 是 TransferHub 的出参。
type TransferResult struct {
	NewHubDSAddr  string
	NewHubTicket  string
	NewHubPodName string
	TicketExpMs   int64
}

// TransferHub 跨分片传送:先占新分片(失败不动旧分片),再切归属到新分片,最后退旧分片占位,重签票据。
func (u *HubUsecase) TransferHub(ctx context.Context, playerID uint64, targetHubID uint64) (*TransferResult, error) {
	if playerID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	for attempt := 0; attempt < 8; attempt++ {
		assignment, found, err := u.repo.GetAssignment(ctx, playerID)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errcode.New(errcode.ErrHubTransferFailed, "player %d not in any hub", playerID)
		}
		if assignment.GetTransferCleanupPending() || assignment.GetReleaseCleanupPending() {
			var stillFound bool
			assignment, stillFound, err = u.resumeAssignmentCleanup(ctx, playerID, assignment.GetAssignmentId())
			if err != nil {
				return nil, err
			}
			if !stillFound {
				return nil, errcode.New(errcode.ErrHubTransferFailed,
					"player %d Hub assignment was released", playerID)
			}
		}
		if _, trackErr := stickyReleaseTrack(assignment.GetReleaseTrack()); trackErr != nil {
			return nil, trackErr
		}
		if u.authRepo != nil && !assignmentBindingV2Complete(assignment, playerID) {
			return nil, errcode.New(errcode.ErrHubTransferFailed,
				"player %d assignment is not a complete writer-v2 binding", playerID)
		}
		shards, err := u.repo.ListShards(ctx)
		if err != nil {
			return nil, err
		}
		target := selectTransferTarget(shards, assignment, targetHubID)
		if target == nil {
			return nil, errcode.New(errcode.ErrHubTransferFailed,
				"no ready target shard for player %d (target_hub_id=%d)", playerID, targetHubID)
		}

		if target.HubPodName == assignment.HubPodName {
			current, reusable, rerr := u.assignmentRoutable(ctx, playerID, assignment)
			if rerr != nil {
				return nil, errcode.New(errcode.ErrHubTransferFailed, "check current shard: %v", rerr)
			}
			if reusable || assignmentSameInstance(assignment, &current) {
				ensured, ensureErr := u.ensureExistingAssignmentSeat(ctx, playerID, assignment, &current)
				if ensureErr != nil {
					return nil, errcode.New(errcode.ErrHubTransferFailed,
						"ensure current shard %s admission seat: %v", target.HubPodName, ensureErr)
				}
				next := proto.Clone(assignment).(*hubv1.HubAssignmentStorageRecord)
				next.HubAddr, next.ShardId, next.Region = ensured.HubAddr, ensured.ShardID, ensured.Region
				next.ReleaseTrack = ensured.ReleaseTrack
				if next.AssignmentId == "" {
					next.AssignmentId = uuid.NewString()
				}
				bindAssignmentAuth(next, ensured)
				var signedResult *TransferResult
				if u.placementMode != placement.ModeEnforce {
					var signErr error
					signedResult, signErr = u.transferResult(ctx, playerID, next.RoleId, next)
					if signErr != nil {
						return nil, signErr
					}
				}
				swapped, serr := u.repo.CompareAndSwapAssignment(ctx, playerID, assignment, next, u.assignmentSagaTTL())
				if serr != nil {
					return nil, serr
				}
				if !swapped {
					continue
				}
				bound, bindErr := u.bindPlacementTarget(ctx, playerID, next)
				if bindErr != nil {
					return nil, bindErr
				}
				if u.placementMode == placement.ModeEnforce ||
					(u.placementMode == placement.ModeShadow && placementBindingFromAssignment(bound).Complete()) {
					// Bind may have advanced a STABLE same-target assignment to a new
					// HubTransfer version/operation.  Rotate the pending successor to
					// that exact canonical lineage before exposing its ticket.
					if _, ensureErr := u.ensureExistingAssignmentSeat(ctx, playerID, bound, ensured); ensureErr != nil {
						return nil, ensureErr
					}
					var signErr error
					signedResult, signErr = u.transferResult(ctx, playerID, bound.RoleId, bound)
					if signErr != nil {
						return nil, signErr
					}
				}
				return signedResult, nil
			}
		}

		newAssignmentID := uuid.NewString()
		seat, rerr := u.reserveRoutableSeat(ctx, target.HubPodName, playerID, newAssignmentID)
		if rerr != nil {
			return nil, errcode.New(errcode.ErrHubTransferFailed,
				"reserve target shard %s failed: %v", target.HubPodName, rerr)
		}
		target = authoritativeShard(target, seat)
		newAssignment := proto.Clone(assignment).(*hubv1.HubAssignmentStorageRecord)
		newAssignment.PlayerId = playerID
		newAssignment.HubPodName = target.HubPodName
		newAssignment.HubAddr = target.HubAddr
		newAssignment.ShardId = target.ShardId
		newAssignment.Region = target.Region
		newAssignment.TeamId = assignment.TeamId
		newAssignment.AssignedAtMs = time.Now().UnixMilli()
		newAssignment.RoleId = assignment.RoleId
		newAssignment.AssignmentId = newAssignmentID
		newAssignment.ReleaseTrack = target.ReleaseTrack
		bindAssignmentAuth(newAssignment, seat)
		if u.placementMode != placement.ModeOff {
			oldBinding := placementBindingFromAssignment(assignment)
			if !oldBinding.Complete() {
				if u.placementMode == placement.ModeEnforce {
					u.compensateReservedSeat(ctx, target.HubPodName, playerID, newAssignmentID, seat)
					return nil, errcode.New(errcode.ErrLocatorNotFound,
						"Hub transfer source placement binding unavailable")
				}
				plog.With(ctx).Warnw("msg", "hub_transfer_placement_shadow_missing",
					"player_id", playerID, "assignment_id", assignment.GetAssignmentId())
			} else {
				// Persist the sole HUB->HUB operation in the same assignment CAS that
				// switches the target. If Begin/Bind response is lost, a retry reads
				// and resumes these exact values.
				newAssignment.PlacementVersion = oldBinding.Version + 1
				newAssignment.PlacementOperationId = uuid.NewString()
				newAssignment.SourceMatchId = 0
				newAssignment.PlacementProofType = uint32(placement.ProofHubTransfer)
			}
		}
		var signedResult *TransferResult
		if u.placementMode != placement.ModeEnforce {
			var signErr error
			signedResult, signErr = u.transferResult(ctx, playerID, assignment.RoleId, newAssignment)
			if signErr != nil {
				u.compensateReservedSeat(ctx, target.HubPodName, playerID, newAssignmentID, seat)
				return nil, signErr
			}
		}
		cleanupRegistered := false
		if u.placementMode == placement.ModeEnforce {
			if cleanupErr := u.registerTransferCleanup(ctx, newAssignment, assignment); cleanupErr != nil {
				u.compensateReservedSeat(ctx, target.HubPodName, playerID, newAssignmentID, seat)
				return nil, cleanupErr
			}
			cleanupRegistered = true
		}
		swapped, serr := u.repo.CompareAndSwapAssignment(ctx, playerID, assignment, newAssignment, u.assignmentSagaTTL())
		if serr != nil {
			if !cleanupRegistered {
				u.compensateReservedSeat(ctx, target.HubPodName, playerID, newAssignmentID, seat)
			}
			return nil, serr
		}
		if !swapped {
			if cleanupRegistered {
				u.removeTransferCleanupRef(ctx, assignment.GetHubPodName(), transferCleanupRef(newAssignment))
			}
			u.compensateReservedSeat(ctx, target.HubPodName, playerID, newAssignmentID, seat)
			continue
		}
		u.addShardMember(ctx, target.HubPodName, playerID)
		boundAssignment := newAssignment
		if cleanupRegistered {
			var stillFound bool
			var cleanupErr error
			boundAssignment, stillFound, cleanupErr = u.resumeAssignmentCleanup(ctx, playerID, newAssignmentID)
			if cleanupErr != nil {
				return nil, cleanupErr
			}
			if !stillFound {
				return nil, errcode.New(errcode.ErrInvalidState,
					"Hub transfer assignment disappeared during cleanup")
			}
		} else {
			if boundAssignment, err = u.bindPlacementTarget(ctx, playerID, newAssignment); err != nil {
				return nil, err
			}
			u.releaseAssignmentSeat(ctx, assignment)
			u.removeShardMember(ctx, assignment.HubPodName, playerID)
		}
		if u.placementMode == placement.ModeEnforce {
			var signErr error
			signedResult, signErr = u.transferResult(ctx, playerID, boundAssignment.RoleId, boundAssignment)
			if signErr != nil {
				return nil, signErr
			}
		}
		plog.With(ctx).Infow("msg", "hub_transferred",
			"player_id", playerID, "from", assignment.HubPodName, "to", target.HubPodName)
		return signedResult, nil
	}
	return nil, errcode.New(errcode.ErrHubTransferFailed, "player %d assignment changed concurrently", playerID)
}

// ── 玩家侧:线路列表 + 主动切线 ────────────────────────────────────────────────
// 经 Envoy :8443 客户端面(jwt_authn 注入 x-pandora-player-id),player_id 取自 JWT sub,
// 不信请求体。ListHubs/TransferHub 是后端内部/DS 调用,不经客户端面路由。

// HubLineView 是 ListHubLinesForPlayer 的单条出参(service 层转 proto HubLine)。
type HubLineView struct {
	LineNo      uint32
	ShardID     uint32
	PlayerCount int32
	Capacity    int32
	IsFull      bool
	IsCurrent   bool
}

// ListHubLinesForPlayer 列出玩家当前 region 可切换的大厅线路(客户端可见视图,隐藏 pod 名)。
// region 留空 = 用玩家当前归属的 region(服务端权威,忽略客户端申报);
// 玩家无归属时回退 reqRegion 或默认 region。
func (u *HubUsecase) ListHubLinesForPlayer(ctx context.Context, playerID uint64, reqRegion string) ([]*HubLineView, error) {
	if playerID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	region, curPod := reqRegion, ""
	releaseTrack := u.releasePolicy.Select(playerID)
	if assignment, found, err := u.repo.GetAssignment(ctx, playerID); err != nil {
		return nil, err
	} else if found {
		if u.authRepo != nil {
			_, reusable, rerr := u.assignmentRoutable(ctx, playerID, assignment)
			if rerr != nil {
				return nil, rerr
			}
			if !reusable {
				return nil, errcode.New(errcode.ErrInvalidState,
					"hub assignment is not bound to the current active credential")
			}
		}
		region = assignment.Region // 归属 region 权威
		curPod = assignment.HubPodName
		var trackErr error
		releaseTrack, trackErr = stickyReleaseTrack(assignment.GetReleaseTrack())
		if trackErr != nil {
			return nil, trackErr
		}
	}
	if region == "" {
		region = u.cfg.DefaultRegion
	}
	shards, err := u.repo.ListShards(ctx)
	if err != nil {
		return nil, err
	}
	shards, err = u.routableShardViews(ctx, shards)
	if err != nil {
		return nil, err
	}
	return buildHubLinesForTrack(shards, region, curPod, releaseTrack), nil
}

// buildHubLines 把某 region 的 ready 分片按 shard_id 升序编成 1-based 线路视图("1线/2线/…")。
func buildHubLines(shards []*hubv1.HubShardStorageRecord, region, curPod string) []*HubLineView {
	return buildHubLinesForTrack(shards, region, curPod, "")
}

func buildHubLinesForTrack(shards []*hubv1.HubShardStorageRecord, region, curPod, releaseTrack string) []*HubLineView {
	ready := make([]*hubv1.HubShardStorageRecord, 0, len(shards))
	for _, s := range shards {
		track, err := stickyReleaseTrack(s.GetReleaseTrack())
		if err == nil && s.Region == region && s.State == stateReady && (releaseTrack == "" || track == releaseTrack) {
			ready = append(ready, s)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].ShardId < ready[j].ShardId })
	out := make([]*HubLineView, 0, len(ready))
	for i, s := range ready {
		out = append(out, &HubLineView{
			LineNo:      uint32(i + 1),
			ShardID:     s.ShardId,
			PlayerCount: s.PlayerCount,
			Capacity:    s.Capacity,
			IsFull:      s.PlayerCount >= s.Capacity,
			IsCurrent:   curPod != "" && s.HubPodName == curPod,
		})
	}
	return out
}

// lineNoOfShard 返回某 region 内目标 shard_id 的 1-based 线路号(不在 ready 列表返 0)。
func lineNoOfShard(shards []*hubv1.HubShardStorageRecord, region, releaseTrack string, shardID uint32) uint32 {
	for _, v := range buildHubLinesForTrack(shards, region, "", releaseTrack) {
		if v.ShardID == shardID {
			return v.LineNo
		}
	}
	return 0
}

// TransferToLineResult 是 TransferToLineForPlayer 的出参。
type TransferToLineResult struct {
	NewHubDSAddr string
	NewHubTicket string
	NewShardID   uint32
	LineNo       uint32
}

// TransferToLineForPlayer 玩家主动切换到指定线路(换实例,AB 互不可见)。护栏:
//  1. 战斗/匹配中禁切(查 player_locator,弱依赖:locator 不可达时放行低危的大厅切线)
//  2. 冷却防刷(SET NX EX,窗口内再切拒绝;失败释放占坑让玩家可重试)
//  3. 目标线路不存在/非本 region → ErrHubTransferFailed;已满 → ErrHubLineFull
//  4. 复用内部 TransferHub 完成 占新→切归属→退旧→重签票
func (u *HubUsecase) TransferToLineForPlayer(ctx context.Context, playerID uint64, targetShardID uint32) (*TransferToLineResult, error) {
	if playerID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}

	// 护栏 1:战斗/匹配中禁切(弱依赖,locator 不可达仅告警放行)
	if u.locator != nil {
		if blocked, err := u.locator.InBattleOrMatching(ctx, playerID); err != nil {
			plog.With(ctx).Warnw("msg", "transfer_locator_check_failed", "player_id", playerID, "err", err)
		} else if blocked {
			return nil, errcode.New(errcode.ErrHubTransferNotInHub,
				"player %d in battle/matching, cannot switch hub line", playerID)
		}
	}

	// 任何 cooldown SET 副作用之前先证明当前 assignment 是完整 writer-v2 且仍精确绑定
	// Redis active。legacy/future/缺 JTI 的旧 writer 记录必须零变更拒绝。
	if u.authRepo != nil {
		assignment, found, readErr := u.repo.GetAssignment(ctx, playerID)
		if readErr != nil {
			return nil, readErr
		}
		if !found {
			return nil, errcode.New(errcode.ErrHubTransferNotInHub, "player %d not in any hub", playerID)
		}
		_, reusable, routeErr := u.assignmentRoutable(ctx, playerID, assignment)
		if routeErr != nil {
			return nil, routeErr
		}
		if !reusable {
			return nil, errcode.New(errcode.ErrInvalidState,
				"hub assignment is not bound to the current active credential")
		}
	}

	// 护栏 2:冷却防刷(先占坑;后续失败再释放让玩家可立即重试)
	ok, err := u.repo.TryTransferCooldown(ctx, playerID, u.transferCooldown())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errcode.New(errcode.ErrHubTransferCooldown, "player %d transfer on cooldown", playerID)
	}

	res, terr := u.transferToLineInner(ctx, playerID, targetShardID)
	if terr != nil {
		if cerr := u.repo.ClearTransferCooldown(ctx, playerID); cerr != nil {
			plog.With(ctx).Warnw("msg", "clear_transfer_cooldown_failed", "player_id", playerID, "err", cerr)
		}
		return nil, terr
	}
	return res, nil
}

// transferToLineInner 做目标解析 + 满员判定 + 委托内部 TransferHub。
func (u *HubUsecase) transferToLineInner(ctx context.Context, playerID uint64, targetShardID uint32) (*TransferToLineResult, error) {
	assignment, found, err := u.repo.GetAssignment(ctx, playerID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errcode.New(errcode.ErrHubTransferNotInHub, "player %d not in any hub", playerID)
	}
	if u.authRepo != nil {
		_, reusable, routeErr := u.assignmentRoutable(ctx, playerID, assignment)
		if routeErr != nil {
			return nil, routeErr
		}
		if !reusable {
			return nil, errcode.New(errcode.ErrInvalidState,
				"hub assignment is not bound to the current active credential")
		}
	}

	shards, err := u.repo.ListShards(ctx)
	if err != nil {
		return nil, err
	}
	// 目标线路必须是本 region 的 ready 分片
	var target *hubv1.HubShardStorageRecord
	assignmentTrack, trackErr := stickyReleaseTrack(assignment.GetReleaseTrack())
	if trackErr != nil {
		return nil, trackErr
	}
	for _, s := range shards {
		shardTrack, shardTrackErr := stickyReleaseTrack(s.GetReleaseTrack())
		if shardTrackErr == nil && shardTrack == assignmentTrack && s.ShardId == targetShardID && s.Region == assignment.Region && s.State == stateReady {
			target = s
			break
		}
	}
	if target == nil {
		return nil, errcode.New(errcode.ErrHubTransferFailed,
			"line shard_id=%d not available in region %s", targetShardID, assignment.Region)
	}
	// 已满且不是当前线路 → 明确"线路已满"
	if target.HubPodName != assignment.HubPodName && target.PlayerCount >= target.Capacity {
		return nil, errcode.New(errcode.ErrHubLineFull, "line shard_id=%d is full", targetShardID)
	}

	tr, err := u.TransferHub(ctx, playerID, uint64(targetShardID))
	if err != nil {
		return nil, err
	}
	lineNo := lineNoOfShard(shards, assignment.Region, assignmentTrack, targetShardID)
	return &TransferToLineResult{
		NewHubDSAddr: tr.NewHubDSAddr,
		NewHubTicket: tr.NewHubTicket,
		NewShardID:   targetShardID,
		LineNo:       lineNo,
	}, nil
}

// ── RPC 4:ListHubs ────────────────────────────────────────────────────────────

// ListHubs 列出分片负载,region 非空时过滤。
func (u *HubUsecase) ListHubs(ctx context.Context, region string) ([]*hubv1.HubInfo, error) {
	shards, err := u.repo.ListShards(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*hubv1.HubInfo, 0, len(shards))
	for _, s := range shards {
		if region != "" && s.Region != region {
			continue
		}
		out = append(out, &hubv1.HubInfo{
			HubPodName:  s.HubPodName,
			HubAddr:     s.HubAddr,
			Region:      s.Region,
			PlayerCount: s.PlayerCount,
			Capacity:    s.Capacity,
			State:       s.State,
		})
	}
	return out, nil
}

// ── RPC 5:Heartbeat ───────────────────────────────────────────────────────────

// HeartbeatResult 是 Heartbeat 的出参(下发给 Hub DS 的控制指令)。
type HeartbeatResult struct {
	Command        string
	GraceSeconds   int32 // command=="drain"/"stop" 时的优雅迁移倒计时(秒),其余为 0
	EvictionOrders []HubEvictionOrder
	// AcceptedTokenGen:Model B 令牌激活确认(§7)。promote/validate 通过后回显当前 active
	// 代际,DS 据此确认「本令牌已被服务端接纳为权威」;legacy/off 路径恒 0。
	AcceptedTokenGen      uint64
	AcceptedTokenJTI      string
	AcceptedInstanceUID   string
	AcceptedProtocolEpoch uint32
	AcceptedWriterEpoch   uint32
}

// HubEvictionOrder names one exact physical source connection. It is returned
// only on a heartbeat authenticated as that source GameServer instance.
type HubEvictionOrder struct {
	PlayerID            uint64
	AssignmentID        string
	AdmissionID         string
	AdmissionSeq        uint64
	SourceInstanceUID   string
	SourceProtocolEpoch uint32
	SourceWriterEpoch   uint32
	CleanupAssignmentID string
}

// Heartbeat 处理 Hub DS 上报(单向 unary,DS 每 5s 调)。刷新在线数 + 心跳时刻。
// 分片镜像不存在(孤儿 DS)→ 返回 stop 指令让其自行停机。
// 分片已被强制整合标记 draining → 下发 drain + grace_seconds,Hub DS 引导在场玩家倒计时切大厅。
// tokenGen:本次心跳携带的**已验签**DS 回调令牌代际(service 层从 Guard claims 的 ds_gen 取,
// 无已验签令牌时为 0)。代际绑定下 warming→ready 只接受与镜像代际**精确相等**的心跳(审核 P1-6/P1-8)。
func (u *HubUsecase) Heartbeat(ctx context.Context, pod string, playerCount int32, state string, tsMs int64, tokenGen uint64) (*HeartbeatResult, error) {
	return u.heartbeat(ctx, pod, playerCount, nil, 0, state, tsMs, tokenGen, nil)
}

// HeartbeatWithCredential 是 Model B 心跳入口(§7):service 层验签抽出 Model B 凭据后调用。
// authRepo 已装配 → cred 携带的 (uid,epoch,gen,jti) 走 ActivateHeartbeat 单事务线性化点:
// 首个合法 pending 心跳在 authKey+shardKey 同事务内原子完成 promote(pending→active)+ 分片
// warming→ready + 投影 active 元组;stale(无授权记录/uid|epoch 不符/相位锁定/都不匹配)一律
// fail-closed 返回 ErrUnauthorized(两键零变更)。分片镜像缺失 → reconcile 拓扑后重试一次,
// 保证 promote 与 ready 恒同事务(杜绝半激活)。返回 accepted gen 供 DS 回显。
func (u *HubUsecase) HeartbeatWithCredential(ctx context.Context, pod string, playerCount int32,
	playerIDs []uint64, maxPlayers uint32, state string, tsMs int64, cred *HubCredential) (*HeartbeatResult, error) {
	return u.heartbeat(ctx, pod, playerCount, playerIDs, maxPlayers, state, tsMs, 0, cred)
}

func (u *HubUsecase) heartbeat(ctx context.Context, pod string, playerCount int32, playerIDs []uint64,
	maxPlayers uint32, state string, tsMs int64, tokenGen uint64, cred *HubCredential) (*HeartbeatResult, error) {
	if pod == "" {
		return nil, errcode.New(errcode.ErrInvalidArg, "hub_pod_name required")
	}
	// 请求 ts_ms 不参与授权/存活权威；统一用服务端接收时间，future ts 不能延长可路由窗口。
	tsMs = time.Now().UnixMilli()
	// Model B:授权记录仓已装配 → 走 ActivateHeartbeat 单事务线性化点(authRepo=nil 时落 legacy 分支)。
	if u.authRepo != nil {
		return u.heartbeatModelB(ctx, pod, playerCount, playerIDs, maxPlayers, state, tsMs, cred)
	}
	found, err := u.repo.HeartbeatShard(ctx, pod, playerCount, state, tsMs, tokenGen, u.dsTokenGeneration, u.shardTTL())
	if err != nil {
		return nil, err
	}
	if !found {
		// 新建/重建的 Hub GameServer 可能早于周期拓扑刷新发来业务心跳。
		// 这里先主动刷新一次拓扑,避免首跳把健康 pod 误判成孤儿并下发 stop。
		if rerr := u.reconcileShardTopology(ctx); rerr != nil {
			plog.With(ctx).Warnw("msg", "heartbeat_topology_reconcile_failed", "pod", pod, "err", rerr)
		} else {
			found, err = u.repo.HeartbeatShard(ctx, pod, playerCount, state, tsMs, tokenGen, u.dsTokenGeneration, u.shardTTL())
			if err != nil {
				return nil, err
			}
		}
		if !found {
			plog.With(ctx).Warnw("msg", "heartbeat_unknown_hub_waiting_topology", "pod", pod)
			return nil, errcode.New(errcode.ErrUnavailable, "hub shard %s topology not confirmed", pod)
		}
	}
	// 分片被标记 draining/stopping → 下发迁移/停机指令(与 Kafka 推送双通道)。
	if shard, ok, gerr := u.repo.GetShard(ctx, pod); gerr == nil && ok {
		switch shard.State {
		case stateDraining:
			return &HeartbeatResult{Command: commandDrain, GraceSeconds: u.cfg.MigrateGraceSeconds}, nil
		case stateStopping:
			return &HeartbeatResult{Command: commandStop, GraceSeconds: u.cfg.MigrateGraceSeconds}, nil
		}
	}
	return &HeartbeatResult{Command: commandNone}, nil
}

// heartbeatModelB 处理 Model B 心跳(authRepo 已装配)。
// cred==nil(legacy 令牌 / 无凭据)在 Model B 下一律拒:Redis 授权权威模式不接受未携带 Model B
// 凭据的心跳借旧令牌保活或翻 ready(纵深防御,service 层也会拦;审核二轮 CE1/CE2)。
func (u *HubUsecase) heartbeatModelB(ctx context.Context, pod string, playerCount int32, playerIDs []uint64,
	maxPlayers uint32, state string, tsMs int64, cred *HubCredential) (*HeartbeatResult, error) {
	if cred == nil {
		return nil, errcode.New(errcode.ErrUnauthorized, "hub heartbeat requires model B credential under redis authority")
	}
	in := data.ActivateHeartbeatInput{
		PlayerCount: playerCount,
		PlayerIDs:   append([]uint64(nil), playerIDs...),
		MaxPlayers:  maxPlayers,
		State:       state,
		TsMs:        tsMs,
		AuthTTL:     u.authTTLDur(),
		ShardTTL:    u.shardTTL(),
	}
	id := data.CredentialIdentity{
		Gen:           cred.Gen,
		JTI:           cred.JTI,
		InstanceUID:   cred.InstanceUID,
		ProtocolEpoch: cred.ProtocolEpoch,
		TokenSHA256:   cred.TokenSHA256,
		Kid:           cred.Kid,
		WriterEpoch:   cred.WriterEpoch,
	}
	res, err := u.authRepo.ActivateHeartbeat(ctx, pod, id, in)
	if err != nil {
		return nil, err // ErrUnauthorized:授权未激活/不匹配/相位锁定,fail-closed
	}
	if !res.ShardFound {
		// 分片镜像缺失(孤儿 / 早于拓扑种子):先刷一次拓扑再重试,保证 promote 与 ready 同事务。
		if rerr := u.reconcileShardTopology(ctx); rerr != nil {
			plog.With(ctx).Warnw("msg", "heartbeat_topology_reconcile_failed", "pod", pod, "err", rerr)
			return nil, errcode.New(errcode.ErrUnavailable, "hub shard %s topology reconcile: %v", pod, rerr)
		}
		res, err = u.authRepo.ActivateHeartbeat(ctx, pod, id, in)
		if err != nil {
			return nil, err
		}
		if !res.ShardFound {
			plog.With(ctx).Warnw("msg", "heartbeat_unknown_hub_waiting_topology", "pod", pod)
			return nil, errcode.New(errcode.ErrUnavailable, "hub shard %s topology not confirmed", pod)
		}
	}
	command, graceSeconds := commandNone, int32(0)
	switch res.ShardState {
	case stateDraining:
		command, graceSeconds = commandDrain, u.cfg.MigrateGraceSeconds
	case stateStopping:
		command, graceSeconds = commandStop, u.cfg.MigrateGraceSeconds
	}
	out := modelBHeartbeatResult(res, command, graceSeconds)
	orders, orderErr := u.pendingHubEvictionOrders(ctx, pod, res.InstanceUID,
		res.ProtocolEpoch, res.WriterEpoch)
	if orderErr != nil {
		// Heartbeat/auth was already committed. Keep the DS authorization lease
		// healthy and retry order discovery next tick; never downgrade a healthy
		// heartbeat into an apparent credential failure after that commit.
		plog.With(ctx).Warnw("msg", "hub_eviction_order_discovery_failed", "pod", pod, "err", orderErr)
	} else {
		out.EvictionOrders = orders
	}
	return out, nil
}

func modelBHeartbeatResult(res data.ActivateResult, command string, graceSeconds int32) *HeartbeatResult {
	return &HeartbeatResult{
		Command: command, GraceSeconds: graceSeconds,
		AcceptedTokenGen: res.ActiveGen, AcceptedTokenJTI: res.ActiveJTI,
		AcceptedInstanceUID: res.InstanceUID, AcceptedProtocolEpoch: res.ProtocolEpoch,
		AcceptedWriterEpoch: res.WriterEpoch,
	}
}

const maxHubEvictionOrdersPerHeartbeat = 256

// pendingHubEvictionOrders projects the durable cleanup index into the source
// DS control plane. Connected ledger state is read-only here: the order is not
// complete until the DS kicks the exact local admission and AcknowledgeDeparture
// removes it (or confirmed GameServer teardown removes the whole UID ledger).
func (u *HubUsecase) pendingHubEvictionOrders(ctx context.Context, sourcePod, instanceUID string,
	protocolEpoch, writerEpoch uint32) ([]HubEvictionOrder, error) {
	refs, err := u.repo.ListTransferCleanups(ctx, sourcePod)
	if err != nil {
		return nil, err
	}
	orders := make([]HubEvictionOrder, 0, len(refs))
	for _, ref := range refs {
		if len(orders) >= maxHubEvictionOrdersPerHeartbeat {
			break
		}
		assignment, found, readErr := u.repo.GetAssignment(ctx, ref.PlayerID)
		if readErr != nil {
			return nil, readErr
		}
		if !found || assignment.GetAssignmentId() != ref.TargetAssignmentID {
			continue // orphan cleanup is removed by the reconciler
		}
		var source *hubv1.HubAssignmentStorageRecord
		switch {
		case assignment.GetTransferCleanupPending():
			if !assignment.GetTransferTargetBound() {
				continue // never evict source before the exact target Bind is durable
			}
			source, readErr = transferCleanupSource(assignment)
		case assignment.GetReleaseCleanupPending():
			source = assignment
		default:
			continue
		}
		if readErr != nil {
			return nil, readErr
		}
		if source.GetHubPodName() != sourcePod || source.GetHubInstanceUid() != instanceUID ||
			source.GetAuthEpoch() != protocolEpoch || source.GetAuthWriterEpoch() != writerEpoch {
			// A replacement DS must never receive an order for the dead old UID.
			// Authoritative GameServer teardown owns that ledger cleanup proof.
			continue
		}
		seat, inspectErr := u.authRepo.InspectAssignmentSeat(ctx, sourcePod,
			assignmentInstanceIdentity(source))
		if inspectErr != nil {
			return nil, inspectErr
		}
		if seat.Conflict {
			return nil, errcode.New(errcode.ErrInvalidState,
				"Hub eviction source owner identity conflict")
		}
		if !seat.Connected {
			continue
		}
		orders = append(orders, HubEvictionOrder{
			PlayerID: ref.PlayerID, AssignmentID: source.GetAssignmentId(),
			AdmissionID: seat.AdmissionID, AdmissionSeq: seat.AdmissionSeq,
			SourceInstanceUID: source.GetHubInstanceUid(), SourceProtocolEpoch: source.GetAuthEpoch(),
			SourceWriterEpoch: source.GetAuthWriterEpoch(), CleanupAssignmentID: ref.TargetAssignmentID,
		})
	}
	return orders, nil
}

// AcknowledgeAdmissionResult / AcknowledgeDepartureResult 只暴露 DS 状态机需要的结果。
type AcknowledgeAdmissionResult struct {
	Admitted           bool
	PlacementCommitted bool
	Placement          placement.Binding
}
type AcknowledgeDepartureResult struct {
	Departed bool
	Conflict bool
}

// AcknowledgeAdmission 把本地已验签 Hub DSTicket 对应 reservation 原子转为 connected owner。
func (u *HubUsecase) AcknowledgeAdmission(ctx context.Context, playerID uint64, assignmentID, pod,
	admissionID string, admissionSeq uint64, cred *HubCredential) (*AcknowledgeAdmissionResult, error) {
	return u.AcknowledgeAdmissionWithPlacement(ctx, playerID, assignmentID, pod, admissionID,
		admissionSeq, placement.Binding{}, cred)
}

func (u *HubUsecase) AcknowledgeAdmissionWithPlacement(ctx context.Context, playerID uint64, assignmentID, pod,
	admissionID string, admissionSeq uint64, requestedPlacement placement.Binding,
	cred *HubCredential) (*AcknowledgeAdmissionResult, error) {
	if u.authRepo == nil || cred == nil {
		return nil, errcode.New(errcode.ErrUnauthorized, "hub admission requires model B authority")
	}
	assignment, found, err := u.repo.GetAssignment(ctx, playerID)
	if err != nil {
		return nil, err
	}
	if found && (assignment.GetTransferCleanupPending() || assignment.GetReleaseCleanupPending()) {
		var stillFound bool
		assignment, stillFound, err = u.resumeAssignmentCleanup(ctx, playerID, assignment.GetAssignmentId())
		if err != nil {
			// This is before target AcknowledgeAdmission: Redis/source cleanup
			// failure is retryable and creates no new target session/spawn.
			return nil, err
		}
		if !stillFound {
			return nil, errcode.New(errcode.ErrInvalidState,
				"Hub admission assignment was released during owner cleanup")
		}
	}
	if !found || !assignmentMatchesAdmission(assignment, playerID, assignmentID, pod, cred) {
		return nil, errcode.New(errcode.ErrInvalidState, "hub admission assignment is no longer current")
	}
	if err := requestedPlacement.ValidateOptional(); err != nil {
		return nil, errcode.NewCause(errcode.ErrInvalidArg, err, "invalid admission placement binding")
	}
	assignmentPlacement := placementBindingFromAssignment(assignment)
	if requestedPlacement.Complete() && !requestedPlacement.Equal(assignmentPlacement) {
		return nil, errcode.New(errcode.ErrInvalidState, "admission placement does not match assignment")
	}
	if u.placementMode == placement.ModeEnforce && (!requestedPlacement.Complete() || !assignmentPlacement.Complete()) {
		return nil, errcode.New(errcode.ErrLocatorNotFound, "strict Hub admission requires placement binding")
	}
	if u.placementMode == placement.ModeEnforce {
		if u.placementClient == nil {
			return nil, errcode.New(errcode.ErrUnavailable,
				"strict Hub admission placement authority unavailable")
		}
		snapshot, placementErr := u.placementClient.GetHubPlacement(ctx, playerID)
		if placementErr != nil {
			return nil, placementErr
		}
		if !hubTicketPlacementReady(playerID, snapshot, assignmentPlacement,
			placementTargetFromAssignment(assignment), time.Now().UnixMilli()) {
			return nil, errcode.New(errcode.ErrLocatorConflict,
				"Hub admission requires exact committed or source-confirmed placement")
		}
	}
	id := hubCredentialIdentity(cred)
	reservation := data.ReservationIdentity{
		PlayerID: playerID, AssignmentID: assignmentID, InstanceUID: cred.InstanceUID,
		ProtocolEpoch: cred.ProtocolEpoch, WriterEpoch: cred.WriterEpoch,
		PlacementVersion: assignmentPlacement.Version, PlacementOperationID: assignmentPlacement.OperationID,
		SourceMatchID: assignmentPlacement.SourceMatchID,
	}
	result, err := u.authRepo.AcknowledgeAdmission(ctx, pod, id, reservation,
		admissionID, admissionSeq, time.Now().UnixMilli(), u.shardTTL())
	if err != nil {
		return nil, err
	}
	if !result.Admitted {
		return &AcknowledgeAdmissionResult{Admitted: false}, nil
	}
	placementCommitted := false
	if u.placementMode != placement.ModeOff {
		var commitErr error
		if u.placementClient == nil || !assignmentPlacement.Complete() {
			commitErr = errcode.New(errcode.ErrUnavailable, "placement admission client/binding unavailable")
		} else {
			commitErr = u.placementClient.CommitHubAdmission(ctx, playerID, assignmentPlacement,
				placementTargetFromAssignment(assignment), admissionID)
		}
		if commitErr == nil {
			placementCommitted = true
		} else {
			plog.With(ctx).Warnw("msg", "hub_placement_commit_failed", "mode", u.placementMode.String(),
				"player_id", playerID, "assignment_id", assignmentID, "admission_id", admissionID, "err", commitErr)
			if u.placementMode == placement.ModeEnforce {
				// Keep the exact connected session for both unknown and definite
				// failures. The Hub still has a real PlayerController (with its
				// spawn gate closed); only DS Kick -> Logout -> exact Departure, or
				// confirmed GameServer UID teardown, is physical-exit proof.
				return nil, commitErr
			}
		}
	}
	// assignment 与 {pod} ledger 不同 slot：ACK 后必须再查一次。若 Transfer/Release
	// 已赢得 CAS，保留 exact connected owner 并拒绝开放 spawn gate。DS 收到拒绝后必须
	// Kick，等 Logout 的物理 proof 才能删除；服务端不能在 PC/Pawn 尚存时自证 Departure。
	current, stillFound, postErr := u.repo.GetAssignment(ctx, playerID)
	if postErr != nil || !stillFound || !assignmentMatchesAdmission(current, playerID, assignmentID, pod, cred) {
		if postErr != nil {
			return nil, postErr
		}
		return nil, errcode.New(errcode.ErrInvalidState, "hub admission assignment changed during acknowledge")
	}
	if u.placementMode == placement.ModeEnforce && !placementCommitted {
		return nil, errcode.New(errcode.ErrUnavailable, "placement commit not confirmed")
	}
	if u.placementMode == placement.ModeEnforce && placementCommitted {
		// Re-SET the exact same canonical owner without TTL. Besides fencing a
		// concurrent Transfer/Release, this upgrades assignments written by the
		// previous TTL-based binary during rolling deployment. A response loss is
		// replayed with the same admission id; spawn remains closed meanwhile.
		next := proto.Clone(current).(*hubv1.HubAssignmentStorageRecord)
		persisted, persistErr := u.repo.CompareAndSwapAssignment(ctx, playerID, current, next,
			u.assignmentSagaTTL())
		if persistErr != nil {
			return nil, persistErr
		}
		if !persisted {
			return nil, errcode.New(errcode.ErrInvalidState,
				"Hub admission assignment changed during owner persistence")
		}
	}
	return &AcknowledgeAdmissionResult{Admitted: result.Admitted,
		PlacementCommitted: placementCommitted, Placement: assignmentPlacement}, nil
}

func assignmentMatchesAdmission(a *hubv1.HubAssignmentStorageRecord, playerID uint64,
	assignmentID, pod string, cred *HubCredential) bool {
	return a != nil && cred != nil && playerID != 0 && a.GetPlayerId() == playerID &&
		a.GetAssignmentId() == assignmentID && a.GetHubPodName() == pod &&
		a.GetHubInstanceUid() == cred.InstanceUID && a.GetAuthEpoch() == cred.ProtocolEpoch &&
		a.GetAuthWriterEpoch() == cred.WriterEpoch
}

// AcknowledgeDeparture exact 删除当前 admission owner；Conflict 由旧连接晚到 Logout 触发。
func (u *HubUsecase) AcknowledgeDeparture(ctx context.Context, playerID uint64, assignmentID, pod,
	admissionID string, admissionSeq uint64, cred *HubCredential) (*AcknowledgeDepartureResult, error) {
	if u.authRepo == nil || cred == nil {
		return nil, errcode.New(errcode.ErrUnauthorized, "hub departure requires model B authority")
	}
	id := hubCredentialIdentity(cred)
	result, err := u.authRepo.AcknowledgeDeparture(ctx, pod, id, data.ReservationIdentity{
		PlayerID: playerID, AssignmentID: assignmentID, InstanceUID: cred.InstanceUID,
		ProtocolEpoch: cred.ProtocolEpoch, WriterEpoch: cred.WriterEpoch,
	}, admissionID, admissionSeq, time.Now().UnixMilli(), u.shardTTL())
	if err != nil {
		return nil, err
	}
	// Departure removes physical connected ownership only. The durable
	// assignment/member index remains until an exact Release/Transfer phase
	// replaces or deletes it, so an offline player is still discoverable when
	// this shard drains.
	return &AcknowledgeDepartureResult{Departed: result.Departed, Conflict: result.Conflict}, nil
}

func hubCredentialIdentity(cred *HubCredential) data.CredentialIdentity {
	return data.CredentialIdentity{
		Gen: cred.Gen, JTI: cred.JTI, InstanceUID: cred.InstanceUID, ProtocolEpoch: cred.ProtocolEpoch,
		TokenSHA256: cred.TokenSHA256, Kid: cred.Kid, WriterEpoch: cred.WriterEpoch,
	}
}

// RefreshHubPresence 把 Hub DS 心跳捎带的在场 player_ids 转发给 player_locator
// 批量续期 HUB 位置 TTL(在线保活链路:DS 每 5s 上报,locator TTL 30s,
// 玩家掉线 → DS 停报该 id → 30s 自然过期 = 好友视角离线)。
//
// fire-and-forget(同 ds_allocator.refreshBattleLocations):goroutine + detached ctx
// (context.WithoutCancel 保留 trace_id 满足不变量 §8,脱离心跳 RPC 取消)+ 独立短超时,
// locator 抖动/卡死既不拖慢心跳响应尾延迟,也不泄漏 goroutine。
// best-effort 弱依赖:locator 未配(nil)/ 转发失败只记 Warn,绝不影响心跳主流程
// (心跳是分片存活信号,不能因旁路观测链路抖动而失败)。
func (u *HubUsecase) RefreshHubPresence(ctx context.Context, pod string, playerIDs []uint64, bearerToken string) {
	if u.locator == nil || pod == "" || len(playerIDs) == 0 {
		return
	}
	players := append([]uint64(nil), playerIDs...) // 拷贝,脱离调用方切片复用
	token := bearerToken                           // 仅闭包内存中短暂转发；禁止日志/持久化
	go func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), presenceRefreshTimeout)
		defer cancel()
		if _, err := u.locator.RefreshHubLocations(rctx, pod, players, token); err != nil {
			plog.With(rctx).Warnw("msg", "hub_presence_refresh_failed",
				"pod", pod, "players", len(players), "err", err)
		}
	}()
}

// ── 后台心跳超时扫描 ──────────────────────────────────────────────────────────

// RunHeartbeatSweep 启动后台心跳超时扫描,直到 ctx 取消(不变量 §4)。
func (u *HubUsecase) RunHeartbeatSweep(ctx context.Context) {
	ticker := time.NewTicker(u.cfg.SweepInterval.Std())
	defer ticker.Stop()
	plog.With(ctx).Infow("msg", "hub_heartbeat_sweep_started",
		"interval", u.cfg.SweepInterval.String(), "timeout", u.cfg.HeartbeatTimeout.String())
	for {
		select {
		case <-ctx.Done():
			plog.With(ctx).Infow("msg", "hub_heartbeat_sweep_stopped")
			return
		case <-ticker.C:
			if err := u.reconcileOwnerCleanups(ctx); err != nil {
				plog.With(ctx).Warnw("msg", "hub_owner_cleanup_reconcile_failed", "err", err)
			}
			if err := u.reconcileShardTopology(ctx); err != nil {
				plog.With(ctx).Warnw("msg", "hub_reconcile_topology_failed", "err", err)
			}
			if err := u.sweepOnce(ctx); err != nil {
				plog.With(ctx).Warnw("msg", "hub_heartbeat_sweep_failed", "err", err)
			}
			if err := u.reconcileFleetReplicas(ctx); err != nil {
				plog.With(ctx).Warnw("msg", "hub_reconcile_replicas_failed", "err", err)
			}
		}
	}
}

// reconcileOwnerCleanups is restart recovery for index-first transfer/release
// sagas. The global pod index is a persistent superset; stale refs are removed
// only by exact (player,target-assignment) identity and can never delete a
// concurrent winner's ref.
func (u *HubUsecase) reconcileOwnerCleanups(ctx context.Context) error {
	pods, err := u.repo.ListTransferCleanupPods(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, sourcePod := range pods {
		refs, listErr := u.repo.ListTransferCleanups(ctx, sourcePod)
		if listErr != nil {
			if firstErr == nil {
				firstErr = listErr
			}
			continue
		}
		for _, ref := range refs {
			assignment, found, readErr := u.repo.GetAssignment(ctx, ref.PlayerID)
			if readErr != nil {
				if firstErr == nil {
					firstErr = readErr
				}
				continue
			}
			expectedSource := ""
			if found && assignment.GetAssignmentId() == ref.TargetAssignmentID {
				switch {
				case assignment.GetTransferCleanupPending():
					expectedSource = assignment.GetTransferSourceHubPodName()
				case assignment.GetReleaseCleanupPending():
					expectedSource = assignment.GetHubPodName()
				}
			}
			if expectedSource == "" || expectedSource != sourcePod {
				u.removeTransferCleanupRef(ctx, sourcePod, ref)
				continue
			}
			if _, _, cleanupErr := u.resumeAssignmentCleanup(ctx, ref.PlayerID, ref.TargetAssignmentID); cleanupErr != nil {
				plog.With(ctx).Warnw("msg", "hub_owner_cleanup_retry_failed", "source_pod", sourcePod,
					"player_id", ref.PlayerID, "assignment_id", ref.TargetAssignmentID, "err", cleanupErr)
				if firstErr == nil {
					firstErr = cleanupErr
				}
			}
		}
	}
	return firstErr
}

// sweepOnce 扫描一次:last_heartbeat_ms 早于阈值的分片 → 标记 draining + 移出 active(停止分配)。
// 注意:从未心跳的 Mock 种子分片(score=0)被 RangeStaleShards 排除,不会被误标 draining。
func (u *HubUsecase) sweepOnce(ctx context.Context) error {
	threshold := time.Now().Add(-u.cfg.HeartbeatTimeout.Std()).UnixMilli()
	stale, err := u.repo.RangeStaleShards(ctx, threshold)
	if err != nil {
		return err
	}
	for _, pod := range stale {
		lerr := u.repo.UpdateShardWithLock(ctx, pod, u.retry(), func(s *hubv1.HubShardStorageRecord) error {
			if s.State == stateReady {
				s.State = stateDraining // 心跳超时:停止向其分配新玩家
			}
			return nil
		}, u.shardTTL())
		if lerr != nil && errcode.As(lerr) != errcode.ErrHubNoAvailable {
			plog.With(ctx).Warnw("msg", "sweep_mark_draining_failed", "pod", pod, "err", lerr)
		}
		if rerr := u.repo.RemoveActive(ctx, pod); rerr != nil {
			plog.With(ctx).Warnw("msg", "sweep_remove_active_failed", "pod", pod, "err", rerr)
		}
		plog.With(ctx).Warnw("msg", "hub_shard_heartbeat_timeout", "pod", pod)
	}
	return nil
}

// ── 内部辅助 ──────────────────────────────────────────────────────────────────

// ensureShards:region 无候选分片时,按 Fleet 拓扑种入 Redis(W4 ⑤ Mock 期 lazy-seed)。
// 热路径只在该 region 首次无分片时打 Fleet 拉起种子;已有分片直接返回(不打 k8s,保持 AssignHub 轻量)。
// 拓扑漂移(pod 改名/下线)的对账交后台 reconcileShardTopology 处理,避免每次登录都查 apiserver。
func (u *HubUsecase) ensureShards(ctx context.Context, region, releaseTrack string) error {
	if !releasetrack.Valid(releaseTrack) {
		return errcode.New(errcode.ErrInvalidArg, "invalid hub release_track %q", releaseTrack)
	}
	shards, err := u.repo.ListShards(ctx)
	if err != nil {
		return err
	}
	for _, s := range shards {
		track, trackErr := stickyReleaseTrack(s.GetReleaseTrack())
		if trackErr == nil && s.Region == region && track == releaseTrack {
			return nil // 已有该 region + track 分片
		}
	}
	cands, err := u.fleet.ListShards(ctx, region)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, c := range cands {
		if !c.TokenReady || !releasetrack.Valid(c.ReleaseTrack) {
			// enforce 下令牌不可用的分片:不种 ready 镜像(否则 AssignHub 会分到回调被全拒的 Hub)。
			continue
		}
		rec := &hubv1.HubShardStorageRecord{
			HubPodName:        c.PodName,
			HubAddr:           c.Addr,
			Region:            c.Region,
			ShardId:           c.ShardID,
			PlayerCount:       0,
			Capacity:          c.Capacity,
			State:             u.initialShardState(),
			LastHeartbeatMs:   0, // 种子:从未心跳(扫描排除;requireHeartbeatReady 时为 warming 不可分配)
			CreatedAtMs:       now,
			CurrentTokenExpMs: u.candidateTokenExp(c.TokenExpMs), // exp 镜像(调试用,仅 enforce 门控下非 0)
			CurrentTokenGen:   u.candidateTokenGen(c.TokenGen),   // 令牌代际(仅 enforce 门控下非 0)
			ReleaseTrack:      c.ReleaseTrack,
		}
		if cerr := u.repo.CreateShard(ctx, rec, u.shardTTL()); cerr != nil {
			return cerr
		}
	}
	return nil
}

// reconcileShardTopology 后台按 Fleet 拓扑对账 Redis 分片镜像(每个 sweep tick 一次)。
// 解决:minikube/Agones 重启后 pod 名/端口变化,旧分片在 Redis 里成为孤儿 —— 心跳超时只会把它
// 标 draining(无 draining_since_ms),reclaimDrainedShards 跳过、sweep 又每 tick 续期 TTL,导致
// 永久残留并让重登玩家拿到过期 hub_ds_addr。这里以 Fleet 为权威补齐 live 分片并清理 stale 孤儿。
// 放后台而非 AssignHub 热路径:避免每次登录都打 k8s apiserver。
// Fleet 暂不可用或某 region 候选为空时,保留现有镜像作为降级(绝不误删)。
func (u *HubUsecase) reconcileShardTopology(ctx context.Context) error {
	shards, err := u.repo.ListShards(ctx)
	if err != nil {
		return err
	}
	// 需对账的 region:已存在分片的 region + 默认 region(便于发现首个分片)。
	regions := map[string]struct{}{u.cfg.DefaultRegion: {}}
	for _, s := range shards {
		if s.Region != "" {
			regions[s.Region] = struct{}{}
		}
	}
	now := time.Now().UnixMilli()
	for region := range regions {
		cands, lerr := u.fleet.ListShards(ctx, region)
		if lerr != nil {
			plog.With(ctx).Warnw("msg", "reconcile_topology_list_failed", "region", region, "err", lerr)
			continue // 降级:Fleet 不可用时保留现有镜像
		}
		// An empty routable list is not physical teardown.  We still walk the
		// existing mirrors below, mark them unroutable, and require an exact
		// GameServer+Pod observation before minting teardown proof.
		live := make(map[string]struct{}, len(cands))
		presentCandidate := make(map[string]struct{}, len(cands))
		for _, c := range cands {
			presentCandidate[c.PodName] = struct{}{}
			if !c.TokenReady || !releasetrack.Valid(c.ReleaseTrack) {
				// Credential/release metadata failure removes routing eligibility but
				// says nothing about the process.  Keep all ownership ledgers and
				// force a fresh authenticated heartbeat after recovery.
				_ = u.repo.UpdateShardWithLock(ctx, c.PodName, u.retry(), func(s *hubv1.HubShardStorageRecord) error {
					if s.State != stateStopping && !(s.State == stateDraining && s.DrainingSinceMs > 0) {
						s.State = stateWarming
					}
					return nil
				}, u.shardTTL())
				continue
			}
			live[c.PodName] = struct{}{}
			_, found, gerr := u.repo.GetShard(ctx, c.PodName)
			if gerr != nil {
				plog.With(ctx).Warnw("msg", "reconcile_topology_get_failed", "pod", c.PodName, "err", gerr)
				continue
			}
			if found {
				// 已有镜像:刷新地址/容量(pod 复用旧名但换端口/扩缩容时同步)。
				if uerr := u.repo.UpdateShardWithLock(ctx, c.PodName, u.retry(), func(s *hubv1.HubShardStorageRecord) error {
					if s.ReleaseTrack == "" {
						s.ReleaseTrack = c.ReleaseTrack // additive rollout:旧镜像只迁为 stable/实际 metadata 轨
					} else if s.ReleaseTrack != c.ReleaseTrack {
						s.State = stateDraining
						return nil // metadata 轨漂移：保留原轨证据并退出可分配集
					}
					s.HubAddr = c.Addr
					s.Region = c.Region
					s.ShardId = c.ShardID
					s.Capacity = c.Capacity
					// 滚动升级投毒防护(审核 P1 #2):旧镜像 allocator(requireHeartbeatReady=false)
					// 可能把分片建成 ready+LastHeartbeatMs=0。新镜像开启心跳门控后,该分片从未发过
					// (鉴权)心跳却是 ready,会被 AssignHub 直接选中 —— 把玩家分到 DS 令牌握手尚未
					// 经真实鉴权心跳确认的 Hub。这里把「从未心跳且非排空态」的分片降级回 warming,
					// 等首个通过 Guard 的心跳(HeartbeatShard 里 warming→ready)再放行分配。
					if u.requireHeartbeatReady && s.LastHeartbeatMs == 0 &&
						s.State != stateDraining && s.State != stateStopping {
						s.State = stateWarming
					}
					// Topology/heartbeat-loss draining has no consolidation timestamp.
					// A routable candidate may re-enter only through a fresh heartbeat;
					// deliberate scale-in draining (timestamped) remains irreversible.
					if s.State == stateDraining && s.DrainingSinceMs == 0 {
						if u.requireHeartbeatReady {
							s.State = stateWarming
						} else {
							s.State = stateReady
						}
					}
					// 令牌代际单调推进(审核 P1 #3/#12):CurrentTokenGen **只增不减**,绝不被 0/低代际清除或回退。
					// permissive 副本 / annotation 缺失 → 候选 gen=0 → **保持镜像既有代际不变**(不降级已生效的
					// enforce 代际门;旧方案的「off/permissive 清 0 自愈」是 fail-open 降级向量,已废弃)。
					// 只有严格更高的候选代际(重签/轮换,含密钥轮换验签失败触发的重签)才推进,并复位 warming
					// 等新代际鉴权心跳——挡住旧令牌迟到心跳把轮换后的分片重新置 ready。gen 精确单调(非秒级)。
					if gen := u.candidateTokenGen(c.TokenGen); gen > s.CurrentTokenGen {
						s.CurrentTokenGen = gen
						s.CurrentTokenExpMs = u.candidateTokenExp(c.TokenExpMs) // 同步 exp 镜像(调试用)
						if u.requireHeartbeatReady && s.State != stateDraining && s.State != stateStopping {
							s.State = stateWarming // 新代际未证明:等带新令牌的鉴权心跳再放行分配
						}
					}
					return nil
				}, u.shardTTL()); uerr != nil && errcode.As(uerr) != errcode.ErrHubNoAvailable {
					plog.With(ctx).Warnw("msg", "reconcile_topology_update_failed", "pod", c.PodName, "err", uerr)
				}
				continue
			}
			// 新 pod:补齐镜像。
			rec := &hubv1.HubShardStorageRecord{
				HubPodName:        c.PodName,
				HubAddr:           c.Addr,
				Region:            c.Region,
				ShardId:           c.ShardID,
				PlayerCount:       0,
				Capacity:          c.Capacity,
				State:             u.initialShardState(),
				LastHeartbeatMs:   0,
				CreatedAtMs:       now,
				CurrentTokenExpMs: u.candidateTokenExp(c.TokenExpMs),
				CurrentTokenGen:   u.candidateTokenGen(c.TokenGen),
				ReleaseTrack:      c.ReleaseTrack,
			}
			if cerr := u.repo.CreateShard(ctx, rec, u.shardTTL()); cerr != nil {
				plog.With(ctx).Warnw("msg", "reconcile_topology_create_failed", "pod", c.PodName, "err", cerr)
			}
		}
		// A shard absent from the routable set is retained as a physical-owner
		// fence.  Candidate absence includes Scheduled/Unhealthy/token failures
		// and must never erase sessions.  Only an optional exact observer can
		// record UID-specific teardown proof; durable assignment cleanup consumes
		// that proof one owner at a time.
		for _, s := range shards {
			if s.Region != region {
				continue
			}
			if _, ok := live[s.HubPodName]; ok {
				continue
			}
			if _, returnedButUnroutable := presentCandidate[s.HubPodName]; !returnedButUnroutable {
				if rerr := u.repo.UpdateShardWithLock(ctx, s.HubPodName, u.retry(), func(current *hubv1.HubShardStorageRecord) error {
					if current.State != stateStopping && !(current.State == stateDraining && current.DrainingSinceMs > 0) {
						current.State = stateDraining
					}
					return nil
				}, u.shardTTL()); rerr != nil && errcode.As(rerr) != errcode.ErrHubNoAvailable {
					plog.With(ctx).Warnw("msg", "reconcile_topology_fence_stale_failed", "pod", s.HubPodName, "region", region, "err", rerr)
				}
			}
			observer, observerOK := u.fleet.(HubFleetPhysicalObserver)
			proofRepo, proofOK := u.authRepo.(data.HubInstanceTeardownProofRepo)
			if observerOK && proofOK && s.GameserverUid != "" {
				observation, observeErr := observer.ObserveShardInstance(ctx, s.HubPodName)
				if observeErr != nil {
					plog.With(ctx).Warnw("msg", "reconcile_topology_physical_observation_failed",
						"pod", s.HubPodName, "expected_uid", s.GameserverUid, "err", observeErr)
				} else if observation.ProvesTeardown(s.GameserverUid) {
					if proofErr := proofRepo.RecordInstanceTeardownProof(ctx, s.HubPodName,
						s.GameserverUid, u.assignmentSagaTTL()); proofErr != nil {
						plog.With(ctx).Warnw("msg", "reconcile_topology_record_teardown_failed",
							"pod", s.HubPodName, "expected_uid", s.GameserverUid, "err", proofErr)
					} else {
						plog.With(ctx).Warnw("msg", "reconcile_topology_exact_uid_teardown_confirmed",
							"pod", s.HubPodName, "expected_uid", s.GameserverUid,
							"observed_uid", observation.GameServerUID)
					}
				}
			}
		}
	}
	return nil
}

// selectShard:队友所在分片优先,否则同 region 最空 ready 分片(并列取 shard_id 小者,稳定)。
func (u *HubUsecase) selectShard(ctx context.Context, region string, teamID uint64) (*hubv1.HubShardStorageRecord, error) {
	shards, err := u.repo.ListShards(ctx)
	if err != nil {
		return nil, err
	}
	if teamID != 0 {
		if pod, ok, gerr := u.repo.GetTeamShard(ctx, teamID); gerr == nil && ok {
			for _, s := range shards {
				if s.HubPodName == pod && s.Region == region && s.State == stateReady && s.PlayerCount < s.Capacity {
					return s, nil
				}
			}
		}
	}
	best := leastLoaded(shards, region, releasetrack.Stable, "")
	if best == nil {
		return nil, errcode.New(errcode.ErrHubNoAvailable, "no ready hub shard with capacity in region %s", region)
	}
	return best, nil
}

// reserveSeat:乐观锁占一个座位(复核 ready + 容量,player_count++)。
func (u *HubUsecase) reserveSeat(ctx context.Context, pod string) error {
	return u.repo.UpdateShardWithLock(ctx, pod, u.retry(), func(s *hubv1.HubShardStorageRecord) error {
		if s.State != stateReady {
			return errcode.New(errcode.ErrHubNoAvailable, "hub shard %s not ready", pod)
		}
		if s.PlayerCount >= s.Capacity {
			return errcode.New(errcode.ErrHubNoAvailable, "hub shard %s full", pod)
		}
		s.PlayerCount++
		return nil
	}, u.shardTTL())
}

// releaseFromShard:退一个座位(floor 0)。分片不存在/锁冲突静默(幂等退位)。
func (u *HubUsecase) releaseFromShard(ctx context.Context, pod string) {
	err := u.repo.UpdateShardWithLock(ctx, pod, u.retry(), func(s *hubv1.HubShardStorageRecord) error {
		if s.PlayerCount > 0 {
			s.PlayerCount--
		}
		return nil
	}, u.shardTTL())
	if err != nil && errcode.As(err) != errcode.ErrHubNoAvailable {
		plog.With(ctx).Warnw("msg", "release_from_shard_failed", "pod", pod, "err", err)
	}
}

// effectiveRoleID 选角生效值:调用方显式传的 roleID(>0)优先(login 是角色数据权威),
// 否则回退归属镜像已存值(Transfer/重签路径不丢角色)。
func effectiveRoleID(requested, stored uint32) uint32 {
	if requested > 0 {
		return requested
	}
	return stored
}

// reserveRoutableSeat 原子占一个座位:Model B(authRepo 装配)走 ReserveRoutableSeat 单事务
// 授权+路由+占座门(审核二轮 CE6),返回本次绑定的 active 元组供钉进归属;legacy 走单纯 reserveSeat。
// 不可路由(授权未激活 / 分片非 ready / 元组不符 / 心跳陈旧 / 已满)→ ErrHubNoAvailable(fail-closed)。
func (u *HubUsecase) reserveRoutableSeat(ctx context.Context, pod string, playerID uint64, assignmentID string) (*data.ReserveResult, error) {
	if u.authRepo == nil {
		return nil, u.reserveSeat(ctx, pod) // legacy:纯容量占座
	}
	nowMs := time.Now().UnixMilli()
	current, err := u.authRepo.CheckRoutable(ctx, pod, nowMs, u.heartbeatMaxAgeMs())
	if err != nil {
		return nil, err
	}
	if !current.OK {
		plog.With(ctx).Warnw("msg", "hub_reserve_not_routable", "pod", pod, "reason", current.Reason)
		return nil, errcode.New(errcode.ErrHubNoAvailable, "hub shard %s not routable: %s", pod, current.Reason)
	}
	res, err := u.authRepo.ReserveAssignment(ctx, pod, data.ReservationIdentity{
		PlayerID: playerID, AssignmentID: assignmentID, InstanceUID: current.InstanceUID,
		ProtocolEpoch: current.ProtocolEpoch, WriterEpoch: current.WriterEpoch,
		ExpiresAtMs:           nowMs + u.reservationTTL().Milliseconds(),
		AssignmentExpiresAtMs: nowMs + u.assignTTL().Milliseconds(),
	}, nowMs, u.heartbeatMaxAgeMs(), u.shardTTL())
	if err != nil {
		return nil, err
	}
	if !res.OK {
		plog.With(ctx).Warnw("msg", "hub_reserve_not_routable", "pod", pod, "reason", res.Reason)
		return nil, errcode.New(errcode.ErrHubNoAvailable, "hub shard %s not routable: %s", pod, res.Reason)
	}
	return &res, nil
}

// ensureExistingAssignmentSeat 是旧 assignment 重签/同实例凭据重绑前的最终容量门。
// assignment key 与 {pod} ledger 不同 slot，故调用方仍需随后对完整旧 assignment 做 CAS；
// 本函数只在线性化的 {pod} 事务中保证以下二选一成立：
//   - exact assignment 已是 connected session：幂等返回，不增加容量；
//   - exact assignment 尚无 ledger owner：在仍可路由且未满时创建/刷新有界 reservation。
//
// 调用方在后续 assignment CAS 失败时不得盲目 ReleaseAssignmentSeat：返回的 seat 可能是
// 另一条仍存活连接的原 session。CAS winner 会按旧 assignment 精确释放；极端反序中新建但未被
// winner 观察到的 reservation 也只存活 ReservationTTL。
func (u *HubUsecase) ensureExistingAssignmentSeat(ctx context.Context, playerID uint64,
	assignment *hubv1.HubAssignmentStorageRecord, current *data.ReserveResult) (*data.ReserveResult, error) {
	if u.authRepo == nil {
		return current, nil
	}
	if assignment == nil || current == nil || !assignmentSameInstance(assignment, current) ||
		assignment.GetPlayerId() != playerID || assignment.GetAssignmentId() == "" {
		return nil, errcode.New(errcode.ErrInvalidState, "hub existing assignment identity is not reusable")
	}
	binding := placementBindingFromAssignment(assignment)
	if u.placementMode == placement.ModeEnforce && !binding.Complete() {
		return nil, errcode.New(errcode.ErrInvalidState,
			"hub existing assignment successor requires exact placement binding")
	}
	nowMs := time.Now().UnixMilli()
	res, err := u.authRepo.ReserveAssignment(ctx, assignment.GetHubPodName(), data.ReservationIdentity{
		PlayerID: playerID, AssignmentID: assignment.GetAssignmentId(), InstanceUID: current.InstanceUID,
		ProtocolEpoch: current.ProtocolEpoch, WriterEpoch: current.WriterEpoch,
		PlacementVersion: binding.Version, PlacementOperationID: binding.OperationID,
		SourceMatchID:         binding.SourceMatchID,
		ExpiresAtMs:           nowMs + u.reservationTTL().Milliseconds(),
		AssignmentExpiresAtMs: nowMs + u.assignTTL().Milliseconds(),
	}, nowMs, u.heartbeatMaxAgeMs(), u.shardTTL())
	if err != nil {
		return nil, err
	}
	if !res.OK {
		return nil, errcode.New(errcode.ErrHubNoAvailable,
			"hub shard %s cannot ensure existing assignment seat: %s", assignment.GetHubPodName(), res.Reason)
	}
	return &res, nil
}

// assignmentRoutable 返回归属目标的权威路由快照，并验证归属钉住的完整 active 身份仍为当前值。
// Model B 数据面永久只接受 writer=2 的完整 assignment tuple；legacy/缺字段/future writer 的
// 一次性迁移必须由 activation 控制面在开放业务流量前完成，不能在请求路径里静默“升级”。
func (u *HubUsecase) assignmentRoutable(
	ctx context.Context,
	playerID uint64,
	a *hubv1.HubAssignmentStorageRecord,
) (data.ReserveResult, bool, error) {
	assignmentTrack, trackErr := stickyReleaseTrack(a.GetReleaseTrack())
	if trackErr != nil {
		return data.ReserveResult{}, false, trackErr
	}
	if u.authRepo == nil {
		shard, found, err := u.repo.GetShard(ctx, a.HubPodName)
		if err != nil || !found || shard.State != stateReady {
			return data.ReserveResult{}, false, err
		}
		shardTrack, shardTrackErr := stickyReleaseTrack(shard.GetReleaseTrack())
		if shardTrackErr != nil || shardTrack != assignmentTrack {
			return data.ReserveResult{}, false, shardTrackErr
		}
		return data.ReserveResult{
			OK: true, ShardID: shard.ShardId, HubAddr: shard.HubAddr, Region: shard.Region,
			PlayerCount: shard.PlayerCount, Capacity: shard.Capacity, ReleaseTrack: shardTrack,
		}, true, nil
	}
	if !assignmentBindingV2Complete(a, playerID) {
		return data.ReserveResult{}, false, errcode.New(errcode.ErrInvalidState,
			"hub assignment is not a complete writer-v2 binding")
	}
	info, err := u.authRepo.CheckRoutable(ctx, a.HubPodName, time.Now().UnixMilli(), u.heartbeatMaxAgeMs())
	if err != nil {
		return data.ReserveResult{}, false, err
	}
	if !info.OK {
		return info, false, nil
	}
	infoTrack, infoTrackErr := stickyReleaseTrack(info.ReleaseTrack)
	if infoTrackErr != nil || infoTrack != assignmentTrack {
		return info, false, infoTrackErr
	}
	info.ReleaseTrack = infoTrack
	if a.HubInstanceUid != info.InstanceUID || a.AuthEpoch != info.ProtocolEpoch || a.AuthGen != info.ActiveGen {
		return info, false, nil
	}
	if a.AuthJti != info.ActiveJTI {
		return info, false, nil
	}
	if a.AuthWriterEpoch != info.WriterEpoch {
		return info, false, nil
	}
	return info, true, nil
}

func assignmentBindingV2Complete(a *hubv1.HubAssignmentStorageRecord, playerID uint64) bool {
	_, trackErr := stickyReleaseTrack(a.GetReleaseTrack())
	return a != nil && trackErr == nil && playerID != 0 && a.GetPlayerId() == playerID && a.GetHubPodName() != "" &&
		a.GetHubInstanceUid() != "" && a.GetAuthEpoch() != 0 && a.GetAuthGen() != 0 &&
		a.GetAuthJti() != "" && a.GetAssignmentId() != "" &&
		a.GetAuthWriterEpoch() == auth.DSAuthWriterEpochV2
}

// assignmentSameInstance 区分“同名 Pod 的凭据轮换”和“同名 GameServer 重建”。
// 只有 UID+protocol epoch 仍完全相同且当前权威可路由时，旧 assignment 的座位才可原地重绑；
// UID/epoch 任一变化必须走新占座，旧 token/assignment 永不复用。
func assignmentSameInstance(a *hubv1.HubAssignmentStorageRecord, current *data.ReserveResult) bool {
	if a == nil || current == nil {
		return false
	}
	aTrack, aTrackErr := stickyReleaseTrack(a.GetReleaseTrack())
	currentTrack, currentTrackErr := stickyReleaseTrack(current.ReleaseTrack)
	return current.OK && a.HubPodName != "" &&
		aTrackErr == nil && currentTrackErr == nil && aTrack == currentTrack &&
		a.HubInstanceUid != "" && a.HubInstanceUid == current.InstanceUID &&
		a.AuthEpoch != 0 && a.AuthEpoch == current.ProtocolEpoch && a.AuthGen != 0 &&
		a.AuthJti != "" && a.AuthWriterEpoch == auth.DSAuthWriterEpochV2 &&
		current.WriterEpoch == auth.DSAuthWriterEpochV2
}

// selectAndReserveShard 按队友优先、负载升序尝试所有候选；每个候选都必须通过最终原子授权+占座门。
func (u *HubUsecase) selectAndReserveShard(ctx context.Context, playerID uint64, assignmentID, region string, teamID uint64, excludePod, releaseTrack string) (*hubv1.HubShardStorageRecord, *data.ReserveResult, error) {
	if !releasetrack.Valid(releaseTrack) {
		return nil, nil, errcode.New(errcode.ErrInvalidArg, "invalid hub release_track %q", releaseTrack)
	}
	shards, err := u.repo.ListShards(ctx)
	if err != nil {
		return nil, nil, err
	}
	candidates := make([]*hubv1.HubShardStorageRecord, 0, len(shards))
	for _, shard := range shards {
		track, trackErr := stickyReleaseTrack(shard.GetReleaseTrack())
		if trackErr == nil && track == releaseTrack && shard.Region == region && shard.HubPodName != excludePod && shard.State == stateReady && shard.PlayerCount < shard.Capacity {
			candidates = append(candidates, shard)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].PlayerCount != candidates[j].PlayerCount {
			return candidates[i].PlayerCount < candidates[j].PlayerCount
		}
		return candidates[i].ShardId < candidates[j].ShardId
	})
	if teamID != 0 {
		if pod, found, gerr := u.repo.GetTeamShard(ctx, teamID); gerr != nil {
			return nil, nil, gerr
		} else if found {
			for i, candidate := range candidates {
				if candidate.HubPodName == pod {
					candidates[0], candidates[i] = candidates[i], candidates[0]
					break
				}
			}
		}
	}
	for _, candidate := range candidates {
		seat, rerr := u.reserveRoutableSeat(ctx, candidate.HubPodName, playerID, assignmentID)
		if rerr != nil {
			if errcode.As(rerr) == errcode.ErrHubNoAvailable {
				continue
			}
			return nil, nil, rerr
		}
		return authoritativeShard(candidate, seat), seat, nil
	}
	return nil, nil, errcode.New(errcode.ErrHubNoAvailable, "no authoritatively routable hub shard in region %s", region)
}

func authoritativeShard(shard *hubv1.HubShardStorageRecord, seat *data.ReserveResult) *hubv1.HubShardStorageRecord {
	out := proto.Clone(shard).(*hubv1.HubShardStorageRecord)
	if seat != nil {
		out.HubAddr = seat.HubAddr
		out.Region = seat.Region
		out.ShardId = seat.ShardID
		out.PlayerCount = seat.PlayerCount
		out.Capacity = seat.Capacity
		out.ReleaseTrack = seat.ReleaseTrack
	}
	return out
}

func (u *HubUsecase) compensateReservedSeat(ctx context.Context, pod string, playerID uint64, assignmentID string, seat *data.ReserveResult) {
	if u.authRepo == nil {
		u.releaseFromShard(ctx, pod)
		return
	}
	if seat == nil {
		return
	}
	released, err := u.authRepo.ReleaseAssignmentSeat(ctx, pod, data.AssignmentInstanceIdentity{
		PlayerID: playerID, AssignmentID: assignmentID, InstanceUID: seat.InstanceUID,
		ProtocolEpoch: seat.ProtocolEpoch, WriterEpoch: seat.WriterEpoch,
	}, u.shardTTL())
	if err != nil || !released {
		plog.With(ctx).Warnw("msg", "hub_reserved_seat_compensation_failed", "pod", pod, "released", released, "err", err)
	}
}

func (u *HubUsecase) releaseAssignmentSeat(ctx context.Context, assignment *hubv1.HubAssignmentStorageRecord) {
	if u.authRepo == nil {
		u.releaseFromShard(ctx, assignment.HubPodName)
		return
	}
	released, err := u.authRepo.ReleaseAssignmentSeat(ctx, assignment.HubPodName,
		assignmentInstanceIdentity(assignment), u.shardTTL())
	if err != nil {
		plog.With(ctx).Warnw("msg", "hub_assignment_seat_release_failed", "pod", assignment.HubPodName, "err", err)
	} else if !released {
		plog.With(ctx).Infow("msg", "hub_assignment_seat_release_skipped_stale_instance", "pod", assignment.HubPodName)
	}
}

func (u *HubUsecase) routableShardViews(ctx context.Context, shards []*hubv1.HubShardStorageRecord) ([]*hubv1.HubShardStorageRecord, error) {
	if u.authRepo == nil {
		return shards, nil
	}
	out := make([]*hubv1.HubShardStorageRecord, 0, len(shards))
	for _, shard := range shards {
		if shard.State != stateReady {
			continue
		}
		info, err := u.authRepo.CheckRoutable(ctx, shard.HubPodName, time.Now().UnixMilli(), u.heartbeatMaxAgeMs())
		if err != nil {
			return nil, err
		}
		if info.OK {
			out = append(out, authoritativeShard(shard, &info))
		}
	}
	return out, nil
}

// bindAssignmentAuth 把 Model B 占座时确认的 active 元组钉进归属记录(审计 + 复用漂移检测,§7)。
// seat=nil(legacy/off,reserveSeat 返回 nil)时不动。
func bindAssignmentAuth(a *hubv1.HubAssignmentStorageRecord, seat *data.ReserveResult) {
	if seat == nil {
		return
	}
	a.HubInstanceUid = seat.InstanceUID
	a.AuthEpoch = seat.ProtocolEpoch
	a.AuthGen = seat.ActiveGen
	a.AuthJti = seat.ActiveJTI
	a.AuthWriterEpoch = seat.WriterEpoch
}

func bindAssignmentPlacement(a *hubv1.HubAssignmentStorageRecord, binding placement.Binding) {
	if a == nil || !binding.Complete() {
		return
	}
	a.PlacementVersion = binding.Version
	a.PlacementOperationId = binding.OperationID
	a.SourceMatchId = binding.SourceMatchID
}

func transferCleanupRef(a *hubv1.HubAssignmentStorageRecord) data.TransferCleanupRef {
	if a == nil {
		return data.TransferCleanupRef{}
	}
	return data.TransferCleanupRef{PlayerID: a.GetPlayerId(), TargetAssignmentID: a.GetAssignmentId()}
}

func bindTransferCleanupSource(target, source *hubv1.HubAssignmentStorageRecord) error {
	if target == nil || source == nil || target.GetPlayerId() == 0 || target.GetPlayerId() != source.GetPlayerId() ||
		target.GetAssignmentId() == "" || source.GetAssignmentId() == "" ||
		target.GetAssignmentId() == source.GetAssignmentId() || source.GetHubPodName() == "" ||
		source.GetHubInstanceUid() == "" || source.GetAuthEpoch() == 0 ||
		source.GetAuthWriterEpoch() != auth.DSAuthWriterEpochV2 {
		return errcode.New(errcode.ErrInvalidState, "complete exact Hub transfer source owner required")
	}
	target.TransferCleanupPending = true
	target.TransferTargetBound = false
	target.TransferSourceHubPodName = source.GetHubPodName()
	target.TransferSourceAssignmentId = source.GetAssignmentId()
	target.TransferSourceInstanceUid = source.GetHubInstanceUid()
	target.TransferSourceAuthEpoch = source.GetAuthEpoch()
	target.TransferSourceAuthWriterEpoch = source.GetAuthWriterEpoch()
	target.ReleaseCleanupPending = false
	return nil
}

func transferCleanupSource(a *hubv1.HubAssignmentStorageRecord) (*hubv1.HubAssignmentStorageRecord, error) {
	if a == nil || !a.GetTransferCleanupPending() || a.GetReleaseCleanupPending() ||
		a.GetPlayerId() == 0 || a.GetAssignmentId() == "" ||
		a.GetTransferSourceHubPodName() == "" || a.GetTransferSourceAssignmentId() == "" ||
		a.GetTransferSourceAssignmentId() == a.GetAssignmentId() || a.GetTransferSourceInstanceUid() == "" ||
		a.GetTransferSourceAuthEpoch() == 0 ||
		a.GetTransferSourceAuthWriterEpoch() != auth.DSAuthWriterEpochV2 {
		return nil, errcode.New(errcode.ErrInvalidState, "Hub transfer cleanup source identity invalid")
	}
	return &hubv1.HubAssignmentStorageRecord{
		PlayerId: a.GetPlayerId(), HubPodName: a.GetTransferSourceHubPodName(),
		AssignmentId: a.GetTransferSourceAssignmentId(), HubInstanceUid: a.GetTransferSourceInstanceUid(),
		AuthEpoch: a.GetTransferSourceAuthEpoch(), AuthWriterEpoch: a.GetTransferSourceAuthWriterEpoch(),
	}, nil
}

func clearTransferCleanup(a *hubv1.HubAssignmentStorageRecord) {
	if a == nil {
		return
	}
	a.TransferCleanupPending = false
	a.TransferTargetBound = false
	a.TransferSourceHubPodName = ""
	a.TransferSourceAssignmentId = ""
	a.TransferSourceInstanceUid = ""
	a.TransferSourceAuthEpoch = 0
	a.TransferSourceAuthWriterEpoch = 0
}

func assignmentInstanceIdentity(a *hubv1.HubAssignmentStorageRecord) data.AssignmentInstanceIdentity {
	if a == nil {
		return data.AssignmentInstanceIdentity{}
	}
	binding := placementBindingFromAssignment(a)
	return data.AssignmentInstanceIdentity{PlayerID: a.GetPlayerId(), AssignmentID: a.GetAssignmentId(),
		InstanceUID: a.GetHubInstanceUid(), ProtocolEpoch: a.GetAuthEpoch(), WriterEpoch: a.GetAuthWriterEpoch(),
		PlacementVersion: binding.Version, PlacementOperationID: binding.OperationID,
		SourceMatchID: binding.SourceMatchID}
}

func (u *HubUsecase) registerTransferCleanup(ctx context.Context, target,
	source *hubv1.HubAssignmentStorageRecord) error {
	if u.placementMode != placement.ModeEnforce {
		return nil
	}
	if u.authRepo == nil {
		return errcode.New(errcode.ErrUnavailable, "Hub owner cleanup authority unavailable")
	}
	if source.GetTransferCleanupPending() || source.GetReleaseCleanupPending() {
		return errcode.New(errcode.ErrInvalidState, "previous Hub owner cleanup is still pending")
	}
	if err := bindTransferCleanupSource(target, source); err != nil {
		return err
	}
	// Index-first is deliberate. A crash/CAS loser can leave only an orphan
	// ref; a successful assignment CAS can never become invisible to restart.
	return u.repo.RegisterTransferCleanup(ctx, source.GetHubPodName(), transferCleanupRef(target))
}

func (u *HubUsecase) removeTransferCleanupRef(ctx context.Context, sourcePod string,
	ref data.TransferCleanupRef) {
	if err := u.repo.RemoveTransferCleanup(ctx, sourcePod, ref); err != nil {
		plog.With(ctx).Warnw("msg", "hub_owner_cleanup_index_remove_failed", "source_pod", sourcePod,
			"player_id", ref.PlayerID, "assignment_id", ref.TargetAssignmentID, "err", err)
	}
}

// resumeAssignmentCleanup is the only phase driver for transfer/release owner
// cleanup. It uses no process-local state: every retry starts from the current
// assignment, confirms the same target Bind before source release, and advances
// with exact CAS. A changed assignment id is never touched.
func (u *HubUsecase) resumeAssignmentCleanup(ctx context.Context, playerID uint64,
	assignmentID string) (*hubv1.HubAssignmentStorageRecord, bool, error) {
	if playerID == 0 || assignmentID == "" {
		return nil, false, errcode.New(errcode.ErrInvalidArg, "Hub cleanup assignment identity required")
	}
	if u.authRepo == nil {
		return nil, false, errcode.New(errcode.ErrUnavailable, "Hub owner cleanup authority unavailable")
	}
	for attempt := 0; attempt < 16; attempt++ {
		current, found, err := u.repo.GetAssignment(ctx, playerID)
		if err != nil {
			return nil, false, err
		}
		if !found {
			return nil, false, nil
		}
		if current.GetAssignmentId() != assignmentID {
			return nil, false, errcode.New(errcode.ErrLocatorConflict,
				"Hub cleanup assignment was superseded")
		}
		if current.GetTransferCleanupPending() && current.GetReleaseCleanupPending() {
			return nil, false, errcode.New(errcode.ErrInvalidState,
				"Hub assignment has conflicting cleanup phases")
		}
		if current.GetReleaseCleanupPending() {
			ref := transferCleanupRef(current)
			hasDepartureBinding := current.GetReleaseCleanupMatchId() != 0 ||
				current.GetReleaseCleanupPlacementVersion() != 0 ||
				current.GetReleaseCleanupOperationId() != ""
			if hasDepartureBinding && (current.GetReleaseCleanupMatchId() == 0 ||
				current.GetReleaseCleanupPlacementVersion() == 0 ||
				!placement.ValidOperationID(current.GetReleaseCleanupOperationId())) {
				return nil, false, errcode.New(errcode.ErrInvalidState,
					"Hub release cleanup has partial Battle departure binding")
			}
			seat, inspectErr := u.authRepo.InspectAssignmentSeat(ctx, current.GetHubPodName(),
				assignmentInstanceIdentity(current))
			if inspectErr != nil {
				return nil, false, inspectErr
			}
			if seat.Conflict || (!seat.Reserved && !seat.Connected && !seat.AlreadyAbsent) {
				return nil, false, errcode.New(errcode.ErrInvalidState,
					"Hub release cleanup exact owner conflict")
			}
			result, releaseErr := u.authRepo.ReleaseAssignmentSeatExact(ctx, current.GetHubPodName(),
				assignmentInstanceIdentity(current), u.shardTTL())
			if releaseErr != nil {
				return nil, false, releaseErr
			}
			if result.DepartureRequired {
				return nil, false, errcode.New(errcode.ErrUnavailable,
					"source Hub became connected while release cleanup was running")
			}
			if result.Conflict || (!result.Released && !result.AlreadyAbsent) {
				return nil, false, errcode.New(errcode.ErrInvalidState,
					"Hub release cleanup exact owner conflict")
			}
			if hasDepartureBinding {
				targetBinding := placement.Binding{Version: current.GetReleaseCleanupPlacementVersion(),
					OperationID: current.GetReleaseCleanupOperationId()}
				if err := u.confirmHubSourceDeparture(ctx, playerID, targetBinding,
					placementBindingFromAssignment(current), placementTargetFromAssignment(current)); err != nil {
					return nil, false, err
				}
			}
			deleted, deleteErr := u.repo.CompareAndSwapAssignment(ctx, playerID, current, nil, 0)
			if deleteErr != nil {
				return nil, false, deleteErr
			}
			if !deleted {
				continue
			}
			u.removeShardMember(ctx, current.GetHubPodName(), playerID)
			u.removeTransferCleanupRef(ctx, current.GetHubPodName(), ref)
			return nil, false, nil
		}
		if !current.GetTransferCleanupPending() {
			if current.GetTransferTargetBound() || current.GetTransferSourceHubPodName() != "" ||
				current.GetTransferSourceAssignmentId() != "" || current.GetTransferSourceInstanceUid() != "" ||
				current.GetTransferSourceAuthEpoch() != 0 || current.GetTransferSourceAuthWriterEpoch() != 0 ||
				current.GetReleaseCleanupMatchId() != 0 || current.GetReleaseCleanupPlacementVersion() != 0 ||
				current.GetReleaseCleanupOperationId() != "" {
				return nil, false, errcode.New(errcode.ErrInvalidState,
					"Hub assignment has orphan cleanup fields")
			}
			return current, true, nil
		}
		if u.placementMode != placement.ModeEnforce {
			return nil, false, errcode.New(errcode.ErrUnavailable,
				"strict placement is required to resume Hub transfer cleanup")
		}
		source, sourceErr := transferCleanupSource(current)
		if sourceErr != nil {
			return nil, false, sourceErr
		}
		ref := transferCleanupRef(current)
		if !current.GetTransferTargetBound() {
			bound, bindErr := u.bindPlacementTarget(ctx, playerID, current)
			if bindErr != nil {
				return nil, false, bindErr
			}
			if bound.GetAssignmentId() != assignmentID || !bound.GetTransferCleanupPending() {
				return nil, false, errcode.New(errcode.ErrLocatorConflict,
					"Hub transfer cleanup changed while binding target")
			}
			next := proto.Clone(bound).(*hubv1.HubAssignmentStorageRecord)
			next.TransferTargetBound = true
			marked, markErr := u.repo.CompareAndSwapAssignment(ctx, playerID, bound, next, u.assignmentSagaTTL())
			if markErr != nil {
				return nil, false, markErr
			}
			if !marked {
				continue
			}
			current = next
		}
		seat, inspectErr := u.authRepo.InspectAssignmentSeat(ctx, source.GetHubPodName(),
			assignmentInstanceIdentity(source))
		if inspectErr != nil {
			return nil, false, inspectErr
		}
		if seat.Conflict || (!seat.Reserved && !seat.Connected && !seat.AlreadyAbsent) {
			return nil, false, errcode.New(errcode.ErrInvalidState,
				"Hub transfer source cleanup exact owner conflict")
		}
		result, releaseErr := u.authRepo.ReleaseAssignmentSeatExact(ctx, source.GetHubPodName(),
			assignmentInstanceIdentity(source), u.shardTTL())
		if releaseErr != nil {
			return nil, false, releaseErr
		}
		if result.DepartureRequired {
			return nil, false, errcode.New(errcode.ErrUnavailable,
				"source Hub became connected while transfer cleanup was running")
		}
		if result.Conflict || (!result.Released && !result.AlreadyAbsent) {
			return nil, false, errcode.New(errcode.ErrInvalidState,
				"Hub transfer source cleanup exact owner conflict")
		}
		if err := u.confirmHubSourceDeparture(ctx, playerID, placementBindingFromAssignment(current),
			placementBindingFromAssignment(source), placementTargetFromAssignment(source)); err != nil {
			return nil, false, err
		}
		next := proto.Clone(current).(*hubv1.HubAssignmentStorageRecord)
		clearTransferCleanup(next)
		cleared, clearErr := u.repo.CompareAndSwapAssignment(ctx, playerID, current, next, u.assignmentSagaTTL())
		if clearErr != nil {
			return nil, false, clearErr
		}
		if !cleared {
			continue
		}
		u.removeShardMember(ctx, source.GetHubPodName(), playerID)
		u.removeTransferCleanupRef(ctx, source.GetHubPodName(), ref)
		return next, true, nil
	}
	return nil, false, errcode.New(errcode.ErrInternal, "Hub owner cleanup CAS retry exhausted")
}

func placementBindingFromAssignment(a *hubv1.HubAssignmentStorageRecord) placement.Binding {
	if a == nil {
		return placement.Binding{}
	}
	return placement.Binding{Version: a.GetPlacementVersion(), OperationID: a.GetPlacementOperationId(),
		SourceMatchID: a.GetSourceMatchId()}
}

func placementTargetFromAssignment(a *hubv1.HubAssignmentStorageRecord) placement.Target {
	if a == nil {
		return placement.Target{}
	}
	return placement.Target{PodName: a.GetHubPodName(), InstanceUID: a.GetHubInstanceUid(),
		InstanceEpoch: a.GetAuthEpoch(), AssignmentID: a.GetAssignmentId(), ReleaseTrack: a.GetReleaseTrack()}
}

func pendingHubRetargetSnapshot(s data.HubPlacementSnapshot) bool {
	return s.Found && s.Binding.Complete() && s.Target.CompleteHub() &&
		s.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
		s.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB && s.TargetMatchID == 0
}

// adoptHubRetarget closes the "locator committed, allocator response/CAS was
// lost" half of the retarget saga.  The locator is the route authority, but we
// only adopt the immediately succeeding binding for this exact persisted
// assignment target; an unrelated/jumped version is never copied locally.
func (u *HubUsecase) adoptHubRetarget(ctx context.Context, playerID uint64,
	a *hubv1.HubAssignmentStorageRecord, snapshot data.HubPlacementSnapshot,
) (*hubv1.HubAssignmentStorageRecord, bool, error) {
	old, target := placementBindingFromAssignment(a), placementTargetFromAssignment(a)
	if !pendingHubRetargetSnapshot(snapshot) || !snapshot.Target.Equal(target) ||
		snapshot.RetargetCount == 0 || !old.Complete() ||
		snapshot.Binding.Version != old.Version+1 ||
		snapshot.Binding.SourceMatchID != old.SourceMatchID ||
		!placement.ValidOperationID(snapshot.Binding.OperationID) {
		return a, false, nil
	}
	// Renew the exact accepted target before publishing a ticket or persisting
	// the new binding locally.  An expired lease is recoverable; another
	// version/op/target still loses the locator CAS.
	if err := u.placementClient.BindHubTarget(ctx, playerID, snapshot.Binding, target,
		time.Now().Add(u.reservationTTL()).UnixMilli()); err != nil {
		return nil, false, err
	}
	next := proto.Clone(a).(*hubv1.HubAssignmentStorageRecord)
	bindAssignmentPlacement(next, snapshot.Binding)
	next.PlacementProofType = uint32(placement.ProofHubTransfer)
	swapped, err := u.repo.CompareAndSwapAssignment(ctx, playerID, a, next, u.assignmentSagaTTL())
	if err != nil {
		return nil, false, err
	}
	if !swapped {
		return nil, false, errcode.New(errcode.ErrLocatorConflict,
			"Hub retarget adoption assignment CAS lost")
	}
	return next, true, nil
}

// retargetUnavailableHubTarget is the only allocator producer of a Hub
// old-target -> new-target proof.  A stale heartbeat or lease timeout is not
// enough: the exact source assignment must have no connected owner.  A still
// present reservation is accepted only after its absolute expiry and exact
// compare-release; live sessions wait for DS Logout/Departure proof.
func (u *HubUsecase) retargetUnavailableHubTarget(ctx context.Context, playerID uint64,
	a *hubv1.HubAssignmentStorageRecord, snapshot data.HubPlacementSnapshot,
) (*hubv1.HubAssignmentStorageRecord, bool, error) {
	if !pendingHubRetargetSnapshot(snapshot) || !snapshot.Binding.Equal(placementBindingFromAssignment(a)) ||
		snapshot.Target.Equal(placementTargetFromAssignment(a)) ||
		u.authRepo == nil || u.placementProofSigner == nil || !a.GetTransferCleanupPending() {
		return a, false, nil
	}
	source, err := transferCleanupSource(a)
	if err != nil {
		return nil, false, err
	}
	oldTarget := snapshot.Target
	if source.GetHubPodName() != oldTarget.PodName ||
		source.GetHubInstanceUid() != oldTarget.InstanceUID ||
		source.GetAuthEpoch() != oldTarget.InstanceEpoch ||
		source.GetAssignmentId() != oldTarget.AssignmentID {
		return nil, false, errcode.New(errcode.ErrLocatorConflict,
			"Hub retarget source does not match exact locator target")
	}
	seat, err := u.authRepo.InspectAssignmentSeat(ctx, source.GetHubPodName(),
		assignmentInstanceIdentity(source))
	if err != nil {
		return nil, false, err
	}
	if seat.Conflict || seat.Connected || (!seat.Reserved && !seat.AlreadyAbsent) {
		return nil, false, errcode.New(errcode.ErrUnavailable,
			"exact old Hub target has not proved unused/departed")
	}
	if seat.Reserved {
		nowMs := time.Now().UnixMilli()
		if seat.ReservationExpiresAtMs <= 0 || seat.ReservationExpiresAtMs > nowMs {
			return nil, false, errcode.New(errcode.ErrUnavailable,
				"exact old Hub reservation is still live")
		}
		released, releaseErr := u.authRepo.ReleaseAssignmentSeatExact(ctx, source.GetHubPodName(),
			assignmentInstanceIdentity(source), u.shardTTL())
		if releaseErr != nil {
			return nil, false, releaseErr
		}
		if released.Conflict || released.DepartureRequired || (!released.Released && !released.AlreadyAbsent) {
			return nil, false, errcode.New(errcode.ErrUnavailable,
				"expired old Hub reservation could not be released exactly")
		}
	}

	expected := snapshot.Binding
	replacement := placement.Binding{Version: expected.Version + 1,
		OperationID: uuid.NewString(), SourceMatchID: expected.SourceMatchID}
	newTarget := placementTargetFromAssignment(a)
	proofID := "hub-target-unavailable:" + oldTarget.AssignmentID + ":" + newTarget.AssignmentID
	proof := placement.TargetUnavailableProof{PlayerID: playerID,
		PlacementVersion: expected.Version, OperationID: expected.OperationID,
		TargetRoute: placement.RouteHub, ExpectedTarget: oldTarget,
		ReplacementVersion: replacement.Version, ReplacementOperationID: replacement.OperationID,
		ReplacementTarget: newTarget, ProofType: placement.ProofHubTransfer,
		Reason: placement.TargetUnavailableReservationExpiredUnused, ProofID: proofID}
	confirmed, err := u.placementClient.RetargetHubTarget(ctx, playerID, expected, replacement,
		oldTarget, newTarget, placement.TargetUnavailableReservationExpiredUnused,
		proofID, u.placementProofSigner.SignTargetUnavailable(proof),
		time.Now().Add(u.reservationTTL()).UnixMilli())
	if err != nil {
		return nil, false, err
	}
	if !confirmed.Equal(replacement) {
		return nil, false, errcode.New(errcode.ErrLocatorConflict,
			"Hub retarget response binding mismatch")
	}
	next := proto.Clone(a).(*hubv1.HubAssignmentStorageRecord)
	bindAssignmentPlacement(next, replacement)
	next.PlacementProofType = uint32(placement.ProofHubTransfer)
	swapped, err := u.repo.CompareAndSwapAssignment(ctx, playerID, a, next, u.assignmentSagaTTL())
	if err != nil {
		return nil, false, err
	}
	if !swapped {
		return nil, false, errcode.New(errcode.ErrLocatorConflict,
			"Hub retarget assignment CAS lost")
	}
	return next, true, nil
}

func (u *HubUsecase) bindPlacementTarget(ctx context.Context, playerID uint64, a *hubv1.HubAssignmentStorageRecord) (*hubv1.HubAssignmentStorageRecord, error) {
	if u.placementMode == placement.ModeOff {
		return a, nil
	}
	binding, target := placementBindingFromAssignment(a), placementTargetFromAssignment(a)
	var err error
	if !binding.Complete() || !target.CompleteHub() {
		err = errcode.New(errcode.ErrLocatorNotFound, "Hub assignment has no complete placement binding")
	} else if u.placementClient == nil {
		err = errcode.New(errcode.ErrUnavailable, "placement client unavailable")
	} else {
		err = u.placementClient.BindHubTarget(ctx, playerID, binding, target,
			time.Now().Add(u.reservationTTL()).UnixMilli())
	}
	if err != nil && errcode.As(err) == errcode.ErrLocatorConflict && binding.Complete() && target.CompleteHub() &&
		u.placementClient != nil {
		// Distinguish an expired/retried PENDING transfer from a STABLE Hub that
		// needs a new target. Reusing a stable operation can never Begin again;
		// inventing a new op while PENDING would create a second writer.
		snapshot, readErr := u.placementClient.GetHubPlacement(ctx, playerID)
		if readErr == nil {
			if adopted, ok, adoptErr := u.adoptHubRetarget(ctx, playerID, a, snapshot); adoptErr != nil {
				err = adoptErr
			} else if ok {
				return adopted, nil
			} else if retargeted, didRetarget, retargetErr := u.retargetUnavailableHubTarget(ctx, playerID, a, snapshot); retargetErr != nil {
				err = retargetErr
			} else if didRetarget {
				return retargeted, nil
			}
		}
		forceNew := readErr == nil && snapshot.Found && snapshot.Binding.Equal(binding) &&
			snapshot.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE &&
			snapshot.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB
		resumePending := readErr == nil && snapshot.Found && snapshot.Binding.Equal(binding) &&
			snapshot.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
			snapshot.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB &&
			a.GetPlacementProofType() == uint32(placement.ProofHubTransfer)
		unbegunPersisted := readErr == nil && snapshot.Found &&
			snapshot.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE &&
			snapshot.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB &&
			a.GetPlacementProofType() == uint32(placement.ProofHubTransfer) &&
			binding.Version == snapshot.Binding.Version+1
		if readErr != nil {
			err = readErr
		} else if errcode.As(err) != errcode.ErrLocatorConflict {
			// A retarget/adoption attempt returned a stronger transient/authority
			// error above. Do not reinterpret it as a fresh Hub transfer.
		} else if !forceNew && !resumePending && !unbegunPersisted {
			err = errcode.New(errcode.ErrLocatorConflict, "Hub placement conflict is not a resumable transfer")
		} else if next, beginErr := u.ensureHubTransferTransition(ctx, playerID, a, forceNew); beginErr == nil {
			a = next
			binding = placementBindingFromAssignment(a)
			err = u.placementClient.BindHubTarget(ctx, playerID, binding, placementTargetFromAssignment(a),
				time.Now().Add(u.reservationTTL()).UnixMilli())
		} else {
			err = beginErr
		}
	}
	if err == nil {
		return a, nil
	}
	plog.With(ctx).Warnw("msg", "hub_placement_bind_failed", "mode", u.placementMode.String(),
		"player_id", playerID, "assignment_id", a.GetAssignmentId(), "err", err)
	if u.placementMode == placement.ModeShadow {
		return a, nil
	}
	return nil, err
}

func (u *HubUsecase) ensureHubTransferTransition(ctx context.Context, playerID uint64,
	a *hubv1.HubAssignmentStorageRecord, forceNew bool) (*hubv1.HubAssignmentStorageRecord, error) {
	if u.placementClient == nil || u.placementProofSigner == nil || a == nil {
		return nil, errcode.New(errcode.ErrUnavailable, "Hub transfer placement authority unavailable")
	}
	next := a
	if forceNew || a.GetPlacementProofType() != uint32(placement.ProofHubTransfer) {
		old := placementBindingFromAssignment(a)
		if !old.Complete() {
			return nil, errcode.New(errcode.ErrLocatorNotFound, "Hub transfer source placement missing")
		}
		next = proto.Clone(a).(*hubv1.HubAssignmentStorageRecord)
		next.PlacementVersion = old.Version + 1
		next.PlacementOperationId = uuid.NewString()
		next.SourceMatchId = 0
		next.PlacementProofType = uint32(placement.ProofHubTransfer)
		swapped, err := u.repo.CompareAndSwapAssignment(ctx, playerID, a, next, u.assignmentSagaTTL())
		if err != nil {
			return nil, err
		}
		if !swapped {
			return nil, errcode.New(errcode.ErrLocatorConflict, "Hub transfer assignment operation CAS lost")
		}
	}
	b := placementBindingFromAssignment(next)
	if b.Version < 2 || !b.Complete() {
		return nil, errcode.New(errcode.ErrInvalidState, "Hub transfer operation identity invalid")
	}
	proofID := "hub-transfer:" + next.GetAssignmentId()
	proof := placement.Proof{PlayerID: playerID, ExpectedVersion: b.Version - 1,
		SourceRoute: placement.RouteHub, TargetRoute: placement.RouteHub,
		ProofType: placement.ProofHubTransfer, ProofID: proofID, OperationID: b.OperationID}
	confirmed, err := u.placementClient.BeginHubTransfer(ctx, playerID, b.Version-1, b.OperationID,
		proofID, u.placementProofSigner.Sign(proof), time.Now().Add(u.reservationTTL()).UnixMilli())
	if err != nil {
		return nil, err
	}
	if !confirmed.Equal(b) {
		return nil, errcode.New(errcode.ErrLocatorConflict, "Hub transfer operation response mismatch")
	}
	return next, nil
}

func ticketBindingFromAssignment(a *hubv1.HubAssignmentStorageRecord) HubTicketBinding {
	releaseTrack, trackErr := stickyReleaseTrack(a.GetReleaseTrack())
	if a == nil || a.HubPodName == "" || a.HubInstanceUid == "" || a.AuthEpoch == 0 || a.AuthGen == 0 ||
		a.AuthJti == "" || a.AssignmentId == "" || a.AuthWriterEpoch != auth.DSAuthWriterEpochV2 || trackErr != nil {
		return HubTicketBinding{}
	}
	return HubTicketBinding{
		PodName: a.HubPodName, InstanceUID: a.HubInstanceUid, ProtocolEpoch: a.AuthEpoch,
		CredentialGen: a.AuthGen, CredentialJTI: a.AuthJti, HubAssignmentID: a.AssignmentId,
		WriterEpoch:  a.AuthWriterEpoch,
		ReleaseTrack: releaseTrack,
		Placement:    placementBindingFromAssignment(a),
	}
}

func (u *HubUsecase) signHubTicket(ctx context.Context, playerID uint64, roleID uint32,
	assignment *hubv1.HubAssignmentStorageRecord,
) (string, int64, error) {
	if assignment == nil || u.signer == nil {
		return "", 0, errcode.New(errcode.ErrUnavailable, "Hub ticket signer unavailable")
	}
	if u.authRepo != nil && !assignmentBindingV2Complete(assignment, playerID) {
		return "", 0, errcode.New(errcode.ErrInvalidState,
			"refuse to sign hub ticket from incomplete writer-v2 assignment")
	}
	if u.placementMode == placement.ModeEnforce {
		if !placementBindingFromAssignment(assignment).Complete() {
			return "", 0, errcode.New(errcode.ErrInvalidState,
				"refuse to sign Hub ticket without placement binding")
		}
		if err := u.requireHubTicketPlacementReady(ctx, playerID, assignment); err != nil {
			return "", 0, err
		}
	}
	token, expMs, err := u.signer.SignHubTicket(playerID, roleID, ticketBindingFromAssignment(assignment))
	if err != nil {
		plog.With(ctx).Errorw("msg", "sign_hub_ticket_failed", "player_id", playerID, "err", err)
		return "", 0, errcode.New(errcode.ErrInternal, "sign hub ticket failed")
	}
	return token, expMs, nil
}

func (u *HubUsecase) signResult(ctx context.Context, playerID uint64, roleID uint32, assignment *hubv1.HubAssignmentStorageRecord) (*AssignResult, error) {
	token, expMs, err := u.signHubTicket(ctx, playerID, roleID, assignment)
	if err != nil {
		return nil, err
	}
	return &AssignResult{
		HubDSAddr:   assignment.HubAddr,
		HubTicket:   token,
		HubPodName:  assignment.HubPodName,
		ShardID:     assignment.ShardId,
		TicketExpMs: expMs,
		Placement:   placementBindingFromAssignment(assignment),
	}, nil
}

func (u *HubUsecase) transferResult(ctx context.Context, playerID uint64, roleID uint32, assignment *hubv1.HubAssignmentStorageRecord) (*TransferResult, error) {
	token, expMs, err := u.signHubTicket(ctx, playerID, roleID, assignment)
	if err != nil {
		return nil, err
	}
	return &TransferResult{
		NewHubDSAddr:  assignment.HubAddr,
		NewHubTicket:  token,
		NewHubPodName: assignment.HubPodName,
		TicketExpMs:   expMs,
	}, nil
}

// stickyReleaseTrack 是 additive rollout 的唯一旧值迁移规则：空轨按 stable 解释；
// 任意其他未知值 fail-closed，绝不被 cohort 策略重算。
func stickyReleaseTrack(track string) (string, error) {
	if track == "" {
		return releasetrack.Stable, nil
	}
	if !releasetrack.Valid(track) {
		return "", errcode.New(errcode.ErrInvalidState, "invalid persisted hub release_track %q", track)
	}
	return track, nil
}

// selectTransferTarget:targetHubID!=0 点名 shard_id 匹配的分片;否则同 region 最空「非当前」ready 分片。
func selectTransferTarget(shards []*hubv1.HubShardStorageRecord, cur *hubv1.HubAssignmentStorageRecord, targetHubID uint64) *hubv1.HubShardStorageRecord {
	curTrack, curTrackErr := stickyReleaseTrack(cur.GetReleaseTrack())
	if curTrackErr != nil {
		return nil
	}
	if targetHubID != 0 {
		if targetHubID > math.MaxUint32 {
			// shard_id 是 uint32(配置 ID),超出范围必然无匹配,直接返回避免截断误匹配
			return nil
		}
		want := uint32(targetHubID)
		for _, s := range shards {
			track, trackErr := stickyReleaseTrack(s.GetReleaseTrack())
			if trackErr == nil && track == curTrack && s.ShardId == want && s.Region == cur.Region && s.State == stateReady {
				if s.HubPodName == cur.HubPodName || s.PlayerCount < s.Capacity {
					return s
				}
			}
		}
		return nil
	}
	return leastLoaded(shards, cur.Region, curTrack, cur.HubPodName)
}

// leastLoaded:返回 region 内最空的 ready 且未满分片;excludePod 非空时排除它。并列取 shard_id 小者。
func leastLoaded(shards []*hubv1.HubShardStorageRecord, region, releaseTrack, excludePod string) *hubv1.HubShardStorageRecord {
	var best *hubv1.HubShardStorageRecord
	for _, s := range shards {
		track, trackErr := stickyReleaseTrack(s.GetReleaseTrack())
		if trackErr != nil || track != releaseTrack || s.Region != region || s.State != stateReady || s.PlayerCount >= s.Capacity {
			continue
		}
		if excludePod != "" && s.HubPodName == excludePod {
			continue
		}
		if best == nil || s.PlayerCount < best.PlayerCount ||
			(s.PlayerCount == best.PlayerCount && s.ShardId < best.ShardId) {
			best = s
		}
	}
	return best
}

// autoScaleEnabled 需同时满足:配置开启 + 存在真实 Fleet scaler。
// scaler 只有真 Agones provider(AgonesHubFleetProvider)才实现 HubFleetScaler;
// Mock provider 是拓扑-only 不实现该接口 → Mock 模式下 scaler==nil,
// 自动扩缩容/强制整合恒不运行(不会跑退化 no-op 误导评估)。
func (u *HubUsecase) autoScaleEnabled() bool {
	return u.cfg.AutoScaleEnabled && u.scaler != nil
}

// tryScaleOutOnNoCapacity 在当前 region 无可用分片时触发兜底扩容(+1)。
// 触发后调用方仍会返回 ErrHubNoAvailable,由上游重试进新副本。
func (u *HubUsecase) tryScaleOutOnNoCapacity(ctx context.Context, region string) {
	if !u.autoScaleEnabled() {
		return
	}
	current, err := u.scaler.GetFleetReplicas(ctx)
	if err != nil {
		plog.With(ctx).Warnw("msg", "hub_scaleout_get_replicas_failed", "region", region, "err", err)
		return
	}
	desired := current + 1
	if desired < u.cfg.MinReplicas {
		desired = u.cfg.MinReplicas
	}
	if desired > u.cfg.MaxReplicas {
		desired = u.cfg.MaxReplicas
	}
	if desired == current {
		return
	}
	if err := u.scaler.SetFleetReplicas(ctx, desired); err != nil {
		plog.With(ctx).Warnw("msg", "hub_scaleout_set_replicas_failed",
			"region", region, "current", current, "desired", desired, "err", err)
		return
	}
	plog.With(ctx).Infow("msg", "hub_scaleout_triggered", "region", region, "from", current, "to", desired)
}

// reconcileFleetReplicas 周期性副本治理(每个 sweep tick 调一次):
//   - ① 扩容(立即,仅向上):总在线 > 0 → ceil(total/players_per_hub) > current 时扩容
//   - ② 强制整合(可选,consolidation_enabled):ready 分片多于负载所需 → 排空最空的多余分片,
//     把分片上的玩家做服务端权威搬迁到目标分片,并下发迁移通知(Hub DS drain 心跳 + Kafka 推送双通道)
//   - ③ 回收 + 缩容:已排空且过 grace 的 draining 分片 → 删镜像 + 把 Fleet 副本降到仍需存活的分片数
//
// 缩容到副本数后由 Agones 决定删哪个 GameServer(可能不是被排空那个),这是当前阶段的已知限制
// (docs/design/agones-dev.md):缩容只在 draining 分片已排空且过 grace 后触发,被删 pod 已无在场玩家。
func (u *HubUsecase) reconcileFleetReplicas(ctx context.Context) error {
	if !u.autoScaleEnabled() {
		return nil
	}
	shards, err := u.repo.ListShards(ctx)
	if err != nil {
		return err
	}

	totalPlayers := sumPlayers(shards)

	current, err := u.scaler.GetFleetReplicas(ctx)
	if err != nil {
		return err
	}

	minReplicas := u.cfg.MinReplicas
	maxReplicas := u.cfg.MaxReplicas
	playersPerHub := u.cfg.PlayersPerHub
	if playersPerHub <= 0 {
		playersPerHub = 500
	}

	// 负载所需 ready 分片数(总在线=0 → min)。
	need := minReplicas
	if totalPlayers > 0 {
		// 在 int64 内先夹紧到 maxReplicas 再转 int32,防止 totalPlayers 极大时
		// 除法结果超 int32 范围转换回绕成负数
		needed := (totalPlayers + int64(playersPerHub) - 1) / int64(playersPerHub)
		if needed > int64(maxReplicas) {
			needed = int64(maxReplicas)
		}
		need = int32(needed)
		if need < minReplicas {
			need = minReplicas
		}
	}

	// ① 扩容(立即,仅向上)。扩容当 tick 不再缩容,等新 pod ready 后下个 tick 再治理。
	if need > current {
		if serr := u.scaler.SetFleetReplicas(ctx, need); serr != nil {
			return serr
		}
		plog.With(ctx).Infow("msg", "hub_fleet_scaled_out",
			"current", current, "desired", need, "players", totalPlayers,
			"players_per_hub", playersPerHub, "min", minReplicas, "max", maxReplicas)
		return nil
	}

	// ② 排空多余分片(标 draining + 盖 draining_since_ms,统一交 ③ 回收):
	//   - 总在线>0 且开启强制整合:搬迁最空多余分片的玩家到目标分片再排空。
	//   - 总在线=0:把超出 min_replicas 的空 ready 分片标 draining 盖戳。
	//     必须盖戳走回收路径删镜像 —— 否则直接把 Fleet 缩到 min 后,Agones 删掉的 pod
	//     只会被心跳超时扫成「无 draining_since_ms」的 draining 分片,reclaimDrainedShards
	//     跳过它,镜像就成了不可回收的 stale shard 永久残留在 shards 集合里。
	drained := false
	if totalPlayers > 0 && u.consolidationEnabled() {
		drained = u.consolidateOnce(ctx, shards, need)
	} else if totalPlayers == 0 {
		drained = u.drainEmptyShards(ctx, shards, minReplicas)
	}
	if drained {
		if fresh, ferr := u.repo.ListShards(ctx); ferr == nil {
			shards = fresh // 重读快照供回收判断
		}
	}

	// ③ 回收已排空且过 grace 的 draining 分片 + 缩容(只在镜像回收后才把 Fleet 降到存活分片数,
	// 保持 Fleet 副本数与镜像一致,避免缩 Fleet 后留下不可回收的 stale 镜像)。
	reclaimed := u.reclaimDrainedShards(ctx, shards)
	if reclaimed == 0 {
		// Never derive scale-in authority from a missing/short Redis mirror list.
		// Exact per-instance teardown is the only future path that may return a
		// positive reclaimed count.
		return nil
	}
	live := int32(len(shards)) - reclaimed
	desired := current
	target := live
	if target < need {
		target = need
	}
	if target < minReplicas {
		target = minReplicas
	}
	if target > maxReplicas {
		target = maxReplicas
	}
	if target < current {
		desired = target // 只在此处缩容
	}

	if desired != current {
		if serr := u.scaler.SetFleetReplicas(ctx, desired); serr != nil {
			return serr
		}
		plog.With(ctx).Infow("msg", "hub_fleet_scaled_in",
			"current", current, "desired", desired, "players", totalPlayers,
			"reclaimed", reclaimed, "min", minReplicas, "max", maxReplicas)
	}
	return nil
}

// consolidationEnabled 强制整合开关(需自动扩缩容已开)。
// 不强制要求 migrate pusher:即便没接 Kafka,服务端权威搬迁 + Hub DS drain 心跳仍能让玩家重连到新分片。
func (u *HubUsecase) consolidationEnabled() bool {
	return u.autoScaleEnabled() && u.cfg.ConsolidationEnabled
}

// consolidateOnce:ready 分片多于 need 时,把最空的多余分片标 draining 并搬迁其玩家。
// 返回是否有分片被排空(供调用方决定是否重读快照)。
func (u *HubUsecase) consolidateOnce(ctx context.Context, shards []*hubv1.HubShardStorageRecord, need int32) bool {
	ready := make([]*hubv1.HubShardStorageRecord, 0, len(shards))
	for _, s := range shards {
		if s.State == stateReady {
			ready = append(ready, s)
		}
	}
	if int32(len(ready)) <= need {
		return false // 没有多余 ready 分片
	}
	// 按负载升序(并列 shard_id 小者优先)排,排空最空的多余分片(保留最满的 need 个分片承接玩家)。
	sort.Slice(ready, func(i, j int) bool {
		if ready[i].PlayerCount != ready[j].PlayerCount {
			return ready[i].PlayerCount < ready[j].PlayerCount
		}
		return ready[i].ShardId < ready[j].ShardId
	})
	surplus := ready[:int32(len(ready))-need] // 升序前段=最空的多余分片
	drained := false
	for _, s := range surplus {
		if u.drainAndMigrate(ctx, s) {
			drained = true
		}
	}
	return drained
}

// drainEmptyShards:大厅没人(总在线=0)时,把超出 keep 的空 ready 分片标 draining + 盖戳,
// 交 reclaimDrainedShards 统一回收镜像(见 reconcileFleetReplicas ② 的说明)。返回是否有分片被排空。
func (u *HubUsecase) drainEmptyShards(ctx context.Context, shards []*hubv1.HubShardStorageRecord, keep int32) bool {
	ready := make([]*hubv1.HubShardStorageRecord, 0, len(shards))
	for _, s := range shards {
		if s.State == stateReady {
			ready = append(ready, s)
		}
	}
	if int32(len(ready)) <= keep {
		return false // 不超过保底,无需排空
	}
	// 保留 shard_id 最小的 keep 个,排空其余空分片(全空,排序仅取确定性)。
	sort.Slice(ready, func(i, j int) bool { return ready[i].ShardId < ready[j].ShardId })
	surplus := ready[keep:]
	drained := false
	for _, s := range surplus {
		if u.drainAndMigrate(ctx, s) {
			drained = true
		}
	}
	return drained
}

// drainAndMigrate:把分片标记 draining(盖时间戳)并服务端权威搬迁其在册玩家到目标分片。
// 单 tick 每分片最多搬 ConsolidationBatch 人(防抢占),剩余留下个 tick 续搬。
func (u *HubUsecase) drainAndMigrate(ctx context.Context, shard *hubv1.HubShardStorageRecord) bool {
	now := time.Now().UnixMilli()
	merr := u.repo.UpdateShardWithLock(ctx, shard.HubPodName, u.retry(), func(s *hubv1.HubShardStorageRecord) error {
		if s.State == stateReady {
			s.State = stateDraining
			s.DrainingSinceMs = now
		}
		return nil
	}, u.shardTTL())
	if merr != nil && errcode.As(merr) != errcode.ErrHubNoAvailable {
		plog.With(ctx).Warnw("msg", "drain_mark_failed", "pod", shard.HubPodName, "err", merr)
	}

	members, lerr := u.repo.ListShardMembers(ctx, shard.HubPodName)
	if lerr != nil {
		plog.With(ctx).Warnw("msg", "drain_list_members_failed", "pod", shard.HubPodName, "err", lerr)
	}
	// 成员反向索引是 best-effort 优化(只在 AssignHub/TransferHub 维护):部署前已在线、索引里
	// 没有的老玩家不会被这里服务端权威搬迁,而是靠 Hub DS drain 心跳兜底 —— 客户端收到 drain 指令
	// 后重连 AssignHub,幂等路径发现旧分片非 ready 即释放旧位重分到 ready 分片,旧分片 player_count
	// 随之递减,最终仍可被回收。member<player_count 时这里只少了对老玩家的无缝推送,不影响最终一致性。
	// 索引数明显少于在册人数时告警,便于观测首次整合的降级范围(详见 docs/design/agones-dev.md §2.2)。
	if shard.PlayerCount > 0 && len(members) < int(shard.PlayerCount) {
		plog.With(ctx).Warnw("msg", "drain_members_index_incomplete",
			"pod", shard.HubPodName, "indexed", len(members), "player_count", shard.PlayerCount)
	}

	fresh, ferr := u.repo.ListShards(ctx)
	if ferr != nil {
		plog.With(ctx).Warnw("msg", "drain_list_shards_failed", "pod", shard.HubPodName, "err", ferr)
		return true // 已标 draining,搬迁留下个 tick
	}
	fresh, ferr = u.routableShardViews(ctx, fresh)
	if ferr != nil {
		plog.With(ctx).Warnw("msg", "drain_authoritative_routes_failed", "pod", shard.HubPodName, "err", ferr)
		return true
	}

	batch := u.cfg.ConsolidationBatch
	if batch <= 0 {
		batch = 50
	}
	moved := 0
	for _, pid := range members {
		if moved >= batch {
			break
		}
		track, trackErr := stickyReleaseTrack(shard.GetReleaseTrack())
		if trackErr != nil {
			plog.With(ctx).Warnw("msg", "drain_invalid_release_track", "pod", shard.HubPodName, "err", trackErr)
			break
		}
		target := leastLoaded(fresh, shard.Region, track, shard.HubPodName)
		if target == nil {
			plog.With(ctx).Warnw("msg", "drain_no_target", "pod", shard.HubPodName, "region", shard.Region)
			break // 无空闲目标分片,留下个 tick
		}
		if u.migratePlayer(ctx, pid, shard, target) {
			moved++
			target.PlayerCount++ // 本地快照计数同步,均衡后续选择
		}
	}
	plog.With(ctx).Infow("msg", "hub_shard_draining",
		"pod", shard.HubPodName, "region", shard.Region, "members", len(members), "moved", moved)
	return true
}

// migratePlayer:把单个玩家从 from 分片服务端权威搬迁到 target 分片(镜像 TransferHub 的占位/切归属/退位顺序),
// 重签 hub 票据并推送 HubMigrateEvent(best-effort)。返回是否搬迁成功。
func (u *HubUsecase) migratePlayer(ctx context.Context, playerID uint64, from, target *hubv1.HubShardStorageRecord) bool {
	// 复核玩家仍在 from 分片(避免与玩家自身 Release/Transfer 竞争)。
	assign, found, err := u.repo.GetAssignment(ctx, playerID)
	if err != nil || !found {
		u.removeShardMember(ctx, from.HubPodName, playerID) // 已不在此分片,清理残留索引
		return false
	}
	resumedTransferCleanup := assign.GetTransferCleanupPending()
	if assign.GetTransferCleanupPending() || assign.GetReleaseCleanupPending() {
		var stillFound bool
		assign, stillFound, err = u.resumeAssignmentCleanup(ctx, playerID, assign.GetAssignmentId())
		if err != nil {
			plog.With(ctx).Warnw("msg", "drain_owner_cleanup_resume_failed", "player_id", playerID, "err", err)
			return false
		}
		if !stillFound {
			u.removeShardMember(ctx, from.HubPodName, playerID)
			return false
		}
	}
	if assign.HubPodName != from.HubPodName {
		u.removeShardMember(ctx, from.HubPodName, playerID)
		if resumedTransferCleanup && u.placementMode == placement.ModeEnforce &&
			target != nil && assign.GetHubPodName() == target.GetHubPodName() {
			// A crash/physical-departure wait can complete the durable cleanup on a
			// later drain tick. Publish that exact already-selected target instead
			// of treating the old source member as a stale index and losing the only
			// migration notification. Refresh an expired target reservation first.
			current, reusable, routeErr := u.assignmentRoutable(ctx, playerID, assign)
			if routeErr != nil || (!reusable && !assignmentSameInstance(assign, &current)) {
				return false
			}
			ensured, ensureErr := u.ensureExistingAssignmentSeat(ctx, playerID, assign, &current)
			if ensureErr != nil {
				return false
			}
			next := proto.Clone(assign).(*hubv1.HubAssignmentStorageRecord)
			next.HubAddr, next.ShardId, next.Region = ensured.HubAddr, ensured.ShardID, ensured.Region
			next.ReleaseTrack = ensured.ReleaseTrack
			bindAssignmentAuth(next, ensured)
			swapped, swapErr := u.repo.CompareAndSwapAssignment(ctx, playerID, assign, next,
				u.assignmentSagaTTL())
			if swapErr != nil || !swapped {
				return false
			}
			bound, bindErr := u.bindPlacementTarget(ctx, playerID, next)
			if bindErr != nil {
				return false
			}
			if u.placementMode == placement.ModeEnforce ||
				(u.placementMode == placement.ModeShadow && placementBindingFromAssignment(bound).Complete()) {
				// As in the interactive re-sign path, a successful bind may advance
				// the placement operation.  Rotate the successor before publishing the
				// migration ticket so Admission consumes the exact canonical lineage.
				if _, ensureErr := u.ensureExistingAssignmentSeat(ctx, playerID, bound, ensured); ensureErr != nil {
					return false
				}
			}
			token, _, signErr := u.signHubTicket(ctx, playerID, bound.GetRoleId(), bound)
			if signErr != nil {
				return false
			}
			u.pushMigrate(ctx, playerID, from, authoritativeShard(target, ensured), token)
			return true
		}
		return false
	}
	if u.authRepo != nil && !assignmentBindingV2Complete(assign, playerID) {
		plog.With(ctx).Warnw("msg", "drain_migration_rejected_invalid_assignment", "player_id", playerID)
		return false
	}
	newAssignmentID := uuid.NewString()
	seat, rerr := u.reserveRoutableSeat(ctx, target.HubPodName, playerID, newAssignmentID)
	if rerr != nil {
		return false // 目标没位置/非 ready,留下个 tick 重试
	}
	target = authoritativeShard(target, seat)
	now := time.Now().UnixMilli()
	newAssign := proto.Clone(assign).(*hubv1.HubAssignmentStorageRecord)
	newAssign.PlayerId = playerID
	newAssign.HubPodName = target.HubPodName
	newAssign.HubAddr = target.HubAddr
	newAssign.ShardId = target.ShardId
	newAssign.Region = target.Region
	newAssign.TeamId = assign.TeamId
	newAssign.AssignedAtMs = now
	newAssign.RoleId = assign.RoleId // 选角镜像随强制整合搬迁
	newAssign.AssignmentId = newAssignmentID
	newAssign.ReleaseTrack = target.ReleaseTrack
	bindAssignmentAuth(newAssign, seat)
	if u.authRepo != nil && !assignmentBindingV2Complete(newAssign, playerID) {
		plog.With(ctx).Errorw("msg", "migrate_assignment_missing_writer_v2_binding", "player_id", playerID)
		u.compensateReservedSeat(ctx, target.HubPodName, playerID, newAssignmentID, seat)
		return false
	}
	if u.placementMode != placement.ModeOff {
		oldBinding := placementBindingFromAssignment(assign)
		if !oldBinding.Complete() {
			if u.placementMode == placement.ModeEnforce {
				plog.With(ctx).Warnw("msg", "drain_migration_missing_source_placement", "player_id", playerID)
				u.compensateReservedSeat(ctx, target.HubPodName, playerID, newAssignmentID, seat)
				return false
			}
			plog.With(ctx).Warnw("msg", "drain_migration_placement_shadow_missing",
				"player_id", playerID, "assignment_id", assign.GetAssignmentId())
		} else {
			// The assignment CAS is the durable migration journal. Persist the
			// sole HUB->HUB operation before Begin/Bind so a process/RPC failure
			// can only resume this exact version, op and target.
			newAssign.PlacementVersion = oldBinding.Version + 1
			newAssign.PlacementOperationId = uuid.NewString()
			newAssign.SourceMatchId = 0
			newAssign.PlacementProofType = uint32(placement.ProofHubTransfer)
		}
	}
	cleanupRegistered := false
	if u.placementMode == placement.ModeEnforce {
		if cleanupErr := u.registerTransferCleanup(ctx, newAssign, assign); cleanupErr != nil {
			plog.With(ctx).Warnw("msg", "drain_owner_cleanup_register_failed", "player_id", playerID,
				"err", cleanupErr)
			u.compensateReservedSeat(ctx, target.HubPodName, playerID, newAssignmentID, seat)
			return false
		}
		cleanupRegistered = true
	}
	var token string
	if u.placementMode != placement.ModeEnforce {
		var terr error
		token, _, terr = u.signHubTicket(ctx, playerID, assign.RoleId, newAssign)
		if terr != nil {
			plog.With(ctx).Warnw("msg", "migrate_sign_ticket_failed", "player_id", playerID, "err", terr)
			u.compensateReservedSeat(ctx, target.HubPodName, playerID, newAssignmentID, seat)
			return false
		}
	}
	swapped, serr := u.repo.CompareAndSwapAssignment(ctx, playerID, assign, newAssign, u.assignmentSagaTTL())
	if serr != nil {
		if !cleanupRegistered {
			u.compensateReservedSeat(ctx, target.HubPodName, playerID, newAssignmentID, seat)
		}
		return false
	}
	if !swapped {
		if cleanupRegistered {
			u.removeTransferCleanupRef(ctx, assign.GetHubPodName(), transferCleanupRef(newAssign))
		}
		u.compensateReservedSeat(ctx, target.HubPodName, playerID, newAssignmentID, seat)
		return false
	}
	u.addShardMember(ctx, target.HubPodName, playerID)
	bound := newAssign
	if cleanupRegistered {
		var stillFound bool
		bound, stillFound, err = u.resumeAssignmentCleanup(ctx, playerID, newAssignmentID)
		if err != nil || !stillFound {
			plog.With(ctx).Warnw("msg", "drain_migration_owner_cleanup_failed",
				"player_id", playerID, "from", from.HubPodName, "to", target.HubPodName, "err", err)
			return false
		}
	} else {
		var bindErr error
		bound, bindErr = u.bindPlacementTarget(ctx, playerID, newAssign)
		if bindErr != nil {
			plog.With(ctx).Warnw("msg", "drain_migration_placement_bind_failed",
				"player_id", playerID, "from", from.HubPodName, "to", target.HubPodName, "err", bindErr)
			return false
		}
	}
	newAssign = bound
	if u.placementMode == placement.ModeEnforce {
		var terr error
		token, _, terr = u.signHubTicket(ctx, playerID, assign.RoleId, newAssign)
		if terr != nil {
			// Placement is bound but not admitted. Keep both seats and withhold
			// the event; an exact retry can re-sign without inventing a route.
			plog.With(ctx).Warnw("msg", "migrate_sign_ticket_failed", "player_id", playerID, "err", terr)
			return false
		}
	}
	if !cleanupRegistered {
		u.releaseAssignmentSeat(ctx, assign)
		u.removeShardMember(ctx, from.HubPodName, playerID)
	}

	// 通知仍是 best-effort；若推送失败，Login 从 durable assignment/op 重签恢复。
	u.pushMigrate(ctx, playerID, from, target, token)
	return true
}

// pushMigrate 推送 HubMigrateEvent 给被迁移玩家(migrate pusher 未接时静默跳过)。
func (u *HubUsecase) pushMigrate(ctx context.Context, playerID uint64, from, target *hubv1.HubShardStorageRecord, token string) {
	if u.migrate == nil {
		return
	}
	ev := &hubv1.HubMigrateEvent{
		PlayerId:     playerID,
		FromHubPod:   from.HubPodName,
		ToHubDsAddr:  target.HubAddr,
		ToHubTicket:  token,
		ToHubPodName: target.HubPodName,
		ToShardId:    target.ShardId,
		GraceSeconds: u.cfg.MigrateGraceSeconds,
		Reason:       migrateReasonConsolidation,
		TsMs:         time.Now().UnixMilli(),
	}
	payload, merr := proto.Marshal(ev)
	if merr != nil {
		plog.With(ctx).Warnw("msg", "migrate_marshal_failed", "player_id", playerID, "err", merr)
		return
	}
	if perr := u.migrate.PushMigrate(ctx, playerID, payload); perr != nil {
		plog.With(ctx).Warnw("msg", "migrate_push_failed", "player_id", playerID, "err", perr)
	}
}

// reclaimDrainedShards intentionally does not erase a mirror merely because a
// logical drain/grace timer completed.  Fleet scale-in does not select a
// particular GameServer, and Kubernetes DELETE acceptance is not process
// teardown.  Until scale-in is upgraded to an exact per-instance teardown
// saga, correctness wins over resource reclamation: the physical-owner fence
// stays durable and this function reports zero reclaimed replicas.
func (u *HubUsecase) reclaimDrainedShards(ctx context.Context, shards []*hubv1.HubShardStorageRecord) int32 {
	graceMs := int64(u.cfg.MigrateGraceSeconds) * 1000
	now := time.Now().UnixMilli()
	for _, s := range shards {
		if s.State != stateDraining || s.PlayerCount > 0 || s.DrainingSinceMs <= 0 {
			continue
		}
		if now-s.DrainingSinceMs < graceMs {
			continue // 未过 grace,保持 pod 存活让在场玩家完成倒计时切换
		}
		plog.With(ctx).Warnw("msg", "hub_scalein_waiting_exact_instance_teardown",
			"pod", s.HubPodName, "region", s.Region, "gameserver_uid", s.GameserverUid)
	}
	return 0
}

// addShardMember / removeShardMember:成员反向索引维护(best-effort,失败仅 Warn 不阻断主流程)。
func (u *HubUsecase) addShardMember(ctx context.Context, pod string, playerID uint64) {
	if err := u.repo.AddShardMember(ctx, pod, playerID, u.assignmentSagaTTL()); err != nil {
		plog.With(ctx).Warnw("msg", "add_shard_member_failed", "pod", pod, "player_id", playerID, "err", err)
	}
}

func (u *HubUsecase) removeShardMember(ctx context.Context, pod string, playerID uint64) {
	if err := u.repo.RemoveShardMember(ctx, pod, playerID); err != nil {
		plog.With(ctx).Warnw("msg", "remove_shard_member_failed", "pod", pod, "player_id", playerID, "err", err)
	}
}

// sumPlayers 汇总分片在册人数(负数视为 0)。
func sumPlayers(shards []*hubv1.HubShardStorageRecord) int64 {
	var total int64
	for _, s := range shards {
		if s.PlayerCount > 0 {
			total += int64(s.PlayerCount)
		}
	}
	return total
}
