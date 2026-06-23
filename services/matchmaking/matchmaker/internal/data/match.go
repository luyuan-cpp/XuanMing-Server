// Package data 是 matchmaker 服务的数据层。
//
// Redis key 模板(所有业务 ID 用 uint64,%d 格式化):
//
//	pandora:match:queue          → ZSET(score=avg_mmr,member=ticket_id),撮合池
//	pandora:match:ticket:%d      → MatchTicketStorageRecord proto bytes,TTL=TicketTTL
//	pandora:match:{%d}           → MatchStorageRecord proto bytes(hashtag 锁 cluster slot)
//	pandora:match:player:%d      → ticket_id(string,SETNX),落"一人只在一个队列"
//	pandora:match:active         → ZSET(score=confirm_deadline_ms,member=match_id),确认期超时扫描
//
// match 状态写用 WATCH/MULTI/EXEC 乐观锁(同 team 服务),冲突重试耗尽返 ErrMatchConcurrent(4006)。
package data

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"
)

// ── key 模板 ─────────────────────────────────────────────────────────────────

const (
	queueKey  = "pandora:match:queue"
	activeKey = "pandora:match:active"
)

// CreateMatch 中 activeKey ZADD 的有界重试参数:matchKey 已 SET 成功后,ZADD 幂等,
// 用短退避吸收 Redis 瞬时抖动,耗尽才删 matchKey 回滚(见 CreateMatch 注释)。
const (
	createMatchZAddRetry   = 3
	createMatchZAddBackoff = 20 * time.Millisecond
)

func ticketKey(ticketID uint64) string { return fmt.Sprintf("pandora:match:ticket:%d", ticketID) }
func matchKey(matchID uint64) string   { return fmt.Sprintf("pandora:match:{%d}", matchID) }
func playerKey(playerID uint64) string { return fmt.Sprintf("pandora:match:player:%d", playerID) }

// ── 接口 ──────────────────────────────────────────────────────────────────────

// MatchRepo 是 matchmaker 数据层抽象。biz 层只依赖此接口,不依赖 redis。
type MatchRepo interface {
	// ClaimPlayer 用 SETNX 原子声明 player→ticketID 归属,落"一人只在一个队列"。
	// 成功返回 (ticketID, true, nil);玩家已在其他票据返回 (existingTicketID, false, nil)。
	ClaimPlayer(ctx context.Context, playerID, ticketID uint64, ttl time.Duration) (uint64, bool, error)
	// GetPlayerTicket 查玩家当前所在票据 ID。not found 返 (0, false, nil)。
	GetPlayerTicket(ctx context.Context, playerID uint64) (uint64, bool, error)
	// DeletePlayerIndex 删除 player→ticketID 映射。
	DeletePlayerIndex(ctx context.Context, playerID uint64) error

	// AddTicket 写票据 proto bytes(TTL=ticketTTL)并 ZADD 进 queue(score=avg_mmr)。
	AddTicket(ctx context.Context, ticket *matchv1.MatchTicketStorageRecord, ticketTTL time.Duration) error
	// GetTicket 读票据。not found 返 (nil, false, nil)。
	GetTicket(ctx context.Context, ticketID uint64) (*matchv1.MatchTicketStorageRecord, bool, error)
	// ReserveTicket 把票据从 queue 移出并持久化(撮合命中:caller 已写好 ticket.match_id)。
	ReserveTicket(ctx context.Context, ticket *matchv1.MatchTicketStorageRecord, ticketTTL time.Duration) error
	// RequeueTicket 把票据重新写回 queue(确认失败退回,保留 enqueued_at_ms 排队时长)。
	RequeueTicket(ctx context.Context, ticket *matchv1.MatchTicketStorageRecord, ticketTTL time.Duration) error
	// DeleteTicket 删票据 record + 移出 queue。
	DeleteTicket(ctx context.Context, ticketID uint64) error
	// RangeQueueTickets 按 avg_mmr 升序返回 queue 中全部 ticket_id。
	RangeQueueTickets(ctx context.Context) ([]uint64, error)

	// CreateMatch 写 match proto bytes(TTL=matchTTL)并 ZADD 进 active(score=confirm_deadline_ms)。
	CreateMatch(ctx context.Context, match *matchv1.MatchStorageRecord, matchTTL time.Duration) error
	// GetMatch 读 match。not found 返 (nil, false, nil)。
	GetMatch(ctx context.Context, matchID uint64) (*matchv1.MatchStorageRecord, bool, error)
	// UpdateMatchWithLock WATCH/MULTI/EXEC 读-改-写 match;CAS 失败重试 maxRetry 次,耗尽返 ErrMatchConcurrent。
	UpdateMatchWithLock(ctx context.Context, matchID uint64, maxRetry int, fn func(*matchv1.MatchStorageRecord) error, matchTTL time.Duration) error
	// RemoveActive 把 match 移出 active ZSET(确认期结束,不再超时扫描)。
	RemoveActive(ctx context.Context, matchID uint64) error
	// ExpireMatch 改短 match key TTL(终态保留供客户端查询)并移出 active。
	ExpireMatch(ctx context.Context, matchID uint64, ttl time.Duration) error
	// DeleteMatch 硬删 match 镜像并移出 active ZSET(对局结算/废弃后释放撮合状态)。
	DeleteMatch(ctx context.Context, matchID uint64) error
	// RangeExpiredMatches 返回 confirm_deadline_ms ≤ nowMs 的 match_id(确认期已超时)。
	RangeExpiredMatches(ctx context.Context, nowMs int64) ([]uint64, error)
}

