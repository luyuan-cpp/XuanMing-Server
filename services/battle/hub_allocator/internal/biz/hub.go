// Package biz 是 hub_allocator 服务的业务逻辑层(W4 ⑤,2026-06-06)。
//
// 职责(docs/design/go-services.md §2.12):大厅 DS 分片调度。
//   - AssignHub:玩家进大厅,按 region + 队友 + 最空分片选一个 hub DS,签 hub DSTicket
//   - ReleaseHub:玩家离开大厅,退分片占位
//   - TransferHub:跨分片传送,先占新分片再切归属,最后退旧分片,重签票据
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
	"sort"
	"time"

	"google.golang.org/protobuf/proto"

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
	stateStopping = "stopping"
)

// Heartbeat 响应控制指令常量。
const (
	commandNone  = ""
	commandStop  = "stop"  // 通知孤儿 Hub DS(无对应分片镜像)自行停机
	commandDrain = "drain" // 通知 draining 分片上的 Hub DS 开始优雅迁移(下发 grace_seconds 倒计时)
)

// 迁移原因常量(HubMigrateEvent.reason)。
const migrateReasonConsolidation = "consolidation"

// TicketSigner 抽象 hub DSTicket 签发(biz 不依赖 pkg/auth 具体实现,便于测试)。
type TicketSigner interface {
	// SignHubTicket 给 playerID 签一张 hub DSTicket,返回 token + 过期毫秒。
	SignHubTicket(playerID uint64) (token string, expiresAtMs int64, err error)
}

// HubMigratePusher 抽象强制整合迁移通知推送(走 Kafka topic pandora.hub.migrate,key=player_id)。
// 弱依赖:nil 时跳过推送(整合仍做服务端权威搬迁,Hub DS drain 心跳指令兼底客户端重连)。
type HubMigratePusher interface {
	// PushMigrate 把 HubMigrateEvent 序列化后的 payload 推给单个玩家。
	PushMigrate(ctx context.Context, playerID uint64, payload []byte) error
}

// HubUsecase 是 hub_allocator 业务逻辑核心。
type HubUsecase struct {
	repo    data.HubRepo
	fleet   HubFleetProvider
	scaler  HubFleetScaler
	signer  TicketSigner
	migrate HubMigratePusher
	cfg     conf.HubConf
}

// NewHubUsecase 构造 HubUsecase。
func NewHubUsecase(repo data.HubRepo, fleet HubFleetProvider, signer TicketSigner, cfg conf.HubConf) *HubUsecase {
	var scaler HubFleetScaler
	if s, ok := fleet.(HubFleetScaler); ok {
		scaler = s
	}
	return &HubUsecase{repo: repo, fleet: fleet, scaler: scaler, signer: signer, cfg: cfg}
}

// SetMigratePusher 注入强制整合迁移通知推送器(弱依赖,不改 NewHubUsecase 签名以不破现有测试/调用方)。
func (u *HubUsecase) SetMigratePusher(p HubMigratePusher) { u.migrate = p }

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
			u.addShardMember(ctx, existing.HubPodName, playerID) // 自愈成员反向索引 + 刷 TTL
			return u.signResult(ctx, playerID, shard)
		}
		// 旧分片下线/漂移:退旧占位后重新分配
		u.releaseFromShard(ctx, existing.HubPodName)
		u.removeShardMember(ctx, existing.HubPodName, playerID)
	}

	// 2. 确保 region 有候选分片(空则按 Fleet 拓扑种子)
	if err := u.ensureShards(ctx, region); err != nil {
		return nil, err
	}

	// 3. 选分片:队友所在分片优先,否则最空 ready 分片
	target, err := u.selectShard(ctx, region, teamID)
	if err != nil {
		if errcode.As(err) == errcode.ErrHubNoAvailable {
			u.tryScaleOutOnNoCapacity(ctx, region)
		}
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
	u.addShardMember(ctx, target.HubPodName, playerID) // 成员反向索引(强制整合枚举用)
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
	u.removeShardMember(ctx, assignment.HubPodName, playerID)
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

// TransferHub 跨分片传送:先占新分片(失败不动旧分片),再切归属到新分片,最后退旧分片占位,重签票据。
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
	// 在退旧分片之前先把归属切到新分片(顺序:reserve 新 → SetAssignment → release 旧)。
	// 这样 SetAssignment 失败时只需回滚新占位,旧分片 player_count 与旧 assignment 仍一致,
	// 玩家保持在旧 hub(不会出现「旧 assignment 指向旧 pod 但旧 pod 计数已减 1」的悬挂状态)。
	if serr := u.repo.SetAssignment(ctx, newAssignment, u.assignTTL()); serr != nil {
		u.releaseFromShard(ctx, target.HubPodName) // 回滚新占位,旧分片不动
		return nil, serr
	}
	u.addShardMember(ctx, target.HubPodName, playerID)
	// 归属已切到新分片,再退旧分片占位(退位幂等,失败仅 Warn 不影响已切换的归属)
	u.releaseFromShard(ctx, assignment.HubPodName)
	u.removeShardMember(ctx, assignment.HubPodName, playerID)

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
	Command      string
	GraceSeconds int32 // command=="drain"/"stop" 时的优雅迁移倒计时(秒),其余为 0
}

