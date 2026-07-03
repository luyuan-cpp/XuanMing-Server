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
	"errors"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"

	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/data"
)

// errHeartbeatTerminal 是 Heartbeat 在镜像已是终态(ended/abandoned)时,
// 从乐观锁回调里返回的哨兵错误:中止写回(不刷新 LastHeartbeatMs / TTL / active score),
// 由 Heartbeat 捕获后转成 stop 指令。保证 abandoned 后 DS 继续心跳不会推迟补偿重试、
// 不会刷新 BattleTTL 上界(W4 ⑧ Codex 复审 P1)。
var errHeartbeatTerminal = errors.New("heartbeat on terminal battle")

// errHeartbeatPodMismatch:Heartbeat 上报的 DsPodName 与镜像里记录的不一致(旧 DS / 孤儿 DS /
// 重分配后残留的上一个 pod)。从乐观锁回调返回此哨兵 → 不写回该镜像,并令上报方停机,
// 避免污染新对局的状态(LastHeartbeatMs / state / player_count)。
var errHeartbeatPodMismatch = errors.New("heartbeat pod mismatch")

// errReadyWaitTimeout:AllocateBattle 等待 DS ready 心跳超时的哨兵,由 waitBattleReady 返回,
// 调用方据此走回收 pod + 删镜像 + 返回 ErrDSAllocationFailed 的清理路径。
var errReadyWaitTimeout = errors.New("ready wait timeout")

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

// readyPollInterval 是 AllocateBattle 等待 DS ready 心跳时轮询 Redis 镜像的间隔。
// 1s 足够:DS 心跳 5s 一跳,ready 等待窗口 10s,1s 轮询既不漏判也不给 Redis 添压。
// 用 var 而非 const,便于单测把它调小以避免慢测(见 allocator_test.go init)。
var readyPollInterval = 1 * time.Second

// detachedCleanupTimeout 是 ready 等待失败后回收 pod + 删镜像的独立 ctx 预算。
// ready 等待失败的常见原因正是入站 ctx 被取消/超时,复用它做 Release/DeleteBattle 会立刻
// 失败,留下 warming 镜像 + 已分配 pod 泄漏;故清理用一个与入站 ctx 解耦的短超时 ctx。
const detachedCleanupTimeout = 5 * time.Second

// locationRefreshTimeout 是心跳后异步续期玩家 BATTLE 位置的独立 ctx 预算(断线重连,弱依赖)。
// 短超时防 locator 卡死时后台续期 goroutine 泄漏。
const locationRefreshTimeout = 3 * time.Second

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

// LocationRefresher 续期玩家 BATTLE 位置 TTL(断线重连,docs/design/battle-reconnect.md §2.2)。
//
// 心跳成功且对局处于 ready/running 时,ds_allocator 用它把该对局玩家的位置刷新为 BATTLE
// (同 match_id 续期,BATTLE→BATTLE),使玩家整局在线期间 login 都能检测到"在战斗中",
// 从而支持中途掉线重登直连回原 battle DS。
//
// 由 player_locator gRPC 客户端实现;可为 nil(未配 locator_addr → 不续期,弱依赖,
// 不影响心跳 / 对局,仅长对局中途重登可能因位置过期退化为回大厅)。
type LocationRefresher interface {
	RefreshBattleLocations(ctx context.Context, playerIDs []uint64, matchID uint64, dsAddr string) error
}

// AllocatorUsecase 是 ds_allocator 业务逻辑核心。
type AllocatorUsecase struct {
	repo      data.BattleRepo
	alloc     GameServerAllocator
	cfg       conf.AllocatorConf
	lifecycle DSLifecyclePusher // 可为 nil(kafka 不可用时静默不发 abandoned 事件)
	locator   LocationRefresher // 可为 nil(未配 locator_addr 时不续期 BATTLE 位置)

	// killOrphanOnStop:心跳判定某 DS 该停机(orphan / pod_mismatch / 终态)时,是否由后端主动
	// 回收该 pod。local 模式打开——本机 UE DS 没有 Agones,收到 stop 指令不会自杀,残留进程会
	// 幽灵般占着监听端口污染下一局;打开后 stop 时异步调 alloc.Release kill 掉它。Agones 模式默认
	// 关闭:孤儿 GameServer 由 Agones 生命周期回收,避免 Redis 抖动误判 orphan 时误删正常 pod。
	killOrphanOnStop bool
}