// ── Redis 实现 ────────────────────────────────────────────────────────────────

// RedisMatchRepo 是基于 go-redis/v9 的 MatchRepo 实现。
type RedisMatchRepo struct {
	rdb redis.UniversalClient
}

// NewRedisMatchRepo 构造 RedisMatchRepo。
func NewRedisMatchRepo(rdb redis.UniversalClient) *RedisMatchRepo {
	return &RedisMatchRepo{rdb: rdb}
}

// --- player index ---

func (r *RedisMatchRepo) ClaimPlayer(ctx context.Context, playerID, ticketID uint64, ttl time.Duration) (uint64, bool, error) {
	key := playerKey(playerID)
	val := strconv.FormatUint(ticketID, 10)
	for attempt := 0; attempt < 2; attempt++ {
		ok, err := r.rdb.SetNX(ctx, key, val, ttl).Result()
		if err != nil {
			return 0, false, err
		}
		if ok {
			return ticketID, true, nil
		}
		cur, err := r.rdb.Get(ctx, key).Result()
		if err == redis.Nil {
			continue // 占用者刚好过期,重试一次 SETNX
		}
		if err != nil {
			return 0, false, err
		}
		existing, err := strconv.ParseUint(cur, 10, 64)
		if err != nil {
			return 0, false, err
		}
		return existing, false, nil
	}
	return 0, false, errcode.New(errcode.ErrMatchConcurrent, "claim player %d concurrent", playerID)
}

func (r *RedisMatchRepo) DeletePlayerIndex(ctx context.Context, playerID uint64) error {
	return r.rdb.Del(ctx, playerKey(playerID)).Err()
}

func (r *RedisMatchRepo) GetPlayerTicket(ctx context.Context, playerID uint64) (uint64, bool, error) {
	val, err := r.rdb.Get(ctx, playerKey(playerID)).Result()
	if err == redis.Nil {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	ticketID, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return 0, false, err
	}
	return ticketID, true, nil
}

// --- ticket ---

func (r *RedisMatchRepo) AddTicket(ctx context.Context, ticket *matchv1.MatchTicketStorageRecord, ticketTTL time.Duration) error {
	payload, err := marshalTicket(ticket)
	if err != nil {
		return err
	}
	// Cluster 兼容(同 trade decision-revisit-trade-crossslot.md):ticketKey 与全局 queueKey 分属不同 slot,
	// 不能捆同一事务(否则 CROSSSLOT)。① ticketKey 单键 SET 权威落库;② queueKey 独立 ZADD 入池。均幂等。
	if err := r.rdb.Set(ctx, ticketKey(ticket.TicketId), payload, ticketTTL).Err(); err != nil {
		return err
	}
	return r.rdb.ZAdd(ctx, queueKey, redis.Z{Score: float64(ticket.AvgMmr), Member: ticket.TicketId}).Err()
}

