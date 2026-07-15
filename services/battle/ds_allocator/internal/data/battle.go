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

const fencedFinalizeReadbackTimeout = 3 * time.Second

// BattleStateAllocationUncertain 表示本轮 allocation_id 已在 Redis 持久化封死，
// 随后的 GameServerAllocation POST 可能尚未发出、也可能已经应用但响应未知。
// 该字符串故意不复用 allocating/warming：旧 writer 遇到未知状态会 fail-closed，
// 不能靠 TTL、sweep 或幂等重试把它当成“未分配”后再次 POST。
const BattleStateAllocationUncertain = "allocation_uncertain"

// BattleStatePreactiveReleasePending 表示已确认 GameServer UID 的未激活分配正在回收。
// 该状态与 auth TERMINATING（若 auth 已建立）共同构成外部 ReleaseExpected 之前的
// 永久墓碑：只有 UID 条件删除被明确确认成功后，才允许按 expected tuple 物理 purge。
// 它不能带 TTL，否则 release 响应未知后墓碑过期会重新开放同 match 的第二次 GSA POST。
const BattleStatePreactiveReleasePending = "preactive_release_pending"

func battleKey(matchID uint64) string { return fmt.Sprintf("pandora:ds:battle:{%d}", matchID) }

// ── 接口 ──────────────────────────────────────────────────────────────────────

