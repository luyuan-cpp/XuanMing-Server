// Package data 是 hub_allocator 服务的数据层(Redis 分片镜像 + 玩家归属)。
//
// Redis key 模板:
//
//	pandora:hub:shard:{<hub_pod_name>}  → HubShardStorageRecord proto bytes(hashtag 锁 slot),TTL=ShardTTL
//	pandora:hub:shards                  → SET(成员=hub_pod_name),ListHubs / 候选分片遍历
//	pandora:hub:active                  → ZSET(score=last_heartbeat_ms,member=hub_pod_name),心跳超时扫描
//	pandora:hub:player:<player_id>      → HubAssignmentStorageRecord proto bytes(不变量 §1 一人一 hub),TTL=AssignmentTTL
//	pandora:hub:team:<team_id>          → string(hub_pod_name),队友同分片提示,TTL=AssignmentTTL
//
// 分片 player_count 写用 WATCH/MULTI/EXEC 乐观锁,冲突重试耗尽返 ErrHubNoAvailable。
package data

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
)

// ── key 模板 ─────────────────────────────────────────────────────────────────

const (
	shardsSetKey = "pandora:hub:shards"
	activeKey    = "pandora:hub:active"
)

func shardKey(pod string) string      { return fmt.Sprintf("pandora:hub:shard:{%s}", pod) }
func assignKey(playerID uint64) string { return fmt.Sprintf("pandora:hub:player:%d", playerID) }
func teamKey(teamID uint64) string     { return fmt.Sprintf("pandora:hub:team:%d", teamID) }

// ── 接口 ──────────────────────────────────────────────────────────────────────

// HubRepo 是 hub_allocator 数据层抽象。biz 层只依赖此接口,不依赖 redis。
type HubRepo interface {
	// GetShard 读分片镜像。not found 返 (nil, false, nil)。
	GetShard(ctx context.Context, pod string) (*hubv1.HubShardStorageRecord, bool, error)
	// ListShards 列出全部已登记分片(ListHubs / 候选遍历用)。
	ListShards(ctx context.Context) ([]*hubv1.HubShardStorageRecord, error)
	// CreateShard 写分片镜像(TTL=shardTTL)并加入 shards SET(不进 active,等首次 Heartbeat)。
	CreateShard(ctx context.Context, rec *hubv1.HubShardStorageRecord, shardTTL time.Duration) error
	// UpdateShardWithLock WATCH/MULTI/EXEC 读-改-写分片;CAS 失败重试 maxRetry 次,耗尽返 ErrHubNoAvailable。
	UpdateShardWithLock(ctx context.Context, pod string, maxRetry int, fn func(*hubv1.HubShardStorageRecord) error, shardTTL time.Duration) error
	// HeartbeatShard Hub DS 心跳上报:仅刷新已存在分片(player_count/state/last_heartbeat_ms)并 ZADD active。
	// 分片不存在(孤儿 DS)返 (false, nil),由 biz 下发 stop 指令。HeartbeatRequest 不含 addr/region,
	// 故不在心跳路径建档(分片拓扑由 Fleet provider 登记)。
	HeartbeatShard(ctx context.Context, pod string, playerCount int32, state string, tsMs int64, shardTTL time.Duration) (bool, error)
	// RemoveShard 删分片镜像 + 移出 shards SET + 移出 active ZSET。
	RemoveShard(ctx context.Context, pod string) error
	// RangeStaleShards 返回 active ZSET 中 last_heartbeat_ms ≤ thresholdMs(且 >0)的 pod(心跳超时)。
	RangeStaleShards(ctx context.Context, thresholdMs int64) ([]string, error)
	// RemoveActive 把 pod 移出 active ZSET(不再心跳扫描)。
	RemoveActive(ctx context.Context, pod string) error

	// GetAssignment 读玩家归属。not found 返 (nil, false, nil)。
	GetAssignment(ctx context.Context, playerID uint64) (*hubv1.HubAssignmentStorageRecord, bool, error)
	// SetAssignment 写玩家归属(TTL=assignmentTTL)。
	SetAssignment(ctx context.Context, rec *hubv1.HubAssignmentStorageRecord, assignmentTTL time.Duration) error
	// DeleteAssignment 删玩家归属。
	DeleteAssignment(ctx context.Context, playerID uint64) error

	// GetTeamShard 读队伍同分片提示。not found 返 ("", false, nil)。
	GetTeamShard(ctx context.Context, teamID uint64) (string, bool, error)
	// SetTeamShard 写队伍同分片提示(TTL=assignmentTTL)。
	SetTeamShard(ctx context.Context, teamID uint64, pod string, assignmentTTL time.Duration) error
}

