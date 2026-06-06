// Package biz 是 ds_allocator 服务的业务逻辑层(W4 ②,2026-06-06)。
//
// 职责(docs/design/go-services.md §2.11):战斗 DS 调度。
//   - AllocateBattle:matchmaker 全员确认后调,申请战斗 DS pod → 写 Redis 镜像 → 回 ds_addr
//   - ReleaseBattle:对局结束/异常,回收 DS pod + 删镜像
//   - Heartbeat:DS 每 5s 主动上报(单向 unary,架构决策 2026-06-03),刷新 last_heartbeat_ms
//   - ListBattles:运维/调试查询当前战斗实例
//   - RunHeartbeatSweep:后台扫描 active ZSET,15s 没心跳 → 标记 abandoned + 回收(不变量 §4)
//
// 关键不变量:
//   - AllocateBattle 幂等(同 match_id 已有镜像 → 直接回已分配地址,不重复 Allocate)
//   - 心跳超时 → abandoned + 发 ds.lifecycle 补偿事件;投递成功才移出 active,
//     失败保留在 active 下一轮重试(W4 ⑧ 可靠补偿,不变量 §4)
package biz

import (
	"context"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"

	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/data"
)

// 战斗 DS 状态常量(对应 proto string state 字段)。
const (
	stateWarming   = "warming"
	stateReady     = "ready"
	stateRunning   = "running"
	stateEnded     = "ended"
	stateAbandoned = "abandoned"
)

// Heartbeat 响应控制指令常量。
const (
	commandNone = ""
	commandStop = "stop" // 通知孤儿 DS(无对应镜像)自行停机
)

// 乐观锁重试次数(心跳/状态更新冲突)。
const updateMaxRetry = 3

// DSLifecyclePusher 发 pandora.ds.lifecycle 事件(W4 ③,2026-06-06)。
//
// 心跳超时标记 abandoned 后,由它把 DSLifecycleEvent{phase=ABANDONED} 发给 battle_result
// 做玩家段位回滚补偿(不变量 §4 DS 崩溃必有补偿)。
//
// W4 ⑧:投递失败不再静默丢——sweepOnce 把对局保留在 active ZSET,下一轮 sweep 重试,
// 直到投递成功或镜像 TTL 过期;配合 battle_result 幂等消费构成 at-least-once 闭环。
// 实现可在内部失败时返回 error(由 sweepOnce 触发重试)。
type DSLifecyclePusher interface {
	PublishLifecycle(ctx context.Context, evt *dsv1.DSLifecycleEvent) error
}

// AllocatorUsecase 是 ds_allocator 业务逻辑核心。
type AllocatorUsecase struct {
	repo      data.BattleRepo
	alloc     GameServerAllocator
	cfg       conf.AllocatorConf
	lifecycle DSLifecyclePusher // 可为 nil(kafka 不可用时静默不发 abandoned 事件)
}

// NewAllocatorUsecase 构造 AllocatorUsecase。
func NewAllocatorUsecase(repo data.BattleRepo, alloc GameServerAllocator, cfg conf.AllocatorConf) *AllocatorUsecase {
	return &AllocatorUsecase{repo: repo, alloc: alloc, cfg: cfg}
}

// SetLifecyclePusher 注入 ds.lifecycle 事件发送器(main 在 kafka 就绪时调用,弱依赖)。
func (u *AllocatorUsecase) SetLifecyclePusher(p DSLifecyclePusher) { u.lifecycle = p }

func (u *AllocatorUsecase) battleTTL() time.Duration { return u.cfg.BattleTTL.Std() }

// ── RPC 1:AllocateBattle ──────────────────────────────────────────────────────

// AllocateResult 是 AllocateBattle 的出参。
type AllocateResult struct {
	DSAddr        string
	DSPodName     string
	AllocatedAtMs int64
}

