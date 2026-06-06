// Package biz 是 hub_allocator 服务的业务逻辑层(W4 ⑤,2026-06-06)。
//
// 职责(docs/design/go-services.md §2.12):大厅 DS 分片调度。
//   - AssignHub:玩家进大厅,按 region + 队友 + 最空分片选一个 hub DS,签 hub DSTicket
//   - ReleaseHub:玩家离开大厅,退分片占位
//   - TransferHub:跨分片传送,先占新分片再退旧分片,重签票据
//   - ListHubs:运维/调试查询分片负载
//   - Heartbeat:Hub DS 每 5s 主动上报(单向 unary),刷新在线数 + 心跳时刻
//   - RunHeartbeatSweep:后台扫描 active ZSET,心跳超时 → 标记 draining 停止分配(不变量 §4)
//
// 关键不变量:
//   - 玩家在线只在一个 hub(不变量 §1,GetAssignment 幂等;已分配 → 重签票不重复占位)
//   - hub DSTicket 短时效(不变量 §3,由 TicketSigner 经 pkg/auth 签 5min)
//
// 容量计数说明:player_count 由 hub_allocator 维护(Assign 自增 / Release 自减,容量判定基准);
// 真实 Hub DS Heartbeat 上报的在线数会回写对账(W4 ⑤ Mock 期无真实 DS,仅由分配计数维护)。
package biz

import (
	"context"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"

	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/data"
)

// 分片状态常量(对应 proto string state 字段)。
const (
	stateReady    = "ready"
	stateDraining = "draining"
)

// Heartbeat 响应控制指令常量。
const (
	commandNone = ""
	commandStop = "stop" // 通知孤儿 Hub DS(无对应分片镜像)自行停机
)

// TicketSigner 抽象 hub DSTicket 签发(biz 不依赖 pkg/auth 具体实现,便于测试)。
type TicketSigner interface {
	// SignHubTicket 给 playerID 签一张 hub DSTicket,返回 token + 过期毫秒。
	SignHubTicket(playerID uint64) (token string, expiresAtMs int64, err error)
}

// HubUsecase 是 hub_allocator 业务逻辑核心。
type HubUsecase struct {
	repo   data.HubRepo
	fleet  HubFleetProvider
	signer TicketSigner
	cfg    conf.HubConf
}

// NewHubUsecase 构造 HubUsecase。
func NewHubUsecase(repo data.HubRepo, fleet HubFleetProvider, signer TicketSigner, cfg conf.HubConf) *HubUsecase {
	return &HubUsecase{repo: repo, fleet: fleet, signer: signer, cfg: cfg}
}

func (u *HubUsecase) shardTTL() time.Duration  { return u.cfg.ShardTTL.Std() }
func (u *HubUsecase) assignTTL() time.Duration { return u.cfg.AssignmentTTL.Std() }
func (u *HubUsecase) retry() int               { return u.cfg.OptimisticRetry }

// ── RPC 1:AssignHub ───────────────────────────────────────────────────────────

// AssignResult 是 AssignHub 的出参。
type AssignResult struct {
	HubDSAddr   string
	HubTicket   string
	HubPodName  string
	ShardID     uint32
	TicketExpMs int64
}