// BattleRepo 是 ds_allocator 数据层抽象。biz 层只依赖此接口,不依赖 redis。
type BattleRepo interface {
	// ClaimBattle 以单个 battle key 的 SET NX 取得本轮分配所有权。只有 claimed=true 的调用者
	// 才允许访问外部 Agones Allocation API；allocation_id 是后续 finalize/cleanup 的 fencing token。
	ClaimBattle(ctx context.Context, claim *dsv1.BattleStorageRecord, battleTTL time.Duration) (claimed bool, existing *dsv1.BattleStorageRecord, err error)
	// FenceBattleAllocation 在任何外部 GameServerAllocation POST 前，把 allocating claim
	// CAS 成 allocation_uncertain 并去掉 TTL。只有 fenced=true 的调用者才准发 POST。
	FenceBattleAllocation(ctx context.Context, matchID uint64, allocationID string) (fenced bool, err error)
	// FinalizeBattleAllocation 把本调用持有的 allocating claim CAS 成 warming 镜像。
	// allocation_id 不匹配或 claim 已被替换时返回 finalized=false，绝不覆盖当前赢家。
	FinalizeBattleAllocation(ctx context.Context, battle *dsv1.BattleStorageRecord, battleTTL time.Duration) (finalized bool, err error)
	// FinalizeFencedBattleAllocation 只把同 allocation_id 的 allocation_uncertain claim
	// CAS 成 warming，且在首个授权心跳激活前继续保持永久。Model B 严格确认
	// GameServer UID/RV 后才可调用；激活事务才给 auth+battle 赋正常 TTL。
	FinalizeFencedBattleAllocation(ctx context.Context, battle *dsv1.BattleStorageRecord, battleTTL time.Duration) (finalized bool, err error)
	// DeleteBattleIfAllocationMatches 仅删除仍属于 expected allocation_id/pod 的已知可回收阶段
	// allocating/warming/abandoned。allocation_uncertain 与所有未知状态一律拒删。
	// deleted=true 才表示调用方取得了释放对应 GameServer 的权利。
	DeleteBattleIfAllocationMatches(ctx context.Context, matchID uint64, allocationID, podName string) (deleted bool, err error)
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
	// EnsurePlayerDeparture 以 placement operation_id + exact source GameServer tuple 幂等
	// 建立 Battle→Hub 物理离场单。没有 credential-bound active snapshot ACK 或 exact
	// UID teardown proof 时只能返回 pending，禁止用 TTL/键缺失推导离场。
	EnsurePlayerDeparture(ctx context.Context, expected BattlePlayerDepartureExpected) (BattlePlayerDepartureResult, error)
	// ReconcilePlayerDepartures 只供已验证 Battle DS credential 的心跳调用。
	// snapshotPresent=false 时绝不提交 departed（滚动更新 fail-closed）。
	ReconcilePlayerDepartures(ctx context.Context, matchID uint64, source BattleDepartureSource,
		snapshotPresent bool, activePlayerIDs []uint64, acknowledgedDepartureIDs []string) ([]*dsv1.BattleEvictionOrder, error)
	// RecordInstanceTeardown 只能在外部 UID 条件 Release 明确成功后调用。
	// 它与 journal 同 slot 原子提交，使源 DS 整体销毁成为离场证明。
	RecordInstanceTeardown(ctx context.Context, matchID uint64, source BattleDepartureSource) error
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

// ClaimBattle 是 AllocateBattle 的线性化点。先持久化 allocation_id claim，再访问 Agones，
// 消除两个 ds_allocator 副本同时 Get miss 后各自分配一个 GameServer 的竞态。
// claim 同时登记 active ZSET 作为 inflight 扫描索引；否则进程在 SETNX 后崩溃会让
// allocating key 卡满 BattleTTL，且 GSA 未知结果永远无人按 allocation_id 对账。
func (r *RedisBattleRepo) ClaimBattle(
	ctx context.Context,
	claim *dsv1.BattleStorageRecord,
	battleTTL time.Duration,
) (bool, *dsv1.BattleStorageRecord, error) {
	if claim == nil || claim.MatchId == 0 || claim.AllocationId == "" || claim.State != "allocating" {
		return false, nil, errcode.New(errcode.ErrInvalidArg, "invalid battle allocation claim")
	}
	payload, err := marshalBattle(claim)
	if err != nil {
		return false, nil, err
	}
	ok, err := r.rdb.SetNX(ctx, battleKey(claim.MatchId), payload, battleTTL).Result()
	if err != nil {
		return false, nil, err
	}
	if ok {
		if zerr := r.rdb.ZAdd(ctx, activeKey, redis.Z{
			Score: float64(claim.LastHeartbeatMs), Member: claim.MatchId,
		}).Err(); zerr != nil {
			// 尚未调用外部 Agones，索引登记失败时可按 allocation_id 安全撤 claim。
			// 撤销也失败只会留下不可分配 claim，不会产生第二个 Pod。
			_, cleanupErr := r.DeleteBattleIfAllocationMatches(
				ctx, claim.MatchId, claim.AllocationId, claim.DsPodName)
			if cleanupErr != nil {
				return false, nil, fmt.Errorf("claim inflight index: %w; cleanup: %v", zerr, cleanupErr)
			}
			return false, nil, fmt.Errorf("claim inflight index: %w", zerr)
		}
		return true, nil, nil
	}
	existing, found, err := r.GetBattle(ctx, claim.MatchId)
	if err != nil {
		return false, nil, err
	}
	if !found {
		// key 可能恰在 SETNX=false 后过期；本轮不擅自再次争抢，避免一次 RPC 内产生
		// 两次外部分配。调用方重试会领取新的 allocation_id。
		return false, nil, errcode.New(errcode.ErrDSAllocationFailed,
			"battle %d allocation claim disappeared", claim.MatchId)
	}
	return false, existing, nil
}

// FenceBattleAllocation 是“是否允许调用外部 GSA POST”的 Redis 线性化点。
// WATCH/MULTI 保证只有当前 allocation_id 的 allocating owner 能成功；事务里的无 TTL SET
// 同时完成状态替换与 PERSIST 语义。若 EXEC 的响应未知，调用方也必须按失败处理且绝不 POST，
// 最坏只留下一个永久 fail-closed、需显式审计的 uncertain claim。
func (r *RedisBattleRepo) FenceBattleAllocation(
	ctx context.Context,
	matchID uint64,
	allocationID string,
) (bool, error) {
	if matchID == 0 || allocationID == "" {
		return false, errcode.New(errcode.ErrInvalidArg, "match_id and allocation_id required")
	}
	key := battleKey(matchID)
	for attempt := 0; attempt <= 3; attempt++ {
		fenced := false
		err := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			currentBytes, gerr := tx.Get(ctx, key).Bytes()
			if gerr == redis.Nil {
				return nil
			}
			if gerr != nil {
				return gerr
			}
			current, uerr := unmarshalBattle(matchID, currentBytes)
			if uerr != nil {
				return uerr
			}
			if current.AllocationId != allocationID || current.State != "allocating" {
				return nil
			}
			current.State = BattleStateAllocationUncertain
			payload, merr := marshalBattle(current)
			if merr != nil {
				return merr
			}
			_, xerr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				// SET KEEPTTL + PERSIST 同处一个 EXEC：状态替换和去 TTL 对外原子可见，
				// 不存在先写 uncertain、进程崩溃后它仍会过期的窗口。
				pipe.Set(ctx, key, payload, redis.KeepTTL)
				pipe.Persist(ctx, key)
				return nil
			})
			if xerr == nil {
				fenced = true
			}
			return xerr
		}, key)
		if err == redis.TxFailedErr {
			continue
		}
		if err != nil || !fenced {
			return fenced, err
		}
		return true, nil
	}
	return false, errcode.New(errcode.ErrDSAllocationFailed,
		"battle %d pre-allocation fence concurrent retry exhausted", matchID)
}