// NewAllocatorUsecase 构造 AllocatorUsecase。
func NewAllocatorUsecase(repo data.BattleRepo, alloc GameServerAllocator, cfg conf.AllocatorConf) *AllocatorUsecase {
	return &AllocatorUsecase{repo: repo, alloc: alloc, cfg: cfg}
}

// SetLifecyclePusher 注入 ds.lifecycle 事件发送器(main 在 kafka 就绪时调用,弱依赖)。
func (u *AllocatorUsecase) SetLifecyclePusher(p DSLifecyclePusher) { u.lifecycle = p }

// SetLocationRefresher 注入 BATTLE 位置续期器(main 在 locator_addr 已配时调用,弱依赖)。
func (u *AllocatorUsecase) SetLocationRefresher(r LocationRefresher) { u.locator = r }

// SetKillOrphanOnStop 打开「心跳 stop 时主动回收该 DS」(main 在 mode=local 时调用)。
// 见 killOrphanOnStop 字段说明:local 模式的 UE DS 收到 stop 不自杀,需后端主动 kill 防幽灵占端口。
func (u *AllocatorUsecase) SetKillOrphanOnStop(v bool) { u.killOrphanOnStop = v }

// killStrandedDS 在心跳判定某 DS 该停机时,异步回收其 pod(local 模式防幽灵 DS 占端口)。
// fire-and-forget:用 detached ctx(保留 trace_id 满足不变量 §8)+ 短超时,不给心跳响应加尾延迟。
// killOrphanOnStop 关闭(Agones 模式)时为 no-op,pod 回收交 Agones 生命周期。
func (u *AllocatorUsecase) killStrandedDS(ctx context.Context, matchID uint64, podName, reason string) {
	if !u.killOrphanOnStop || podName == "" {
		return
	}
	go func() {
		kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detachedCleanupTimeout)
		defer cancel()
		if err := u.alloc.Release(kctx, podName); err != nil {
			plog.With(kctx).Warnw("msg", "kill_stranded_ds_failed", "match_id", matchID, "pod", podName, "reason", reason, "err", err)
			return
		}
		plog.With(kctx).Infow("msg", "kill_stranded_ds", "match_id", matchID, "pod", podName, "reason", reason)
	}()
}

func (u *AllocatorUsecase) battleTTL() time.Duration { return u.cfg.BattleTTL.Std() }

// readyWaitTimeout 是 AllocateBattle 等待 DS ready 心跳的最长时间(默认 10s)。
func (u *AllocatorUsecase) readyWaitTimeout() time.Duration { return u.cfg.ReadyWaitTimeout.Std() }

// ── RPC 1:AllocateBattle ──────────────────────────────────────────────────────

// AllocateResult 是 AllocateBattle 的出参。
type AllocateResult struct {
	DSAddr        string
	DSPodName     string
	AllocatedAtMs int64
}

