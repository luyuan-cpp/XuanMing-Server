// battle_auth.go 提供 Battle DS Redis 授权权威的同槽快照与结算 receipt 写入。
package data

import (
	"context"
	"crypto/subtle"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/dsauthrecord"
	"github.com/luyuancpp/pandora/pkg/errcode"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
)

const battleResultReceiptCASRetries = 4

// BattleResultCredential 是已验签 JWT 的完整、不可降级身份。
type BattleResultCredential struct {
	MatchID       uint64
	PodName       string
	InstanceUID   string
	InstanceEpoch uint32
	Gen           uint64
	JTI           string
	ExpMs         int64
	Kid           string
	TokenSHA256   string
	WriterEpoch   uint32
}

// BattleAuthReader 在任何结算副作用前读取同槽 auth+battle 的单命令快照。
type BattleAuthReader interface {
	GetBattleAuthority(context.Context, uint64) (*dsv1.BattleDSAuthStorageRecord, *dsv1.BattleStorageRecord, bool, error)
}

// BattleResultRecorder 在 MySQL 幂等落库后写 result receipt；ended 心跳只消费该凭据。
type BattleResultRecorder interface {
	RecordBattleResult(context.Context, BattleResultCredential, time.Duration) error
}

// RedisBattleAuthReader 与 ds_allocator 共用 Redis 唯一授权记录。
type RedisBattleAuthReader struct {
	rdb redis.UniversalClient
	now func() time.Time
}

func NewRedisBattleAuthReader(rdb redis.UniversalClient) *RedisBattleAuthReader {
	return &RedisBattleAuthReader{rdb: rdb, now: time.Now}
}

func battleAuthKey(matchID uint64) string { return fmt.Sprintf("pandora:ds:auth:{%d}", matchID) }
func battleKey(matchID uint64) string     { return fmt.Sprintf("pandora:ds:battle:{%d}", matchID) }

func (r *RedisBattleAuthReader) GetBattleAuthority(
	ctx context.Context,
	matchID uint64,
) (*dsv1.BattleDSAuthStorageRecord, *dsv1.BattleStorageRecord, bool, error) {
	if r == nil || r.rdb == nil {
		return nil, nil, false, fmt.Errorf("battle auth redis reader is not initialized")
	}
	if matchID == 0 {
		return nil, nil, false, fmt.Errorf("battle auth match id is zero")
	}
	values, err := r.rdb.MGet(ctx, battleAuthKey(matchID), battleKey(matchID)).Result()
	if err != nil {
		return nil, nil, false, fmt.Errorf("get battle authority %d: %w", matchID, err)
	}
	if len(values) != 2 || values[0] == nil || values[1] == nil {
		return nil, nil, false, nil
	}
	authRaw, err := redisValueBytes(values[0])
	if err != nil {
		return nil, nil, false, fmt.Errorf("battle auth %d: %w", matchID, err)
	}
	battleRaw, err := redisValueBytes(values[1])
	if err != nil {
		return nil, nil, false, fmt.Errorf("battle projection %d: %w", matchID, err)
	}
	authRecord := &dsv1.BattleDSAuthStorageRecord{}
	if err := proto.Unmarshal(authRaw, authRecord); err != nil {
		return nil, nil, false, fmt.Errorf("unmarshal battle auth %d: %w", matchID, err)
	}
	battleRecord := &dsv1.BattleStorageRecord{}
	if err := proto.Unmarshal(battleRaw, battleRecord); err != nil {
		return nil, nil, false, fmt.Errorf("unmarshal battle projection %d: %w", matchID, err)
	}
	return authRecord, battleRecord, true, nil
}

