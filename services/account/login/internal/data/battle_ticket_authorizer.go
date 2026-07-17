// battle_ticket_authorizer.go 在 login 签发 battle DSTicket 前证明 player 属于目标 match。
// 这是签发线性化门，不修改 Redis；local/off 也必须经过 roster，不能因关闭在线 admission
// 就退化成“知道 match_id 即可拿票”。
package data

import (
	"context"
	"slices"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
)

// BattleTicketTarget 是本次 roster 授权读取同一 Redis 快照得到的可路由目标。
// Login reconnect 必须使用这里的 DSAddr，不能在证明当前 projection 后又回退使用可能陈旧的 locator 地址。
type BattleTicketTarget struct {
	DSAddr        string
	PodName       string
	InstanceUID   string
	InstanceEpoch uint32
	// AllocationID 是本局分配 ID(DSTicket v2 allocation_id claim;旧记录可能为空,
	// v2 签发侧对空值 fail-closed 拒签)。
	AllocationID string
	ReleaseTrack string
}

// BattleTicketAuthorizer 是 battle DSTicket 的签发前 player↔match 权威门。
type BattleTicketAuthorizer interface {
	AuthorizeBattleTicket(context.Context, uint64, uint64) (BattleTicketTarget, error)
}

// BattleRouteState 是 Hub 签票门的显式三态判定结果(P0 修复 2026-07-15):
// 通用 ErrPermissionDeny 不得再被当作“对局已终态”的证明——它同时覆盖
// roster 漂移/非成员/记录缺失/stale 心跳,那些都只能是 UNKNOWN。
type BattleRouteState int

const (
	// BattleRouteUnknown:无法权威判定(记录缺失/非成员漂移/stale 心跳/Redis 错误)。
	// 调用方在 locator 阳性 BATTLE 信号下必须 fail-closed。
	BattleRouteUnknown BattleRouteState = iota
	// BattleRouteActive:玩家确属 live 对局(ready/running + 成员 + 心跳新鲜)。
	BattleRouteActive
	// BattleRouteTerminal:权威记录显式终态(ended/abandoned)——唯一允许 Hub 的证明。
	BattleRouteTerminal
)

// BattleRouteInspector 是可选能力接口:Hub 签票门用它区分“仍在活局/已终局/不可判定”。
// 未实现本接口的 authorizer 一律按 UNKNOWN fail-closed。
type BattleRouteInspector interface {
	InspectBattleRoute(ctx context.Context, playerID, matchID uint64) (BattleRouteState, error)
}

// 战斗投影记录的显式终态(与 ds_allocator 状态机常量一致;TerminateExpected 写入)。
const (
	battleStateEnded     = "ended"
	battleStateAbandoned = "abandoned"
)

type RedisBattleTicketAuthorizer struct {
	rdb             redis.UniversalClient
	requireModelB   bool
	now             func() time.Time
	maxHeartbeatAge time.Duration
}

func NewRedisBattleTicketAuthorizer(
	rdb redis.UniversalClient,
	requireModelB bool,
	maxHeartbeatAge time.Duration,
) *RedisBattleTicketAuthorizer {
	if maxHeartbeatAge <= 0 {
		maxHeartbeatAge = 30 * time.Second
	}
	return &RedisBattleTicketAuthorizer{
		rdb: rdb, requireModelB: requireModelB, now: time.Now, maxHeartbeatAge: maxHeartbeatAge,
	}
}

// AuthorizeBattleTicket 的读取时刻是本次签发授权线性化点。Redis 不可判定返回 Unavailable；
// 非成员、空 roster、非 live/stale 或 Model-B 漂移返回 PermissionDeny，绝不签票。
func (c *RedisBattleTicketAuthorizer) AuthorizeBattleTicket(
	ctx context.Context,
	playerID, matchID uint64,
) (BattleTicketTarget, error) {
	if playerID == 0 || matchID == 0 {
		return BattleTicketTarget{}, errcode.New(errcode.ErrInvalidArg, "battle ticket authorization requires player and match")
	}
	if c == nil || c.rdb == nil || c.now == nil {
		return BattleTicketTarget{}, errcode.New(errcode.ErrUnavailable, "battle ticket roster authority unavailable")
	}
	if c.requireModelB {
		return c.authorizeModelB(ctx, playerID, matchID)
	}
	payload, err := c.rdb.Get(ctx, admissionBattleProjectionKey(matchID)).Bytes()
	if err == redis.Nil {
		return BattleTicketTarget{}, errcode.New(errcode.ErrPermissionDeny, "battle ticket target is not live")
	}
	if err != nil {
		return BattleTicketTarget{}, errcode.NewCause(errcode.ErrUnavailable, err, "read battle ticket roster failed")
	}
	battle := &dsv1.BattleStorageRecord{}
	if err := proto.Unmarshal(payload, battle); err != nil {
		return BattleTicketTarget{}, errcode.NewCause(errcode.ErrUnavailable, err, "decode battle ticket roster failed")
	}
	if !c.liveRosterAllows(battle, playerID, matchID) {
		return BattleTicketTarget{}, errcode.New(errcode.ErrPermissionDeny, "player is not authorized for battle ticket target")
	}
	return battleTicketTarget(battle), nil
}

