// Package data 是 ds_allocator 服务的数据层(Redis DS 状态镜像)。
//
// Redis key 模板(所有业务 ID 用 uint64,%d 格式化):
//
//	pandora:ds:battle:{<match_id>}  → BattleStorageRecord proto bytes(hashtag 锁 cluster slot),TTL=BattleTTL
//	pandora:ds:active               → ZSET(score=last_heartbeat_ms,member=match_id),心跳超时扫描
//
// 战斗状态写用 WATCH/MULTI/EXEC 乐观锁,冲突重试耗尽返 ErrDSAllocationFailed。
package data

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
)

// ── key 模板 ─────────────────────────────────────────────────────────────────

const activeKey = "pandora:ds:active"

func battleKey(matchID uint64) string { return fmt.Sprintf("pandora:ds:battle:{%d}", matchID) }

// ── 接口 ──────────────────────────────────────────────────────────────────────

// BattleRepo 是 ds_allocator 数据层抽象。biz 层只依赖此接口,不依赖 redis。
type BattleRepo interface {
	// CreateBattle 写战斗镜像 proto bytes(TTL=battleTTL)并 ZADD 进 active(score=last_heartbeat_ms)。
	CreateBattle(ctx context.Context, battle *dsv1.BattleStorageRecord, battleTTL time.Duration) error
	// GetBattle 读战斗镜像。not found 返 (nil, false, nil)。
	GetBattle(ctx context.Context, matchID uint64) (*dsv1.BattleStorageRecord, bool, error)
	// UpdateBattleWithLock WATCH/MULTI/EXEC 读-改-写;CAS 失败重试 maxRetry 次,耗尽返 ErrDSAllocationFailed。
	// fn 内同步更新 active ZSET score(last_heartbeat_ms)由调用方在 fn 后通过 TouchActive 完成。
	UpdateBattleWithLock(ctx context.Context, matchID uint64, maxRetry int, fn func(*dsv1.BattleStorageRecord) error, battleTTL time.Duration) error
	// TouchActive 刷新 active ZSET 中该 match 的 score(last_heartbeat_ms)。
	TouchActive(ctx context.Context, matchID uint64, lastHeartbeatMs int64) error
	// RemoveActive 把 match 移出 active ZSET(战斗结束/释放,不再心跳扫描)。
	RemoveActive(ctx context.Context, matchID uint64) error
	// DeleteBattle 删战斗镜像 record + 移出 active。
	DeleteBattle(ctx context.Context, matchID uint64) error
	// ExpireBattle 改短 battle key TTL(终态保留供查询)并移出 active。
	ExpireBattle(ctx context.Context, matchID uint64, ttl time.Duration) error
	// RangeStaleBattles 返回 last_heartbeat_ms ≤ thresholdMs 的 match_id(心跳已超时)。
	RangeStaleBattles(ctx context.Context, thresholdMs int64) ([]uint64, error)
	// RangeActiveBattles 返回 active ZSET 中全部 match_id(ListBattles 用)。
	RangeActiveBattles(ctx context.Context) ([]uint64, error)
}

// ── Redis 实现 ────────────────────────────────────────────────────────────────

// RedisBattleRepo 是基于 go-redis/v9 的 BattleRepo 实现。
type RedisBattleRepo struct {
	rdb *redis.Client
}

// NewRedisBattleRepo 构造 RedisBattleRepo。
func NewRedisBattleRepo(rdb *redis.Client) *RedisBattleRepo {
	return &RedisBattleRepo{rdb: rdb}
}

func (r *RedisBattleRepo) CreateBattle(ctx context.Context, battle *dsv1.BattleStorageRecord, battleTTL time.Duration) error {
	payload, err := marshalBattle(battle)
	if err != nil {
		return err
	}
	_, err = r.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Set(ctx, battleKey(battle.MatchId), payload, battleTTL)
		pipe.ZAdd(ctx, activeKey, redis.Z{Score: float64(battle.LastHeartbeatMs), Member: battle.MatchId})
		return nil
	})
	return err
}

