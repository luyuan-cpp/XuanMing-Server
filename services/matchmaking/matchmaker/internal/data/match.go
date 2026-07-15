// Package data 是 matchmaker 服务的数据层。
//
// Redis key 模板(所有业务 ID 用 uint64,%d 格式化;<mode> = game_mode,空则退回无模式段的旧全局 key):
//
//	pandora:match:<mode>:queue   → ZSET(score=avg_mmr,member=ticket_id),撮合池(按 game_mode 隔离)
//	pandora:match:ticket:%d      → MatchTicketStorageRecord proto bytes,非终态持久;显式终态后按 TicketTTL 留存
//	pandora:match:{%d}           → MatchStorageRecord proto bytes(hashtag 锁 cluster slot)
//	pandora:match:player:%d      → ticket_id(string,SETNX),落"一人只在一个队列"(故意全局不分模式)
//	pandora:match:<mode>:active  → ZSET(score=confirm_deadline_ms,member=match_id),确认期超时扫描
//
// match 状态写用 WATCH/MULTI/EXEC 乐观锁(同 team 服务),冲突重试耗尽返 ErrMatchConcurrent(4006)。
package data

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"
)

// ── key 模板 ─────────────────────────────────────────────────────────────────

// queue / active 两个索引 ZSET 按 game_mode 命名空间化(见
// docs/design/decision-revisit-matchmaker-single-writer.md §3.2):同一 Cell 内多个
// game_mode 的 matchmaker 共享同一套 Redis 时,不能让不同模式的票混进同一 queue / active。
// 空 namespace 保留旧全局 key(单测 / 兼容路径)。
//
//	pandora:match:<mode>:queue    pandora:match:<mode>:active
//	空 namespace → pandora:match:queue    pandora:match:active
//
// 注意:ticketKey / matchKey(记录本体)保持全局——由全局唯一 snowflake ID 定址,不跨模式碰撞;
// playerKey(玩家归属 claim)也保持全局——落实"一人同一时刻只在一个队列(跨所有模式)"(不变量 §1)。
func queueKeyFor(namespace string) string {
	if namespace == "" {
		return "pandora:match:queue"
	}
	return "pandora:match:" + namespace + ":queue"
}

func activeKeyFor(namespace string) string {
	if namespace == "" {
		return "pandora:match:active"
	}
	return "pandora:match:" + namespace + ":active"
}

func startActiveKeyFor(namespace string) string {
	if namespace == "" {
		return "pandora:match:start:active"
	}
	return "pandora:match:" + namespace + ":start:active"
}

// CreateMatch 中 activeKey ZADD 的有界重试参数:matchKey 已 SET 成功后,ZADD 幂等,
// 用短退避吸收 Redis 瞬时抖动,耗尽才删 matchKey 回滚(见 CreateMatch 注释)。
const (
	createMatchZAddRetry   = 3
	createMatchZAddBackoff = 20 * time.Millisecond

	// ticketCASRetry 是 ReserveTicket / DeleteTicketIfUnmatched 的 WATCH CAS 冲突重试上限。
	// 票据键的并发写只有"撮合循环预留"与"玩家取消删除"两方,冲突极短暂,3 次足够。
	ticketCASRetry = 3
)

func ticketKey(ticketID uint64) string { return fmt.Sprintf("pandora:match:ticket:%d", ticketID) }
func matchKey(matchID uint64) string   { return fmt.Sprintf("pandora:match:{%d}", matchID) }
func playerKey(playerID uint64) string { return fmt.Sprintf("pandora:match:player:%d", playerID) }
func startOperationKey(ticketID uint64) string {
	return fmt.Sprintf("pandora:match:start:{%d}", ticketID)
}
func startPlayerKey(playerID uint64) string {
	return fmt.Sprintf("pandora:match:start:player:%d", playerID)
}

// ── 接口 ──────────────────────────────────────────────────────────────────────

