// ds_admission.go 为 VerifyDSTicket 提供调用 DS 的 Redis active 权威校验。
//
// DSCallback JWT 只证明凭据由控制面签发；本文件在消费玩家 DSTicket jti 之前再次读取
// Redis 唯一授权权威，严格证明调用者当前仍是 Hub/Battle active。所有方法只读 Redis，
// 不修改 auth/projection/TTL/JTI，Redis 故障与 stale 身份分别映射为 Unavailable/Unauthorized。
package data

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/middleware"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
)

// DSAdmissionBinding 是已经由 Redis active 权威证明的调用 DS 身份。
// TicketUsecase 只接受本结构的完整值，并在 MarkUsed 之前与玩家 DSTicket claims 比对。
type DSAdmissionBinding struct {
	DSType        auth.DSType
	MatchID       uint64
	PodName       string
	InstanceUID   string
	ProtocolEpoch uint32
	CredentialGen uint64
	CredentialJTI string
	ExpMs         int64
	Kid           string
	TokenSHA256   string
	WriterEpoch   uint32
	// PlayerIDs 仅 Battle 使用，来自与 auth 同槽读取的 BattleStorageRecord 权威 roster。
	PlayerIDs []uint64
}

// Complete 检查所有 Hub/Battle 共用的 active 身份字段；Battle 还必须有 MatchID，Hub 必须为 0。
func (b DSAdmissionBinding) Complete() bool {
	if b.DSType != auth.DSTypeHub && b.DSType != auth.DSTypeBattle {
		return false
	}
	if b.PodName == "" || b.InstanceUID == "" || b.ProtocolEpoch == 0 || b.CredentialGen == 0 ||
		b.CredentialJTI == "" || b.ExpMs <= 0 || b.Kid == "" || b.TokenSHA256 == "" ||
		b.WriterEpoch != auth.DSAuthWriterEpochV2 {
		return false
	}
	return b.DSType == auth.DSTypeBattle && b.MatchID != 0 && len(b.PlayerIDs) > 0 ||
		b.DSType == auth.DSTypeHub && b.MatchID == 0 && len(b.PlayerIDs) == 0
}

