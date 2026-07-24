// writer_fence.go — hub_allocator 写者继任 fencing(INC-20260722-004 R9 P0-7 收口;
// docs/design/session-generation-rollout.md §5)。
//
// 语义:每届 hub_allocator 写者持有一个严格单调递增的 fencing token(etcd 继任租约,
// pkg/dsauthfence/writerlease)。所有 per-{pod} 授权/容量账本事务在同一 WATCH/MULTI/EXEC
// 内比较并推进与业务键同 slot 的 fence key:
//
//	pandora:hub:wfence:{<pod>} → 已见最大写者 token(十进制字符串,持久键)
//
//	cur > mine → 拒绝(ErrWriterSuperseded,零写入):继任者已触达此 slot,前任的迟到
//	             写永久出局(即使前任进程尚未察觉失主);
//	cur < mine → 本事务顺带把 fence 推进到 mine(SET 进同一 EXEC);
//	cur == mine → 直接放行。
//
// 逐 slot 懒推进的正确性:继任者第一次写某 {pod} slot 起,前任在该 slot 永久被拒;
// 继任者尚未触达的 slot 上,前任写在语义上线性化于交接之前(继任者随后读到并接续),
// 不构成账本冲突。fence key 故意**不设 TTL、RemoveShard 也不删**:fencing 水位必须
// 比业务记录长寿,否则删除即复位,迟到旧写者可借尸还魂。
//
// 覆盖边界(rollout §5 明示的诚实残差):per-player assignment key
// (pandora:hub:player:<id>)无 hashtag、与任何 {pod} slot 不可同事务,无法加存储级
// fence(迁移现网数据风险更大);该路径由四层组合收口:
//
//	① biz 入口 writer gate(失主副本快速拒写);
//	② 既有 CompareAndSwapAssignment 精确前置快照 CAS;
//	③ 继任者水位推扫(AdvanceWriterFences):新写者当选后主动把**全部已知 pod** 的
//	   fence 推进到本届 token,消灭逐 slot 懒推进留下的「未触碰 pod」盲区——推扫完成
//	   后前任在任何 {pod} slot 上的席位预留/账本写全部被拒,其签出的票在 Admission
//	   点必然找不到席位;
//	④ biz 出票前写者复核(票据只在「入口到返回全程持有租约」时交付,失主瞬间在途
//	   的请求不返回票,ErrUnavailable 引导重试路由到新写者)。
package data

import (
	"context"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// WriterFence 提供当前写者的 fencing token(dsauthfence/writerlease.Lease 满足此接口;
// nil = 未启用继任租约,dev/mock/单副本 Recreate 部署保持原行为)。
type WriterFence interface {
	// Current 返回 (token, 是否持有写者租约)。token 历届严格单调递增。
	Current() (uint64, bool)
}

// wfenceKey 与 shardKey/authKey/capacityLedgerKeys 同 hashtag {pod} 同 slot,
// 可捆进同一 Redis Cluster 事务(decision-revisit-hub-crossslot.md 单 slot 铁律)。
func wfenceKey(pod string) string { return fmt.Sprintf("pandora:hub:wfence:{%s}", pod) }

// ErrWriterSuperseded:本副本的写者租约已失效/被更新写者继任 → fail-closed 零写入。
// ErrUnavailable 语义:对调用方可重试(重试会被路由到新写者副本),对本副本是终态拒绝。
var ErrWriterSuperseded = errcode.New(errcode.ErrUnavailable,
	"hub allocator writer lease superseded; retry against current writer")

// noopAdvance 供未启用 fence / 无需推进时占位。
func noopAdvance(redis.Pipeliner) {}

// guardWriterFence 在 Watch 回调内做 fence 比较,返回「推进闭包」供写事务在同一
// EXEC 内推进水位。调用方必须把 wfenceKey(pod) 加入 WATCH 集(见 fencedWatchKeys),
// 否则比较与推进之间的并发继任会绕过乐观锁。
//
// 只读事务可只调本函数校验、不执行 advance(检查恒保守安全)。
func guardWriterFence(ctx context.Context, tx *redis.Tx, pod string, fence WriterFence) (func(redis.Pipeliner), error) {
	if fence == nil {
		return noopAdvance, nil
	}
	mine, held := fence.Current()
	if !held {
		return nil, ErrWriterSuperseded
	}
	raw, err := tx.Get(ctx, wfenceKey(pod)).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	var cur uint64
	if err == nil {
		cur, err = strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("hub writer fence %s corrupt value %q: %w", pod, raw, err)
		}
	}
	if cur > mine {
		return nil, ErrWriterSuperseded
	}
	if cur == mine {
		return noopAdvance, nil
	}
	key, val := wfenceKey(pod), strconv.FormatUint(mine, 10)
	return func(pipe redis.Pipeliner) {
		pipe.Set(ctx, key, val, 0) // 持久:fencing 水位必须比业务记录长寿
	}, nil
}