// MatchRepo 是 matchmaker 数据层抽象。biz 层只依赖此接口,不依赖 redis。
type MatchRepo interface {
	// ClaimPlayer 用无 TTL 的 SETNX 原子声明 player→ticketID 归属,落"一人只在一个队列"。
	// QUEUED/CONFIRM/ALLOCATING/READY 都是业务状态，只有显式取消/失败/ReleaseMatch
	// 可以 compare-delete；网络断开或进程停机绝不能靠 TTL 暗中释放玩家。
	// 成功返回 (ticketID, true, nil);玩家已在其他票据返回 (existingTicketID, false, nil)。
	ClaimPlayer(ctx context.Context, playerID, ticketID uint64, ttl time.Duration) (uint64, bool, error)
	// GetPlayerTicket 查玩家当前所在票据 ID。not found 返 (0, false, nil)。
	GetPlayerTicket(ctx context.Context, playerID uint64) (uint64, bool, error)
	// DeletePlayerIndexIfMatches 仅当 claim 仍指向 ticketID 时才删除 player→ticketID 映射
	// (Lua 原子比较后 DEL)。僵尸 claim 清理 / 回滚一律用此接口:无条件 DEL 在「读到旧 claim →
	// 旧 claim 自然过期 → 同一玩家新 claim 写入 → 删」的窗口会误删新一局 claim,新票据失去
	// 归属后玩家可再开第二张票(违反不变量 §1)。幂等:值不匹配/已不存在时不动、不报错。
	DeletePlayerIndexIfMatches(ctx context.Context, playerID, ticketID uint64) error
	// RefreshPlayerClaim 是滚动升级兼容门：仅当 claim 仍指向 ticketID 时 PERSIST。
	// 新 claim 本来就无 TTL；旧版本遗留 TTL claim 在 Requeue 时升级为 durable。
	RefreshPlayerClaim(ctx context.Context, playerID, ticketID uint64, ttl time.Duration) error
	// PersistPlayerClaim removes the cache TTL only if the claim still points at
	// this ticket. Reserved/READY matches are business state, not expiring presence.
	PersistPlayerClaim(ctx context.Context, playerID, ticketID uint64) error

	// AddTicket 持久写票据 proto bytes(无 TTL)并 ZADD 进 queue(score=avg_mmr)。
	// 仅供 Requeue / 测试造数据;StartMatch 入队路径必须走 CreateTicketRecord → claims →
	// EnqueueTicket 三步写序(见 CreateTicketRecord 注释),不准直接 AddTicket。
	AddTicket(ctx context.Context, ticket *matchv1.MatchTicketStorageRecord, ticketTTL time.Duration) error
	// CreateTicketRecord 只写票据主体(SET,不入 queue)。
	//
	// 写序铁律(镜像 team CreateTeam 的结论):StartMatch 必须**先写票据主体、再 ClaimPlayer、
	// 最后 EnqueueTicket 入队**。claimPlayer 的僵尸自愈以「票据主体不存在」为判据 —— 若先 claim
	// 后写主体,并发的另一次 StartMatch 会把 in-flight claim 误判僵尸并 CAS 删掉,同批玩家
	// 两张票同时入队(违反不变量 §1)。主体先落地时 ticketID 尚未入队、无人引用,天然安全。
	CreateTicketRecord(ctx context.Context, ticket *matchv1.MatchTicketStorageRecord, ticketTTL time.Duration) error
	// EnqueueTicket 把已写好主体的票据 ZADD 进 queue(score=avg_mmr),撮合循环自此可见。
	EnqueueTicket(ctx context.Context, ticket *matchv1.MatchTicketStorageRecord) error
	// GetTicket 读票据。not found 返 (nil, false, nil)。
	GetTicket(ctx context.Context, ticketID uint64) (*matchv1.MatchTicketStorageRecord, bool, error)
	// ReserveTicket 把票据从 queue 移出并持久化(撮合命中:caller 已写好 ticket.match_id)。
	// WATCH CAS:票据已被 CancelMatch 并发删除 → 返 ErrMatchNotFound;已被并发预留进其他
	// match(leader 交接重叠等)→ 返 ErrMatchConcurrent。调用方按失败回滚本组预留。
	ReserveTicket(ctx context.Context, ticket *matchv1.MatchTicketStorageRecord, ticketTTL time.Duration) error
	// RequeueTicket 把票据重新写回 queue(确认失败退回,保留 enqueued_at_ms 排队时长)。
	RequeueTicket(ctx context.Context, ticket *matchv1.MatchTicketStorageRecord, ticketTTL time.Duration) error
	// DeleteTicket 删票据 record + 移出 queue。
	DeleteTicket(ctx context.Context, ticketID uint64) error
	// DeleteTicketIfUnmatched 仅当票据仍未被撮合(match_id==0)时原子删除(WATCH CAS)并移出 queue。
	// 防 CancelMatch"读到未撮合→删票"与撮合循环 ReserveTicket 之间的竞态窗口。
	// 返回:(true,0) 已删除;(false,matchID) 已被撮合进 match;(false,0) 票据已不存在。
	DeleteTicketIfUnmatched(ctx context.Context, ticketID uint64) (deleted bool, matchID uint64, err error)
	// RangeQueueTickets 按 avg_mmr 升序返回 queue 中全部 ticket_id。
	RangeQueueTickets(ctx context.Context) ([]uint64, error)

	// CreateStartOperation 持久化 StartMatch saga 后登记派生 due 索引。operation record 是权威；
	// 索引写失败可由跨 Redis Cluster 全 master 的 reconciler 重建。
	CreateStartOperation(ctx context.Context, op *matchv1.MatchStartOperationStorageRecord, ttl time.Duration) error
	GetStartOperation(ctx context.Context, ticketID uint64) (*matchv1.MatchStartOperationStorageRecord, bool, error)
	UpdateStartOperationWithLock(ctx context.Context, ticketID uint64, maxRetry int, fn func(*matchv1.MatchStartOperationStorageRecord) error, ttl time.Duration) error
	EnsureStartActive(ctx context.Context, ticketID uint64, scoreMs int64) error
	RangeDueStartOperations(ctx context.Context, nowMs int64) ([]uint64, error)
	RemoveStartActive(ctx context.Context, ticketID uint64) error
	DeleteStartOperation(ctx context.Context, ticketID uint64) error
	ScanStartOperationIDs(ctx context.Context, count int64) ([]uint64, error)
	ClaimStartPlayer(ctx context.Context, playerID, ticketID uint64, ttl time.Duration) (existing uint64, claimed bool, err error)
	GetStartPlayerOperation(ctx context.Context, playerID uint64) (ticketID uint64, found bool, err error)
	DeleteStartPlayerIfMatches(ctx context.Context, playerID, ticketID uint64) error

	// CreateMatch 持久写 match proto bytes 并 ZADD 进 active(score=confirm_deadline_ms)。
	// 非终态不能依赖 matchTTL 消失；显式 FAILED/ReleaseMatch 后才按 retention TTL 留存或删除。
	CreateMatch(ctx context.Context, match *matchv1.MatchStorageRecord, matchTTL time.Duration) error
	// GetMatch 读 match。not found 返 (nil, false, nil)。
	GetMatch(ctx context.Context, matchID uint64) (*matchv1.MatchStorageRecord, bool, error)
	// UpdateMatchWithLock WATCH/MULTI/EXEC 读-改-写 match;CAS 失败重试 maxRetry 次,耗尽返 ErrMatchConcurrent。
	UpdateMatchWithLock(ctx context.Context, matchID uint64, maxRetry int, fn func(*matchv1.MatchStorageRecord) error, matchTTL time.Duration) error
	// RemoveActive 把 match 移出 active ZSET(确认期结束,不再超时扫描)。
	RemoveActive(ctx context.Context, matchID uint64) error
	// EnsureActive 幂等重建 active 索引。canonical match record 是权威，ZSET 只是可重建索引。
	EnsureActive(ctx context.Context, matchID uint64, scoreMs int64) error
	// RangeActiveMatches 返回 active 索引当前全部 match_id，供 durable allocation worker 推进。
	RangeActiveMatches(ctx context.Context) ([]uint64, error)
	// ScanMatchIDs 遍历 Redis Cluster 全部 master 的 canonical match records，供 reconciler
	// 修复丢失 active 索引；不能用 UniversalClient.Scan（它在 Cluster 只扫一个节点）。
	ScanMatchIDs(ctx context.Context, count int64) ([]uint64, error)
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
	rdb       redis.UniversalClient
	queueKey  string // 按 game_mode 命名空间化的 queue 索引 ZSET
	activeKey string // 按 game_mode 命名空间化的 active 索引 ZSET
	startKey  string // StartMatch durable saga 的 due 索引 ZSET
}