// AdmissionAttemptOwner 返回一次 PreLoginAsync 的稳定 owner 摘要。它刻意不含普通 token
// 轮换会改变的 gen/jti/exp/kid/hash，但包含 DS type/match/pod/UID/instance epoch/writer；
// 因而同实例平滑轮换后可重认，UID 重建/跨对局/旧 writer 永远不能重认。
func (b DSAdmissionBinding) AdmissionAttemptOwner(admissionID string) (string, error) {
	parsedAdmissionID, parseErr := uuid.Parse(admissionID)
	if !b.Complete() || parseErr != nil || parsedAdmissionID == uuid.Nil ||
		parsedAdmissionID.String() != admissionID || parsedAdmissionID.Version() != uuid.Version(4) ||
		parsedAdmissionID.Variant() != uuid.RFC4122 {
		return "", fmt.Errorf("invalid admission attempt owner input")
	}
	payload, err := json.Marshal(struct {
		Version       uint32 `json:"v"`
		AdmissionID   string `json:"admission_id"`
		DSType        string `json:"ds_type"`
		MatchID       uint64 `json:"match_id"`
		PodName       string `json:"pod"`
		InstanceUID   string `json:"uid"`
		ProtocolEpoch uint32 `json:"epoch"`
		WriterEpoch   uint32 `json:"writer_epoch"`
	}{
		Version: 3, AdmissionID: admissionID, DSType: string(b.DSType), MatchID: b.MatchID,
		PodName: b.PodName, InstanceUID: b.InstanceUID, ProtocolEpoch: b.ProtocolEpoch,
		WriterEpoch: b.WriterEpoch,
	})
	if err != nil {
		return "", fmt.Errorf("marshal admission attempt owner: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// AcceptedCredentialHash 是首次成功准入时完整 active tuple 的审计绑定；它不参与
// same-attempt 重认判定，故平滑轮换不会覆盖首次接受凭据，也不会造成响应未知后误 replay。
func (b DSAdmissionBinding) AcceptedCredentialHash() (string, error) {
	if !b.Complete() {
		return "", fmt.Errorf("invalid accepted credential input")
	}
	payload, err := json.Marshal(struct {
		Version       uint32 `json:"v"`
		DSType        string `json:"ds_type"`
		MatchID       uint64 `json:"match_id"`
		PodName       string `json:"pod"`
		InstanceUID   string `json:"uid"`
		ProtocolEpoch uint32 `json:"epoch"`
		CredentialGen uint64 `json:"gen"`
		CredentialJTI string `json:"credential_jti"`
		ExpMs         int64  `json:"exp_ms"`
		Kid           string `json:"kid"`
		TokenSHA256   string `json:"token_sha256"`
		WriterEpoch   uint32 `json:"writer_epoch"`
	}{
		Version: 3, DSType: string(b.DSType), MatchID: b.MatchID, PodName: b.PodName,
		InstanceUID: b.InstanceUID, ProtocolEpoch: b.ProtocolEpoch, CredentialGen: b.CredentialGen,
		CredentialJTI: b.CredentialJTI, ExpMs: b.ExpMs, Kid: b.Kid,
		TokenSHA256: b.TokenSHA256, WriterEpoch: b.WriterEpoch,
	})
	if err != nil {
		return "", fmt.Errorf("marshal accepted credential: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// DSAdmissionChecker 证明一份已验签 DSCallback credential 此刻仍等于 Redis active。
type DSAdmissionChecker interface {
	CheckActive(context.Context, string, *middleware.VerifiedCredential) (DSAdmissionBinding, error)
}

// RedisDSAdmissionChecker 读取 hub auth 或同槽 battle auth+projection。
type RedisDSAdmissionChecker struct {
	rdb                   redis.UniversalClient
	now                   func() time.Time
	maxActiveHeartbeatAge time.Duration
}

// NewRedisDSAdmissionChecker 构造入场调用方 active checker。
func NewRedisDSAdmissionChecker(rdb redis.UniversalClient, maxHeartbeatAge time.Duration) *RedisDSAdmissionChecker {
	if maxHeartbeatAge <= 0 {
		maxHeartbeatAge = 30 * time.Second
	}
	return &RedisDSAdmissionChecker{rdb: rdb, now: time.Now, maxActiveHeartbeatAge: maxHeartbeatAge}
}

func admissionHubAuthKey(pod string) string { return fmt.Sprintf("pandora:hub:auth:{%s}", pod) }
func admissionHubProjectionKey(pod string) string {
	return fmt.Sprintf("pandora:hub:shard:{%s}", pod)
}
func admissionBattleAuthKey(matchID uint64) string {
	return fmt.Sprintf("pandora:ds:auth:{%d}", matchID)
}
func admissionBattleProjectionKey(matchID uint64) string {
	return fmt.Sprintf("pandora:ds:battle:{%d}", matchID)
}

// CheckActive 的 Redis 读取发生在玩家票据解析/JTI SETNX 之前。任何 stale/future/wrong
// tuple 都返回 Unauthorized；Redis/坏 protobuf 使权威不可判定，返回 Unavailable。
func (c *RedisDSAdmissionChecker) CheckActive(
	ctx context.Context,
	pod string,
	credential *middleware.VerifiedCredential,
) (DSAdmissionBinding, error) {
	if pod == "" || credential == nil || credential.Pod != pod || credential.InstanceUID == "" ||
		credential.ProtocolEpoch == 0 || credential.Gen == 0 || credential.JTI == "" || credential.ExpMs <= 0 ||
		credential.Kid == "" || credential.TokenSHA256 == "" || credential.WriterEpoch != auth.DSAuthWriterEpochV2 ||
		(credential.DSType != auth.DSTypeHub && credential.DSType != auth.DSTypeBattle) ||
		(credential.DSType == auth.DSTypeBattle && credential.MatchID == 0) ||
		(credential.DSType == auth.DSTypeHub && credential.MatchID != 0) {
		return DSAdmissionBinding{}, errcode.New(errcode.ErrUnauthorized, "ds admission credential is incomplete or scope mismatched")
	}
	if c == nil || c.rdb == nil || c.now == nil {
		return DSAdmissionBinding{}, errcode.New(errcode.ErrUnavailable, "ds admission authority is unavailable")
	}

	switch credential.DSType {
	case auth.DSTypeHub:
		return c.checkHubActive(ctx, pod, credential)
	case auth.DSTypeBattle:
		return c.checkBattleActive(ctx, pod, credential)
	default:
		return DSAdmissionBinding{}, errcode.New(errcode.ErrUnauthorized, "ds admission type is invalid")
	}
}

func (c *RedisDSAdmissionChecker) checkHubActive(
	ctx context.Context,
	pod string,
	credential *middleware.VerifiedCredential,
) (DSAdmissionBinding, error) {
	values, err := c.rdb.MGet(ctx, admissionHubAuthKey(pod), admissionHubProjectionKey(pod)).Result()
	if err != nil {
		return DSAdmissionBinding{}, errcode.NewCause(errcode.ErrUnavailable, err, "read hub admission authority failed")
	}
	if len(values) != 2 || values[0] == nil || values[1] == nil {
		return DSAdmissionBinding{}, errcode.New(errcode.ErrUnauthorized, "hub admission credential is not active")
	}
	authRaw, err := admissionRedisBytes(values[0])
	if err != nil {
		return DSAdmissionBinding{}, errcode.NewCause(errcode.ErrUnavailable, err, "decode hub admission authority value failed")
	}
	projectionRaw, err := admissionRedisBytes(values[1])
	if err != nil {
		return DSAdmissionBinding{}, errcode.NewCause(errcode.ErrUnavailable, err, "decode hub admission projection value failed")
	}
	record := &hubv1.HubShardAuthStorageRecord{}
	projection := &hubv1.HubShardStorageRecord{}
	if err := proto.Unmarshal(authRaw, record); err != nil {
		return DSAdmissionBinding{}, errcode.NewCause(errcode.ErrUnavailable, err, "decode hub admission authority failed")
	}
	if err := proto.Unmarshal(projectionRaw, projection); err != nil {
		return DSAdmissionBinding{}, errcode.NewCause(errcode.ErrUnavailable, err, "decode hub admission projection failed")
	}
	nowMs, maxAgeMs := c.clock()
	active := record.GetActive()
	if nowMs <= 0 || active == nil ||
		(record.GetPhase() != hubv1.HubAuthPhase_HUB_AUTH_PHASE_ACTIVE &&
			record.GetPhase() != hubv1.HubAuthPhase_HUB_AUTH_PHASE_ROTATING) ||
		record.GetPodName() != pod || record.GetInstanceUid() != credential.InstanceUID ||
		record.GetProtocolEpoch() != credential.ProtocolEpoch ||
		record.GetRequiredWriterEpoch() != auth.DSAuthWriterEpochV2 ||
		(record.GetPending() != nil && record.GetPending().GetWriterEpoch() != auth.DSAuthWriterEpochV2) ||
		record.GetHighWaterGen() < active.GetGen() ||
		record.GetLastActiveHeartbeatMs() <= 0 || record.GetLastActiveHeartbeatMs() > nowMs ||
		nowMs-record.GetLastActiveHeartbeatMs() > maxAgeMs ||
		projection.GetHubPodName() != pod || projection.GetState() != "ready" ||
		projection.GetGameserverUid() != credential.InstanceUID || projection.GetAuthEpoch() != credential.ProtocolEpoch ||
		projection.GetLastVerifiedGen() != credential.Gen || projection.GetLastVerifiedJti() != credential.JTI ||
		projection.GetLastVerifiedWriterEpoch() != auth.DSAuthWriterEpochV2 ||
		projection.GetLastHeartbeatMs() <= 0 || projection.GetLastHeartbeatMs() > nowMs ||
		nowMs-projection.GetLastHeartbeatMs() > maxAgeMs ||
		!hubActiveMatches(active, credential, nowMs) {
		return DSAdmissionBinding{}, errcode.New(errcode.ErrUnauthorized, "hub admission credential does not match active authority")
	}
	return bindingFromCredential(credential), nil
}

func (c *RedisDSAdmissionChecker) checkBattleActive(
	ctx context.Context,
	pod string,
	credential *middleware.VerifiedCredential,
) (DSAdmissionBinding, error) {
	values, err := c.rdb.MGet(ctx,
		admissionBattleAuthKey(credential.MatchID),
		admissionBattleProjectionKey(credential.MatchID),
	).Result()
	if err != nil {
		return DSAdmissionBinding{}, errcode.NewCause(errcode.ErrUnavailable, err, "read battle admission authority failed")
	}
	if len(values) != 2 || values[0] == nil || values[1] == nil {
		return DSAdmissionBinding{}, errcode.New(errcode.ErrUnauthorized, "battle admission credential is not active")
	}
	authRaw, err := admissionRedisBytes(values[0])
	if err != nil {
		return DSAdmissionBinding{}, errcode.NewCause(errcode.ErrUnavailable, err, "decode battle admission authority value failed")
	}
	projectionRaw, err := admissionRedisBytes(values[1])
	if err != nil {
		return DSAdmissionBinding{}, errcode.NewCause(errcode.ErrUnavailable, err, "decode battle admission projection value failed")
	}
	record := &dsv1.BattleDSAuthStorageRecord{}
	projection := &dsv1.BattleStorageRecord{}
	if err := proto.Unmarshal(authRaw, record); err != nil {
		return DSAdmissionBinding{}, errcode.NewCause(errcode.ErrUnavailable, err, "decode battle admission authority failed")
	}
	if err := proto.Unmarshal(projectionRaw, projection); err != nil {
		return DSAdmissionBinding{}, errcode.NewCause(errcode.ErrUnavailable, err, "decode battle admission projection failed")
	}

	nowMs, maxAgeMs := c.clock()
	active := record.GetActive()
	if nowMs <= 0 || active == nil ||
		(record.GetPhase() != dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_ACTIVE &&
			record.GetPhase() != dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_ROTATING) ||
		record.GetMatchId() != credential.MatchID || record.GetDsPodName() != pod || record.GetAllocationId() == "" ||
		record.GetInstanceUid() != credential.InstanceUID || record.GetInstanceEpoch() != credential.ProtocolEpoch ||
		record.GetRequiredWriterEpoch() != auth.DSAuthWriterEpochV2 ||
		(record.GetPending() != nil && record.GetPending().GetWriterEpoch() != auth.DSAuthWriterEpochV2) ||
		record.GetHighWaterGen() < active.GetGen() ||
		record.GetLastActiveHeartbeatMs() <= 0 || record.GetLastActiveHeartbeatMs() > nowMs ||
		nowMs-record.GetLastActiveHeartbeatMs() > maxAgeMs ||
		projection.GetMatchId() != credential.MatchID || projection.GetAllocationId() != record.GetAllocationId() ||
		projection.GetDsPodName() != pod || projection.GetGameserverUid() != credential.InstanceUID ||
		projection.GetInstanceEpoch() != credential.ProtocolEpoch ||
		(projection.GetState() != "ready" && projection.GetState() != "running") ||
		len(projection.GetPlayerIds()) == 0 ||
		projection.GetLastVerifiedGen() != credential.Gen || projection.GetLastVerifiedJti() != credential.JTI ||
		projection.GetLastVerifiedWriterEpoch() != auth.DSAuthWriterEpochV2 ||
		!battleActiveMatches(active, credential, nowMs) {
		return DSAdmissionBinding{}, errcode.New(errcode.ErrUnauthorized, "battle admission credential does not match active authority")
	}
	binding := bindingFromCredential(credential)
	binding.PlayerIDs = append([]uint64(nil), projection.GetPlayerIds()...)
	return binding, nil
}

func (c *RedisDSAdmissionChecker) clock() (int64, int64) {
	nowMs := c.now().UnixMilli()
	maxAge := c.maxActiveHeartbeatAge
	if maxAge <= 0 {
		maxAge = 30 * time.Second
	}
	return nowMs, maxAge.Milliseconds()
}

func hubActiveMatches(active *hubv1.HubDSCredential, credential *middleware.VerifiedCredential, nowMs int64) bool {
	return active.GetInstanceUid() == credential.InstanceUID &&
		active.GetProtocolEpoch() == credential.ProtocolEpoch && active.GetGen() == credential.Gen &&
		active.GetJti() == credential.JTI && active.GetExpMs() == uint64(credential.ExpMs) &&
		active.GetExpMs() > uint64(nowMs) && active.GetKid() != "" && active.GetKid() == credential.Kid &&
		active.GetWriterEpoch() == auth.DSAuthWriterEpochV2 && active.GetWriterEpoch() == credential.WriterEpoch &&
		active.GetTokenSha256() != "" &&
		subtle.ConstantTimeCompare([]byte(active.GetTokenSha256()), []byte(credential.TokenSHA256)) == 1
}

func battleActiveMatches(active *dsv1.BattleDSCredential, credential *middleware.VerifiedCredential, nowMs int64) bool {
	return active.GetInstanceUid() == credential.InstanceUID &&
		active.GetInstanceEpoch() == credential.ProtocolEpoch && active.GetGen() == credential.Gen &&
		active.GetJti() == credential.JTI && active.GetExpMs() == uint64(credential.ExpMs) &&
		active.GetExpMs() > uint64(nowMs) && active.GetKid() != "" && active.GetKid() == credential.Kid &&
		active.GetWriterEpoch() == auth.DSAuthWriterEpochV2 && active.GetWriterEpoch() == credential.WriterEpoch &&
		active.GetTokenSha256() != "" &&
		subtle.ConstantTimeCompare([]byte(active.GetTokenSha256()), []byte(credential.TokenSHA256)) == 1
}

func bindingFromCredential(credential *middleware.VerifiedCredential) DSAdmissionBinding {
	return DSAdmissionBinding{
		DSType: credential.DSType, MatchID: credential.MatchID, PodName: credential.Pod,
		InstanceUID: credential.InstanceUID, ProtocolEpoch: credential.ProtocolEpoch,
		CredentialGen: credential.Gen, CredentialJTI: credential.JTI, ExpMs: credential.ExpMs,
		Kid: credential.Kid, TokenSHA256: credential.TokenSHA256, WriterEpoch: credential.WriterEpoch,
	}
}

func admissionRedisBytes(value any) ([]byte, error) {
	switch typed := value.(type) {
	case string:
		return []byte(typed), nil
	case []byte:
		return typed, nil
	default:
		return nil, fmt.Errorf("unexpected redis value type %T", value)
	}
}