// AllocateBattle 为 match 申请战斗 DS。幂等:同 match_id 已分配 → 直接返回已有地址。
func (u *AllocatorUsecase) AllocateBattle(ctx context.Context, matchID uint64, playerIDs []uint64, mapID uint32, gameMode string) (*AllocateResult, error) {
	if matchID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "match_id required")
	}

	// 幂等:已有镜像直接回(防 matchmaker 重试导致重复拉 DS)
	if existing, found, err := u.repo.GetBattle(ctx, matchID); err != nil {
		return nil, err
	} else if found {
		plog.With(ctx).Infow("msg", "allocate_idempotent_hit", "match_id", matchID, "ds_addr", existing.DsAddr)
		return &AllocateResult{DSAddr: existing.DsAddr, DSPodName: existing.DsPodName, AllocatedAtMs: existing.AllocatedAtMs}, nil
	}

	podName, addr, err := u.alloc.Allocate(ctx, matchID, mapID, gameMode)
	if err != nil {
		plog.With(ctx).Errorw("msg", "gameserver_allocate_failed", "match_id", matchID, "err", err)
		return nil, errcode.New(errcode.ErrDSAllocationFailed, "allocate ds for match %d failed", matchID)
	}

	now := time.Now().UnixMilli()
	battle := &dsv1.BattleStorageRecord{
		MatchId:         matchID,
		DsPodName:       podName,
		DsAddr:          addr,
		State:           stateReady,
		PlayerIds:       playerIDs,
		MapId:           mapID,
		GameMode:        gameMode,
		AllocatedAtMs:   now,
		LastHeartbeatMs: now, // 视分配时刻为首次心跳,给 DS warming→ready 的宽限窗口
		PlayerCount:     int32(len(playerIDs)),
	}
	if err := u.repo.CreateBattle(ctx, battle, u.battleTTL()); err != nil {
		// 镜像写失败:回收已分配 pod 避免泄漏
		if rerr := u.alloc.Release(ctx, podName); rerr != nil {
			plog.With(ctx).Warnw("msg", "rollback_release_failed", "pod", podName, "err", rerr)
		}
		return nil, err
	}

	plog.With(ctx).Infow("msg", "battle_allocated", "match_id", matchID, "pod", podName, "ds_addr", addr, "players", len(playerIDs))
	return &AllocateResult{DSAddr: addr, DSPodName: podName, AllocatedAtMs: now}, nil
}

// ── RPC 2:ReleaseBattle ───────────────────────────────────────────────────────

// ReleaseBattle 回收战斗 DS。幂等:镜像不存在视为已释放,返回成功。
func (u *AllocatorUsecase) ReleaseBattle(ctx context.Context, matchID uint64, reason string) error {
	if matchID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "match_id required")
	}
	battle, found, err := u.repo.GetBattle(ctx, matchID)
	if err != nil {
		return err
	}
	if !found {
		plog.With(ctx).Infow("msg", "release_idempotent_miss", "match_id", matchID, "reason", reason)
		return nil
	}
	if err := u.alloc.Release(ctx, battle.DsPodName); err != nil {
		plog.With(ctx).Warnw("msg", "gameserver_release_failed", "match_id", matchID, "pod", battle.DsPodName, "err", err)
	}
	if err := u.repo.DeleteBattle(ctx, matchID); err != nil {
		return err
	}
	plog.With(ctx).Infow("msg", "battle_released", "match_id", matchID, "pod", battle.DsPodName, "reason", reason)
	return nil
}

// ── RPC 3:Heartbeat ───────────────────────────────────────────────────────────

// HeartbeatResult 是 Heartbeat 的出参(下发给 DS 的控制指令)。
type HeartbeatResult struct {
	Command string
}

// Heartbeat 处理 DS 上报(单向 unary,DS 每 5s 调)。刷新 last_heartbeat_ms + 状态。
// 镜像不存在(孤儿 DS)→ 返回 stop 指令让其自行停机。
func (u *AllocatorUsecase) Heartbeat(ctx context.Context, matchID uint64, podName string, playerCount int32, state string, tsMs int64) (*HeartbeatResult, error) {
	if matchID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "match_id required")
	}
	now := time.Now().UnixMilli()

	err := u.repo.UpdateBattleWithLock(ctx, matchID, updateMaxRetry, func(b *dsv1.BattleStorageRecord) error {
		b.LastHeartbeatMs = now
		b.PlayerCount = playerCount
		// 已是终态(ended/abandoned)不被心跳改回 running
		if b.State != stateEnded && b.State != stateAbandoned && state != "" {
			b.State = state
		}
		return nil
	}, u.battleTTL())

	if err != nil {
		if errcode.As(err) == errcode.ErrDSPodNotFound {
			// 孤儿 DS:无镜像,通知停机
			plog.With(ctx).Warnw("msg", "heartbeat_orphan_ds", "match_id", matchID, "pod", podName)
			return &HeartbeatResult{Command: commandStop}, nil
		}
		return nil, err
	}
	return &HeartbeatResult{Command: commandNone}, nil
}