// InspectBattleRoute 是 Hub 签票门的显式三态权威判定(P0 修复 2026-07-15):
//
//	TERMINAL:投影记录存在、match_id 一致且 state ∈ {ended, abandoned}——唯一允许 Hub 的证明。
//	ACTIVE  :liveRosterAllows(ready/running + 成员 + 心跳新鲜)。
//	UNKNOWN :其余一切——记录缺失(redis.Nil,可能是终局清理也可能是 TTL 漂移,不可区分)、
//	         match_id 不匹配、running 但玩家非成员(roster 漂移)、stale 心跳(DS 可能崩溃)、
//	         Redis/解码错误。调用方必须 fail-closed。
//
// 注意与 AuthorizeBattleTicket 的区别:后者把上述 UNKNOWN 情形折叠进 ErrPermissionDeny
// (签票语义"不给票"正确),但作为 Hub 放行证明会把 roster 漂移误判成终局(Codex 复审 P0)。
func (c *RedisBattleTicketAuthorizer) InspectBattleRoute(
	ctx context.Context,
	playerID, matchID uint64,
) (BattleRouteState, error) {
	if playerID == 0 || matchID == 0 {
		return BattleRouteUnknown, errcode.New(errcode.ErrInvalidArg, "battle route inspection requires player and match")
	}
	if c == nil || c.rdb == nil || c.now == nil {
		return BattleRouteUnknown, errcode.New(errcode.ErrUnavailable, "battle route authority unavailable")
	}
	payload, err := c.rdb.Get(ctx, admissionBattleProjectionKey(matchID)).Bytes()
	if err == redis.Nil {
		// 记录缺失 ≠ 终态:可能是终局后清理,也可能是 DS 续期失败导致 TTL 漂移(活局仍在)。
		// 没有版本化 placement lease 前无法区分,一律 UNKNOWN。
		return BattleRouteUnknown, errcode.New(errcode.ErrUnavailable,
			"battle projection missing; cannot prove match %d is terminal", matchID)
	}
	if err != nil {
		return BattleRouteUnknown, errcode.NewCause(errcode.ErrUnavailable, err, "read battle route projection failed")
	}
	battle := &dsv1.BattleStorageRecord{}
	if err := proto.Unmarshal(payload, battle); err != nil {
		return BattleRouteUnknown, errcode.NewCause(errcode.ErrUnavailable, err, "decode battle route projection failed")
	}
	if battle.GetMatchId() != matchID {
		return BattleRouteUnknown, errcode.New(errcode.ErrUnavailable,
			"battle projection match mismatch (want %d got %d)", matchID, battle.GetMatchId())
	}
	if battle.GetState() == battleStateEnded {
		// ended = DS 自己上报的正常终局:DS 已按结算流程收尾,无脑裂窗口,立即放行。
		return BattleRouteTerminal, nil
	}
	if battle.GetState() == battleStateAbandoned {
		// abandoned = 心跳超时判死(补偿性终态),DS 可能只是与后端分区、其上玩家仍可玩。
		// 脑裂再入屏障(2026-07-16,pkg/placement 契约):必须等旧 DS 的授权租约上限 +
		// 偏差余量过去(它届时已对存量玩家自我 fencing),才能把玩家放去 Hub/新局。
		// LastHeartbeatMs==0 = 从未有过成功心跳:DS 从未取得授权租约,其准入门从未打开,
		// 不可能有玩家在其上,立即 Terminal 安全。
		if last := battle.GetLastHeartbeatMs(); last > 0 {
			if wait := last + placement.DSFenceReentryBarrier.Milliseconds() - c.now().UnixMilli(); wait > 0 {
				return BattleRouteUnknown, errcode.New(errcode.ErrUnavailable,
					"abandoned battle %d is inside the DS fence re-entry barrier (%dms left); retry", matchID, wait)
			}
		}
		return BattleRouteTerminal, nil
	}
	if c.liveRosterAllows(battle, playerID, matchID) {
		return BattleRouteActive, nil
	}
	// 非终态且非可证明 live:running 但非成员(漂移)/stale 心跳/warming 等中间态,全部不可判定。
	return BattleRouteUnknown, errcode.New(errcode.ErrUnavailable,
		"battle route not provably terminal (state=%q)", battle.GetState())
}

