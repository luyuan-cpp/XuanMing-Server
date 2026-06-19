// Package data 是 push 服务的数据层。
//
// W3 ④(2026-06-05):新增离线消息 Redis ZSET 缓存。
//
//   - key: pandora:push:offline:<player_id>
//   - 类型: ZSET
//   - score: 事件 ts_ms(int64,client 重连用 last_seen_ms 范围查)
//   - member: PushFrame proto bytes(append `:<seq>` 后缀,防止同 ts_ms 多帧互相覆盖)
//   - TTL: 每次 Append 用 TxPipeline 刷新(默认 5min,见 PushConf.OfflineCacheTTL)
//
// 设计要点:
//   - member 后缀 seq 通过本进程原子计数器递增,**不参与反序列化**,Range 时拆掉
//   - 选 ZSET 而不是 LIST:支持按 score 范围查(last_seen_ms),天然时间排序
//   - 选 proto bytes 而不是 JSON:跟在线推送帧格式统一,补推时直接 Send,不需要二次序列化
package data

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	pushv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/push/v1"
)

// OfflineFrame 是从离线缓存读出的一条帧,score 是事件 ts_ms。
type OfflineFrame struct {
	Frame   *pushv1.PushFrame
	ScoreMs int64
}

// OfflineCacheRepo 是离线消息缓存的抽象接口。
type OfflineCacheRepo interface {
	// Append 把一帧 PushFrame 追加到该玩家的离线 ZSET,并刷新 TTL。
	Append(ctx context.Context, playerID uint64, frame *pushv1.PushFrame) error

	// Range 拉取该玩家 score > sinceMs 的离线帧(按时间升序)。
	// sinceMs=0 返回所有未过期帧。
	Range(ctx context.Context, playerID uint64, sinceMs int64) ([]OfflineFrame, error)
}

// RedisOfflineCacheRepo 是基于 go-redis/v9 ZSET 的实现。
type RedisOfflineCacheRepo struct {
	rdb redis.UniversalClient
	ttl time.Duration

	// seq 单进程内自增,跟 score 拼成 member 后缀,
	// 防止同一毫秒多帧 ZAdd 时(score+member 完全相同)被去重塌缩成一条。
	seq atomic.Uint64
}

// NewRedisOfflineCacheRepo 构造,ttl<=0 时 fallback 到 5min(防 cfg 漏配)。
func NewRedisOfflineCacheRepo(rdb redis.UniversalClient, ttl time.Duration) *RedisOfflineCacheRepo {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &RedisOfflineCacheRepo{rdb: rdb, ttl: ttl}
}

// offlineKey 是 push 离线 ZSET 的 key 模板。
func offlineKey(playerID uint64) string {
	return fmt.Sprintf("pandora:push:offline:%d", playerID)
}

// memberSeparator 把 proto bytes 和 seq 后缀分开;选 0x1F(unit separator)
// 这种 protobuf 字段标签从不会出现的字节,避免误拆。
const memberSeparator = byte(0x1F)

// encodeMember 在 proto bytes 后追加 `0x1F<seq>` 防去重塌陷。
func (r *RedisOfflineCacheRepo) encodeMember(payload []byte) []byte {
	seq := r.seq.Add(1)
	suffix := strconv.FormatUint(seq, 10)
	out := make([]byte, 0, len(payload)+1+len(suffix))
	out = append(out, payload...)
	out = append(out, memberSeparator)
	out = append(out, suffix...)
	return out
}

// decodeMember 反解 encodeMember 的产物,返回原始 proto bytes(seq 后缀丢弃)。
func decodeMember(raw []byte) []byte {
	for i := len(raw) - 1; i >= 0; i-- {
		if raw[i] == memberSeparator {
			return raw[:i]
		}
	}
	// 无分隔符 = 老格式或脏数据 → 原样返回,反序列化失败由调用方处理
	return raw
}

// Append 实现 OfflineCacheRepo.Append。
//
// 用 TxPipeline(ZAdd + Expire),保证 ZSET 写入后 TTL 立即刷新。
// frame 为 nil 直接返参数错误。
func (r *RedisOfflineCacheRepo) Append(ctx context.Context, playerID uint64, frame *pushv1.PushFrame) error {
	if frame == nil {
		return errcode.New(errcode.ErrInvalidArg, "nil frame")
	}
	payload, err := proto.Marshal(frame)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "marshal push frame: %v", err)
	}

	key := offlineKey(playerID)
	member := r.encodeMember(payload)

	pipe := r.rdb.TxPipeline()
	pipe.ZAdd(ctx, key, redis.Z{Score: float64(frame.GetTsMs()), Member: member})
	pipe.Expire(ctx, key, r.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return errcode.New(errcode.ErrInternal, "redis offline zadd: %v", err)
	}
	return nil
}

// Range 实现 OfflineCacheRepo.Range。
//
// sinceMs=0 → 拉全部未过期帧;
// sinceMs>0 → 拉 score > sinceMs 的(client 重连时 last_seen_ms 之后的新帧)。
//
// 用 ZRangeByScoreWithScores 一次拉完;假设单玩家离线峰值 <= 几百帧,
// 不分页(若后续监控发现热玩家堆积,再加 Count 分页)。
func (r *RedisOfflineCacheRepo) Range(ctx context.Context, playerID uint64, sinceMs int64) ([]OfflineFrame, error) {
	min := "-inf"
	if sinceMs > 0 {
		// (sinceMs → score > sinceMs(开区间)
		min = "(" + strconv.FormatInt(sinceMs, 10)
	}

	zs, err := r.rdb.ZRangeByScoreWithScores(ctx, offlineKey(playerID), &redis.ZRangeBy{
		Min: min,
		Max: "+inf",
	}).Result()
	// 注意:go-redis v9 的 ZRangeByScoreWithScores 在 key 不存在时返回 ([], nil),
	// 不会返回 redis.Nil(那是 GET / HGET 一类单值命令的语义),所以这里**不需要**判 redis.Nil。
	// W3 ④ 二次修复(Opus 审查 R4):删除原先冗余的 errors.Is(err, redis.Nil) 分支。
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "redis offline zrange: %v", err)
	}
	if len(zs) == 0 {
		return nil, nil
	}

	out := make([]OfflineFrame, 0, len(zs))
	for _, z := range zs {
		raw, ok := z.Member.(string)
		if !ok {
			continue
		}
		payload := decodeMember([]byte(raw))
		var frame pushv1.PushFrame
		if err := proto.Unmarshal(payload, &frame); err != nil {
			// 单帧坏不阻断其它(避免一条脏数据拖死整个补推)
			continue
		}
		out = append(out, OfflineFrame{Frame: &frame, ScoreMs: int64(z.Score)})
	}
	return out, nil
}