// NewRedisMatchRepo 构造 RedisMatchRepo。
//
// namespace 通常传服务的 game_mode(如 "5v5_ranked");空串保留旧全局 key(单测 / 兼容路径)。
// 仅影响 queue / active 两个扫描索引;ticketKey / matchKey / playerKey 仍全局(见 key 模板注释)。
func NewRedisMatchRepo(rdb redis.UniversalClient, namespace string) *RedisMatchRepo {
	return &RedisMatchRepo{
		rdb:       rdb,
		queueKey:  queueKeyFor(namespace),
		activeKey: activeKeyFor(namespace),
		startKey:  startActiveKeyFor(namespace),
	}
}

// --- player index ---

func (r *RedisMatchRepo) ClaimPlayer(ctx context.Context, playerID, ticketID uint64, _ time.Duration) (uint64, bool, error) {
	key := playerKey(playerID)
	val := strconv.FormatUint(ticketID, 10)
	for attempt := 0; attempt < 2; attempt++ {
		ok, err := r.rdb.SetNX(ctx, key, val, 0).Result()
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

// DeletePlayerIndex 无条件删除 player→ticket 映射。不在 MatchRepo 接口内:biz 清理路径一律用
// DeletePlayerIndexIfMatches(CAS),无条件删存在误删并发新 claim 的窗口;仅供测试造数据用。
func (r *RedisMatchRepo) DeletePlayerIndex(ctx context.Context, playerID uint64) error {
	return r.rdb.Del(ctx, playerKey(playerID)).Err()
}

// deleteClaimScript 仅当 claim 当前值仍指向待清理的旧票据时才 DEL(原子比较,
// 防「读旧 claim → 过期 → 新 claim 写入 → 误删」的并发窗口)。
var deleteClaimScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`)

func (r *RedisMatchRepo) DeletePlayerIndexIfMatches(ctx context.Context, playerID, ticketID uint64) error {
	return deleteClaimScript.Run(ctx, r.rdb,
		[]string{playerKey(playerID)},
		strconv.FormatUint(ticketID, 10)).Err()
}

// refreshClaimScript 仅当 claim 当前值仍是本票据时移除旧 TTL。新版本从创建
// 起就是 persistent；该脚本用于滚动升级期间把旧 claim 原子升级为 durable。
var refreshClaimScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('PERSIST', KEYS[1])
  return 1
end
return 0`)

// persistClaimScript upgrades an old TTL-backed start-saga index without a
// read/expire/recreate race. Only the same operation may remove its expiry.
var persistClaimScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('PERSIST', KEYS[1])
  return 1
end
return 0`)

func (r *RedisMatchRepo) RefreshPlayerClaim(ctx context.Context, playerID, ticketID uint64, _ time.Duration) error {
	return refreshClaimScript.Run(ctx, r.rdb,
		[]string{playerKey(playerID)},
		strconv.FormatUint(ticketID, 10)).Err()
}

func (r *RedisMatchRepo) PersistPlayerClaim(ctx context.Context, playerID, ticketID uint64) error {
	result, err := persistClaimScript.Run(ctx, r.rdb,
		[]string{playerKey(playerID)}, strconv.FormatUint(ticketID, 10)).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return errcode.New(errcode.ErrMatchConcurrent,
			"player %d claim no longer belongs to ticket %d", playerID, ticketID)
	}
	return nil
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
	// Cluster 兼容(同 trade decision-revisit-trade-crossslot.md):ticketKey 与全局 queueKey 分属不同 slot,
	// 不能捆同一事务(否则 CROSSSLOT)。① ticketKey 单键 SET 权威落库;② queueKey 独立 ZADD 入池。均幂等。
	if err := r.CreateTicketRecord(ctx, ticket, ticketTTL); err != nil {
		return err
	}
	return r.EnqueueTicket(ctx, ticket)
}

// CreateTicketRecord 见接口注释:只 SET 票据主体,不入 queue(StartMatch 三步写序第 1 步)。
func (r *RedisMatchRepo) CreateTicketRecord(ctx context.Context, ticket *matchv1.MatchTicketStorageRecord, ticketTTL time.Duration) error {
	payload, err := marshalTicket(ticket)
	if err != nil {
		return err
	}
	// A queued ticket is canonical player intent, not presence. Keep it until an
	// explicit cancel/failure/release path removes it. The duration argument is
	// retained for interface compatibility and terminal-retention policy only.
	return r.rdb.Set(ctx, ticketKey(ticket.TicketId), payload, 0).Err()
}

// EnqueueTicket 见接口注释:ZADD 入 queue(StartMatch 三步写序第 3 步)。幂等。
func (r *RedisMatchRepo) EnqueueTicket(ctx context.Context, ticket *matchv1.MatchTicketStorageRecord) error {
	return r.rdb.ZAdd(ctx, r.queueKey, redis.Z{Score: float64(ticket.AvgMmr), Member: ticket.TicketId}).Err()
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
	key := ticketKey(ticket.TicketId)
	// WATCH CAS(修复无锁盲 SET 的双向竞态):
	//   - CancelMatch 并发 CAS 删票后,盲 SET 会把票据"复活"进 match,而成员 claim 已释放
	//     → 玩家可再排队,同人两场(违反不变量 §1)。CAS 下票据已消失 → 返错,由 formMatch 回滚本组。
	//   - leader 交接重叠(旧 leader 失租瞬间新 leader 已跑)时,两个循环可能同时预留同一张票;
	//     CAS 下后到者读到 match_id 已被占 → 返错,不会重复成局。
	for attempt := 0; attempt < ticketCASRetry; attempt++ {
		var gone bool
		var conflictMatch uint64
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			gone, conflictMatch = false, 0
			b, gerr := tx.Get(ctx, key).Bytes()
			if gerr == redis.Nil {
				gone = true
				return nil
			}
			if gerr != nil {
				return gerr
			}
			cur, uerr := unmarshalTicket(ticket.TicketId, b)
			if uerr != nil {
				return uerr
			}
			if cur.MatchId != 0 && cur.MatchId != ticket.MatchId {
				conflictMatch = cur.MatchId
				return nil
			}
			_, perr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, key, payload, 0)
				return nil
			})
			return perr
		}, key)
		if txErr == redis.TxFailedErr {
			continue // 并发写(取消/另一预留),重读再判
		}
		if txErr != nil {
			return txErr
		}
		if gone {
			return errcode.New(errcode.ErrMatchNotFound, "ticket %d gone (cancelled)", ticket.TicketId)
		}
		if conflictMatch != 0 {
			return errcode.New(errcode.ErrMatchConcurrent, "ticket %d already reserved by match %d", ticket.TicketId, conflictMatch)
		}
		// Cluster 兼容:queueKey 与 ticketKey 不同 slot,独立 ZREM 移出池。
		// 若 ZREM 失败残留队列项,matchOnce 加载时 t.MatchId != 0 会跳过(防重复撞合),不影响正确性。
		_ = r.rdb.ZRem(ctx, r.queueKey, ticket.TicketId).Err()
		return nil
	}
	return errcode.New(errcode.ErrMatchConcurrent, "reserve ticket %d concurrent retry exhausted", ticket.TicketId)
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
	return r.rdb.ZRem(ctx, r.queueKey, ticketID).Err()
}

func (r *RedisMatchRepo) DeleteTicketIfUnmatched(ctx context.Context, ticketID uint64) (bool, uint64, error) {
	key := ticketKey(ticketID)
	for attempt := 0; attempt < ticketCASRetry; attempt++ {
		var missing bool
		var reserved uint64
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			missing, reserved = false, 0
			b, gerr := tx.Get(ctx, key).Bytes()
			if gerr == redis.Nil {
				missing = true
				return nil
			}
			if gerr != nil {
				return gerr
			}
			rec, uerr := unmarshalTicket(ticketID, b)
			if uerr != nil {
				return uerr
			}
			if rec.MatchId != 0 {
				reserved = rec.MatchId
				return nil
			}
			_, perr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Del(ctx, key)
				return nil
			})
			return perr
		}, key)
		if txErr == redis.TxFailedErr {
			continue // 撮合循环并发预留了票据 → 重读再判
		}
		if txErr != nil {
			return false, 0, txErr
		}
		if missing {
			return false, 0, nil
		}
		if reserved != 0 {
			return false, reserved, nil
		}
		// 删除已提交,queue ZREM best-effort(不同 slot);失败由 matchOnce miss 自愈补清。
		_ = r.rdb.ZRem(ctx, r.queueKey, ticketID).Err()
		return true, 0, nil
	}
	return false, 0, errcode.New(errcode.ErrMatchConcurrent, "delete ticket %d concurrent retry exhausted", ticketID)
}

func (r *RedisMatchRepo) RangeQueueTickets(ctx context.Context) ([]uint64, error) {
	vals, err := r.rdb.ZRange(ctx, r.queueKey, 0, -1).Result()
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

// --- StartMatch durable saga ---

func (r *RedisMatchRepo) CreateStartOperation(ctx context.Context, op *matchv1.MatchStartOperationStorageRecord, ttl time.Duration) error {
	payload, err := marshalStartOperation(op)
	if err != nil {
		return err
	}
	key := startOperationKey(op.GetTicketId())
	// ACCEPTED is a business fact, not a cache entry. It must survive longer
	// than ticketTTL and is deleted/expired only after an explicit terminal.
	ok, err := r.rdb.SetNX(ctx, key, payload, 0).Result()
	if err != nil {
		return err
	}
	if !ok {
		existing, found, gerr := r.GetStartOperation(ctx, op.GetTicketId())
		if gerr != nil {
			return gerr
		}
		if !found || existing.GetOperationId() != op.GetOperationId() {
			return errcode.New(errcode.ErrMatchConcurrent, "start operation ticket %d already exists", op.GetTicketId())
		}
	}

	// player→start-op 是 Resume/并发 Start 的派生索引。record 已先持久化，所以从这里
	// 开始 RPC 只能是“已受理”：Redis 瞬态失败或 preflight 后发生的真实 claim 冲突都
	// 交给 saga/reconciler。尤其不能在 canonical ACCEPTED 已提交后向 caller 返回业务
	// 拒绝；否则若 COMPENSATING 写失败，caller 看见失败而 ACCEPTED 记录稍后仍会入队。
	// Worker 会把冲突可靠 CAS 到 COMPENSATING，并 compare-delete 仅属于本 operation 的
	// 派生索引；这里的 claim 仅用于尽早建立冷启动 discoverability。
	for _, member := range op.GetMembers() {
		existing, claimed, cerr := r.ClaimStartPlayer(ctx, member.GetPlayerId(), op.GetTicketId(), ttl)
		if cerr != nil {
			break
		}
		if !claimed && existing != op.GetTicketId() {
			break
		}
	}

	// record 已经是可恢复的 canonical 接受点。due 索引失败时不回滚 record，也不能向
	// caller 谎报未受理；全 master reconciler 会把它补回。这里做与 CreateMatch 相同的有界重试。
	z := redis.Z{Score: float64(op.GetNextAttemptAtMs()), Member: op.GetTicketId()}
	var zerr error
	for attempt := 0; attempt < createMatchZAddRetry; attempt++ {
		if zerr = r.rdb.ZAdd(ctx, r.startKey, z).Err(); zerr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(createMatchZAddBackoff):
		}
	}
	return nil
}

func (r *RedisMatchRepo) ClaimStartPlayer(ctx context.Context, playerID, ticketID uint64, _ time.Duration) (uint64, bool, error) {
	key := startPlayerKey(playerID)
	value := strconv.FormatUint(ticketID, 10)
	for attempt := 0; attempt < 2; attempt++ {
		// This is the only discoverability edge for an in-flight durable saga.
		// Keep it persistent until QUEUED handoff or compensation compare-deletes it.
		ok, err := r.rdb.SetNX(ctx, key, value, 0).Result()
		if err != nil {
			return 0, false, err
		}
		if ok {
			return ticketID, true, nil
		}
		current, err := r.rdb.Get(ctx, key).Result()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			return 0, false, err
		}
		existing, err := strconv.ParseUint(current, 10, 64)
		if err != nil {
			return 0, false, err
		}
		if existing == ticketID {
			if err := persistClaimScript.Run(ctx, r.rdb, []string{key}, value).Err(); err != nil {
				return 0, false, err
			}
		}
		return existing, false, nil
	}
	return 0, false, errcode.New(errcode.ErrMatchConcurrent, "claim start player %d concurrent", playerID)
}

func (r *RedisMatchRepo) GetStartPlayerOperation(ctx context.Context, playerID uint64) (uint64, bool, error) {
	value, err := r.rdb.Get(ctx, startPlayerKey(playerID)).Result()
	if err == redis.Nil {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	ticketID, err := strconv.ParseUint(value, 10, 64)
	if err != nil || ticketID == 0 {
		return 0, false, fmt.Errorf("start player %d bad ticket id %q", playerID, value)
	}
	return ticketID, true, nil
}

func (r *RedisMatchRepo) DeleteStartPlayerIfMatches(ctx context.Context, playerID, ticketID uint64) error {
	return deleteClaimScript.Run(ctx, r.rdb, []string{startPlayerKey(playerID)}, strconv.FormatUint(ticketID, 10)).Err()
}

func (r *RedisMatchRepo) GetStartOperation(ctx context.Context, ticketID uint64) (*matchv1.MatchStartOperationStorageRecord, bool, error) {
	b, err := r.rdb.Get(ctx, startOperationKey(ticketID)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rec, err := unmarshalStartOperation(ticketID, b)
	if err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

func (r *RedisMatchRepo) UpdateStartOperationWithLock(
	ctx context.Context,
	ticketID uint64,
	maxRetry int,
	fn func(*matchv1.MatchStartOperationStorageRecord) error,
	ttl time.Duration,
) error {
	key := startOperationKey(ticketID)
	for attempt := 0; attempt <= maxRetry; attempt++ {
		var fnErr error
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			b, err := tx.Get(ctx, key).Bytes()
			if err == redis.Nil {
				return errcode.New(errcode.ErrMatchNotFound, "start operation %d not found", ticketID)
			}
			if err != nil {
				return err
			}
			op, err := unmarshalStartOperation(ticketID, b)
			if err != nil {
				return err
			}
			if fnErr = fn(op); fnErr != nil {
				return fnErr
			}
			payload, err := marshalStartOperation(op)
			if err != nil {
				return err
			}
			retention := time.Duration(0)
			if op.GetPhase() == matchv1.MatchStartPhase_MATCH_START_PHASE_QUEUED ||
				op.GetPhase() == matchv1.MatchStartPhase_MATCH_START_PHASE_FAILED {
				retention = ttl
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, key, payload, retention)
				return nil
			})
			return err
		}, key)
		if txErr == nil {
			return nil
		}
		if txErr == fnErr && fnErr != nil {
			return fnErr
		}
		if txErr == redis.TxFailedErr {
			continue
		}
		return txErr
	}
	return errcode.New(errcode.ErrMatchConcurrent, "start operation %d update concurrent retry exhausted", ticketID)
}

func (r *RedisMatchRepo) EnsureStartActive(ctx context.Context, ticketID uint64, scoreMs int64) error {
	return r.rdb.ZAdd(ctx, r.startKey, redis.Z{Score: float64(scoreMs), Member: ticketID}).Err()
}

func (r *RedisMatchRepo) RangeDueStartOperations(ctx context.Context, nowMs int64) ([]uint64, error) {
	vals, err := r.rdb.ZRangeByScore(ctx, r.startKey, &redis.ZRangeBy{
		Min: "-inf",
		Max: strconv.FormatInt(nowMs, 10),
	}).Result()
	if err != nil {
		return nil, err
	}
	return parseMatchIDs("start active", vals)
}

func (r *RedisMatchRepo) RemoveStartActive(ctx context.Context, ticketID uint64) error {
	return r.rdb.ZRem(ctx, r.startKey, ticketID).Err()
}

func (r *RedisMatchRepo) DeleteStartOperation(ctx context.Context, ticketID uint64) error {
	zerr := r.rdb.ZRem(ctx, r.startKey, ticketID).Err()
	derr := r.rdb.Del(ctx, startOperationKey(ticketID)).Err()
	return errors.Join(zerr, derr)
}

func (r *RedisMatchRepo) ScanStartOperationIDs(ctx context.Context, count int64) ([]uint64, error) {
	keys, err := scanAllMasters(ctx, r.rdb, "pandora:match:start:{*}", count)
	if err != nil {
		return nil, err
	}
	return parseIDsFromKeys(keys, "pandora:match:start:{", "start operation")
}

// --- match ---

func (r *RedisMatchRepo) CreateMatch(ctx context.Context, match *matchv1.MatchStorageRecord, matchTTL time.Duration) error {
	payload, err := marshalMatch(match)
	if err != nil {
		return err
	}
	// Cluster 兼容:matchKey{id} 与全局 activeKey 不同 slot。① matchKey 单键 SET 权威落库;
	// ② activeKey 独立 ZADD 登记确认期超时扫描。
	// FOUND/CONFIRM/ALLOCATING/READY are authoritative business states. Their
	// lifetime cannot be encoded by Redis TTL: explicit FAILED/ReleaseMatch owns
	// terminal cleanup.
	if err := r.rdb.Set(ctx, matchKey(match.MatchId), payload, 0).Err(); err != nil {
		return err
	}
	// ZADD 幂等,先有界重试吸收 Redis 瞬时抖动。canonical match 已持久化后绝不能
	// 因派生 active 索引失败而删除；全-master reconciler 会重建索引。
	zadd := redis.Z{Score: float64(match.ConfirmDeadlineMs), Member: match.MatchId}
	var zerr error
	for attempt := 0; attempt < createMatchZAddRetry; attempt++ {
		if zerr = r.rdb.ZAdd(ctx, r.activeKey, zadd).Err(); zerr == nil {
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
	return nil
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
				pipe.Set(ctx, key, payload, 0)
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
	return r.rdb.ZRem(ctx, r.activeKey, matchID).Err()
}

func (r *RedisMatchRepo) EnsureActive(ctx context.Context, matchID uint64, scoreMs int64) error {
	return r.rdb.ZAdd(ctx, r.activeKey, redis.Z{Score: float64(scoreMs), Member: matchID}).Err()
}

func (r *RedisMatchRepo) RangeActiveMatches(ctx context.Context) ([]uint64, error) {
	vals, err := r.rdb.ZRange(ctx, r.activeKey, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	return parseMatchIDs("active", vals)
}

func (r *RedisMatchRepo) ScanMatchIDs(ctx context.Context, count int64) ([]uint64, error) {
	keys, err := scanAllMasters(ctx, r.rdb, "pandora:match:{*}", count)
	if err != nil {
		return nil, err
	}
	return parseIDsFromKeys(keys, "pandora:match:{", "match")
}

func (r *RedisMatchRepo) ExpireMatch(ctx context.Context, matchID uint64, ttl time.Duration) error {
	// Cleanup ACK and terminal retention are committed together on the canonical
	// key. -1 is an internal durable sentinel: reconcilers may remove a FAILED
	// match from active only after all ticket/claim compensation succeeded.
	key := matchKey(matchID)
	err := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
		payload, err := tx.Get(ctx, key).Bytes()
		if err == redis.Nil {
			return nil
		}
		if err != nil {
			return err
		}
		m, err := unmarshalMatch(matchID, payload)
		if err != nil {
			return err
		}
		if m.GetStage() == matchv1.MatchStage_MATCH_STAGE_FAILED {
			m.AllocationNextAttemptAtMs = -1
		}
		payload, err = marshalMatch(m)
		if err != nil {
			return err
		}
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Set(ctx, key, payload, ttl)
			return nil
		})
		return err
	}, key)
	if err != nil {
		return err
	}
	return r.rdb.ZRem(ctx, r.activeKey, matchID).Err()
}

func (r *RedisMatchRepo) DeleteMatch(ctx context.Context, matchID uint64) error {
	// Cluster 兼容:matchKey 与 activeKey 不同 slot,无法 MULTI 原子,拆为独立命令。两步均幂等。
	// 先 ZRem 把 match 移出确认期扫描索引(避免删了镜像后 RangeExpiredMatches 仍命中已空的
	// match_id),再 Del 镜像;两步都执行(不在前一步失败时 early-return,否则会残留另一半),
	// 用 errors.Join 聚合上报。任一步残留均可由 RangeExpiredMatches→GetMatch miss 自愈。
	zerr := r.rdb.ZRem(ctx, r.activeKey, matchID).Err()
	derr := r.rdb.Del(ctx, matchKey(matchID)).Err()
	return errors.Join(zerr, derr)
}

func (r *RedisMatchRepo) RangeExpiredMatches(ctx context.Context, nowMs int64) ([]uint64, error) {
	vals, err := r.rdb.ZRangeByScore(ctx, r.activeKey, &redis.ZRangeBy{
		Min: "-inf",
		Max: strconv.FormatInt(nowMs, 10),
	}).Result()
	if err != nil {
		return nil, err
	}
	return parseMatchIDs("active", vals)
}

func parseMatchIDs(index string, vals []string) ([]uint64, error) {
	out := make([]uint64, 0, len(vals))
	for _, v := range vals {
		id, perr := strconv.ParseUint(v, 10, 64)
		if perr != nil {
			return nil, fmt.Errorf("%s bad match_id %q: %w", index, v, perr)
		}
		out = append(out, id)
	}
	return out, nil
}

type redisScanner interface {
	Scan(ctx context.Context, cursor uint64, match string, count int64) *redis.ScanCmd
}

// scanAllMasters 完整遍历 Redis Cluster 每个 master。UniversalClient.Scan 在 Cluster
// 只会发给单一节点，会永久漏掉其他 slot 上的 canonical record，不能用于恢复索引。
func scanAllMasters(ctx context.Context, client redis.UniversalClient, pattern string, count int64) ([]string, error) {
	if count <= 0 {
		count = 128
	}
	var (
		mu   sync.Mutex
		keys []string
	)
	scan := func(ctx context.Context, node redisScanner) error {
		cursor := uint64(0)
		for {
			batch, next, err := node.Scan(ctx, cursor, pattern, count).Result()
			if err != nil {
				return err
			}
			mu.Lock()
			keys = append(keys, batch...)
			mu.Unlock()
			cursor = next
			if cursor == 0 {
				return nil
			}
		}
	}

	var err error
	switch c := client.(type) {
	case *redis.ClusterClient:
		err = c.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
			return scan(ctx, node)
		})
	case *redis.Ring:
		err = c.ForEachShard(ctx, func(ctx context.Context, node *redis.Client) error {
			return scan(ctx, node)
		})
	default:
		err = scan(ctx, client)
	}
	if err != nil {
		return nil, err
	}
	return keys, nil
}

func parseIDsFromKeys(keys []string, prefix, index string) ([]uint64, error) {
	seen := make(map[uint64]struct{}, len(keys))
	out := make([]uint64, 0, len(keys))
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, "}") {
			continue
		}
		raw := strings.TrimSuffix(strings.TrimPrefix(key, prefix), "}")
		id, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || id == 0 {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
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

func marshalStartOperation(op *matchv1.MatchStartOperationStorageRecord) ([]byte, error) {
	if op == nil {
		return nil, fmt.Errorf("nil start operation")
	}
	return proto.Marshal(op)
}

func unmarshalStartOperation(ticketID uint64, payload []byte) (*matchv1.MatchStartOperationStorageRecord, error) {
	rec := &matchv1.MatchStartOperationStorageRecord{}
	if err := proto.Unmarshal(payload, rec); err != nil {
		return nil, fmt.Errorf("start operation %d bad proto: %w", ticketID, err)
	}
	if rec.TicketId == 0 {
		rec.TicketId = ticketID
	}
	if rec.TicketId != ticketID {
		return nil, fmt.Errorf("start operation %d id mismatch: %d", ticketID, rec.TicketId)
	}
	return rec, nil
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
