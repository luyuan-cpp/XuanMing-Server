// Package data 是 player_locator 服务的数据层(redis-only)。
//
// W3 ⑤(2026-06-05):
//   - Redis hash: pandora:locator:<player_id>
//   - TTL 30s,SetLocation 每次刷新
//   - 不接 MySQL(locator 是临时态,玩家离线 → 30s 后自动消失)
package data

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// LocationRecord 是写入 / 读出 redis 的中间结构(避免 data 层依赖 proto)。
//
// state 用 int32 保存(直接对应 pandora.locator.v1.LocationState 枚举值),
// service 层负责跟 proto enum 互转。
type LocationRecord struct {
	State       int32
	HubPod      string
	ShardID     int32
	MatchID     uint64
	BattlePod   string
	UpdatedAtMs int64
}

// LocationRepo 玩家位置仓储接口。
type LocationRepo interface {
	Set(ctx context.Context, playerID uint64, rec LocationRecord, ttl time.Duration) error
	Get(ctx context.Context, playerID uint64) (LocationRecord, bool, error)
	Delete(ctx context.Context, playerID uint64) error
}

// RedisLocationRepo 基于 go-redis/v9 的实现。
type RedisLocationRepo struct {
	rdb *redis.Client
}

// NewRedisLocationRepo 构造。
func NewRedisLocationRepo(rdb *redis.Client) *RedisLocationRepo {
	return &RedisLocationRepo{rdb: rdb}
}

func locKey(playerID uint64) string {
	return fmt.Sprintf("pandora:locator:%d", playerID)
}

// Set HSET 覆盖式写入 + EXPIRE 刷新 TTL,用 pipeline 单 RT。
func (r *RedisLocationRepo) Set(ctx context.Context, playerID uint64, rec LocationRecord, ttl time.Duration) error {
	if playerID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "playerID must > 0")
	}
	key := locKey(playerID)
	if rec.UpdatedAtMs == 0 {
		rec.UpdatedAtMs = time.Now().UnixMilli()
	}
	pipe := r.rdb.TxPipeline()
	// 先 DEL 再 HSET,保证不同 state 切换时不残留旧字段(BATTLE → HUB 时 match_id 不清除会误读)
	pipe.Del(ctx, key)
	pipe.HSet(ctx, key,
		"state", rec.State,
		"hub_pod", rec.HubPod,
		"shard_id", rec.ShardID,
		"match_id", rec.MatchID,
		"battle_pod", rec.BattlePod,
		"updated_at_ms", rec.UpdatedAtMs,
	)
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return errcode.New(errcode.ErrInternal, "redis location set: %v", err)
	}
	return nil
}

// Get 返回 (record, found, err)。key 不存在 → found=false。
func (r *RedisLocationRepo) Get(ctx context.Context, playerID uint64) (LocationRecord, bool, error) {
	if playerID == 0 {
		return LocationRecord{}, false, errcode.New(errcode.ErrInvalidArg, "playerID must > 0")
	}
	m, err := r.rdb.HGetAll(ctx, locKey(playerID)).Result()
	if err != nil {
		return LocationRecord{}, false, errcode.New(errcode.ErrInternal, "redis location get: %v", err)
	}
	if len(m) == 0 {
		return LocationRecord{}, false, nil
	}
	rec := LocationRecord{
		HubPod:    m["hub_pod"],
		BattlePod: m["battle_pod"],
	}
	if v, ok := m["state"]; ok {
		if x, e := strconv.ParseInt(v, 10, 32); e == nil {
			rec.State = int32(x)
		}
	}
	if v, ok := m["shard_id"]; ok {
		if x, e := strconv.ParseInt(v, 10, 32); e == nil {
			rec.ShardID = int32(x)
		}
	}
	if v, ok := m["match_id"]; ok {
		if x, e := strconv.ParseUint(v, 10, 64); e == nil {
			rec.MatchID = x
		}
	}
	if v, ok := m["updated_at_ms"]; ok {
		if x, e := strconv.ParseInt(v, 10, 64); e == nil {
			rec.UpdatedAtMs = x
		}
	}
	return rec, true, nil
}

// Delete UNLINK(异步删,避免大 key 阻塞);TTL 已经在 set 时挂了,Delete 失败不致命。
func (r *RedisLocationRepo) Delete(ctx context.Context, playerID uint64) error {
	if playerID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "playerID must > 0")
	}
	if err := r.rdb.Unlink(ctx, locKey(playerID)).Err(); err != nil && !errors.Is(err, redis.Nil) {
		return errcode.New(errcode.ErrInternal, "redis location del: %v", err)
	}
	return nil
}