// FinalizeBattleAllocation 只允许 allocation_id 对应的 allocating claim 写成 warming。
// 权威 battle key 与 CAS 在同 slot/单事务；active ZSET 是跨 slot 派生索引，ZADD 失败会把
// 错误返回给调用方，由 expected-allocation cleanup 把 warming 镜像撤掉，绝不放行 ready。
func (r *RedisBattleRepo) FinalizeBattleAllocation(
	ctx context.Context,
	battle *dsv1.BattleStorageRecord,
	battleTTL time.Duration,
) (bool, error) {
	return r.finalizeBattleAllocation(ctx, battle, "allocating", battleTTL, false)
}

// FinalizeFencedBattleAllocation 是 Model B 唯一 finalize 入口。它拒绝直接从 allocating
// 跳到 warming，保证严格 UID/RV 确认之前 Redis claim 一直处于永久 uncertain 状态。
func (r *RedisBattleRepo) FinalizeFencedBattleAllocation(
	ctx context.Context,
	battle *dsv1.BattleStorageRecord,
	battleTTL time.Duration,
) (bool, error) {
	return r.finalizeBattleAllocation(ctx, battle, BattleStateAllocationUncertain, battleTTL, true)
}

func (r *RedisBattleRepo) finalizeBattleAllocation(
	ctx context.Context,
	battle *dsv1.BattleStorageRecord,
	expectedState string,
	battleTTL time.Duration,
	persistent bool,
) (bool, error) {
	if battle == nil || battle.MatchId == 0 || battle.AllocationId == "" || battle.State != "warming" || battle.DsPodName == "" {
		return false, errcode.New(errcode.ErrInvalidArg, "invalid finalized battle allocation")
	}
	if expectedState != "allocating" && expectedState != BattleStateAllocationUncertain {
		return false, errcode.New(errcode.ErrInvalidArg, "invalid battle allocation source state")
	}
	key := battleKey(battle.MatchId)
	for attempt := 0; attempt <= 3; attempt++ {
		matched := false
		postCommitCtx := ctx
		var postCommitCancel context.CancelFunc
		err := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			currentBytes, gerr := tx.Get(ctx, key).Bytes()
			if gerr == redis.Nil {
				return nil
			}
			if gerr != nil {
				return gerr
			}
			current, uerr := unmarshalBattle(battle.MatchId, currentBytes)
			if uerr != nil {
				return uerr
			}
			if current.AllocationId != battle.AllocationId || current.State != expectedState {
				return nil
			}
			// finalize 是 read-modify-write；以 WATCH 内刚读到的权威 unknown fields
			// 覆盖调用方快照，防旧/并发 writer 在滚动更新中静默丢未来字段。
			next := proto.Clone(battle).(*dsv1.BattleStorageRecord)
			next.ProtoReflect().SetUnknown(append([]byte(nil), current.ProtoReflect().GetUnknown()...))
			payload, merr := marshalBattle(next)
			if merr != nil {
				return merr
			}
			matched = true
			_, xerr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				ttl := battleTTL
				if persistent {
					ttl = 0
				}
				pipe.Set(ctx, key, payload, ttl)
				return nil
			})
			return xerr
		}, key)
		if err == redis.TxFailedErr {
			continue
		}
		if persistent && (err != nil || !matched) {
			// EXEC 可能已经提交、但响应在客户端侧丢失。此时不能把永久
			// allocation_uncertain 错当成“仍未 finalize”后直接返回：提交后的
			// warming 也是 GSA 生命周期 fence，调用方应继续凭据投递。只在严格
			// GET read-back 同时确认 allocation/UID/pod/state 且 PTTL=-1 时认定成功。
			// 外层请求常因“EXEC 响应超时”已经 canceled；read-back 若复用它会
			// 立刻失败，等同没有确认。保留 trace/value，但用独立短预算完成严格 GET。
			readCtx, cancel := context.WithTimeout(
				context.WithoutCancel(ctx), fencedFinalizeReadbackTimeout)
			confirmed, readErr := r.confirmPersistentFencedFinalize(readCtx, battle)
			if confirmed && readErr == nil {
				matched = true
				err = nil
				postCommitCtx = readCtx
				postCommitCancel = cancel
			} else if err != nil {
				cancel()
				if readErr != nil {
					return false, fmt.Errorf("battle %d finalize response uncertain: %w; read-back: %v", battle.MatchId, err, readErr)
				}
				return false, err
			} else if readErr != nil {
				cancel()
				return false, readErr
			} else {
				cancel()
			}
		}
		if err != nil {
			if postCommitCancel != nil {
				postCommitCancel()
			}
			return false, err
		}
		if !matched {
			if postCommitCancel != nil {
				postCommitCancel()
			}
			return false, nil
		}
		if err := r.rdb.ZAdd(postCommitCtx, activeKey, redis.Z{
			Score: float64(battle.LastHeartbeatMs), Member: battle.MatchId,
		}).Err(); err != nil {
			if postCommitCancel != nil {
				postCommitCancel()
			}
			return false, err
		}
		if postCommitCancel != nil {
			postCommitCancel()
		}
		return true, nil
	}
	return false, errcode.New(errcode.ErrDSAllocationFailed,
		"battle %d finalize concurrent retry exhausted", battle.MatchId)
}