func (c *RedisBattleTicketAuthorizer) authorizeModelB(
	ctx context.Context,
	playerID, matchID uint64,
) (BattleTicketTarget, error) {
	values, err := c.rdb.MGet(ctx,
		admissionBattleAuthKey(matchID), admissionBattleProjectionKey(matchID)).Result()
	if err != nil {
		return BattleTicketTarget{}, errcode.NewCause(errcode.ErrUnavailable, err, "read battle ticket authority failed")
	}
	if len(values) != 2 || values[0] == nil || values[1] == nil {
		return BattleTicketTarget{}, errcode.New(errcode.ErrPermissionDeny, "battle ticket authority is not active")
	}
	authRaw, err := admissionRedisBytes(values[0])
	if err != nil {
		return BattleTicketTarget{}, errcode.NewCause(errcode.ErrUnavailable, err, "decode battle ticket auth value failed")
	}
	battleRaw, err := admissionRedisBytes(values[1])
	if err != nil {
		return BattleTicketTarget{}, errcode.NewCause(errcode.ErrUnavailable, err, "decode battle ticket projection value failed")
	}
	record := &dsv1.BattleDSAuthStorageRecord{}
	battle := &dsv1.BattleStorageRecord{}
	if err := proto.Unmarshal(authRaw, record); err != nil {
		return BattleTicketTarget{}, errcode.NewCause(errcode.ErrUnavailable, err, "decode battle ticket auth failed")
	}
	if err := proto.Unmarshal(battleRaw, battle); err != nil {
		return BattleTicketTarget{}, errcode.NewCause(errcode.ErrUnavailable, err, "decode battle ticket projection failed")
	}
	if !c.modelBAllows(record, battle, playerID, matchID) {
		return BattleTicketTarget{}, errcode.New(errcode.ErrPermissionDeny, "battle ticket authority or roster is not routable")
	}
	return battleTicketTarget(battle), nil
}

func (c *RedisBattleTicketAuthorizer) liveRosterAllows(
	battle *dsv1.BattleStorageRecord,
	playerID, matchID uint64,
) bool {
	if battle == nil || battle.GetMatchId() != matchID || battle.GetDsPodName() == "" || battle.GetDsAddr() == "" ||
		(battle.GetState() != "ready" && battle.GetState() != "running") ||
		len(battle.GetPlayerIds()) == 0 || !slices.Contains(battle.GetPlayerIds(), playerID) {
		return false
	}
	nowMs := c.now().UnixMilli()
	return ticketHeartbeatFresh(battle.GetLastHeartbeatMs(), nowMs, c.heartbeatAgeLimit())
}

func battleTicketTarget(battle *dsv1.BattleStorageRecord) BattleTicketTarget {
	if battle == nil {
		return BattleTicketTarget{}
	}
	return BattleTicketTarget{
		DSAddr: battle.GetDsAddr(), PodName: battle.GetDsPodName(),
		InstanceUID: battle.GetGameserverUid(), InstanceEpoch: battle.GetInstanceEpoch(),
		AllocationID: battle.GetAllocationId(), ReleaseTrack: battle.GetReleaseTrack(),
	}
}

func (c *RedisBattleTicketAuthorizer) modelBAllows(
	record *dsv1.BattleDSAuthStorageRecord,
	battle *dsv1.BattleStorageRecord,
	playerID, matchID uint64,
) bool {
	if !c.liveRosterAllows(battle, playerID, matchID) || record == nil {
		return false
	}
	active := record.GetActive()
	nowMs := c.now().UnixMilli()
	return active != nil &&
		(record.GetPhase() == dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_ACTIVE ||
			record.GetPhase() == dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_ROTATING) &&
		record.GetMatchId() == matchID && record.GetAllocationId() != "" &&
		record.GetAllocationId() == battle.GetAllocationId() &&
		record.GetInstanceUid() != "" && record.GetInstanceEpoch() > 0 &&
		battle.GetGameserverUid() != "" && battle.GetInstanceEpoch() > 0 &&
		record.GetDsPodName() == battle.GetDsPodName() && record.GetInstanceUid() == battle.GetGameserverUid() &&
		record.GetInstanceEpoch() == battle.GetInstanceEpoch() &&
		record.GetRequiredWriterEpoch() == auth.DSAuthWriterEpochV2 &&
		(record.GetPending() == nil || record.GetPending().GetWriterEpoch() == auth.DSAuthWriterEpochV2) &&
		record.GetHighWaterGen() >= active.GetGen() &&
		ticketHeartbeatFresh(record.GetLastActiveHeartbeatMs(), nowMs, c.heartbeatAgeLimit()) &&
		battle.GetLastHeartbeatMs() == record.GetLastActiveHeartbeatMs() &&
		active.GetInstanceUid() != "" && active.GetInstanceEpoch() > 0 &&
		active.GetInstanceUid() == record.GetInstanceUid() && active.GetInstanceEpoch() == record.GetInstanceEpoch() &&
		active.GetGen() > 0 && active.GetJti() != "" && active.GetExpMs() > uint64(nowMs) &&
		active.GetKid() != "" && active.GetTokenSha256() != "" &&
		active.GetWriterEpoch() == auth.DSAuthWriterEpochV2 &&
		battle.GetLastVerifiedGen() == active.GetGen() && battle.GetLastVerifiedJti() == active.GetJti() &&
		battle.GetLastVerifiedWriterEpoch() == auth.DSAuthWriterEpochV2
}

func (c *RedisBattleTicketAuthorizer) heartbeatAgeLimit() time.Duration {
	if c.maxHeartbeatAge <= 0 {
		return 30 * time.Second
	}
	return c.maxHeartbeatAge
}

func ticketHeartbeatFresh(value, nowMs int64, maxAge time.Duration) bool {
	return value > 0 && value <= nowMs && nowMs-value <= maxAge.Milliseconds()
}