func (r *RedisMatchRepo) GetTicket(ctx context.Context, ticketID uint64) (*matchv1.MatchTicketStorageRecord, bool, error) {
	b, err := r.rdb.Get(ctx, ticketKey(ticketID)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rec, err := unmarshalTicket(ticketID, b)
	if err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

func (r *RedisMatchRepo) ReserveTicket(ctx context.Context, ticket *matchv1.MatchTicketStorageRecord, ticketTTL time.Duration) error {
	payload, err := marshalTicket(ticket)
	if err != nil {
		return err
	}
	// Cluster 兼容:① ticketKey 单键 SET 写回带 match_id 的权威状态;② queueKey 独立 ZREM 移出池。
	// 若 ZREM 失败残留队列项,matchOnce 加载时 t.MatchId != 0 会跳过(防重复撞合),不影响正确性。
	if err := r.rdb.Set(ctx, ticketKey(ticket.TicketId), payload, ticketTTL).Err(); err != nil {
		return err
	}
	return r.rdb.ZRem(ctx, queueKey, ticket.TicketId).Err()
}

func (r *RedisMatchRepo) RequeueTicket(ctx context.Context, ticket *matchv1.MatchTicketStorageRecord, ticketTTL time.Duration) error {
	return r.AddTicket(ctx, ticket, ticketTTL)
}

func (r *RedisMatchRepo) DeleteTicket(ctx context.Context, ticketID uint64) error {
	// Cluster 兼容:ticketKey 与 queueKey 不同 slot,拆为独立命令。均幂等;若 ZREM 失败残留队列项,
	// matchOnce 加载时 GetTicket miss 跳过并 best-effort 补清(自愈)。
	if err := r.rdb.Del(ctx, ticketKey(ticketID)).Err(); err != nil {
		return err
	}
	return r.rdb.ZRem(ctx, queueKey, ticketID).Err()
}

func (r *RedisMatchRepo) RangeQueueTickets(ctx context.Context) ([]uint64, error) {
	vals, err := r.rdb.ZRange(ctx, queueKey, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	out := make([]uint64, 0, len(vals))
	for _, v := range vals {
		id, perr := strconv.ParseUint(v, 10, 64)
		if perr != nil {
			return nil, fmt.Errorf("queue bad ticket_id %q: %w", v, perr)
		}
		out = append(out, id)
	}
	return out, nil
}

// --- match ---

func (r *RedisMatchRepo) CreateMatch(ctx context.Context, match *matchv1.MatchStorageRecord, matchTTL time.Duration) error {
	payload, err := marshalMatch(match)
	if err != nil {
		return err
	}
	// Cluster 兼容:matchKey{id} 与全局 activeKey 不同 slot。① matchKey 单键 SET 权威落库;
	// ② activeKey 独立 ZADD 登记确认期超时扫描。
	if err := r.rdb.Set(ctx, matchKey(match.MatchId), payload, matchTTL).Err(); err != nil {
		return err
	}
	// ZADD 幂等,先有界重试吸收 Redis 瞬时抖动(避免一次网络毛刺就让整场撮合走 rollback);
	// 耗尽仍失败才 best-effort 删掉刚写入的 matchKey,让上层 rollbackReservations 后不残留
	// 「match 已建但票据已回队列且不在 active ZSET」的悬空记录(它永远进不了超时扫描 = 死状态)。
	zadd := redis.Z{Score: float64(match.ConfirmDeadlineMs), Member: match.MatchId}
	var zerr error
	for attempt := 0; attempt < createMatchZAddRetry; attempt++ {
		if zerr = r.rdb.ZAdd(ctx, activeKey, zadd).Err(); zerr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			zerr = ctx.Err()
		case <-time.After(createMatchZAddBackoff):
		}
		if ctx.Err() != nil {
			break
		}
	}
	_ = r.rdb.Del(ctx, matchKey(match.MatchId)).Err()
	return zerr
}

func (r *RedisMatchRepo) GetMatch(ctx context.Context, matchID uint64) (*matchv1.MatchStorageRecord, bool, error) {
	b, err := r.rdb.Get(ctx, matchKey(matchID)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rec, err := unmarshalMatch(matchID, b)
	if err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

func (r *RedisMatchRepo) UpdateMatchWithLock(
	ctx context.Context,
	matchID uint64,
	maxRetry int,
	fn func(*matchv1.MatchStorageRecord) error,
	matchTTL time.Duration,
) error {
	key := matchKey(matchID)

	for attempt := 0; attempt <= maxRetry; attempt++ {
		var fnErr error

		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			b, err := tx.Get(ctx, key).Bytes()
			if err == redis.Nil {
				return errcode.New(errcode.ErrMatchNotFound, "match %d not found", matchID)
			}
			if err != nil {
				return err
			}
			match, err := unmarshalMatch(matchID, b)
			if err != nil {
				return err
			}
			if fnErr = fn(match); fnErr != nil {
				return fnErr
			}
			payload, err := marshalMatch(match)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, key, payload, matchTTL)
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
	return errcode.New(errcode.ErrMatchConcurrent, "match %d update concurrent retry exhausted", matchID)
}

func (r *RedisMatchRepo) RemoveActive(ctx context.Context, matchID uint64) error {
	return r.rdb.ZRem(ctx, activeKey, matchID).Err()
}

func (r *RedisMatchRepo) ExpireMatch(ctx context.Context, matchID uint64, ttl time.Duration) error {
	// Cluster 兼容:matchKey 与 activeKey 不同 slot,拆为独立命令。
	if err := r.rdb.Expire(ctx, matchKey(matchID), ttl).Err(); err != nil {
		return err
	}
	return r.rdb.ZRem(ctx, activeKey, matchID).Err()
}

func (r *RedisMatchRepo) DeleteMatch(ctx context.Context, matchID uint64) error {
	// Cluster 兼容:matchKey 与 activeKey 不同 slot,拆为独立命令。均幂等。
	if err := r.rdb.Del(ctx, matchKey(matchID)).Err(); err != nil {
		return err
	}
	return r.rdb.ZRem(ctx, activeKey, matchID).Err()
}

func (r *RedisMatchRepo) RangeExpiredMatches(ctx context.Context, nowMs int64) ([]uint64, error) {
	vals, err := r.rdb.ZRangeByScore(ctx, activeKey, &redis.ZRangeBy{
		Min: "-inf",
		Max: strconv.FormatInt(nowMs, 10),
	}).Result()
	if err != nil {
		return nil, err
	}
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

// ── 序列化辅助 ────────────────────────────────────────────────────────────────

func marshalTicket(t *matchv1.MatchTicketStorageRecord) ([]byte, error) {
	if t == nil {
		return nil, fmt.Errorf("nil ticket")
	}
	return proto.Marshal(t)
}

func unmarshalTicket(ticketID uint64, payload []byte) (*matchv1.MatchTicketStorageRecord, error) {
	rec := &matchv1.MatchTicketStorageRecord{}
	if err := proto.Unmarshal(payload, rec); err != nil {
		return nil, fmt.Errorf("ticket %d bad proto: %w", ticketID, err)
	}
	if rec.TicketId == 0 {
		rec.TicketId = ticketID
	}
	if rec.TicketId != ticketID {
		return nil, fmt.Errorf("ticket %d id mismatch: %d", ticketID, rec.TicketId)
	}
	return rec, nil
}

func marshalMatch(m *matchv1.MatchStorageRecord) ([]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("nil match")
	}
	return proto.Marshal(m)
}

func unmarshalMatch(matchID uint64, payload []byte) (*matchv1.MatchStorageRecord, error) {
	rec := &matchv1.MatchStorageRecord{}
	if err := proto.Unmarshal(payload, rec); err != nil {
		return nil, fmt.Errorf("match %d bad proto: %w", matchID, err)
	}
	if rec.MatchId == 0 {
		rec.MatchId = matchID
	}
	if rec.MatchId != matchID {
		return nil, fmt.Errorf("match %d id mismatch: %d", matchID, rec.MatchId)
	}
	return rec, nil
}
