// Package data 是 player_locator 服务的数据层(redis-only)。
//
// W3 ⑤(2026-06-05):
//   - Redis hash: pandora:locator:<player_id>
//   - TTL 30s,SetLocation 每次刷新
//   - 不接 MySQL(locator 是临时态,玩家离线 → 30s 后自动消失)
//
// W4 ⑩(2026-06-06):
//   - 覆盖式 Set 升级为 SetGuarded:WATCH/MULTI/EXEC 原子读-判-写,
//     先把当前记录交给 biz guard 决策(不变量 §1 状态机守卫),通过才覆盖写。
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
	ShardID     uint32
	MatchID     uint64
	BattlePod   string
	UpdatedAtMs int64
}

// LocationRepo 玩家位置仓储接口。
type LocationRepo interface {
	// SetGuarded WATCH/MULTI/EXEC 原子读-判-写:先读当前记录交给 guard 决策,
	// guard 返回非 nil 则中止写并原样返回该错误(用于不变量 §1 状态机守卫);
	// guard 通过则 DEL+HSET+EXPIRE 覆盖式写入。CAS 冲突重试 maxRetry 次。
	SetGuarded(ctx context.Context, playerID uint64, rec LocationRecord, ttl time.Duration, maxRetry int, guard func(cur LocationRecord, found bool) error) error
	Get(ctx context.Context, playerID uint64) (LocationRecord, bool, error)
	Delete(ctx context.Context, playerID uint64) error
}

// RedisLocationRepo 基于 go-redis/v9 的实现。
type RedisLocationRepo struct {
	rdb redis.UniversalClient
}

// NewRedisLocationRepo 构造。
func NewRedisLocationRepo(rdb redis.UniversalClient) *RedisLocationRepo {
	return &RedisLocationRepo{rdb: rdb}
}

func locKey(playerID uint64) string {
	return fmt.Sprintf("pandora:locator:%d", playerID)
}

// SetGuarded WATCH/MULTI/EXEC 原子读-判-写。
//
// 流程(每次重试一轮 WATCH):
//  1. WATCH key 并读当前记录
//  2. guard(cur, found):返回非 nil → 中止写,原样返回该错误(业务守卫拒绝,不重试)
//  3. MULTI:DEL + HSET 覆盖 + EXPIRE 刷新 TTL
//
// 先 DEL 再 HSET,保证不同 state 切换时不残留旧字段(BATTLE → HUB 时 match_id 不清除会误读)。
// CAS 冲突(EXEC 期间 key 被并发改)返回 TxFailedErr → 重试;耗尽 maxRetry 返 ErrLocatorConflict。
func (r *RedisLocationRepo) SetGuarded(
	ctx context.Context,
	playerID uint64,
	rec LocationRecord,
	ttl time.Duration,
	maxRetry int,
	guard func(cur LocationRecord, found bool) error,
) error {
	if playerID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "playerID must > 0")
	}
	key := locKey(playerID)
	if rec.UpdatedAtMs == 0 {
		rec.UpdatedAtMs = time.Now().UnixMilli()
	}

	for attempt := 0; attempt <= maxRetry; attempt++ {
		var guardErr error
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			cur, found, err := readLocation(ctx, tx, key)
			if err != nil {
				return err
			}
			if guard != nil {
				if guardErr = guard(cur, found); guardErr != nil {
					return guardErr
				}
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
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
				return nil
			})
			return err
		}, key)

		if txErr == nil {
			return nil
		}
		if guardErr != nil && txErr == guardErr {
			return guardErr // 业务守卫拒绝,不重试
		}
		if txErr == redis.TxFailedErr {
			continue // CAS 冲突,重试
		}
		return errcode.New(errcode.ErrInternal, "redis location set: %v", txErr)
	}
	return errcode.New(errcode.ErrLocatorConflict, "player %d location set concurrent retry exhausted", playerID)
}

// Get 返回 (record, found, err)。key 不存在 → found=false。
func (r *RedisLocationRepo) Get(ctx context.Context, playerID uint64) (LocationRecord, bool, error) {
	if playerID == 0 {
		return LocationRecord{}, false, errcode.New(errcode.ErrInvalidArg, "playerID must > 0")
	}
	rec, found, err := readLocation(ctx, r.rdb, locKey(playerID))
	if err != nil {
		return LocationRecord{}, false, errcode.New(errcode.ErrInternal, "redis location get: %v", err)
	}
	return rec, found, nil
}

// readLocation HGETALL 并解析为 LocationRecord。c 可以是 *redis.Client 或 WATCH 内的 *redis.Tx。
func readLocation(ctx context.Context, c redis.Cmdable, key string) (LocationRecord, bool, error) {
	m, err := c.HGetAll(ctx, key).Result()
	if err != nil {
		return LocationRecord{}, false, err
	}
	if len(m) == 0 {
		return LocationRecord{}, false, nil
	}
	return parseLocationMap(m), true, nil
}

// parseLocationMap 把 redis hash 字段解析成 LocationRecord(容错:解析失败的字段留零值)。
func parseLocationMap(m map[string]string) LocationRecord {
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
		if x, e := strconv.ParseUint(v, 10, 32); e == nil {
			rec.ShardID = uint32(x)
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
	return rec
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