// confirmPersistentFencedFinalize 只为 Model B 的 response-lost/read-back 使用。
// 不能只看 state=warming：同 match 的另一次分配、同名 Pod 重建或有限 TTL 的旧 writer
// 都不能被误认成本次提交。UID、allocation_id、pod、地址与实例 epoch 必须逐项一致，
// 且 key 必须已经无过期时间。
func (r *RedisBattleRepo) confirmPersistentFencedFinalize(
	ctx context.Context,
	expected *dsv1.BattleStorageRecord,
) (bool, error) {
	key := battleKey(expected.GetMatchId())
	payload, err := r.rdb.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	current, err := unmarshalBattle(expected.GetMatchId(), payload)
	if err != nil {
		return false, err
	}
	strictExpected := proto.Clone(expected).(*dsv1.BattleStorageRecord)
	// future unknown fields 来自 WATCH 内原 claim，属于必须保留的滚动升级数据；
	// 除它们外，所有已知 allocation/UID/state/roster/address/timestamp 字段都须
	// 与本次 intended write 完全相等，不能只抽查三四个身份字段。
	strictExpected.ProtoReflect().SetUnknown(
		append([]byte(nil), current.ProtoReflect().GetUnknown()...))
	if current.GetState() != "warming" || !proto.Equal(current, strictExpected) {
		return false, nil
	}
	pttl, err := r.rdb.PTTL(ctx, key).Result()
	if err != nil {
		return false, err
	}
	if pttl != -1 {
		return false, errcode.New(errcode.ErrInvalidState,
			"battle %d fenced finalize read-back is not persistent", expected.GetMatchId())
	}
	return true, nil
}

// DeleteBattleIfAllocationMatches 是旧请求清理路径的 fencing delete。事务内再次确认
// allocation_id/pod，且只允许已知的 allocating/warming/abandoned；allocation_uncertain、ready/running
// 以及未来未知状态全部 fail-closed。只有 deleted=true 的调用方才可 Release。
func (r *RedisBattleRepo) DeleteBattleIfAllocationMatches(
	ctx context.Context,
	matchID uint64,
	allocationID, podName string,
) (bool, error) {
	if matchID == 0 || allocationID == "" {
		return false, errcode.New(errcode.ErrInvalidArg, "match_id and allocation_id required")
	}
	key := battleKey(matchID)
	for attempt := 0; attempt <= 3; attempt++ {
		deleted := false
		err := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			payload, gerr := tx.Get(ctx, key).Bytes()
			if gerr == redis.Nil {
				return nil
			}
			if gerr != nil {
				return gerr
			}
			current, uerr := unmarshalBattle(matchID, payload)
			if uerr != nil {
				return uerr
			}
			if current.AllocationId != allocationID ||
				(podName != "" && current.DsPodName != podName) ||
				(current.State != "allocating" && current.State != "warming" && current.State != "abandoned") {
				return nil
			}
			deleted = true
			_, xerr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Del(ctx, key)
				return nil
			})
			return xerr
		}, key)
		if err == redis.TxFailedErr {
			continue
		}
		if err != nil {
			return false, err
		}
		if !deleted {
			return false, nil
		}
		if err := r.rdb.ZRem(ctx, activeKey, matchID).Err(); err != nil {
			// 权威 key 已按 fencing 条件删除；即使派生索引清理失败，调用方仍必须知道
			// 自己赢得了删除权并释放对应 Pod，残留 ZSET 由 sweep 的 miss 分支清理。
			return true, err
		}
		return true, nil
	}
	return false, errcode.New(errcode.ErrDSAllocationFailed,
		"battle %d cleanup concurrent retry exhausted", matchID)
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
	ok, err := r.rdb.SetNX(ctx, battleKey(battle.MatchId), payload, battleTTL).Result()
	if err != nil {
		return err
	}
	if !ok {
		return errcode.New(errcode.ErrDSAllocationFailed,
			"battle %d already exists; refusing overwrite", battle.MatchId)
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