// ── Redis 实现 ────────────────────────────────────────────────────────────────

// RedisHubRepo 是基于 go-redis/v9 的 HubRepo 实现。
type RedisHubRepo struct {
	rdb *redis.Client
}

// NewRedisHubRepo 构造 RedisHubRepo。
func NewRedisHubRepo(rdb *redis.Client) *RedisHubRepo {
	return &RedisHubRepo{rdb: rdb}
}

func (r *RedisHubRepo) GetShard(ctx context.Context, pod string) (*hubv1.HubShardStorageRecord, bool, error) {
	b, err := r.rdb.Get(ctx, shardKey(pod)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rec, err := unmarshalShard(pod, b)
	if err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

func (r *RedisHubRepo) ListShards(ctx context.Context) ([]*hubv1.HubShardStorageRecord, error) {
	pods, err := r.rdb.SMembers(ctx, shardsSetKey).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*hubv1.HubShardStorageRecord, 0, len(pods))
	for _, pod := range pods {
		rec, found, gerr := r.GetShard(ctx, pod)
		if gerr != nil {
			return nil, gerr
		}
		if !found {
			// 镜像已过期但 SET 残留 → 顺手清理
			_ = r.rdb.SRem(ctx, shardsSetKey, pod).Err()
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

func (r *RedisHubRepo) CreateShard(ctx context.Context, rec *hubv1.HubShardStorageRecord, shardTTL time.Duration) error {
	payload, err := marshalShard(rec)
	if err != nil {
		return err
	}
	_, err = r.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Set(ctx, shardKey(rec.HubPodName), payload, shardTTL)
		pipe.SAdd(ctx, shardsSetKey, rec.HubPodName)
		return nil
	})
	return err
}

func (r *RedisHubRepo) UpdateShardWithLock(
	ctx context.Context,
	pod string,
	maxRetry int,
	fn func(*hubv1.HubShardStorageRecord) error,
	shardTTL time.Duration,
) error {
	key := shardKey(pod)

	for attempt := 0; attempt <= maxRetry; attempt++ {
		var fnErr error

		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			b, err := tx.Get(ctx, key).Bytes()
			if err == redis.Nil {
				return errcode.New(errcode.ErrHubNoAvailable, "hub shard %s not found", pod)
			}
			if err != nil {
				return err
			}
			rec, err := unmarshalShard(pod, b)
			if err != nil {
				return err
			}
			if fnErr = fn(rec); fnErr != nil {
				return fnErr
			}
			payload, err := marshalShard(rec)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, key, payload, shardTTL)
				pipe.SAdd(ctx, shardsSetKey, pod)
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
	return errcode.New(errcode.ErrHubNoAvailable, "hub shard %s update concurrent retry exhausted", pod)
}

func (r *RedisHubRepo) HeartbeatShard(ctx context.Context, pod string, playerCount int32, state string, tsMs int64, shardTTL time.Duration) (bool, error) {
	key := shardKey(pod)
	found := false
	err := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
		b, gerr := tx.Get(ctx, key).Bytes()
		if gerr == redis.Nil {
			found = false
			return nil // 孤儿 DS:不建档,由 biz 回 stop
		}
		if gerr != nil {
			return gerr
		}
		rec, uerr := unmarshalShard(pod, b)
		if uerr != nil {
			return uerr
		}
		found = true
		// Hub DS 上报为准:对账在线数 / 状态 / 心跳时刻
		rec.PlayerCount = playerCount
		if state != "" {
			rec.State = state
		}
		rec.LastHeartbeatMs = tsMs
		payload, merr := marshalShard(rec)
		if merr != nil {
			return merr
		}
		_, perr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Set(ctx, key, payload, shardTTL)
			pipe.SAdd(ctx, shardsSetKey, pod)
			pipe.ZAdd(ctx, activeKey, redis.Z{Score: float64(rec.LastHeartbeatMs), Member: pod})
			return nil
		})
		return perr
	}, key)
	if err != nil {
		return false, err
	}
	return found, nil
}