// AssignHub 为玩家分配一个大厅 DS 分片。幂等:已分配且分片可用 → 重签票返回。
func (u *HubUsecase) AssignHub(ctx context.Context, playerID uint64, region string, teamID uint64) (*AssignResult, error) {
	if playerID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if region == "" {
		region = u.cfg.DefaultRegion
	}

	// 1. 幂等:已有归属且分片仍 ready → 重签票返回(不重复占位,落不变量 §1)
	if existing, found, err := u.repo.GetAssignment(ctx, playerID); err != nil {
		return nil, err
	} else if found {
		if shard, ok, gerr := u.repo.GetShard(ctx, existing.HubPodName); gerr == nil && ok && shard.State == stateReady {
			return u.signResult(ctx, playerID, shard)
		}
		// 旧分片下线/漂移:退旧占位后重新分配
		u.releaseFromShard(ctx, existing.HubPodName)
	}

	// 2. 确保 region 有候选分片(空则按 Fleet 拓扑种子)
	if err := u.ensureShards(ctx, region); err != nil {
		return nil, err
	}

	// 3. 选分片:队友所在分片优先,否则最空 ready 分片
	target, err := u.selectShard(ctx, region, teamID)
	if err != nil {
		return nil, err
	}

	// 4. 占位(乐观锁内复核 ready + 容量)
	if rerr := u.reserveSeat(ctx, target.HubPodName); rerr != nil {
		return nil, rerr
	}

	// 5. 写玩家归属(不变量 §1)+ 队友同分片提示
	now := time.Now().UnixMilli()
	assignment := &hubv1.HubAssignmentStorageRecord{
		PlayerId:     playerID,
		HubPodName:   target.HubPodName,
		HubAddr:      target.HubAddr,
		ShardId:      target.ShardId,
		Region:       region,
		TeamId:       teamID,
		AssignedAtMs: now,
	}
	if serr := u.repo.SetAssignment(ctx, assignment, u.assignTTL()); serr != nil {
		u.releaseFromShard(ctx, target.HubPodName) // 回滚占位避免泄漏
		return nil, serr
	}
	if teamID != 0 {
		if terr := u.repo.SetTeamShard(ctx, teamID, target.HubPodName, u.assignTTL()); terr != nil {
			plog.With(ctx).Warnw("msg", "set_team_shard_failed", "team_id", teamID, "err", terr)
		}
	}

	plog.With(ctx).Infow("msg", "hub_assigned",
		"player_id", playerID, "pod", target.HubPodName, "shard_id", target.ShardId, "region", region)
	return u.signResult(ctx, playerID, target)
}

// ── RPC 2:ReleaseHub ──────────────────────────────────────────────────────────

// ReleaseHub 玩家离开大厅,退分片占位 + 删归属。幂等:无归属视为已离开。
func (u *HubUsecase) ReleaseHub(ctx context.Context, playerID uint64) error {
	if playerID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	assignment, found, err := u.repo.GetAssignment(ctx, playerID)
	if err != nil {
		return err
	}
	if !found {
		return nil // 幂等
	}
	u.releaseFromShard(ctx, assignment.HubPodName)
	if derr := u.repo.DeleteAssignment(ctx, playerID); derr != nil {
		return derr
	}
	plog.With(ctx).Infow("msg", "hub_released", "player_id", playerID, "pod", assignment.HubPodName)
	return nil
}

// ── RPC 3:TransferHub ─────────────────────────────────────────────────────────

// TransferResult 是 TransferHub 的出参。
type TransferResult struct {
	NewHubDSAddr  string
	NewHubTicket  string
	NewHubPodName string
	TicketExpMs   int64
}