// ── RPC 4:ListBattles ─────────────────────────────────────────────────────────

// ListBattles 列出当前战斗实例,stateFilter 非空时按 state 过滤。
func (u *AllocatorUsecase) ListBattles(ctx context.Context, stateFilter string) ([]*dsv1.BattleInfo, error) {
	matchIDs, err := u.repo.RangeActiveBattles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*dsv1.BattleInfo, 0, len(matchIDs))
	for _, mid := range matchIDs {
		b, found, gerr := u.repo.GetBattle(ctx, mid)
		if gerr != nil || !found {
			continue
		}
		if stateFilter != "" && b.State != stateFilter {
			continue
		}
		out = append(out, &dsv1.BattleInfo{
			MatchId:       b.MatchId,
			DsPodName:     b.DsPodName,
			DsAddr:        b.DsAddr,
			State:         b.State,
			PlayerCount:   b.PlayerCount,
			AllocatedAtMs: b.AllocatedAtMs,
		})
	}
	return out, nil
}

// ── 后台心跳超时扫描 ──────────────────────────────────────────────────────────

// RunHeartbeatSweep 启动后台心跳超时扫描,直到 ctx 取消(不变量 §4)。
func (u *AllocatorUsecase) RunHeartbeatSweep(ctx context.Context) {
	ticker := time.NewTicker(u.cfg.SweepInterval.Std())
	defer ticker.Stop()
	plog.With(ctx).Infow("msg", "heartbeat_sweep_started",
		"interval", u.cfg.SweepInterval.String(), "timeout", u.cfg.HeartbeatTimeout.String())
	for {
		select {
		case <-ctx.Done():
			plog.With(ctx).Infow("msg", "heartbeat_sweep_stopped")
			return
		case <-ticker.C:
			if err := u.sweepOnce(ctx); err != nil {
				plog.With(ctx).Warnw("msg", "heartbeat_sweep_failed", "err", err)
			}
		}
	}
}