func (r *RedisBattleRepo) GetBattle(ctx context.Context, matchID uint64) (*dsv1.BattleStorageRecord, bool, error) {
	b, err := r.rdb.Get(ctx, battleKey(matchID)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rec, err := unmarshalBattle(matchID, b)
	if err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

func (r *RedisBattleRepo) UpdateBattleWithLock(
	ctx context.Context,
	matchID uint64,
	maxRetry int,
	fn func(*dsv1.BattleStorageRecord) error,
	battleTTL time.Duration,
) error {
	key := battleKey(matchID)

	for attempt := 0; attempt <= maxRetry; attempt++ {
		var fnErr error

		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			b, err := tx.Get(ctx, key).Bytes()
			if err == redis.Nil {
				return errcode.New(errcode.ErrDSPodNotFound, "battle %d not found", matchID)
			}
			if err != nil {
				return err
			}
			battle, err := unmarshalBattle(matchID, b)
			if err != nil {
				return err
			}
			if fnErr = fn(battle); fnErr != nil {
				return fnErr
			}
			payload, err := marshalBattle(battle)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, key, payload, battleTTL)
				pipe.ZAdd(ctx, activeKey, redis.Z{Score: float64(battle.LastHeartbeatMs), Member: battle.MatchId})
				return nil
			})
			return err
		}, key)

		if txErr == nil {
			return nil
		}
		if txErr == fnErr && fnErr != nil {
			return fnErr // fn 业务错误,不重试
		}
		if txErr == redis.TxFailedErr {
			continue // CAS 冲突,重试
		}
		return txErr
	}
	return errcode.New(errcode.ErrDSAllocationFailed, "battle %d update concurrent retry exhausted", matchID)
}

func (r *RedisBattleRepo) TouchActive(ctx context.Context, matchID uint64, lastHeartbeatMs int64) error {
	return r.rdb.ZAdd(ctx, activeKey, redis.Z{Score: float64(lastHeartbeatMs), Member: matchID}).Err()
}

func (r *RedisBattleRepo) RemoveActive(ctx context.Context, matchID uint64) error {
	return r.rdb.ZRem(ctx, activeKey, matchID).Err()
}

func (r *RedisBattleRepo) DeleteBattle(ctx context.Context, matchID uint64) error {
	_, err := r.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Del(ctx, battleKey(matchID))
		pipe.ZRem(ctx, activeKey, matchID)
		return nil
	})
	return err
}

func (r *RedisBattleRepo) ExpireBattle(ctx context.Context, matchID uint64, ttl time.Duration) error {
	_, err := r.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Expire(ctx, battleKey(matchID), ttl)
		pipe.ZRem(ctx, activeKey, matchID)
		return nil
	})
	return err
}

func (r *RedisBattleRepo) RangeStaleBattles(ctx context.Context, thresholdMs int64) ([]uint64, error) {
	vals, err := r.rdb.ZRangeByScore(ctx, activeKey, &redis.ZRangeBy{
		Min: "-inf",
		Max: strconv.FormatInt(thresholdMs, 10),
	}).Result()
	if err != nil {
		return nil, err
	}
	return parseIDs(vals)
}

func (r *RedisBattleRepo) RangeActiveBattles(ctx context.Context) ([]uint64, error) {
	vals, err := r.rdb.ZRange(ctx, activeKey, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	return parseIDs(vals)
}

// ── 序列化辅助 ────────────────────────────────────────────────────────────────

func parseIDs(vals []string) ([]uint64, error) {
	out := make([]uint64, 0, len(vals))
	for _, v := range vals {
		id, perr := strconv.ParseUint(v, 10, 64)
		if perr != nil {
			return nil, fmt.Errorf("active bad match_id %q: %w", v, perr)
		}
		out = append(out, id)
	}
	return out, nil
}

func marshalBattle(b *dsv1.BattleStorageRecord) ([]byte, error) {
	if b == nil {
		return nil, fmt.Errorf("nil battle")
	}
	return proto.Marshal(b)
}

func unmarshalBattle(matchID uint64, payload []byte) (*dsv1.BattleStorageRecord, error) {
	rec := &dsv1.BattleStorageRecord{}
	if err := proto.Unmarshal(payload, rec); err != nil {
		return nil, fmt.Errorf("battle %d bad proto: %w", matchID, err)
	}
	if rec.MatchId == 0 {
		rec.MatchId = matchID
	}
	if rec.MatchId != matchID {
		return nil, fmt.Errorf("battle %d id mismatch: %d", matchID, rec.MatchId)
	}
	return rec, nil
}