// Heartbeat 处理 Hub DS 上报(单向 unary,DS 每 5s 调)。刷新在线数 + 心跳时刻。
// 分片镜像不存在(孤儿 DS)→ 返回 stop 指令让其自行停机。
// 分片已被强制整合标记 draining → 下发 drain + grace_seconds,Hub DS 引导在场玩家倒计时切大厅。
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
	// 分片被标记 draining/stopping → 下发迁移/停机指令(与 Kafka 推送双通道)。
	if shard, ok, gerr := u.repo.GetShard(ctx, pod); gerr == nil && ok {
		switch shard.State {
		case stateDraining:
			return &HeartbeatResult{Command: commandDrain, GraceSeconds: u.cfg.MigrateGraceSeconds}, nil
		case stateStopping:
			return &HeartbeatResult{Command: commandStop, GraceSeconds: u.cfg.MigrateGraceSeconds}, nil
		}
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
			if err := u.reconcileFleetReplicas(ctx); err != nil {
				plog.With(ctx).Warnw("msg", "hub_reconcile_replicas_failed", "err", err)
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

// autoScaleEnabled 需同时满足:配置开启 + 存在真实 Fleet scaler。
// scaler 只有真 Agones provider(AgonesHubFleetProvider)才实现 HubFleetScaler;
// Mock provider 是拓扑-only 不实现该接口 → Mock 模式下 scaler==nil,
// 自动扩缩容/强制整合恒不运行(不会跑退化 no-op 误导评估)。
func (u *HubUsecase) autoScaleEnabled() bool {
	return u.cfg.AutoScaleEnabled && u.scaler != nil
}

// tryScaleOutOnNoCapacity 在当前 region 无可用分片时触发兜底扩容(+1)。
// 触发后调用方仍会返回 ErrHubNoAvailable,由上游重试进新副本。
func (u *HubUsecase) tryScaleOutOnNoCapacity(ctx context.Context, region string) {
	if !u.autoScaleEnabled() {
		return
	}
	current, err := u.scaler.GetFleetReplicas(ctx)
	if err != nil {
		plog.With(ctx).Warnw("msg", "hub_scaleout_get_replicas_failed", "region", region, "err", err)
		return
	}
	desired := current + 1
	if desired < u.cfg.MinReplicas {
		desired = u.cfg.MinReplicas
	}
	if desired > u.cfg.MaxReplicas {
		desired = u.cfg.MaxReplicas
	}
	if desired == current {
		return
	}
	if err := u.scaler.SetFleetReplicas(ctx, desired); err != nil {
		plog.With(ctx).Warnw("msg", "hub_scaleout_set_replicas_failed",
			"region", region, "current", current, "desired", desired, "err", err)
		return
	}
	plog.With(ctx).Infow("msg", "hub_scaleout_triggered", "region", region, "from", current, "to", desired)
}

// reconcileFleetReplicas 周期性副本治理(每个 sweep tick 调一次):
//   - ① 扩容(立即,仅向上):总在线 > 0 → ceil(total/players_per_hub) > current 时扩容
//   - ② 强制整合(可选,consolidation_enabled):ready 分片多于负载所需 → 排空最空的多余分片,
//     把分片上的玩家做服务端权威搬迁到目标分片,并下发迁移通知(Hub DS drain 心跳 + Kafka 推送双通道)
//   - ③ 回收 + 缩容:已排空且过 grace 的 draining 分片 → 删镜像 + 把 Fleet 副本降到仍需存活的分片数
//
// 缩容到副本数后由 Agones 决定删哪个 GameServer(可能不是被排空那个),这是当前阶段的已知限制
// (docs/design/agones-dev.md):缩容只在 draining 分片已排空且过 grace 后触发,被删 pod 已无在场玩家。
func (u *HubUsecase) reconcileFleetReplicas(ctx context.Context) error {
	if !u.autoScaleEnabled() {
		return nil
	}
	shards, err := u.repo.ListShards(ctx)
	if err != nil {
		return err
	}

	totalPlayers := sumPlayers(shards)

	current, err := u.scaler.GetFleetReplicas(ctx)
	if err != nil {
		return err
	}

	minReplicas := u.cfg.MinReplicas
	maxReplicas := u.cfg.MaxReplicas
	playersPerHub := u.cfg.PlayersPerHub
	if playersPerHub <= 0 {
		playersPerHub = 500
	}

	// 负载所需 ready 分片数(总在线=0 → min)。
	need := minReplicas
	if totalPlayers > 0 {
		need = int32((totalPlayers + int64(playersPerHub) - 1) / int64(playersPerHub))
		if need < minReplicas {
			need = minReplicas
		}
		if need > maxReplicas {
			need = maxReplicas
		}
	}

	// ① 扩容(立即,仅向上)。扩容当 tick 不再缩容,等新 pod ready 后下个 tick 再治理。
	if need > current {
		if serr := u.scaler.SetFleetReplicas(ctx, need); serr != nil {
			return serr
		}
		plog.With(ctx).Infow("msg", "hub_fleet_scaled_out",
			"current", current, "desired", need, "players", totalPlayers,
			"players_per_hub", playersPerHub, "min", minReplicas, "max", maxReplicas)
		return nil
	}

	// ② 排空多余分片(标 draining + 盖 draining_since_ms,统一交 ③ 回收):
	//   - 总在线>0 且开启强制整合:搬迁最空多余分片的玩家到目标分片再排空。
	//   - 总在线=0:把超出 min_replicas 的空 ready 分片标 draining 盖戳。
	//     必须盖戳走回收路径删镜像 —— 否则直接把 Fleet 缩到 min 后,Agones 删掉的 pod
	//     只会被心跳超时扫成「无 draining_since_ms」的 draining 分片,reclaimDrainedShards
	//     跳过它,镜像就成了不可回收的 stale shard 永久残留在 shards 集合里。
	drained := false
	if totalPlayers > 0 && u.consolidationEnabled() {
		drained = u.consolidateOnce(ctx, shards, need)
	} else if totalPlayers == 0 {
		drained = u.drainEmptyShards(ctx, shards, minReplicas)
	}
	if drained {
		if fresh, ferr := u.repo.ListShards(ctx); ferr == nil {
			shards = fresh // 重读快照供回收判断
		}
	}

	// ③ 回收已排空且过 grace 的 draining 分片 + 缩容(只在镜像回收后才把 Fleet 降到存活分片数,
	// 保持 Fleet 副本数与镜像一致,避免缩 Fleet 后留下不可回收的 stale 镜像)。
	reclaimed := u.reclaimDrainedShards(ctx, shards)
	live := int32(len(shards)) - reclaimed
	desired := current
	target := live
	if target < need {
		target = need
	}
	if target < minReplicas {
		target = minReplicas
	}
	if target > maxReplicas {
		target = maxReplicas
	}
	if target < current {
		desired = target // 只在此处缩容
	}

	if desired != current {
		if serr := u.scaler.SetFleetReplicas(ctx, desired); serr != nil {
			return serr
		}
		plog.With(ctx).Infow("msg", "hub_fleet_scaled_in",
			"current", current, "desired", desired, "players", totalPlayers,
			"reclaimed", reclaimed, "min", minReplicas, "max", maxReplicas)
	}
	return nil
}

// consolidationEnabled 强制整合开关(需自动扩缩容已开)。
// 不强制要求 migrate pusher:即便没接 Kafka,服务端权威搬迁 + Hub DS drain 心跳仍能让玩家重连到新分片。
func (u *HubUsecase) consolidationEnabled() bool {
	return u.autoScaleEnabled() && u.cfg.ConsolidationEnabled
}

// consolidateOnce:ready 分片多于 need 时,把最空的多余分片标 draining 并搬迁其玩家。
// 返回是否有分片被排空(供调用方决定是否重读快照)。
func (u *HubUsecase) consolidateOnce(ctx context.Context, shards []*hubv1.HubShardStorageRecord, need int32) bool {
	ready := make([]*hubv1.HubShardStorageRecord, 0, len(shards))
	for _, s := range shards {
		if s.State == stateReady {
			ready = append(ready, s)
		}
	}
	if int32(len(ready)) <= need {
		return false // 没有多余 ready 分片
	}
	// 按负载升序(并列 shard_id 小者优先)排,排空最空的多余分片(保留最满的 need 个分片承接玩家)。
	sort.Slice(ready, func(i, j int) bool {
		if ready[i].PlayerCount != ready[j].PlayerCount {
			return ready[i].PlayerCount < ready[j].PlayerCount
		}
		return ready[i].ShardId < ready[j].ShardId
	})
	surplus := ready[:int32(len(ready))-need] // 升序前段=最空的多余分片
	drained := false
	for _, s := range surplus {
		if u.drainAndMigrate(ctx, s) {
			drained = true
		}
	}
	return drained
}

// drainEmptyShards:大厅没人(总在线=0)时,把超出 keep 的空 ready 分片标 draining + 盖戳,
// 交 reclaimDrainedShards 统一回收镜像(见 reconcileFleetReplicas ② 的说明)。返回是否有分片被排空。
func (u *HubUsecase) drainEmptyShards(ctx context.Context, shards []*hubv1.HubShardStorageRecord, keep int32) bool {
	ready := make([]*hubv1.HubShardStorageRecord, 0, len(shards))
	for _, s := range shards {
		if s.State == stateReady {
			ready = append(ready, s)
		}
	}
	if int32(len(ready)) <= keep {
		return false // 不超过保底,无需排空
	}
	// 保留 shard_id 最小的 keep 个,排空其余空分片(全空,排序仅取确定性)。
	sort.Slice(ready, func(i, j int) bool { return ready[i].ShardId < ready[j].ShardId })
	surplus := ready[keep:]
	drained := false
	for _, s := range surplus {
		if u.drainAndMigrate(ctx, s) {
			drained = true
		}
	}
	return drained
}

// drainAndMigrate:把分片标记 draining(盖时间戳)并服务端权威搬迁其在册玩家到目标分片。
// 单 tick 每分片最多搬 ConsolidationBatch 人(防抢占),剩余留下个 tick 续搬。
func (u *HubUsecase) drainAndMigrate(ctx context.Context, shard *hubv1.HubShardStorageRecord) bool {
	now := time.Now().UnixMilli()
	merr := u.repo.UpdateShardWithLock(ctx, shard.HubPodName, u.retry(), func(s *hubv1.HubShardStorageRecord) error {
		if s.State == stateReady {
			s.State = stateDraining
			s.DrainingSinceMs = now
		}
		return nil
	}, u.shardTTL())
	if merr != nil && errcode.As(merr) != errcode.ErrHubNoAvailable {
		plog.With(ctx).Warnw("msg", "drain_mark_failed", "pod", shard.HubPodName, "err", merr)
	}

	members, lerr := u.repo.ListShardMembers(ctx, shard.HubPodName)
	if lerr != nil {
		plog.With(ctx).Warnw("msg", "drain_list_members_failed", "pod", shard.HubPodName, "err", lerr)
	}
	// 成员反向索引是 best-effort 优化(只在 AssignHub/TransferHub 维护):部署前已在线、索引里
	// 没有的老玩家不会被这里服务端权威搬迁,而是靠 Hub DS drain 心跳兜底 —— 客户端收到 drain 指令
	// 后重连 AssignHub,幂等路径发现旧分片非 ready 即释放旧位重分到 ready 分片,旧分片 player_count
	// 随之递减,最终仍可被回收。member<player_count 时这里只少了对老玩家的无缝推送,不影响最终一致性。
	// 索引数明显少于在册人数时告警,便于观测首次整合的降级范围(详见 docs/design/agones-dev.md §2.2)。
	if shard.PlayerCount > 0 && len(members) < int(shard.PlayerCount) {
		plog.With(ctx).Warnw("msg", "drain_members_index_incomplete",
			"pod", shard.HubPodName, "indexed", len(members), "player_count", shard.PlayerCount)
	}

	fresh, ferr := u.repo.ListShards(ctx)
	if ferr != nil {
		plog.With(ctx).Warnw("msg", "drain_list_shards_failed", "pod", shard.HubPodName, "err", ferr)
		return true // 已标 draining,搬迁留下个 tick
	}

	batch := u.cfg.ConsolidationBatch
	if batch <= 0 {
		batch = 50
	}
	moved := 0
	for _, pid := range members {
		if moved >= batch {
			break
		}
		target := leastLoaded(fresh, shard.Region, shard.HubPodName)
		if target == nil {
			plog.With(ctx).Warnw("msg", "drain_no_target", "pod", shard.HubPodName, "region", shard.Region)
			break // 无空闲目标分片,留下个 tick
		}
		if u.migratePlayer(ctx, pid, shard, target) {
			moved++
			target.PlayerCount++ // 本地快照计数同步,均衡后续选择
		}
	}
	plog.With(ctx).Infow("msg", "hub_shard_draining",
		"pod", shard.HubPodName, "region", shard.Region, "members", len(members), "moved", moved)
	return true
}

// migratePlayer:把单个玩家从 from 分片服务端权威搬迁到 target 分片(镜像 TransferHub 的占位/切归属/退位顺序),
// 重签 hub 票据并推送 HubMigrateEvent(best-effort)。返回是否搬迁成功。
func (u *HubUsecase) migratePlayer(ctx context.Context, playerID uint64, from, target *hubv1.HubShardStorageRecord) bool {
	// 复核玩家仍在 from 分片(避免与玩家自身 Release/Transfer 竞争)。
	assign, found, err := u.repo.GetAssignment(ctx, playerID)
	if err != nil || !found || assign.HubPodName != from.HubPodName {
		u.removeShardMember(ctx, from.HubPodName, playerID) // 已不在此分片,清理残留索引
		return false
	}
	if rerr := u.reserveSeat(ctx, target.HubPodName); rerr != nil {
		return false // 目标没位置/非 ready,留下个 tick 重试
	}
	now := time.Now().UnixMilli()
	newAssign := &hubv1.HubAssignmentStorageRecord{
		PlayerId:     playerID,
		HubPodName:   target.HubPodName,
		HubAddr:      target.HubAddr,
		ShardId:      target.ShardId,
		Region:       target.Region,
		TeamId:       assign.TeamId,
		AssignedAtMs: now,
	}
	if serr := u.repo.SetAssignment(ctx, newAssign, u.assignTTL()); serr != nil {
		u.releaseFromShard(ctx, target.HubPodName) // 回滚新占位,旧分片不动
		return false
	}
	u.addShardMember(ctx, target.HubPodName, playerID)
	u.releaseFromShard(ctx, from.HubPodName)
	u.removeShardMember(ctx, from.HubPodName, playerID)

	// 重签新分片票据 + 推送迁移通知(best-effort:失败不回滚已切换的归属,
	// Hub DS drain 心跳指令会兜底让客户端重连,AssignHub 幂等返回新分片)。
	token, _, terr := u.signer.SignHubTicket(playerID)
	if terr != nil {
		plog.With(ctx).Warnw("msg", "migrate_sign_ticket_failed", "player_id", playerID, "err", terr)
		return true
	}
	u.pushMigrate(ctx, playerID, from, target, token)
	return true
}

// pushMigrate 推送 HubMigrateEvent 给被迁移玩家(migrate pusher 未接时静默跳过)。
func (u *HubUsecase) pushMigrate(ctx context.Context, playerID uint64, from, target *hubv1.HubShardStorageRecord, token string) {
	if u.migrate == nil {
		return
	}
	ev := &hubv1.HubMigrateEvent{
		PlayerId:     playerID,
		FromHubPod:   from.HubPodName,
		ToHubDsAddr:  target.HubAddr,
		ToHubTicket:  token,
		ToHubPodName: target.HubPodName,
		ToShardId:    target.ShardId,
		GraceSeconds: u.cfg.MigrateGraceSeconds,
		Reason:       migrateReasonConsolidation,
		TsMs:         time.Now().UnixMilli(),
	}
	payload, merr := proto.Marshal(ev)
	if merr != nil {
		plog.With(ctx).Warnw("msg", "migrate_marshal_failed", "player_id", playerID, "err", merr)
		return
	}
	if perr := u.migrate.PushMigrate(ctx, playerID, payload); perr != nil {
		plog.With(ctx).Warnw("msg", "migrate_push_failed", "player_id", playerID, "err", perr)
	}
}

// reclaimDrainedShards:删除「已排空(player_count<=0)且过 grace」的强制整合 draining 分片。
// 只回收带 draining_since_ms>0 戳的分片(强制整合排空的),不碰心跳超时标 draining 的死 DS 分片。
func (u *HubUsecase) reclaimDrainedShards(ctx context.Context, shards []*hubv1.HubShardStorageRecord) int32 {
	graceMs := int64(u.cfg.MigrateGraceSeconds) * 1000
	now := time.Now().UnixMilli()
	var reclaimed int32
	for _, s := range shards {
		if s.State != stateDraining || s.PlayerCount > 0 || s.DrainingSinceMs <= 0 {
			continue
		}
		if now-s.DrainingSinceMs < graceMs {
			continue // 未过 grace,保持 pod 存活让在场玩家完成倒计时切换
		}
		if rerr := u.repo.RemoveShard(ctx, s.HubPodName); rerr != nil {
			plog.With(ctx).Warnw("msg", "reclaim_remove_shard_failed", "pod", s.HubPodName, "err", rerr)
			continue
		}
		reclaimed++
		plog.With(ctx).Infow("msg", "hub_shard_reclaimed", "pod", s.HubPodName, "region", s.Region)
	}
	return reclaimed
}

// addShardMember / removeShardMember:成员反向索引维护(best-effort,失败仅 Warn 不阻断主流程)。
func (u *HubUsecase) addShardMember(ctx context.Context, pod string, playerID uint64) {
	if err := u.repo.AddShardMember(ctx, pod, playerID, u.assignTTL()); err != nil {
		plog.With(ctx).Warnw("msg", "add_shard_member_failed", "pod", pod, "player_id", playerID, "err", err)
	}
}

func (u *HubUsecase) removeShardMember(ctx context.Context, pod string, playerID uint64) {
	if err := u.repo.RemoveShardMember(ctx, pod, playerID); err != nil {
		plog.With(ctx).Warnw("msg", "remove_shard_member_failed", "pod", pod, "player_id", playerID, "err", err)
	}
}

// sumPlayers 汇总分片在册人数(负数视为 0)。
func sumPlayers(shards []*hubv1.HubShardStorageRecord) int64 {
	var total int64
	for _, s := range shards {
		if s.PlayerCount > 0 {
			total += int64(s.PlayerCount)
		}
	}
	return total
}