// TransferHub 跨分片传送:先占新分片(失败不动旧分片),再退旧分片,重签票据。
func (u *HubUsecase) TransferHub(ctx context.Context, playerID uint64, targetHubID uint64) (*TransferResult, error) {
	if playerID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	assignment, found, err := u.repo.GetAssignment(ctx, playerID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errcode.New(errcode.ErrHubTransferFailed, "player %d not in any hub", playerID)
	}

	shards, err := u.repo.ListShards(ctx)
	if err != nil {
		return nil, err
	}
	target := selectTransferTarget(shards, assignment, targetHubID)
	if target == nil {
		return nil, errcode.New(errcode.ErrHubTransferFailed,
			"no ready target shard for player %d (target_hub_id=%d)", playerID, targetHubID)
	}

	// 已在目标分片 → 仅重签票
	if target.HubPodName == assignment.HubPodName {
		return u.transferResult(ctx, playerID, target)
	}

	// 先占新分片(失败不动旧分片)
	if rerr := u.reserveSeat(ctx, target.HubPodName); rerr != nil {
		return nil, errcode.New(errcode.ErrHubTransferFailed,
			"reserve target shard %s failed: %v", target.HubPodName, rerr)
	}
	// 退旧分片
	u.releaseFromShard(ctx, assignment.HubPodName)

	now := time.Now().UnixMilli()
	newAssignment := &hubv1.HubAssignmentStorageRecord{
		PlayerId:     playerID,
		HubPodName:   target.HubPodName,
		HubAddr:      target.HubAddr,
		ShardId:      target.ShardId,
		Region:       target.Region,
		TeamId:       assignment.TeamId,
		AssignedAtMs: now,
	}
	if serr := u.repo.SetAssignment(ctx, newAssignment, u.assignTTL()); serr != nil {
		u.releaseFromShard(ctx, target.HubPodName) // 回滚新占位
		return nil, serr
	}
	plog.With(ctx).Infow("msg", "hub_transferred",
		"player_id", playerID, "from", assignment.HubPodName, "to", target.HubPodName)
	return u.transferResult(ctx, playerID, target)
}

// ── RPC 4:ListHubs ────────────────────────────────────────────────────────────

// ListHubs 列出分片负载,region 非空时过滤。
func (u *HubUsecase) ListHubs(ctx context.Context, region string) ([]*hubv1.HubInfo, error) {
	shards, err := u.repo.ListShards(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*hubv1.HubInfo, 0, len(shards))
	for _, s := range shards {
		if region != "" && s.Region != region {
			continue
		}
		out = append(out, &hubv1.HubInfo{
			HubPodName:  s.HubPodName,
			HubAddr:     s.HubAddr,
			Region:      s.Region,
			PlayerCount: s.PlayerCount,
			Capacity:    s.Capacity,
			State:       s.State,
		})
	}
	return out, nil
}

// ── RPC 5:Heartbeat ───────────────────────────────────────────────────────────

// HeartbeatResult 是 Heartbeat 的出参(下发给 Hub DS 的控制指令)。
type HeartbeatResult struct {
	Command string
}

// Heartbeat 处理 Hub DS 上报(单向 unary,DS 每 5s 调)。刷新在线数 + 心跳时刻。
// 分片镜像不存在(孤儿 DS)→ 返回 stop 指令让其自行停机。
func (u *HubUsecase) Heartbeat(ctx context.Context, pod string, playerCount int32, state string, tsMs int64) (*HeartbeatResult, error) {
	if pod == "" {
		return nil, errcode.New(errcode.ErrInvalidArg, "hub_pod_name required")
	}
	if tsMs <= 0 {
		tsMs = time.Now().UnixMilli()
	}
	found, err := u.repo.HeartbeatShard(ctx, pod, playerCount, state, tsMs, u.shardTTL())
	if err != nil {
		return nil, err
	}
	if !found {
		plog.With(ctx).Warnw("msg", "heartbeat_orphan_hub", "pod", pod)
		return &HeartbeatResult{Command: commandStop}, nil
	}
	return &HeartbeatResult{Command: commandNone}, nil
}

// ── 后台心跳超时扫描 ──────────────────────────────────────────────────────────

// RunHeartbeatSweep 启动后台心跳超时扫描,直到 ctx 取消(不变量 §4)。
func (u *HubUsecase) RunHeartbeatSweep(ctx context.Context) {
	ticker := time.NewTicker(u.cfg.SweepInterval.Std())
	defer ticker.Stop()
	plog.With(ctx).Infow("msg", "hub_heartbeat_sweep_started",
		"interval", u.cfg.SweepInterval.String(), "timeout", u.cfg.HeartbeatTimeout.String())
	for {
		select {
		case <-ctx.Done():
			plog.With(ctx).Infow("msg", "hub_heartbeat_sweep_stopped")
			return
		case <-ticker.C:
			if err := u.sweepOnce(ctx); err != nil {
				plog.With(ctx).Warnw("msg", "hub_heartbeat_sweep_failed", "err", err)
			}
		}
	}
}

