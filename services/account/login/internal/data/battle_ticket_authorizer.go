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