// sweepOnce 扫描一次:last_heartbeat_ms 早于阈值的战斗 → 标记 abandoned + 回收 + 可靠补偿。
//
// W4 ⑧ 可靠补偿(不变量 §4 DS 崩溃必有补偿):
// 把 active ZSET 自身当作补偿事件的「outbox」——abandoned 的对局在 ds.lifecycle 事件
// 成功投递前**不移出 active**,故下一轮 sweep 会再次命中并重试投递;只有投递成功(或未配置
// kafka 的 best-effort 回退)才 ExpireBattle 移出 active。配合 battle_result 幂等消费
// (不变量 §2),整条补偿链是 at-least-once 闭环,可穿越 Kafka 临时不可用。
//
// 天然上界靠 UpdateBattleKeepTTL(KEEPTTL):标记 abandoned + 每轮重试都**保留**镜像原 TTL
// 不刷新,故 Kafka 长期不可用时镜像最终在 BattleTTL(从最后一次心跳起算)后过期 →
// GetBattle miss → RemoveActive 清理,补偿重试不会无限延长 TTL / 无限堆积。
func (u *AllocatorUsecase) sweepOnce(ctx context.Context) error {
	threshold := time.Now().Add(-u.cfg.HeartbeatTimeout.Std()).UnixMilli()
	stale, err := u.repo.RangeStaleBattles(ctx, threshold)
	if err != nil {
		return err
	}
	for _, mid := range stale {
		var podName string
		var endedSkip bool
		var wasAbandoned bool
		var playerIDs []uint64
		var mapID uint32
		var gameMode string
		// KEEPTTL:标记 abandoned / 每轮重试不刷新 battle key TTL,保证 BattleTTL 是补偿重试上界。
		lerr := u.repo.UpdateBattleKeepTTL(ctx, mid, updateMaxRetry, func(b *dsv1.BattleStorageRecord) error {
			if b.State == stateEnded {
				endedSkip = true // 正常结算,移出 active 不补偿
				return nil
			}
			wasAbandoned = b.State == stateAbandoned // 已 abandoned 仅重试投递,不重复回收 pod
			b.State = stateAbandoned
			podName = b.DsPodName
			playerIDs = b.PlayerIds
			mapID = b.MapId
			gameMode = b.GameMode
			return nil
		})
		if lerr != nil {
			if errcode.As(lerr) == errcode.ErrDSPodNotFound {
				_ = u.repo.RemoveActive(ctx, mid) // 镜像 TTL 过期:清理残留 active(补偿重试的天然上界)
				continue
			}
			plog.With(ctx).Warnw("msg", "sweep_lock_failed", "match_id", mid, "err", lerr)
			continue
		}
		if endedSkip {
			_ = u.repo.RemoveActive(ctx, mid)
			continue
		}
		// 仅首次转入 abandoned 时回收 pod(避免补偿重试期间对同一 pod 重复 Release)
		if !wasAbandoned {
			if rerr := u.alloc.Release(ctx, podName); rerr != nil {
				plog.With(ctx).Warnw("msg", "sweep_release_failed", "match_id", mid, "pod", podName, "err", rerr)
			}
			plog.With(ctx).Infow("msg", "battle_abandoned_heartbeat_timeout", "match_id", mid, "pod", podName)
		}
		// 投递 abandoned 补偿事件:成功(或未配 kafka 的 best-effort 回退)才移出 active;
		// 失败则保留在 active,下一轮 sweep 重试(可靠补偿,不变量 §4)。
		if u.deliverAbandoned(ctx, mid, podName, playerIDs, mapID, gameMode) {
			// 终态镜像保留一段供查询,移出 active 不再扫描
			if eerr := u.repo.ExpireBattle(ctx, mid, u.battleTTL()); eerr != nil {
				plog.With(ctx).Warnw("msg", "sweep_expire_failed", "match_id", mid, "err", eerr)
			}
		}
	}
	return nil
}

// deliverAbandoned 发 DSLifecycleEvent{phase=ABANDONED} 给 battle_result 做玩家段位回滚补偿。
//
// 返回值语义(给 sweepOnce 决定是否移出 active):
//   - true  → 可移出 active:已成功投递,或未配置 kafka(无补偿通道)走 best-effort 回退。
//   - false → 投递失败,保留在 active 下一轮 sweep 重试(可靠补偿,不变量 §4)。
//
// 未配置 kafka 时返回 true 而非把对局永久卡在 active:此时显式选择了「无补偿通道」,
// abandoned 镜像仍落 Redis 供查;若卡在 active 只会每轮 sweep 重复回收且无人消费。
func (u *AllocatorUsecase) deliverAbandoned(ctx context.Context, matchID uint64, podName string, playerIDs []uint64, mapID uint32, gameMode string) bool {
	if u.lifecycle == nil {
		return true // 未配置补偿通道:best-effort 回退,直接移出 active
	}
	evt := &dsv1.DSLifecycleEvent{
		MatchId:   matchID,
		DsPodName: podName,
		Phase:     dsv1.DSLifecyclePhase_DS_LIFECYCLE_PHASE_ABANDONED,
		PlayerIds: playerIDs,
		MapId:     mapID,
		GameMode:  gameMode,
		TsMs:      time.Now().UnixMilli(),
	}
	if err := u.lifecycle.PublishLifecycle(ctx, evt); err != nil {
		// 保留在 active,下轮 sweep 重试(穿越 Kafka 临时不可用)
		plog.With(ctx).Warnw("msg", "ds_lifecycle_publish_failed_will_retry", "match_id", matchID, "err", err)
		return false
	}
	plog.With(ctx).Infow("msg", "ds_lifecycle_published", "match_id", matchID)
	return true
}