// sweepOnce 扫描一次:last_heartbeat_ms 早于阈值的分片 → 标记 draining + 移出 active(停止分配)。
// 注意:从未心跳的 Mock 种子分片(score=0)被 RangeStaleShards 排除,不会被误标 draining。
func (u *HubUsecase) sweepOnce(ctx context.Context) error {
	threshold := time.Now().Add(-u.cfg.HeartbeatTimeout.Std()).UnixMilli()
	stale, err := u.repo.RangeStaleShards(ctx, threshold)
	if err != nil {
		return err
	}
	for _, pod := range stale {
		lerr := u.repo.UpdateShardWithLock(ctx, pod, u.retry(), func(s *hubv1.HubShardStorageRecord) error {
			if s.State == stateReady {
				s.State = stateDraining // 心跳超时:停止向其分配新玩家
			}
			return nil
		}, u.shardTTL())
		if lerr != nil && errcode.As(lerr) != errcode.ErrHubNoAvailable {
			plog.With(ctx).Warnw("msg", "sweep_mark_draining_failed", "pod", pod, "err", lerr)
		}
		if rerr := u.repo.RemoveActive(ctx, pod); rerr != nil {
			plog.With(ctx).Warnw("msg", "sweep_remove_active_failed", "pod", pod, "err", rerr)
		}
		plog.With(ctx).Warnw("msg", "hub_shard_heartbeat_timeout", "pod", pod)
	}
	return nil
}

// ── 内部辅助 ──────────────────────────────────────────────────────────────────

// ensureShards:region 无候选分片时,按 Fleet 拓扑种入 Redis(W4 ⑤ Mock 期 lazy-seed)。
func (u *HubUsecase) ensureShards(ctx context.Context, region string) error {
	shards, err := u.repo.ListShards(ctx)
	if err != nil {
		return err
	}
	for _, s := range shards {
		if s.Region == region {
			return nil // 已有该 region 分片
		}
	}
	cands, err := u.fleet.ListShards(ctx, region)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, c := range cands {
		rec := &hubv1.HubShardStorageRecord{
			HubPodName:      c.PodName,
			HubAddr:         c.Addr,
			Region:          c.Region,
			ShardId:         c.ShardID,
			PlayerCount:     0,
			Capacity:        c.Capacity,
			State:           stateReady,
			LastHeartbeatMs: 0, // Mock 种子:从未心跳(扫描排除)
			CreatedAtMs:     now,
		}
		if cerr := u.repo.CreateShard(ctx, rec, u.shardTTL()); cerr != nil {
			return cerr
		}
	}
	return nil
}

// selectShard:队友所在分片优先,否则同 region 最空 ready 分片(并列取 shard_id 小者,稳定)。
func (u *HubUsecase) selectShard(ctx context.Context, region string, teamID uint64) (*hubv1.HubShardStorageRecord, error) {
	shards, err := u.repo.ListShards(ctx)
	if err != nil {
		return nil, err
	}
	if teamID != 0 {
		if pod, ok, gerr := u.repo.GetTeamShard(ctx, teamID); gerr == nil && ok {
			for _, s := range shards {
				if s.HubPodName == pod && s.Region == region && s.State == stateReady && s.PlayerCount < s.Capacity {
					return s, nil
				}
			}
		}
	}
	best := leastLoaded(shards, region, "")
	if best == nil {
		return nil, errcode.New(errcode.ErrHubNoAvailable, "no ready hub shard with capacity in region %s", region)
	}
	return best, nil
}

