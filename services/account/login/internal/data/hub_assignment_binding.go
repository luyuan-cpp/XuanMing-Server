// hub_assignment_binding.go 校验 Hub DSTicket 是否仍绑定 Redis 中的当前玩家归属。
//
// Redis `pandora:hub:player:<player_id>` 是玩家当前 Hub 归属的终态镜像。login 在消费
// DSTicket jti 前读取并严格比对完整实例/凭据/归属版本，避免旧 Pod、旧凭据或旧 assignment
// 的票据在同名 GameServer 重建、轮换或 Transfer 后继续被接受。
package data

import (
	"bytes"
	"context"
	"crypto/subtle"
	"fmt"
	"time"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

// HubAssignmentBinding 是 Hub DSTicket 签名内必须完整携带的当前归属身份。
type HubAssignmentBinding struct {
	PodName       string
	InstanceUID   string
	ProtocolEpoch uint32
	CredentialGen uint64
	CredentialJTI string
	AssignmentID  string
	WriterEpoch   uint32
	// 以下三项仅 StableHubAssignmentChecker 的 activeExpected 使用；票据/assignment
	// 本身不存这些字段，因此 Complete 不把它们列为通用必填。
	ExpMs       int64
	Kid         string
	TokenSHA256 string
}

// Complete 仅在所有安全绑定字段均非零时返回 true。
func (b HubAssignmentBinding) Complete() bool {
	return b.PodName != "" && b.InstanceUID != "" && b.ProtocolEpoch > 0 &&
		b.CredentialGen > 0 && b.CredentialJTI != "" && b.AssignmentID != "" && b.WriterEpoch > 0
}

// Empty 仅表示旧格式票据完全没有归属绑定；任何半绑定都不是兼容旧票，必须拒绝。
func (b HubAssignmentBinding) Empty() bool {
	return b.PodName == "" && b.InstanceUID == "" && b.ProtocolEpoch == 0 &&
		b.CredentialGen == 0 && b.CredentialJTI == "" && b.AssignmentID == "" && b.WriterEpoch == 0
}

// HubAssignmentChecker 校验一张已验签 Hub DSTicket 是否仍属于 Redis 当前归属。
type HubAssignmentChecker interface {
	CheckCurrent(ctx context.Context, playerID uint64, expected HubAssignmentBinding) error
}

// StableHubAssignmentChecker 用于已存在同 admission attempt marker 的响应未知重认。
// stable 钉 ticket/assignment 的 pod+UID+epoch+assignment_id；active 是本次已由 Redis
// caller checker 证明的当前凭据。普通 gen/jti 轮换允许二者不同，但 assignment A1/A2
// 稳定且 auth+shard 必须精确等于 active，UID/epoch/assignment 变化仍拒绝。
type StableHubAssignmentChecker interface {
	CheckCurrentStable(ctx context.Context, playerID uint64, stable, active HubAssignmentBinding) error
}

// AdmissionHubAssignmentChecker 是在线准入专用门。strictAssignmentCredential=true
// 用于首次 marker：assignment 与 active 的 gen/jti 也必须一致；false 用于同 attempt
// 短窗重认：assignment gen/jti 可滞后普通轮换。两种都精确比较当前 caller 的 exp/kid/hash。
type AdmissionHubAssignmentChecker interface {
	CheckCurrentAdmission(ctx context.Context, playerID uint64, stable, active HubAssignmentBinding, strictAssignmentCredential bool) error
}

// RedisHubAssignmentChecker 从 hub_allocator 的玩家归属 key 读取 protobuf 快照。
type RedisHubAssignmentChecker struct {
	rdb             hubBindingRedis
	now             func() time.Time
	maxHeartbeatAge time.Duration
}

type hubBindingRedis interface {
	Get(context.Context, string) *redis.StringCmd
	MGet(context.Context, ...string) *redis.SliceCmd
}

// NewRedisHubAssignmentChecker 构造 Redis 当前归属校验器。
func NewRedisHubAssignmentChecker(rdb redis.UniversalClient) *RedisHubAssignmentChecker {
	return &RedisHubAssignmentChecker{rdb: rdb, now: time.Now, maxHeartbeatAge: 30 * time.Second}
}

func hubPlayerAssignmentKey(playerID uint64) string {
	return fmt.Sprintf("pandora:hub:player:%d", playerID)
}

func hubAuthAuthorityKey(pod string) string { return fmt.Sprintf("pandora:hub:auth:{%s}", pod) }

// CheckCurrent 的线性化点是 Redis GET 返回当前 assignment 快照的时刻。
//
// missing/字段不一致是无效旧票；Redis 故障或坏 protobuf 是授权权威不可判定，必须
// fail-closed 返回 Unavailable，不能把基础设施错误伪装成可重试同一旧票的鉴权成功。
func (c *RedisHubAssignmentChecker) CheckCurrent(ctx context.Context, playerID uint64, expected HubAssignmentBinding) error {
	return c.checkCurrent(ctx, playerID, expected, expected, true, false)
}

func (c *RedisHubAssignmentChecker) CheckCurrentStable(
	ctx context.Context,
	playerID uint64,
	stable, activeExpected HubAssignmentBinding,
) error {
	return c.checkCurrent(ctx, playerID, stable, activeExpected, false, true)
}

func (c *RedisHubAssignmentChecker) CheckCurrentAdmission(
	ctx context.Context,
	playerID uint64,
	stable, activeExpected HubAssignmentBinding,
	strictAssignmentCredential bool,
) error {
	return c.checkCurrent(ctx, playerID, stable, activeExpected, strictAssignmentCredential, true)
}

func (c *RedisHubAssignmentChecker) checkCurrent(
	ctx context.Context,
	playerID uint64,
	stableExpected, activeExpected HubAssignmentBinding,
	strictAssignmentCredential, requireFullActive bool,
) error {
	if playerID == 0 || !stableExpected.Complete() || !activeExpected.Complete() ||
		stableExpected.WriterEpoch != auth.DSAuthWriterEpochV2 || activeExpected.WriterEpoch != auth.DSAuthWriterEpochV2 ||
		stableExpected.PodName != activeExpected.PodName || stableExpected.InstanceUID != activeExpected.InstanceUID ||
		stableExpected.ProtocolEpoch != activeExpected.ProtocolEpoch || stableExpected.AssignmentID != activeExpected.AssignmentID {
		return errcode.New(errcode.ErrLoginTicketInvalid, "hub ticket assignment binding incomplete")
	}
	if strictAssignmentCredential &&
		(stableExpected.CredentialGen != activeExpected.CredentialGen || stableExpected.CredentialJTI != activeExpected.CredentialJTI) {
		return errcode.New(errcode.ErrLoginTicketInvalid, "hub first admission credential changed")
	}
	if requireFullActive &&
		(activeExpected.ExpMs <= 0 || activeExpected.Kid == "" || activeExpected.TokenSHA256 == "") {
		return errcode.New(errcode.ErrLoginTicketInvalid, "hub current active credential binding incomplete")
	}
	if c == nil || c.rdb == nil {
		return errcode.New(errcode.ErrUnavailable, "hub assignment authority unavailable")
	}

	assignmentKey := hubPlayerAssignmentKey(playerID)
	payload, err := c.rdb.Get(ctx, assignmentKey).Bytes()
	if err == redis.Nil {
		return errcode.New(errcode.ErrLoginTicketInvalid, "hub assignment not found")
	}
	if err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err, "read hub assignment authority failed")
	}

	rec := &hubv1.HubAssignmentStorageRecord{}
	if err := proto.Unmarshal(payload, rec); err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err, "decode hub assignment authority failed")
	}

	if rec.GetPlayerId() != playerID ||
		rec.GetAssignmentId() != stableExpected.AssignmentID ||
		rec.GetHubPodName() != stableExpected.PodName ||
		rec.GetHubInstanceUid() != stableExpected.InstanceUID ||
		rec.GetAuthEpoch() != stableExpected.ProtocolEpoch ||
		rec.GetAuthWriterEpoch() != stableExpected.WriterEpoch ||
		(strictAssignmentCredential && (rec.GetAuthGen() != stableExpected.CredentialGen ||
			rec.GetAuthJti() != stableExpected.CredentialJTI)) {
		return errcode.New(errcode.ErrLoginTicketInvalid, "hub ticket no longer matches current assignment")
	}

	// assignment 与 {pod} 槽的 auth+shard 不同 slot，不能用 MULTI 跨 slot。双采集给出
	// 等价线性化证明：A1 → MGET(auth,shard) → A2 且 A1==A2；入场不能只看 auth，
	// 已 draining/stopping 或投影漂移的 shard 即使 assignment 尚未迁移也必须拒绝。
	values, err := c.rdb.MGet(ctx, hubAuthAuthorityKey(activeExpected.PodName), admissionHubProjectionKey(activeExpected.PodName)).Result()
	if err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err, "read hub active authority/projection failed")
	}
	if len(values) != 2 || values[0] == nil || values[1] == nil {
		return errcode.New(errcode.ErrLoginTicketInvalid, "hub active authority or projection not found")
	}
	authPayload, err := admissionRedisBytes(values[0])
	if err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err, "decode hub active authority value failed")
	}
	shardPayload, err := admissionRedisBytes(values[1])
	if err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err, "decode hub active projection value failed")
	}
	authRec := &hubv1.HubShardAuthStorageRecord{}
	if err := proto.Unmarshal(authPayload, authRec); err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err, "decode hub active credential failed")
	}
	shardRec := &hubv1.HubShardStorageRecord{}
	if err := proto.Unmarshal(shardPayload, shardRec); err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err, "decode hub active projection failed")
	}
	payload2, err := c.rdb.Get(ctx, assignmentKey).Bytes()
	if err == redis.Nil {
		return errcode.New(errcode.ErrLoginTicketInvalid, "hub assignment changed during validation")
	}
	if err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err, "re-read hub assignment authority failed")
	}
	if !bytes.Equal(payload, payload2) {
		return errcode.New(errcode.ErrLoginTicketInvalid, "hub assignment changed during validation")
	}
	active := authRec.GetActive()
	nowFn := c.now
	if nowFn == nil {
		nowFn = time.Now
	}
	nowMs := nowFn().UnixMilli()
	maxAge := c.maxHeartbeatAge
	if maxAge <= 0 {
		maxAge = 30 * time.Second
	}
	if nowMs <= 0 || activeExpected.WriterEpoch != auth.DSAuthWriterEpochV2 ||
		authRec.GetPhase() != hubv1.HubAuthPhase_HUB_AUTH_PHASE_ACTIVE && authRec.GetPhase() != hubv1.HubAuthPhase_HUB_AUTH_PHASE_ROTATING ||
		active == nil || authRec.GetPodName() != activeExpected.PodName || authRec.GetInstanceUid() != activeExpected.InstanceUID ||
		authRec.GetProtocolEpoch() != activeExpected.ProtocolEpoch || active.GetInstanceUid() != activeExpected.InstanceUID ||
		active.GetProtocolEpoch() != activeExpected.ProtocolEpoch || active.GetGen() != activeExpected.CredentialGen ||
		active.GetJti() != activeExpected.CredentialJTI || active.GetWriterEpoch() != auth.DSAuthWriterEpochV2 ||
		active.GetWriterEpoch() != activeExpected.WriterEpoch ||
		authRec.GetRequiredWriterEpoch() != auth.DSAuthWriterEpochV2 ||
		(authRec.GetPending() != nil && authRec.GetPending().GetWriterEpoch() != auth.DSAuthWriterEpochV2) ||
		authRec.GetHighWaterGen() < active.GetGen() || active.GetExpMs() <= uint64(nowMs) || active.GetKid() == "" ||
		active.GetTokenSha256() == "" || authRec.GetLastActiveHeartbeatMs() <= 0 || authRec.GetLastActiveHeartbeatMs() > nowMs ||
		nowMs-authRec.GetLastActiveHeartbeatMs() > maxAge.Milliseconds() ||
		shardRec.GetHubPodName() != activeExpected.PodName || shardRec.GetState() != "ready" ||
		shardRec.GetGameserverUid() != activeExpected.InstanceUID || shardRec.GetAuthEpoch() != activeExpected.ProtocolEpoch ||
		shardRec.GetLastVerifiedGen() != activeExpected.CredentialGen || shardRec.GetLastVerifiedJti() != activeExpected.CredentialJTI ||
		shardRec.GetLastVerifiedWriterEpoch() != auth.DSAuthWriterEpochV2 ||
		shardRec.GetLastHeartbeatMs() <= 0 || shardRec.GetLastHeartbeatMs() > nowMs ||
		nowMs-shardRec.GetLastHeartbeatMs() > maxAge.Milliseconds() {
		return errcode.New(errcode.ErrLoginTicketInvalid, "hub assignment credential is no longer active")
	}
	if requireFullActive &&
		(active.GetExpMs() != uint64(activeExpected.ExpMs) || active.GetKid() != activeExpected.Kid ||
			subtle.ConstantTimeCompare([]byte(active.GetTokenSha256()), []byte(activeExpected.TokenSHA256)) != 1) {
		return errcode.New(errcode.ErrLoginTicketInvalid, "hub current active credential full tuple changed")
	}
	return nil
}
