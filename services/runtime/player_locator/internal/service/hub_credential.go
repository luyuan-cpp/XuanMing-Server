// hub_credential.go 实现 player_locator 的 Hub DS active credential 终态门。
//
// JWT 验签只证明“令牌由受信签发方签过”；本文件再读取 Redis 唯一授权权威，证明这份
// (GameServer UID, protocol epoch, gen, jti) 凭据此刻仍是 active。任一失败都在位置/TTL/
// presence 副作用之前返回 fail-closed。
package service

import (
	"context"
	"crypto/subtle"
	"time"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/middleware"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/data"
)

// HubCredentialStateChecker 校验一份已验签 Hub 凭据是否仍为 Redis active。
type HubCredentialStateChecker interface {
	CheckActive(ctx context.Context, pod string, cred *middleware.VerifiedCredential) error
}

type redisHubCredentialStateChecker struct {
	reader                data.HubAuthReader
	now                   func() time.Time
	maxActiveHeartbeatAge time.Duration
}

// NewHubCredentialStateChecker 构造 Redis active credential 终态门。
func NewHubCredentialStateChecker(reader data.HubAuthReader, maxAge ...time.Duration) HubCredentialStateChecker {
	age := 30 * time.Second
	if len(maxAge) > 0 && maxAge[0] > 0 {
		age = maxAge[0]
	}
	return &redisHubCredentialStateChecker{reader: reader, now: time.Now, maxActiveHeartbeatAge: age}
}

func (c *redisHubCredentialStateChecker) CheckActive(ctx context.Context, pod string, cred *middleware.VerifiedCredential) error {
	// Model B 下 legacy/不完整凭据绝不回退放行。JWT exp 虽已由 verifier 校验，这里仍将
	// claim exp 与 Redis active.exp_ms 精确绑定，避免 annotation/外部数字参与授权。
	if pod == "" || cred == nil || cred.Pod != pod || cred.InstanceUID == "" ||
		cred.ProtocolEpoch == 0 || cred.Gen == 0 || cred.JTI == "" || cred.ExpMs <= 0 ||
		cred.TokenSHA256 == "" || cred.Kid == "" || cred.WriterEpoch != auth.DSAuthWriterEpochV2 {
		return errcode.New(errcode.ErrUnauthorized, "hub credential is incomplete or scope mismatched")
	}
	if c == nil || c.reader == nil || c.now == nil {
		return errcode.New(errcode.ErrUnavailable, "hub credential authority is unavailable")
	}

	rec, found, err := c.reader.GetHubAuth(ctx, pod)
	if err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err, "hub credential authority read failed")
	}
	if !found || rec == nil {
		return errcode.New(errcode.ErrUnauthorized, "hub credential is not active")
	}

	// ROTATING 表示 active+pending 并存；旧 active 在 pending 被激活前仍是权威，必须继续
	// 可用以保证零停机。其余 phase 都没有可用于普通写 RPC 的 active 权限。
	if rec.GetPhase() != hubv1.HubAuthPhase_HUB_AUTH_PHASE_ACTIVE &&
		rec.GetPhase() != hubv1.HubAuthPhase_HUB_AUTH_PHASE_ROTATING {
		return errcode.New(errcode.ErrUnauthorized, "hub credential phase is not active")
	}
	active := rec.GetActive()
	if active == nil {
		return errcode.New(errcode.ErrUnauthorized, "hub credential active record is missing")
	}

	nowMs := c.now().UnixMilli()
	if nowMs <= 0 || cred.ExpMs <= nowMs || active.GetExpMs() == 0 || uint64(nowMs) >= active.GetExpMs() {
		return errcode.New(errcode.ErrUnauthorized, "hub credential has expired")
	}
	lastHeartbeatMs := rec.GetLastActiveHeartbeatMs()
	maxHeartbeatAge := c.maxActiveHeartbeatAge
	if maxHeartbeatAge <= 0 {
		maxHeartbeatAge = 30 * time.Second
	}
	if lastHeartbeatMs <= 0 || lastHeartbeatMs > nowMs ||
		nowMs-lastHeartbeatMs > maxHeartbeatAge.Milliseconds() {
		return errcode.New(errcode.ErrUnauthorized, "hub credential active heartbeat is not fresh")
	}

	// record 顶层实例身份、active 内嵌身份和 JWT claims 三者必须完全一致。high-water
	// 小于 active.gen 表示权威记录自身不完整/回退，同样 fail-closed。
	if rec.GetPodName() != pod ||
		rec.GetInstanceUid() == "" || rec.GetInstanceUid() != cred.InstanceUID ||
		rec.GetProtocolEpoch() == 0 || rec.GetProtocolEpoch() != cred.ProtocolEpoch ||
		rec.GetRequiredWriterEpoch() != auth.DSAuthWriterEpochV2 ||
		(rec.GetPending() != nil && rec.GetPending().GetWriterEpoch() != auth.DSAuthWriterEpochV2) ||
		active.GetInstanceUid() == "" || active.GetInstanceUid() != cred.InstanceUID ||
		active.GetProtocolEpoch() == 0 || active.GetProtocolEpoch() != cred.ProtocolEpoch ||
		active.GetGen() == 0 || active.GetGen() != cred.Gen ||
		active.GetJti() == "" || active.GetJti() != cred.JTI ||
		active.GetExpMs() != uint64(cred.ExpMs) ||
		active.GetKid() == "" || active.GetKid() != cred.Kid || active.GetTokenSha256() == "" ||
		active.GetWriterEpoch() != auth.DSAuthWriterEpochV2 || active.GetWriterEpoch() != cred.WriterEpoch ||
		rec.GetHighWaterGen() < active.GetGen() ||
		subtle.ConstantTimeCompare([]byte(active.GetTokenSha256()), []byte(cred.TokenSHA256)) != 1 {
		return errcode.New(errcode.ErrUnauthorized, "hub credential does not match active authority")
	}
	return nil
}