// AllocateBattle 为 match 申请战斗 DS。
//
// 关键:Agones Allocated(pod 被分配)≠ 战斗 DS Ready。DS 进程要先读到 pandora.dev/match-id
// 才能在 PreLogin 放行客户端票据。所以这里不再一拿到 pod 就回 ds_addr,而是:
//
//	Allocate → CreateBattle(state=warming) → 轮询等 DS Heartbeat 上报正确 match_id/pod 且
//	进入 ready/running → 回 ds_addr;ReadyWaitTimeout 内没等到 → 回收 pod + 删镜像 + 分配失败。
//
// 用 Redis 镜像轮询(而非内存 channel):Heartbeat RPC 可能落到另一个 ds_allocator pod,
// 只有共享的 Redis 镜像能跨 pod 观察到 DS 的就绪心跳。
//
// 幂等:同 match_id 已有镜像时——ready/running 且有有效心跳 → 直接回;warming → 继续等 ready;
// 终态/不可用 → 返回分配失败(绝不把 ds_addr 回给 matchmaker)。
func (u *AllocatorUsecase) AllocateBattle(ctx context.Context, matchID uint64, playerIDs []uint64, mapID uint32, gameMode string) (*AllocateResult, error) {
	if matchID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "match_id required")
	}

	// 幂等:已有镜像 → 按状态决定直接回 / 继续等 ready / 判失败(防 matchmaker 重试重复拉 DS)
	if existing, found, err := u.repo.GetBattle(ctx, matchID); err != nil {
		return nil, err
	} else if found {
		switch {
		case battleReadyForPod(existing, existing.DsPodName, matchID, existing.AllocatedAtMs):
			// ready/running 且已有分配后的有效心跳 → 直接回已分配地址
			plog.With(ctx).Infow("msg", "allocate_idempotent_hit", "match_id", matchID, "ds_addr", existing.DsAddr, "state", existing.State)
			return &AllocateResult{DSAddr: existing.DsAddr, DSPodName: existing.DsPodName, AllocatedAtMs: existing.AllocatedAtMs}, nil
		case existing.State == stateWarming:
			// warming:DS 还没用心跳确认 ready,继续等 ready 心跳
			plog.With(ctx).Infow("msg", "allocate_idempotent_warming", "match_id", matchID, "pod", existing.DsPodName)
			res, werr := u.waitBattleReady(ctx, matchID, existing.DsPodName, existing.AllocatedAtMs)
			if werr != nil {
				if errors.Is(werr, errReadyWaitTimeout) {
					return nil, u.failReadyWaitTimeout(ctx, matchID, existing.DsPodName)
				}
				return nil, werr
			}
			plog.With(ctx).Infow("msg", "battle_ready_after_heartbeat", "match_id", matchID, "pod", existing.DsPodName)
			return res, nil
		default:
			// 终态(ended/abandoned)等不可用状态:不把 ds_addr 回给 matchmaker
			plog.With(ctx).Warnw("msg", "allocate_idempotent_unusable", "match_id", matchID, "state", existing.State)
			return nil, errcode.New(errcode.ErrDSAllocationFailed, "battle %d in state %s, not allocatable", matchID, existing.State)
		}
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
		State:           stateWarming, // 等 DS 心跳确认 ready 才回 matchmaker;不把 Agones Allocated 当成 ready
		PlayerIds:       playerIDs,
		MapId:           mapID,
		GameMode:        gameMode,
		AllocatedAtMs:   now,
		LastHeartbeatMs: now, // 仅作 sweep 宽限基准;ready 判定要求 LastHeartbeatMs 严格大于此(即真实心跳)
		PlayerCount:     int32(len(playerIDs)),
	}
	if err := u.repo.CreateBattle(ctx, battle, u.battleTTL()); err != nil {
		// 镜像写失败:回收已分配 pod 避免泄漏
		if rerr := u.alloc.Release(ctx, podName); rerr != nil {
			plog.With(ctx).Warnw("msg", "rollback_release_failed", "pod", podName, "err", rerr)
		}
		return nil, err
	}

	plog.With(ctx).Infow("msg", "battle_warming", "match_id", matchID, "pod", podName, "ds_addr", addr, "players", len(playerIDs))

	// 等 DS 用正确 match_id/pod 的心跳上报 ready/running,后端才把 ds_addr 回给 matchmaker。
	res, werr := u.waitBattleReady(ctx, matchID, podName, now)
	if werr != nil {
		if errors.Is(werr, errReadyWaitTimeout) {
			return nil, u.failReadyWaitTimeout(ctx, matchID, podName)
		}
		// 入站 ctx 取消/超时或 repo 出错等非超时失败:本次刚分配的 pod 由本调用持有,
		// 用独立 cleanup ctx 回收 pod + 删 warming 镜像,避免泄漏(入站 ctx 多半已失效)。
		u.cleanupAllocatedBattle(ctx, matchID, podName)
		return nil, werr
	}

	plog.With(ctx).Infow("msg", "battle_ready_after_heartbeat", "match_id", matchID, "pod", podName, "ds_addr", addr)
	return res, nil
}

// battleReadyForPod 判定 DS 是否已用 Heartbeat 确认 ready:pod/match 对得上、有分配后的真实心跳
// (LastHeartbeatMs 严格大于 allocatedAtMs)、状态进入 ready 或 running。
// 当前 UE 侧上报的是 running(不一定先发 ready),所以后端先把 running 也视为可进入状态。
func battleReadyForPod(b *dsv1.BattleStorageRecord, podName string, matchID uint64, allocatedAtMs int64) bool {
	return b != nil &&
		b.MatchId == matchID &&
		b.DsPodName == podName &&
		b.LastHeartbeatMs > allocatedAtMs &&
		(b.State == stateReady || b.State == stateRunning)
}

