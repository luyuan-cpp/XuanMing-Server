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
	// SET 刷新 battle key TTL=battleTTL(心跳 / 正常状态更新用,续命活对局)。
	//
	// ⚠️ fn 重跑契约(UpdateBattleKeepTTL 同):CAS 冲突(并发副本改了同一 key)时 fn 会**基于重新
	// GET 的最新镜像整体重跑**,故 fn 必须无副作用——只准改 *b 和写调用方捕获的出参变量,且出参
	// 必须在 fn 开头重置(以最后一次成功事务为准,失败轮次的残值会被覆盖)。由此,「读到旧状态 X
	// 才置位」的出参标记天然具备跨副本恰好一次语义:状态迁移 X→Y 全局只有一个 EXEC 能成功,
	// 输家重跑后读到 Y 不再置位(sweepOnce 的 firstAbandon 防 double-release 即靠这一条)。
	UpdateBattleWithLock(ctx context.Context, matchID uint64, maxRetry int, fn func(*dsv1.BattleStorageRecord) error, battleTTL time.Duration) error
	// UpdateBattleKeepTTL 同 UpdateBattleWithLock,但 SET 用 redis.KeepTTL 保留 battle key 原 TTL **不刷新**。
	// sweep abandoned 标记 + 补偿重试路径专用:保证 BattleTTL(从最后一次心跳起算)是补偿重试的天然上界,
	// Kafka 长期不可用时镜像最终过期 → GetBattle miss → 清理 active,不会因每轮重试无限刷 TTL / 无限堆积。
	UpdateBattleKeepTTL(ctx context.Context, matchID uint64, maxRetry int, fn func(*dsv1.BattleStorageRecord) error) error
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
	rdb redis.UniversalClient
}

// NewRedisBattleRepo 构造 RedisBattleRepo。
func NewRedisBattleRepo(rdb redis.UniversalClient) *RedisBattleRepo {
	return &RedisBattleRepo{rdb: rdb}
}

// CreateBattle 写战斗镜像(权威)并登记到全局 active ZSET。
// Redis Cluster 兼容(同 hub decision-revisit-hub-crossslot.md):battleKey{match} 与全局
// activeKey 分属不同 slot,不能捆同一事务(否则 CROSSSLOT)。① battleKey 单键 SET 权威落库;
// ② activeKey 独立 ZADD 登记(必须成功,否则心跳扫描漏这个对局)。两步幂等,失败重试可重入。
func (r *RedisBattleRepo) CreateBattle(ctx context.Context, battle *dsv1.BattleStorageRecord, battleTTL time.Duration) error {
	payload, err := marshalBattle(battle)
	if err != nil {
		return err
	}
	if err := r.rdb.Set(ctx, battleKey(battle.MatchId), payload, battleTTL).Err(); err != nil {
		return err
	}
	return r.rdb.ZAdd(ctx, activeKey, redis.Z{Score: float64(battle.LastHeartbeatMs), Member: battle.MatchId}).Err()
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
	return r.updateWithLock(ctx, matchID, maxRetry, fn, battleTTL)
}

// UpdateBattleKeepTTL 同 UpdateBattleWithLock,但 SET 用 redis.KeepTTL(-1)保留 battle key 原 TTL 不刷新。
func (r *RedisBattleRepo) UpdateBattleKeepTTL(
	ctx context.Context,
	matchID uint64,
	maxRetry int,
	fn func(*dsv1.BattleStorageRecord) error,
) error {
	return r.updateWithLock(ctx, matchID, maxRetry, fn, redis.KeepTTL)
}

// updateWithLock 是 UpdateBattleWithLock / UpdateBattleKeepTTL 的共享实现。
// expiration 传 battleTTL 则刷新 TTL;传 redis.KeepTTL 则保留原 TTL 不刷新(补偿重试天然上界靠此)。
func (r *RedisBattleRepo) updateWithLock(
	ctx context.Context,
	matchID uint64,
	maxRetry int,
	fn func(*dsv1.BattleStorageRecord) error,
	expiration time.Duration,
) error {
	key := battleKey(matchID)

	for attempt := 0; attempt <= maxRetry; attempt++ {
		var fnErr error
		var lastHeartbeatMs int64

		// Cluster 兼容:WATCH/SET 只围 battleKey 单 slot(权威镜像);全局 activeKey 移出事务,
		// 事务成功后独立 ZADD(不同 slot)。
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
			lastHeartbeatMs = battle.LastHeartbeatMs
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, key, payload, expiration)
				return nil
			})
			return err
		}, key)

		if txErr == nil {
			// active 索引:与 battleKey 不同 slot,独立 ZADD 刷新 score(last_heartbeat_ms)。
			// 幂等;失败下一轮心跳/sweep 即补,不影响权威镜像。
			return r.rdb.ZAdd(ctx, activeKey, redis.Z{Score: float64(lastHeartbeatMs), Member: matchID}).Err()
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

// DeleteBattle 删战斗镜像 record + 移出 active ZSET。
// Cluster 兼容:battleKey 与 activeKey 不同 slot,拆为独立命令。均幂等;若 ZRem 失败残留 active,
// 由 sweep / ListBattles 扫到镜像已删时跳过并补清(自愈)。
func (r *RedisBattleRepo) DeleteBattle(ctx context.Context, matchID uint64) error {
	if err := r.rdb.Del(ctx, battleKey(matchID)).Err(); err != nil {
		return err
	}
	return r.rdb.ZRem(ctx, activeKey, matchID).Err()
}

// ExpireBattle 改短 battle key TTL(终态保留供查询)并移出 active。
// Cluster 兼容:battleKey 与 activeKey 不同 slot,拆为独立命令。
func (r *RedisBattleRepo) ExpireBattle(ctx context.Context, matchID uint64, ttl time.Duration) error {
	if err := r.rdb.Expire(ctx, battleKey(matchID), ttl).Err(); err != nil {
		return err
	}
	return r.rdb.ZRem(ctx, activeKey, matchID).Err()
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