// fencedWatchKeys 在启用 fence 时把 wfenceKey(pod) 并入 WATCH 集(同 slot)。
func fencedWatchKeys(keys []string, pod string, fence WriterFence) []string {
	if fence == nil {
		return keys
	}
	return append(keys, wfenceKey(pod))
}

// AdvanceWriterFences 是继任者水位推扫(见文件头覆盖边界 ③):把**全部已知 pod**
// (分片 SET ∪ 待清理 saga 源 pod)的 fence 主动推进到本届 token。逐 slot 懒推进只在
// 继任者写过的 slot 生效;推扫消灭「继任者尚未触碰的 pod」盲区——完成后前任写者在
// 任何 {pod} slot 上的席位/账本写永久出局。幂等,可在同一届内重复调用(cur==mine
// 直接跳过)。任一 pod 推扫遇到更大 token(自己已被继任)立即返回 ErrWriterSuperseded。
// fence 未注入时是 no-op。
func (r *RedisHubRepo) AdvanceWriterFences(ctx context.Context) error {
	if r.fence == nil {
		return nil
	}
	mine, held := r.fence.Current()
	if !held {
		return ErrWriterSuperseded
	}
	pods := map[string]struct{}{}
	shardPods, err := r.rdb.SMembers(ctx, shardsSetKey).Result()
	if err != nil {
		return err
	}
	for _, p := range shardPods {
		pods[p] = struct{}{}
	}
	cleanupPods, err := r.ListTransferCleanupPods(ctx)
	if err != nil {
		return err
	}
	for _, p := range cleanupPods {
		pods[p] = struct{}{}
	}
	for pod := range pods {
		if err := r.advanceWriterFencePod(ctx, pod, mine); err != nil {
			return err
		}
	}
	return nil
}

// advanceWriterFencePod 单 pod 水位推进:WATCH/MULTI/EXEC 只进不退。
func (r *RedisHubRepo) advanceWriterFencePod(ctx context.Context, pod string, mine uint64) error {
	key := wfenceKey(pod)
	const casMaxRetry = 8
	for attempt := 0; attempt < casMaxRetry; attempt++ {
		err := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			raw, gerr := tx.Get(ctx, key).Result()
			if gerr != nil && gerr != redis.Nil {
				return gerr
			}
			var cur uint64
			if gerr == nil {
				cur, gerr = strconv.ParseUint(raw, 10, 64)
				if gerr != nil {
					return fmt.Errorf("hub writer fence %s corrupt value %q: %w", pod, raw, gerr)
				}
			}
			if cur > mine {
				return ErrWriterSuperseded
			}
			if cur == mine {
				return nil
			}
			_, perr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, key, strconv.FormatUint(mine, 10), 0) // 持久:水位比业务记录长寿
				return nil
			})
			return perr
		}, key)
		if err == redis.TxFailedErr {
			continue
		}
		return err
	}
	return errcode.New(errcode.ErrUnavailable, "hub writer fence advance contention on pod %s", pod)
}