// reserveSeat:乐观锁占一个座位(复核 ready + 容量,player_count++)。
func (u *HubUsecase) reserveSeat(ctx context.Context, pod string) error {
	return u.repo.UpdateShardWithLock(ctx, pod, u.retry(), func(s *hubv1.HubShardStorageRecord) error {
		if s.State != stateReady {
			return errcode.New(errcode.ErrHubNoAvailable, "hub shard %s not ready", pod)
		}
		if s.PlayerCount >= s.Capacity {
			return errcode.New(errcode.ErrHubNoAvailable, "hub shard %s full", pod)
		}
		s.PlayerCount++
		return nil
	}, u.shardTTL())
}

// releaseFromShard:退一个座位(floor 0)。分片不存在/锁冲突静默(幂等退位)。
func (u *HubUsecase) releaseFromShard(ctx context.Context, pod string) {
	err := u.repo.UpdateShardWithLock(ctx, pod, u.retry(), func(s *hubv1.HubShardStorageRecord) error {
		if s.PlayerCount > 0 {
			s.PlayerCount--
		}
		return nil
	}, u.shardTTL())
	if err != nil && errcode.As(err) != errcode.ErrHubNoAvailable {
		plog.With(ctx).Warnw("msg", "release_from_shard_failed", "pod", pod, "err", err)
	}
}

func (u *HubUsecase) signResult(ctx context.Context, playerID uint64, shard *hubv1.HubShardStorageRecord) (*AssignResult, error) {
	token, expMs, err := u.signer.SignHubTicket(playerID)
	if err != nil {
		plog.With(ctx).Errorw("msg", "sign_hub_ticket_failed", "player_id", playerID, "err", err)
		return nil, errcode.New(errcode.ErrInternal, "sign hub ticket failed")
	}
	return &AssignResult{
		HubDSAddr:   shard.HubAddr,
		HubTicket:   token,
		HubPodName:  shard.HubPodName,
		ShardID:     shard.ShardId,
		TicketExpMs: expMs,
	}, nil
}

func (u *HubUsecase) transferResult(ctx context.Context, playerID uint64, shard *hubv1.HubShardStorageRecord) (*TransferResult, error) {
	token, expMs, err := u.signer.SignHubTicket(playerID)
	if err != nil {
		plog.With(ctx).Errorw("msg", "sign_hub_ticket_failed", "player_id", playerID, "err", err)
		return nil, errcode.New(errcode.ErrInternal, "sign hub ticket failed")
	}
	return &TransferResult{
		NewHubDSAddr:  shard.HubAddr,
		NewHubTicket:  token,
		NewHubPodName: shard.HubPodName,
		TicketExpMs:   expMs,
	}, nil
}

// selectTransferTarget:targetHubID!=0 点名 shard_id 匹配的分片;否则同 region 最空「非当前」ready 分片。
func selectTransferTarget(shards []*hubv1.HubShardStorageRecord, cur *hubv1.HubAssignmentStorageRecord, targetHubID uint64) *hubv1.HubShardStorageRecord {
	if targetHubID != 0 {
		want := uint32(targetHubID)
		for _, s := range shards {
			if s.ShardId == want && s.Region == cur.Region && s.State == stateReady && s.PlayerCount < s.Capacity {
				return s
			}
		}
		return nil
	}
	return leastLoaded(shards, cur.Region, cur.HubPodName)
}

// leastLoaded:返回 region 内最空的 ready 且未满分片;excludePod 非空时排除它。并列取 shard_id 小者。
func leastLoaded(shards []*hubv1.HubShardStorageRecord, region, excludePod string) *hubv1.HubShardStorageRecord {
	var best *hubv1.HubShardStorageRecord
	for _, s := range shards {
		if s.Region != region || s.State != stateReady || s.PlayerCount >= s.Capacity {
			continue
		}
		if excludePod != "" && s.HubPodName == excludePod {
			continue
		}
		if best == nil || s.PlayerCount < best.PlayerCount ||
			(s.PlayerCount == best.PlayerCount && s.ShardId < best.ShardId) {
			best = s
		}
	}
	return best
}