// waitBattleReady 轮询 Redis 镜像直到 DS 心跳确认 ready,或 ReadyWaitTimeout 超时(返回 errReadyWaitTimeout)。
func (u *AllocatorUsecase) waitBattleReady(ctx context.Context, matchID uint64, podName string, allocatedAtMs int64) (*AllocateResult, error) {
	deadline := time.Now().Add(u.readyWaitTimeout())
	ticker := time.NewTicker(readyPollInterval)
	defer ticker.Stop()
	for {
		b, found, err := u.repo.GetBattle(ctx, matchID)
		if err != nil {
			return nil, err
		}
		if found && battleReadyForPod(b, podName, matchID, allocatedAtMs) {
			return &AllocateResult{DSAddr: b.DsAddr, DSPodName: b.DsPodName, AllocatedAtMs: b.AllocatedAtMs}, nil
		}
		if !time.Now().Before(deadline) {
			return nil, errReadyWaitTimeout
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// failReadyWaitTimeout 处理 ready 等待超时:回收 pod + 删镜像,返回 ErrDSAllocationFailed
// (绝不把 ds_addr 回给 matchmaker,否则客户端连上 match_id 仍为 0 的 DS 会被 PreLogin 拒)。
func (u *AllocatorUsecase) failReadyWaitTimeout(ctx context.Context, matchID uint64, podName string) error {
	plog.With(ctx).Warnw("msg", "battle_ready_wait_timeout", "match_id", matchID, "pod", podName)
	u.cleanupAllocatedBattle(ctx, matchID, podName)
	return errcode.New(errcode.ErrDSAllocationFailed, "battle %d ds not ready within wait timeout", matchID)
}

// cleanupAllocatedBattle 用与入站 ctx 解耦的独立 ctx 回收已分配 pod + 删镜像。
// 入站 ctx 在 ready 等待失败时多半已被取消/超时,直接复用它做 Release/DeleteBattle 会立刻
// 失败,从而留下 warming 镜像 + 已分配 pod 泄漏;故这里 detach 出一个短超时 ctx 兜底回收。
func (u *AllocatorUsecase) cleanupAllocatedBattle(ctx context.Context, matchID uint64, podName string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detachedCleanupTimeout)
	defer cancel()
	if rerr := u.alloc.Release(cleanupCtx, podName); rerr != nil {
		plog.With(ctx).Warnw("msg", "ready_wait_cleanup_release_failed", "match_id", matchID, "pod", podName, "err", rerr)
	}
	if derr := u.repo.DeleteBattle(cleanupCtx, matchID); derr != nil {
		plog.With(ctx).Warnw("msg", "ready_wait_cleanup_delete_failed", "match_id", matchID, "err", derr)
	}
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
//
// 已是终态(ended/abandoned)的镜像:直接返回 stop,且**不写回记录**——不刷新
// LastHeartbeatMs / TTL,也不重新 ZAdd active。否则 abandoned 后仍在心跳的 DS(pod
// release 失败 / 延迟终止)会不断推迟 sweep 补偿重试并刷新 BattleTTL 上界,使 active
// 重新可能无限堆积(W4 ⑧ Codex 复审 P1)。
func (u *AllocatorUsecase) Heartbeat(ctx context.Context, matchID uint64, podName string, playerCount int32, state string, tsMs int64) (*HeartbeatResult, error) {
	if matchID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "match_id required")
	}
	now := time.Now().UnixMilli()

	var becameReady bool
	// 断线重连(docs/design/battle-reconnect.md §2.2):捕获对局在 ready/running 时的玩家名单 +
	// ds_addr,心跳成功后续期这些玩家的 BATTLE 位置 TTL。回调可能因 CAS 冲突重跑,故每轮重置。
	var refreshActive bool
	var refreshAddr string
	var refreshPlayers []uint64
	err := u.repo.UpdateBattleWithLock(ctx, matchID, updateMaxRetry, func(b *dsv1.BattleStorageRecord) error {
		// 已是终态(ended/abandoned):中止写回(哨兵错误),不刷新 TTL/active,令 DS 停机
		if b.State == stateEnded || b.State == stateAbandoned {
			return errHeartbeatTerminal
		}
		// podName 校验:镜像已绑定某个 pod,但上报方是另一个 pod(旧 DS / 孤儿 DS / 重分配残留)→
		// 不写回该镜像,令上报方停机,避免污染新对局(防进错对局的 DS 刷 state/心跳)。
		if b.DsPodName != "" && podName != "" && b.DsPodName != podName {
			return errHeartbeatPodMismatch
		}
		prevState := b.State
		b.LastHeartbeatMs = now
		b.PlayerCount = playerCount
		if state != "" {
			b.State = state
		}
		// warming → ready/running:DS 首次确认就绪,这一跳让 AllocateBattle 得以放行 matchmaker。
		if prevState == stateWarming && (b.State == stateReady || b.State == stateRunning) {
			becameReady = true
		}
		// 对局活跃(ready/running):记下玩家名单 + ds_addr,供心跳后续期 BATTLE 位置。
		if b.State == stateReady || b.State == stateRunning {
			refreshActive = true
			refreshAddr = b.DsAddr
			refreshPlayers = append(refreshPlayers[:0], b.PlayerIds...)
		}
		return nil
	}, u.battleTTL())

	if err != nil {
		switch {
		case errors.Is(err, errHeartbeatTerminal):
			// 终态 DS:不写回、通知停机,补偿重试与 TTL 上界不受影响
			plog.With(ctx).Infow("msg", "heartbeat_terminal_stop", "match_id", matchID, "pod", podName)
			u.killStrandedDS(ctx, matchID, podName, "terminal")
			return &HeartbeatResult{Command: commandStop}, nil
		case errors.Is(err, errHeartbeatPodMismatch):
			// pod 不匹配:不写回镜像,令旧/孤儿 DS 停机(防污染新对局)
			plog.With(ctx).Warnw("msg", "heartbeat_pod_mismatch", "match_id", matchID, "pod", podName)
			u.killStrandedDS(ctx, matchID, podName, "pod_mismatch")
			return &HeartbeatResult{Command: commandStop}, nil
		case errcode.As(err) == errcode.ErrDSPodNotFound:
			// 孤儿 DS:无镜像,通知停机
			plog.With(ctx).Warnw("msg", "heartbeat_orphan_ds", "match_id", matchID, "pod", podName)
			u.killStrandedDS(ctx, matchID, podName, "orphan")
			return &HeartbeatResult{Command: commandStop}, nil
		default:
			return nil, err
		}
	}
	if becameReady {
		// 验收日志:Battle DS heartbeat match_id=<id> pod=<pod> state=running/ready
		plog.With(ctx).Infow("msg", "battle_ds_heartbeat_ready", "match_id", matchID, "pod", podName, "state", state)
	}
	// 断线重连(docs/design/battle-reconnect.md §2.2):对局活跃时续期玩家 BATTLE 位置 TTL,
	// 使玩家整局在线期间 login 都能检测到"在战斗中",支持中途掉线重登直连回原 battle DS。
	if u.locator != nil && refreshActive && refreshAddr != "" && len(refreshPlayers) > 0 {
		u.refreshBattleLocations(ctx, refreshPlayers, matchID, refreshAddr)
	}
	return &HeartbeatResult{Command: commandNone}, nil
}

// refreshBattleLocations 异步续期一批玩家的 BATTLE 位置 TTL(断线重连,弱依赖)。
//
// fire-and-forget:不给心跳响应加尾延迟;用 detached ctx(保留 trace_id 满足不变量 §8
// 写带 trace_id,但脱离心跳 RPC 取消)+ 短超时防 locator 卡死泄漏 goroutine。
// 失败只 Warn,绝不影响心跳 / 对局。
func (u *AllocatorUsecase) refreshBattleLocations(ctx context.Context, playerIDs []uint64, matchID uint64, dsAddr string) {
	players := append([]uint64(nil), playerIDs...) // 拷贝,脱离调用方切片复用
	go func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), locationRefreshTimeout)
		defer cancel()
		if err := u.locator.RefreshBattleLocations(rctx, players, matchID, dsAddr); err != nil {
			plog.With(rctx).Warnw("msg", "refresh_battle_locations_failed", "match_id", matchID, "err", err)
		}
	}()
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
				endedSkip = true      // 正常结算,移出 active 不补偿
				podName = b.DsPodName // 捕获用于 local 幽灵 DS 收尾(见下方 killStrandedDS)
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
			// 正常结算的 DS 收尾回收(2026-07-03):battle_result 不再在结算响应路径直杀 DS(那会抢在
			// DS 通知客户端回大厅之前把它杀掉),DS 生命周期归此处兜底。DS 发完 ended 心跳后即
			// StopBattleHeartbeat(无第二跳,心跳终态 killStrandedDS 永不触发),且 local 模式 DS 的
			// Agones Shutdown 是 no-op → 进程不会自退。故这里在 ended 且失联(≥HeartbeatTimeout 未心跳,
			// 此时 DS 早已通知客户端回大厅)时主动 taskkill,防幽灵 DS 占端口耗尽端口池。
			// killOrphanOnStop 门控:仅 local 打开;Agones 关(DS 已自身 Shutdown,pod 交 Fleet 回收)。
			u.killStrandedDS(ctx, mid, podName, "ended")
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