// RecordBattleResult 的 SET 是“结算已权威接收”的线性化点。它在同一 WATCH 中重新核验
// active tuple 与 battle 投影；并发轮换、终止、UID 重建或 allocation 漂移都会零写入失败。
func (r *RedisBattleAuthReader) RecordBattleResult(
	ctx context.Context,
	credential BattleResultCredential,
	maxHeartbeatAge time.Duration,
) error {
	if r == nil || r.rdb == nil || r.now == nil {
		return errcode.New(errcode.ErrUnavailable, "battle result receipt authority unavailable")
	}
	if maxHeartbeatAge <= 0 {
		maxHeartbeatAge = 30 * time.Second
	}
	nowMs := r.now().UnixMilli()
	if !validResultCredential(credential, nowMs) {
		return errcode.New(errcode.ErrUnauthorized, "battle result credential incomplete or expired")
	}
	aKey := battleAuthKey(credential.MatchID)
	bKey := battleKey(credential.MatchID)
	rKey := dsauthrecord.BattleResultReceiptKey(credential.MatchID)
	for attempt := 0; attempt < battleResultReceiptCASRetries; attempt++ {
		var bizErr error
		err := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			nowMs = r.now().UnixMilli()
			values, getErr := tx.MGet(ctx, aKey, bKey, rKey).Result()
			if getErr != nil {
				return getErr
			}
			if len(values) != 3 || values[0] == nil || values[1] == nil {
				bizErr = errcode.New(errcode.ErrUnauthorized, "battle authority missing")
				return bizErr
			}
			authRaw, convErr := redisValueBytes(values[0])
			if convErr != nil {
				return convErr
			}
			battleRaw, convErr := redisValueBytes(values[1])
			if convErr != nil {
				return convErr
			}
			authRecord := &dsv1.BattleDSAuthStorageRecord{}
			battleRecord := &dsv1.BattleStorageRecord{}
			if unmarshalErr := proto.Unmarshal(authRaw, authRecord); unmarshalErr != nil {
				return unmarshalErr
			}
			if unmarshalErr := proto.Unmarshal(battleRaw, battleRecord); unmarshalErr != nil {
				return unmarshalErr
			}
			if !resultAuthorityMatches(authRecord, battleRecord, credential, nowMs, maxHeartbeatAge) {
				bizErr = errcode.New(errcode.ErrUnauthorized, "battle authority changed before result receipt")
				return bizErr
			}
			receipt := dsauthrecord.NewBattleResultReceipt(
				credential.MatchID, authRecord.GetAllocationId(), credential.PodName,
				credential.InstanceUID, credential.InstanceEpoch, credential.Gen, credential.JTI,
				credential.ExpMs, credential.Kid, credential.TokenSHA256, credential.WriterEpoch, nowMs)
			battleTTL, ttlErr := tx.PTTL(ctx, bKey).Result()
			if ttlErr != nil || battleTTL <= 0 {
				return errcode.NewCause(errcode.ErrUnavailable, ttlErr,
					"battle projection has no bounded receipt retention")
			}
			if values[2] != nil {
				existingRaw, existingErr := redisValueBytes(values[2])
				if existingErr != nil {
					return existingErr
				}
				existing, existingErr := dsauthrecord.UnmarshalBattleResultReceipt(existingRaw)
				if existingErr != nil || !existing.Valid(nowMs) || !existing.SameCredential(receipt) {
					bizErr = errcode.New(errcode.ErrUnauthorized, "battle result receipt belongs to another credential")
					return bizErr
				}
				// 必须执行 EXEC 才能让 WATCH 对上述 authority 快照生效；同时把旧 receipt
				// 的保留期对齐当前 battle 生命周期，不能被旧 token exp 提前截断。
				_, pipeErr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
					pipe.PExpire(ctx, rKey, battleTTL)
					return nil
				})
				return pipeErr
			}
			payload, marshalErr := dsauthrecord.MarshalBattleResultReceipt(receipt)
			if marshalErr != nil {
				return marshalErr
			}
			_, pipeErr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, rKey, payload, battleTTL)
				return nil
			})
			return pipeErr
		}, aKey, bKey, rKey)
		if err == nil {
			return nil
		}
		if bizErr != nil {
			return bizErr
		}
		if err != redis.TxFailedErr {
			return errcode.NewCause(errcode.ErrUnavailable, err, "write battle result receipt failed")
		}
	}
	return errcode.New(errcode.ErrUnavailable, "battle result receipt concurrent retry exhausted")
}

func resultAuthorityMatches(
	a *dsv1.BattleDSAuthStorageRecord,
	b *dsv1.BattleStorageRecord,
	c BattleResultCredential,
	nowMs int64,
	maxAge time.Duration,
) bool {
	active := a.GetActive()
	return (a.GetPhase() == dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_ACTIVE ||
		a.GetPhase() == dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_ROTATING) &&
		a.GetMatchId() == c.MatchID && b.GetMatchId() == c.MatchID &&
		a.GetAllocationId() != "" && a.GetAllocationId() == b.GetAllocationId() &&
		a.GetDsPodName() == c.PodName && b.GetDsPodName() == c.PodName &&
		a.GetInstanceUid() == c.InstanceUID && b.GetGameserverUid() == c.InstanceUID &&
		a.GetInstanceEpoch() == c.InstanceEpoch && b.GetInstanceEpoch() == c.InstanceEpoch &&
		(b.GetState() == "ready" || b.GetState() == "running") &&
		a.GetLastActiveHeartbeatMs() > 0 && a.GetLastActiveHeartbeatMs() <= nowMs &&
		nowMs-a.GetLastActiveHeartbeatMs() <= maxAge.Milliseconds() &&
		active.GetGen() == c.Gen && active.GetJti() == c.JTI && active.GetExpMs() == uint64(c.ExpMs) &&
		active.GetKid() == c.Kid && active.GetInstanceUid() == c.InstanceUID &&
		active.GetInstanceEpoch() == c.InstanceEpoch && active.GetWriterEpoch() == auth.DSAuthWriterEpochV2 &&
		active.GetWriterEpoch() == c.WriterEpoch &&
		active.GetTokenSha256() != "" && subtle.ConstantTimeCompare([]byte(active.GetTokenSha256()), []byte(c.TokenSHA256)) == 1 &&
		b.GetLastVerifiedGen() == c.Gen && b.GetLastVerifiedJti() == c.JTI &&
		b.GetLastVerifiedWriterEpoch() == c.WriterEpoch &&
		a.GetRequiredWriterEpoch() == auth.DSAuthWriterEpochV2 &&
		(a.GetPending() == nil || a.GetPending().GetWriterEpoch() == auth.DSAuthWriterEpochV2) &&
		a.GetHighWaterGen() >= c.Gen
}

func validResultCredential(c BattleResultCredential, nowMs int64) bool {
	return c.MatchID != 0 && c.PodName != "" && c.InstanceUID != "" && c.InstanceEpoch != 0 &&
		c.Gen != 0 && c.JTI != "" && c.ExpMs > nowMs && c.Kid != "" &&
		c.TokenSHA256 != "" && c.WriterEpoch == auth.DSAuthWriterEpochV2
}

func redisValueBytes(value any) ([]byte, error) {
	switch typed := value.(type) {
	case string:
		return []byte(typed), nil
	case []byte:
		return typed, nil
	default:
		return nil, fmt.Errorf("unexpected redis value type %T", value)
	}
}