func (r *RedisHubRepo) RemoveShard(ctx context.Context, pod string) error {
	_, err := r.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Del(ctx, shardKey(pod))
		pipe.SRem(ctx, shardsSetKey, pod)
		pipe.ZRem(ctx, activeKey, pod)
		return nil
	})
	return err
}

func (r *RedisHubRepo) RangeStaleShards(ctx context.Context, thresholdMs int64) ([]string, error) {
	// Min "(0" 排除从未心跳的 Mock 种子(score=0);Max=threshold 含等于。
	return r.rdb.ZRangeByScore(ctx, activeKey, &redis.ZRangeBy{
		Min: "(0",
		Max: strconv.FormatInt(thresholdMs, 10),
	}).Result()
}

func (r *RedisHubRepo) RemoveActive(ctx context.Context, pod string) error {
	return r.rdb.ZRem(ctx, activeKey, pod).Err()
}

func (r *RedisHubRepo) GetAssignment(ctx context.Context, playerID uint64) (*hubv1.HubAssignmentStorageRecord, bool, error) {
	b, err := r.rdb.Get(ctx, assignKey(playerID)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rec := &hubv1.HubAssignmentStorageRecord{}
	if uerr := proto.Unmarshal(b, rec); uerr != nil {
		return nil, false, fmt.Errorf("assignment %d bad proto: %w", playerID, uerr)
	}
	return rec, true, nil
}

func (r *RedisHubRepo) SetAssignment(ctx context.Context, rec *hubv1.HubAssignmentStorageRecord, assignmentTTL time.Duration) error {
	payload, err := proto.Marshal(rec)
	if err != nil {
		return err
	}
	return r.rdb.Set(ctx, assignKey(rec.PlayerId), payload, assignmentTTL).Err()
}

func (r *RedisHubRepo) DeleteAssignment(ctx context.Context, playerID uint64) error {
	return r.rdb.Del(ctx, assignKey(playerID)).Err()
}

func (r *RedisHubRepo) GetTeamShard(ctx context.Context, teamID uint64) (string, bool, error) {
	pod, err := r.rdb.Get(ctx, teamKey(teamID)).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return pod, true, nil
}

func (r *RedisHubRepo) SetTeamShard(ctx context.Context, teamID uint64, pod string, assignmentTTL time.Duration) error {
	return r.rdb.Set(ctx, teamKey(teamID), pod, assignmentTTL).Err()
}

// ── 序列化辅助 ────────────────────────────────────────────────────────────────

func marshalShard(rec *hubv1.HubShardStorageRecord) ([]byte, error) {
	if rec == nil {
		return nil, fmt.Errorf("nil hub shard")
	}
	return proto.Marshal(rec)
}

func unmarshalShard(pod string, payload []byte) (*hubv1.HubShardStorageRecord, error) {
	rec := &hubv1.HubShardStorageRecord{}
	if err := proto.Unmarshal(payload, rec); err != nil {
		return nil, fmt.Errorf("hub shard %s bad proto: %w", pod, err)
	}
	if rec.HubPodName == "" {
		rec.HubPodName = pod
	}
	if rec.HubPodName != pod {
		return nil, fmt.Errorf("hub shard %s pod mismatch: %s", pod, rec.HubPodName)
	}
	return rec, nil
}
