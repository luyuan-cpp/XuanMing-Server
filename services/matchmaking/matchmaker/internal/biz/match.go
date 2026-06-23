// Package biz 是 matchmaker 服务的业务逻辑层(W4 ①,2026-06-06)。
//
// 撮合流水线(docs/design/go-services.md §2.8):
//
//	StartMatch(team) → 写排队票据(MMR 入 ZSET)
//	   后台 RunMatchLoop:matchOnce 按 MMR 窗口贪心装箱凑齐 5+5 → 建 match → 进确认期
//	   ConfirmMatch:全员 accept → 拉 DS → READY;任一 reject/超时 → FAILED + 其余票据退回队列
//
// 协议铁律(docs/design/protocol-ordering-rules.md):
//   - 4 个 RPC 全"已受理型"(原则 3):客户端 UI 状态机由 pandora.match.progress push 驱动
//   - **原则 3 例外**:match 进度 push 发给所有人(含发起方),callerPlayerID=0
//   - kafka key=player_id(不变量 §9)由 PushToPlayers 保证
//
// 关键不变量(go-services.md §2.8):
//   - 同一玩家只能在一个 match 队列(ClaimPlayer SETNX)
//   - 确认期内有人拒绝 → 其他人退回队列(保留排队时长 enqueued_at_ms)
package biz

import (
	"context"
	"fmt"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"
	teamv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/team/v1"

	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/conf"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/data"
)

// ── 解耦接口(biz 不依赖 grpc/kafka 具体实现)─────────────────────────────────

// TeamReader 拉取 team 服务的队伍快照(StartMatch 校验 READY)。
// 实现:data.GrpcTeamReader(team 服务 gRPC client)。nil 时跳过校验。
type TeamReader interface {
	GetTeam(ctx context.Context, teamID uint64) (*teamv1.Team, bool, error)
}

// MatchEventPusher 把 match 进度事件推给玩家(kafka pandora.match.progress)。
// 实现:kafkax.KeyOrderedProducer.PushToPlayers 包装。
type MatchEventPusher interface {
	// PushMatchProgress 向 toPlayerIDs 推送进度事件字节。
	// 原则 3 例外:match 进度发给所有人,callerPlayerID 恒传 0。
	PushMatchProgress(ctx context.Context, callerPlayerID uint64, toPlayerIDs []uint64, payload []byte) (sent int, err error)
}

// DSAllocator 申请战斗 DS（W4 ① 打桩，W4 ② 接 ds_allocator gRPC）。
type DSAllocator interface {
	// AllocateBattle 为 match 申请战斗 DS，返回 ds 地址 + 每个玩家的入场票据。
	AllocateBattle(ctx context.Context, matchID uint64, playerIDs []uint64) (dsAddr string, tickets map[uint64]string, err error)

	// SignBattleTicket 给（重连 / 换设备的）玩家现签一张新的 battle DSTicket（新 jti、sub=playerID）。
	// GetMatchProgress 在 READY 阶段调用它下发票据：每次新 jti，避免复用同一张票撞 DS 侧 jti
	// 一次性防重放（换手机 / 掉线重连刚需）；票 sub 锁定调用者本人，比共享票更严。
	SignBattleTicket(ctx context.Context, playerID, matchID uint64) (token string, err error)
}

// LocationNotifier 把玩家位置变更上报给 player_locator（不变量 §1：玩家同一时刻只在一个 Location）。
//
// 状态权属：matchmaker 是 MATCHING / BATTLE 两个状态的权威（它掌握撮合生命周期）；
// HUB 状态由 hub DS 上报，故撮合失败 / 取消时 matchmaker 不回写 HUB（交回 hub DS）。
// 弱依赖：addr 未配 → main 注入 nil，biz 检查 nil 跳过；调用失败仅 Warn 不阻断撮合。
type LocationNotifier interface {
	// NotifyMatching 撮合成局（进入确认期）→ 把成员标记为 MATCHING（带 match_id）。
	NotifyMatching(ctx context.Context, playerIDs []uint64, matchID uint64) error
	// NotifyBattle 全员确认 + DS 就绪 → 把成员标记为 BATTLE（带 match_id + battle_pod）。
	NotifyBattle(ctx context.Context, playerIDs []uint64, matchID uint64, battlePod string) error
}

// IDGenerator 生成唯一 match_id(snowflake)。
type IDGenerator interface {
	Generate() uint64
}

// ── 常量 ─────────────────────────────────────────────────────────────────────

const (
	stageQueueing   = matchv1.MatchStage_MATCH_STAGE_QUEUEING
	stageFound      = matchv1.MatchStage_MATCH_STAGE_FOUND
	stageConfirm    = matchv1.MatchStage_MATCH_STAGE_CONFIRM
	stageAllocating = matchv1.MatchStage_MATCH_STAGE_ALLOCATING
	stageReady      = matchv1.MatchStage_MATCH_STAGE_READY
	stageFailed     = matchv1.MatchStage_MATCH_STAGE_FAILED

	confirmPending  = matchv1.MatchConfirmStatus_MATCH_CONFIRM_STATUS_PENDING
	confirmAccepted = matchv1.MatchConfirmStatus_MATCH_CONFIRM_STATUS_ACCEPTED
	confirmRejected = matchv1.MatchConfirmStatus_MATCH_CONFIRM_STATUS_REJECTED
)

// ── MatchUsecase ──────────────────────────────────────────────────────────────

// MatchUsecase 是 matchmaker 业务逻辑核心。
type MatchUsecase struct {
	repo      data.MatchRepo
	reader    TeamReader // 可为 nil(本机不起 team 时跳过校验)
	pusher    MatchEventPusher
	allocator DSAllocator
	idGen     IDGenerator
	locator   LocationNotifier // 可为 nil（本机不起 player_locator 时不上报位置）
	cfg       conf.MatchConf
}

// NewMatchUsecase 构造 MatchUsecase。locator 可为 nil（弱依赖，不上报位置）。
func NewMatchUsecase(repo data.MatchRepo, reader TeamReader, pusher MatchEventPusher, allocator DSAllocator, idGen IDGenerator, locator LocationNotifier, cfg conf.MatchConf) *MatchUsecase {
	return &MatchUsecase{repo: repo, reader: reader, pusher: pusher, allocator: allocator, idGen: idGen, locator: locator, cfg: cfg}
}

// notifyMatching 把 match 成员位置标记为 MATCHING（弱依赖：nil 跳过 / 失败仅 Warn）。
func (u *MatchUsecase) notifyMatching(ctx context.Context, playerIDs []uint64, matchID uint64) {
	if u.locator == nil {
		return
	}
	if err := u.locator.NotifyMatching(ctx, playerIDs, matchID); err != nil {
		plog.With(ctx).Warnw("msg", "locator_notify_matching_failed", "match_id", matchID, "err", err)
	}
}

// notifyBattle 把 match 成员位置标记为 BATTLE（弱依赖：nil 跳过 / 失败仅 Warn）。
func (u *MatchUsecase) notifyBattle(ctx context.Context, playerIDs []uint64, matchID uint64, battlePod string) {
	if u.locator == nil {
		return
	}
	if err := u.locator.NotifyBattle(ctx, playerIDs, matchID, battlePod); err != nil {
		plog.With(ctx).Warnw("msg", "locator_notify_battle_failed", "match_id", matchID, "err", err)
	}
}

func (u *MatchUsecase) ticketTTL() time.Duration { return u.cfg.TicketTTL.Std() }
func (u *MatchUsecase) matchTTL() time.Duration  { return u.cfg.MatchTTL.Std() }

// removeActive 把 match 移出 active ZSET,出错仅警告。
func (u *MatchUsecase) removeActive(ctx context.Context, matchID uint64) {
	if err := u.repo.RemoveActive(ctx, matchID); err != nil {
		plog.With(ctx).Warnw("msg", "remove_active_failed", "match_id", matchID, "err", err)
	}
}

// ── RPC 1:StartMatch ─────────────────────────────────────────────────────────

// StartMatch 把 team 作为一张票据入队。ticketID 由 service 层 snowflake 生成。
// 返回的 ticketID 同时作为客户端 QUEUEING 阶段的 match 句柄(CancelMatch/GetMatchProgress 用)。
//
// 前置(reader 非 nil 时):team 必须存在、state=READY、captainID 为队长、成员数 ≤ 一方人数。
func (u *MatchUsecase) StartMatch(ctx context.Context, ticketID, teamID, captainID uint64) (uint64, error) {
	members, avgMMR, err := u.resolveMembers(ctx, teamID, captainID)
	if err != nil {
		return 0, err
	}

	// 原子声明每个成员归属(SETNX),落不变量"一人只在一个队列";任一冲突则回滚已声明的。
	claimed := make([]uint64, 0, len(members))
	for _, m := range members {
		if _, ok, cerr := u.repo.ClaimPlayer(ctx, m.PlayerId, ticketID, u.ticketTTL()); cerr != nil {
			u.rollbackClaims(ctx, claimed)
			return 0, cerr
		} else if !ok {
			u.rollbackClaims(ctx, claimed)
			return 0, errcode.New(errcode.ErrMatchAlreadyMatching, "player %d already matching", m.PlayerId)
		}
		claimed = append(claimed, m.PlayerId)
	}

	ticket := &matchv1.MatchTicketStorageRecord{
		TicketId:     ticketID,
		TeamId:       teamID,
		CaptainId:    captainID,
		Members:      members,
		AvgMmr:       avgMMR,
		EnqueuedAtMs: time.Now().UnixMilli(),
		MatchId:      0,
	}
	if err := u.repo.AddTicket(ctx, ticket, u.ticketTTL()); err != nil {
		u.rollbackClaims(ctx, claimed)
		return 0, err
	}

	// QUEUEING 进度推给全体成员(原则 3:含发起方,callerID=0)
	u.pushProgress(ctx, ticketID, stageQueueing, members, "", "")

	plog.With(ctx).Infow("msg", "match_start", "ticket_id", ticketID, "team_id", teamID,
		"captain_id", captainID, "members", len(members), "avg_mmr", avgMMR)
	return ticketID, nil
}

// resolveMembers 根据 team 快照构造 match 成员列表 + 计算平均 MMR。
// reader 为 nil 时退化为"仅 captain 单人票据"(本机不起 team 的骨架联调路径)。
func (u *MatchUsecase) resolveMembers(ctx context.Context, teamID, captainID uint64) ([]*matchv1.MatchMemberStorageRecord, int32, error) {
	if u.reader == nil {
		m := []*matchv1.MatchMemberStorageRecord{{PlayerId: captainID, TeamId: teamID, Confirm: confirmPending}}
		return m, 0, nil
	}

	team, found, err := u.reader.GetTeam(ctx, teamID)
	if err != nil {
		return nil, 0, err
	}
	if !found {
		return nil, 0, errcode.New(errcode.ErrMatchTeamNotReady, "team %d not found", teamID)
	}
	if team.State != teamv1.TeamState_TEAM_STATE_READY {
		return nil, 0, errcode.New(errcode.ErrMatchTeamNotReady, "team %d not ready (state=%d)", teamID, team.State)
	}
	if team.CaptainId != captainID {
		return nil, 0, errcode.New(errcode.ErrTeamNotCaptain, "player %d not captain of team %d", captainID, teamID)
	}
	if len(team.Members) == 0 || len(team.Members) > u.cfg.TeamSize {
		return nil, 0, errcode.New(errcode.ErrMatchTeamNotReady, "team %d invalid size %d", teamID, len(team.Members))
	}

	members := make([]*matchv1.MatchMemberStorageRecord, 0, len(team.Members))
	var sum int32
	for _, tm := range team.Members {
		members = append(members, &matchv1.MatchMemberStorageRecord{
			PlayerId: tm.PlayerId,
			TeamId:   teamID,
			Mmr:      tm.Mmr,
			HeroId:   tm.HeroId,
			Confirm:  confirmPending,
		})
		sum += tm.Mmr
	}
	avg := sum / int32(len(members))
	return members, avg, nil
}

// ── RPC 2:CancelMatch ────────────────────────────────────────────────────────

// CancelMatch 取消匹配。以 playerID 为准定位其当前票据:
//   - 票据仍在排队(未撮合)→ 删票据 + 释放成员归属
//   - 票据已进 match(确认期)→ 等价于该玩家拒绝确认,走 match 失败流程
func (u *MatchUsecase) CancelMatch(ctx context.Context, playerID uint64) error {
	ticketID, found, err := u.repo.GetPlayerTicket(ctx, playerID)
	if err != nil {
		return err
	}
	if !found {
		return errcode.New(errcode.ErrMatchNotFound, "player %d not in any queue", playerID)
	}
	ticket, found, err := u.repo.GetTicket(ctx, ticketID)
	if err != nil {
		return err
	}
	if !found {
		// 票据已消失(过期),清理残留 player index
		_ = u.repo.DeletePlayerIndex(ctx, playerID)
		return errcode.New(errcode.ErrMatchNotFound, "ticket %d gone", ticketID)
	}

	// 已被撮合进 match → 视为拒绝确认
	if ticket.MatchId != 0 {
		return u.ConfirmMatch(ctx, playerID, ticket.MatchId, false)
	}

	// 仍在排队 → 删票据 + 释放全体成员归属
	if err := u.repo.DeleteTicket(ctx, ticketID); err != nil {
		return err
	}
	u.rollbackClaims(ctx, memberPlayerIDs(ticket.Members))
	plog.With(ctx).Infow("msg", "match_cancel", "ticket_id", ticketID, "player_id", playerID)
	return nil
}

// ── 对局结束释放:ReleaseMatch ────────────────────────────────────────────────

// ReleaseMatch 释放一场已结束(结算 / abandoned)对局的全部撮合状态,由 battle_result 在
// 结算落库后调用(后端内部接口,不带玩家 JWT)。修复:对局走完 READY → 进战斗 → 结算后,
// onAllConfirmed 故意保留的 player→ticket 归属(SETNX claim)+ 票据 + match 镜像本只能等
// TTL(30min)自然过期;期间玩家回 Hub 再次 StartMatch 会被 ClaimPlayer SETNX 撞上残留 claim
// 报 ErrMatchAlreadyMatching(4002)。此处在结算时主动彻底释放,玩家回 Hub 即可立刻再次匹配。
//
// 释放对象(全部幂等,任一步失败仅 Warn 不中断,best-effort 清完能清的):
//   - 每个成员的 player→ticket 归属(仅当其当前 claim 仍指向本局票据时才删,避免误删
//     玩家结算后已经发起的新一局 claim)
//   - 本局全部排队票据(ticket record + queue ZSET 残留)
//   - match 镜像 + active 索引
//
// fallbackPlayerIDs:battle_result 从 BattleResult.stats 带来的玩家名单。match 镜像若已过 TTL
// 消失,仍可凭它兜底清掉残留 claim(只删确属本局的,见 releasePlayerClaim)。
func (u *MatchUsecase) ReleaseMatch(ctx context.Context, matchID uint64, fallbackPlayerIDs []uint64) error {
	if matchID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "match_id required")
	}

	// 收集成员 + 本局票据(match 镜像若已过期则仅靠 fallback 兜底清 claim)。
	playerSet := make(map[uint64]struct{})
	var ticketIDs []uint64
	matchFound := false

	if m, found, err := u.repo.GetMatch(ctx, matchID); err != nil {
		plog.With(ctx).Warnw("msg", "release_get_match_failed", "match_id", matchID, "err", err)
	} else if found {
		matchFound = true
		ticketIDs = m.TicketIds
		for _, pid := range memberPlayerIDs(m.Members) {
			playerSet[pid] = struct{}{}
		}
	}
	for _, pid := range fallbackPlayerIDs {
		if pid != 0 {
			playerSet[pid] = struct{}{}
		}
	}

	// 删确属本局的票据(idempotent)。
	ticketSet := make(map[uint64]struct{}, len(ticketIDs))
	for _, tid := range ticketIDs {
		ticketSet[tid] = struct{}{}
		if err := u.repo.DeleteTicket(ctx, tid); err != nil {
			plog.With(ctx).Warnw("msg", "release_delete_ticket_failed", "match_id", matchID, "ticket_id", tid, "err", err)
		}
	}

	// 删每个成员的 player→ticket 归属(仅当确属本局,防误删结算后新一局 claim)。
	for pid := range playerSet {
		u.releasePlayerClaim(ctx, matchID, pid, ticketSet)
	}

	// 硬删 match 镜像 + 移出 active。
	if err := u.repo.DeleteMatch(ctx, matchID); err != nil {
		plog.With(ctx).Warnw("msg", "release_delete_match_failed", "match_id", matchID, "err", err)
	}

	plog.With(ctx).Infow("msg", "match_released", "match_id", matchID,
		"match_found", matchFound, "players", len(playerSet), "tickets", len(ticketIDs))
	return nil
}

// releasePlayerClaim 释放单个玩家的 player→ticket 归属,但仅当其当前 claim 确属本局
// (claim 指向的票据 ∈ 本局票据,或该票据的 match_id == 本局)。玩家若已发起新一局,
// 其 claim 指向新票据(不同 match_id / 不在本局票据集),此处不动,避免误删新 claim。
func (u *MatchUsecase) releasePlayerClaim(ctx context.Context, matchID, playerID uint64, ticketSet map[uint64]struct{}) {
	tid, ok, err := u.repo.GetPlayerTicket(ctx, playerID)
	if err != nil {
		plog.With(ctx).Warnw("msg", "release_get_player_ticket_failed", "match_id", matchID, "player_id", playerID, "err", err)
		return
	}
	if !ok {
		return // claim 已释放
	}
	belongs := false
	if _, in := ticketSet[tid]; in {
		belongs = true
	} else if t, found, gerr := u.repo.GetTicket(ctx, tid); gerr == nil && found && t.MatchId == matchID {
		belongs = true
	}
	if !belongs {
		// claim 指向别的票据(玩家结算后已发起新一局)→ 不误删。
		plog.With(ctx).Infow("msg", "release_skip_stale_claim", "match_id", matchID, "player_id", playerID, "current_ticket", tid)
		return
	}
	if err := u.repo.DeletePlayerIndex(ctx, playerID); err != nil {
		plog.With(ctx).Warnw("msg", "release_delete_player_index_failed", "match_id", matchID, "player_id", playerID, "err", err)
	}
}

// ── RPC 3:ConfirmMatch ───────────────────────────────────────────────────────

// ConfirmMatch 确认/拒绝匹配。
//   - accept=false 或任一人拒绝 → match FAILED,其余票据退回队列(保留排队时长)
//   - 全员 accept → ALLOCATING → 拉 DS → READY
func (u *MatchUsecase) ConfirmMatch(ctx context.Context, playerID, matchID uint64, accept bool) error {
	const (
		outcomePending  = 0
		outcomeFailed   = 1
		outcomeAllReady = 2
	)
	outcome := outcomePending
	var snapshot *matchv1.MatchStorageRecord

	err := u.repo.UpdateMatchWithLock(ctx, matchID, u.cfg.OptimisticRetry, func(m *matchv1.MatchStorageRecord) error {
		// 终态幂等:已失败返回 declined;已分配/就绪直接成功返回
		if m.Stage == stageFailed {
			return errcode.New(errcode.ErrMatchDeclined, "match %d already failed", matchID)
		}
		if m.Stage == stageAllocating || m.Stage == stageReady {
			snapshot = cloneMatch(m)
			outcome = outcomePending
			return nil
		}
		idx := memberIndex(m.Members, playerID)
		if idx < 0 {
			return errcode.New(errcode.ErrMatchNotFound, "player %d not in match %d", playerID, matchID)
		}

		if !accept {
			m.Members[idx].Confirm = confirmRejected
			m.Stage = stageFailed
			outcome = outcomeFailed
			snapshot = cloneMatch(m)
			return nil
		}

		m.Members[idx].Confirm = confirmAccepted
		if allAccepted(m.Members) {
			m.Stage = stageAllocating
			outcome = outcomeAllReady
		} else {
			m.Stage = stageConfirm
			outcome = outcomePending
		}
		snapshot = cloneMatch(m)
		return nil
	}, u.matchTTL())
	if err != nil {
		return err
	}

	switch outcome {
	case outcomeFailed:
		u.onMatchFailed(ctx, snapshot, playerID)
	case outcomeAllReady:
		u.onAllConfirmed(ctx, snapshot)
	default:
		// 仍有人未确认:推 CONFIRM 进度给全体
		if snapshot != nil && snapshot.Stage == stageConfirm {
			u.pushProgress(ctx, matchID, stageConfirm, snapshot.Members, "", "")
		}
	}
	plog.With(ctx).Infow("msg", "match_confirm", "match_id", matchID, "player_id", playerID,
		"accept", accept, "outcome", outcome)
	return nil
}

// onMatchFailed 处理确认失败:其余票据退回队列,拒绝者票据删除,推 FAILED 进度。
func (u *MatchUsecase) onMatchFailed(ctx context.Context, m *matchv1.MatchStorageRecord, rejecterID uint64) {
	// 推 FAILED 给全体(含拒绝者)
	u.pushProgress(ctx, m.MatchId, stageFailed, m.Members, "", "")

	rejecterTicket := uint64(0)
	if tid, ok, _ := u.repo.GetPlayerTicket(ctx, rejecterID); ok {
		rejecterTicket = tid
	}

	for _, tid := range m.TicketIds {
		ticket, found, err := u.repo.GetTicket(ctx, tid)
		if err != nil || !found {
			continue
		}
		if tid == rejecterTicket {
			// 拒绝者整队删除并释放归属(不退回队列)
			_ = u.repo.DeleteTicket(ctx, tid)
			u.rollbackClaims(ctx, memberPlayerIDs(ticket.Members))
			continue
		}
		// 其余票据退回队列,保留 enqueued_at_ms(排队时长),清掉 match_id
		ticket.MatchId = 0
		if err := u.repo.RequeueTicket(ctx, ticket, u.ticketTTL()); err != nil {
			plog.With(ctx).Warnw("msg", "match_requeue_failed", "ticket_id", tid, "err", err)
		}
	}

	u.removeActive(ctx, m.MatchId)
	if err := u.repo.ExpireMatch(ctx, m.MatchId, u.matchTTL()); err != nil {
		plog.With(ctx).Warnw("msg", "match_expire_failed", "match_id", m.MatchId, "err", err)
	}
	plog.With(ctx).Infow("msg", "match_failed", "match_id", m.MatchId, "rejecter_id", rejecterID)
}

// onAllConfirmed 处理全员确认:拉 DS → 写 match READY → 推 READY 进度 → 清理票据归属。
func (u *MatchUsecase) onAllConfirmed(ctx context.Context, m *matchv1.MatchStorageRecord) {
	playerIDs := memberPlayerIDs(m.Members)

	dsAddr, tickets, err := u.allocator.AllocateBattle(ctx, m.MatchId, playerIDs)
	if err != nil {
		plog.With(ctx).Errorw("msg", "ds_allocate_failed", "match_id", m.MatchId, "err", err)
		// 分配失败:整场失败,票据退回队列
		u.onMatchFailed(ctx, m, 0)
		return
	}

	// 写 match → READY
	var ready *matchv1.MatchStorageRecord
	werr := u.repo.UpdateMatchWithLock(ctx, m.MatchId, u.cfg.OptimisticRetry, func(rec *matchv1.MatchStorageRecord) error {
		rec.Stage = stageReady
		rec.BattleDsAddr = dsAddr
		ready = cloneMatch(rec)
		return nil
	}, u.matchTTL())
	if werr != nil {
		plog.With(ctx).Errorw("msg", "match_set_ready_failed", "match_id", m.MatchId, "err", werr)
		return
	}

	// 全员确认 + DS 就绪：上报 locator BATTLE（battle_pod 用 ds_addr 唯一标识 DS，不变量 §1，弱依赖）
	u.notifyBattle(ctx, playerIDs, m.MatchId, dsAddr)

	// 每个玩家单独带自己的 battle_ticket 推 READY 进度
	now := time.Now().UnixMilli()
	for _, member := range ready.Members {
		u.pushOne(ctx, member.PlayerId, ready, dsAddr, tickets[member.PlayerId], now)
	}

	// 确认期结束:移出 active。票据保留到 TTL, 让客户端用 StartMatch 返回的 ticket_id
	// 继续轮询时也能解析到 READY match, 避免错过 push 后 GetMatchProgress 变成 4001。
	u.removeActive(ctx, m.MatchId)
	plog.With(ctx).Infow("msg", "match_ready", "match_id", m.MatchId, "ds_addr", dsAddr, "players", len(playerIDs))
}

// ── RPC 4:GetMatchProgress ───────────────────────────────────────────────────

// GetMatchProgress 查询进度。
//   - id 是客户端句柄:match_id(已撮合)或 ticket_id(排队中)。重新登录 / 换设备丢了句柄时
//     传 0,服务端用 callerID 反查其当前所在票据(GetPlayerTicket),解决"重连拿不到自己进度"。
//   - 鉴权(不变量 §14 / 反外挂):callerID 必须是该 match/ticket 的成员才返回进度;否则按
//     "不存在"处理(ErrMatchNotFound),不暴露他人对局的存在性,杜绝外挂用任意 match_id 拉别人
//     的双方名单 / DS 地址。match_id 不是秘密,绝不能再当授权凭证。
//   - READY 阶段且 caller 是本局成员时,给他现签一张新 battle DSTicket(新 jti)下发,支持
//     换手机 / 掉线重连(见 refreshBattleTicket)。
func (u *MatchUsecase) GetMatchProgress(ctx context.Context, callerID, id uint64) (*matchv1.MatchProgress, error) {
	if callerID == 0 {
		return nil, errcode.New(errcode.ErrUnauthorized, "missing caller identity")
	}

	// 重连兜底:句柄丢失(id==0)时用 callerID 反查自己当前所在票据。
	if id == 0 {
		tid, found, err := u.repo.GetPlayerTicket(ctx, callerID)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errcode.New(errcode.ErrMatchNotFound, "player %d not in any queue", callerID)
		}
		id = tid
	}

	if m, found, err := u.repo.GetMatch(ctx, id); err != nil {
		return nil, err
	} else if found {
		if memberIndex(m.Members, callerID) < 0 {
			return nil, errcode.New(errcode.ErrMatchNotFound, "match/ticket %d not found", id)
		}
		prog := matchToProgress(m)
		u.refreshBattleTicket(ctx, m, callerID, prog)
		return prog, nil
	}
	if t, found, err := u.repo.GetTicket(ctx, id); err != nil {
		return nil, err
	} else if found {
		if memberIndex(t.Members, callerID) < 0 {
			return nil, errcode.New(errcode.ErrMatchNotFound, "match/ticket %d not found", id)
		}
		if t.MatchId != 0 {
			if m, found, err := u.repo.GetMatch(ctx, t.MatchId); err != nil {
				return nil, err
			} else if found {
				// 票据已撮合进 match,caller 既是票据成员即本局成员,直接给 match 进度。
				prog := matchToProgress(m)
				u.refreshBattleTicket(ctx, m, callerID, prog)
				return prog, nil
			}
		}
		return ticketToProgress(t), nil
	}
	return nil, errcode.New(errcode.ErrMatchNotFound, "match/ticket %d not found", id)
}

// refreshBattleTicket 在 READY 阶段为发起查询的本人现签一张新的 battle DSTicket(新 jti)，
// 覆盖 prog 里来自存储的票字段。这样换手机 / 掉线重连每次都拿新 jti，不会撞 DS 侧 jti 一次性
// 防重放；票 sub 锁定调用者本人。
// 守卫：callerID!=0 且 stage=READY 且有 ds_addr 且 caller 是本局成员才签；任何不满足或签发失败
// 都保留存储票字段(dev/stub 兜底，绝不让查询失败)。
func (u *MatchUsecase) refreshBattleTicket(ctx context.Context, m *matchv1.MatchStorageRecord, callerID uint64, prog *matchv1.MatchProgress) {
	if callerID == 0 || m.Stage != stageReady || m.BattleDsAddr == "" {
		return
	}
	if memberIndex(m.Members, callerID) < 0 {
		return // 非本局成员，不签票
	}
	token, err := u.allocator.SignBattleTicket(ctx, callerID, m.MatchId)
	if err != nil {
		plog.With(ctx).Warnw("msg", "resign_battle_ticket_failed", "match_id", m.MatchId, "player_id", callerID, "err", err)
		return
	}
	prog.BattleTicket = token
}

// ── 后台撮合循环 ──────────────────────────────────────────────────────────────

// RunMatchLoop 启动后台撮合 + 确认期超时扫描,直到 ctx 取消。
func (u *MatchUsecase) RunMatchLoop(ctx context.Context) {
	ticker := time.NewTicker(u.cfg.MatchInterval.Std())
	defer ticker.Stop()
	plog.With(ctx).Infow("msg", "match_loop_started", "interval", u.cfg.MatchInterval.String())
	for {
		select {
		case <-ctx.Done():
			plog.With(ctx).Infow("msg", "match_loop_stopped")
			return
		case <-ticker.C:
			if err := u.matchOnce(ctx); err != nil {
				plog.With(ctx).Warnw("msg", "match_once_failed", "err", err)
			}
			if err := u.expireOnce(ctx); err != nil {
				plog.With(ctx).Warnw("msg", "expire_once_failed", "err", err)
			}
		}
	}
}

// matchOnce 扫描一次队列,尽可能多地凑出 match(5+5)。
//
// 算法(W4 ① 骨架版):按 avg_mmr 升序取票据,贪心累积进一个组,当组内总人数达到
// 2×TeamSize 且 MMR 跨度在动态窗口内时,用 largest-first 装箱拆成两边各 TeamSize。
// 装箱失败则前移起点重试。生产级更优撮合留 TODO。
func (u *MatchUsecase) matchOnce(ctx context.Context) error {
	ticketIDs, err := u.repo.RangeQueueTickets(ctx)
	if err != nil {
		return err
	}
	if len(ticketIDs) == 0 {
		return nil
	}

	// 载入票据(过滤已消失的),按 avg_mmr 升序
	tickets := make([]*matchv1.MatchTicketStorageRecord, 0, len(ticketIDs))
	for _, tid := range ticketIDs {
		t, found, gerr := u.repo.GetTicket(ctx, tid)
		if gerr != nil {
			continue
		}
		if !found {
			// 票据 record 已过期/删除但 queue ZSET 残留(Redis Cluster 拆事务后索引漂移的天然兜底):
			// best-effort 补清,避免 queue 无界堆积。失败无妨,下一轮再补。
			_ = u.repo.DeleteTicket(ctx, tid)
			continue
		}
		if t.MatchId != 0 {
			continue
		}
		tickets = append(tickets, t)
	}
	sort.SliceStable(tickets, func(i, j int) bool { return tickets[i].AvgMmr < tickets[j].AvgMmr })

	if u.cfg.EnableSoloMatch {
		for _, t := range tickets {
			if err := u.formSoloMatch(ctx, t); err != nil {
				plog.With(ctx).Warnw("msg", "form_solo_match_failed", "ticket_id", t.TicketId, "err", err)
			}
		}
		return nil
	}

	need := 2 * u.cfg.TeamSize
	now := time.Now().UnixMilli()
	used := make(map[uint64]bool)

	for start := 0; start < len(tickets); start++ {
		if used[tickets[start].TicketId] {
			continue
		}
		group := make([]*matchv1.MatchTicketStorageRecord, 0, need)
		total := 0
		for j := start; j < len(tickets) && total < need; j++ {
			t := tickets[j]
			if used[t.TicketId] {
				continue
			}
			if len(group) > 0 && !withinWindow(group[0], t, now, u.cfg) {
				break // 已按 MMR 排序,后面只会更远
			}
			group = append(group, t)
			total += len(t.Members)
		}
		if total != need {
			continue
		}
		sideA, sideB, ok := binPack(group, u.cfg.TeamSize)
		if !ok {
			continue
		}
		if err := u.formMatch(ctx, sideA, sideB); err != nil {
			plog.With(ctx).Warnw("msg", "form_match_failed", "err", err)
			continue
		}
		for _, t := range group {
			used[t.TicketId] = true
		}
	}
	return nil
}

// formSoloMatch 是本地端到端测试路径:单张队伍票据直接成局,跳过多人确认,立即拉 Battle DS。
func (u *MatchUsecase) formSoloMatch(ctx context.Context, ticket *matchv1.MatchTicketStorageRecord) error {
	// StartMatch 返回 ticket_id 作为客户端进度句柄。单人联调复用它做 match_id,
	// 让轮询和 push 驱动的进战流程使用同一个 ID。
	matchID := ticket.TicketId
	now := time.Now().UnixMilli()

	members := make([]*matchv1.MatchMemberStorageRecord, 0, len(ticket.Members))
	for _, m := range ticket.Members {
		members = append(members, &matchv1.MatchMemberStorageRecord{
			PlayerId: m.PlayerId,
			TeamId:   m.TeamId,
			Mmr:      m.Mmr,
			HeroId:   m.HeroId,
			Side:     0,
			Confirm:  confirmAccepted,
		})
	}
	match := &matchv1.MatchStorageRecord{
		MatchId:           matchID,
		Stage:             stageAllocating,
		Members:           members,
		TicketIds:         []uint64{ticket.TicketId},
		CreatedAtMs:       now,
		ConfirmDeadlineMs: now,
	}

	ticket.MatchId = matchID
	if err := u.repo.ReserveTicket(ctx, ticket, u.ticketTTL()); err != nil {
		return fmt.Errorf("reserve solo ticket %d: %w", ticket.TicketId, err)
	}
	if err := u.repo.CreateMatch(ctx, match, u.matchTTL()); err != nil {
		u.rollbackReservations(ctx, []*matchv1.MatchTicketStorageRecord{ticket})
		return err
	}

	u.notifyMatching(ctx, memberPlayerIDs(members), matchID)
	plog.With(ctx).Infow("msg", "solo_match_found", "match_id", matchID, "ticket_id", ticket.TicketId, "players", len(members))
	u.onAllConfirmed(ctx, match)
	return nil
}

// formMatch 把两边票据组成一场 match:写 match record + 预留票据 + 推 FOUND/CONFIRM。
func (u *MatchUsecase) formMatch(ctx context.Context, sideA, sideB []*matchv1.MatchTicketStorageRecord) error {
	matchID := u.idGen.Generate()
	now := time.Now().UnixMilli()
	deadline := now + u.cfg.ConfirmTimeout.Std().Milliseconds()

	members := make([]*matchv1.MatchMemberStorageRecord, 0, 2*u.cfg.TeamSize)
	ticketIDs := make([]uint64, 0, len(sideA)+len(sideB))
	initialConfirm := confirmPending
	if u.cfg.AutoConfirmMatch {
		initialConfirm = confirmAccepted
	}
	collect := func(side []*matchv1.MatchTicketStorageRecord, sideIdx int32) {
		for _, t := range side {
			ticketIDs = append(ticketIDs, t.TicketId)
			for _, m := range t.Members {
				members = append(members, &matchv1.MatchMemberStorageRecord{
					PlayerId: m.PlayerId,
					TeamId:   m.TeamId,
					Mmr:      m.Mmr,
					HeroId:   m.HeroId,
					Side:     sideIdx,
					Confirm:  initialConfirm,
				})
			}
		}
	}
	collect(sideA, 0)
	collect(sideB, 1)

	match := &matchv1.MatchStorageRecord{
		MatchId:           matchID,
		Stage:             stageConfirm,
		Members:           members,
		TicketIds:         ticketIDs,
		CreatedAtMs:       now,
		ConfirmDeadlineMs: deadline,
	}

	// 一致性流程(先预留票据,再建 match):
	//   1. 逐张预留票据(移出队列 + 写 match_id),防止下一轮 matchOnce 重复撮合
	//   2. 任一票据预留失败 → 把已预留的票据全部退回队列(补偿),不建 match,返回错误
	//   3. 全部预留成功后才 CreateMatch;若 CreateMatch 仍失败 → 同样回滚全部预留
	// 终态只有两种:票据全在 match 里且已出队,或全部退回队列且无残留 match——
	// 不会出现"match 已建但部分票据仍在 queue"的不一致。
	reserved := make([]*matchv1.MatchTicketStorageRecord, 0, len(sideA)+len(sideB))
	for _, side := range [][]*matchv1.MatchTicketStorageRecord{sideA, sideB} {
		for _, t := range side {
			t.MatchId = matchID
			if err := u.repo.ReserveTicket(ctx, t, u.ticketTTL()); err != nil {
				u.rollbackReservations(ctx, reserved)
				plog.With(ctx).Errorw("msg", "reserve_ticket_failed", "match_id", matchID,
					"ticket_id", t.TicketId, "err", err)
				return fmt.Errorf("reserve ticket %d: %w", t.TicketId, err)
			}
			reserved = append(reserved, t)
		}
	}

	if err := u.repo.CreateMatch(ctx, match, u.matchTTL()); err != nil {
		u.rollbackReservations(ctx, reserved)
		plog.With(ctx).Errorw("msg", "create_match_failed", "match_id", matchID, "err", err)
		return err
	}
	// 撮合成局，成员进入确认期：上报 locator MATCHING（不变量 §1，弱依赖）
	u.notifyMatching(ctx, memberPlayerIDs(members), matchID)
	// 推 FOUND → CONFIRM 进度给全体(原则 3 例外:含发起方)
	u.pushProgress(ctx, matchID, stageFound, members, "", "")
	u.pushProgress(ctx, matchID, stageConfirm, members, "", "")
	plog.With(ctx).Infow("msg", "match_found", "match_id", matchID, "players", len(members))
	if u.cfg.AutoConfirmMatch {
		plog.With(ctx).Infow("msg", "match_auto_confirm", "match_id", matchID, "players", len(members))
		u.onAllConfirmed(ctx, match)
	}
	return nil
}

// rollbackReservations 把一批已预留的票据退回队列(清掉 match_id,保留 enqueued_at_ms),
// 用于 formMatch 中途失败时的补偿,避免票据停留在"已出队但无 match"的悬空状态。
func (u *MatchUsecase) rollbackReservations(ctx context.Context, reserved []*matchv1.MatchTicketStorageRecord) {
	for _, t := range reserved {
		t.MatchId = 0
		if err := u.repo.RequeueTicket(ctx, t, u.ticketTTL()); err != nil {
			plog.With(ctx).Warnw("msg", "rollback_reservation_failed", "ticket_id", t.TicketId, "err", err)
		}
	}
}

// expireOnce 扫描 active ZSET,把确认期已超时的 match 标记失败。
func (u *MatchUsecase) expireOnce(ctx context.Context) error {
	now := time.Now().UnixMilli()
	matchIDs, err := u.repo.RangeExpiredMatches(ctx, now)
	if err != nil {
		return err
	}
	for _, mid := range matchIDs {
		var snapshot *matchv1.MatchStorageRecord
		lerr := u.repo.UpdateMatchWithLock(ctx, mid, u.cfg.OptimisticRetry, func(m *matchv1.MatchStorageRecord) error {
			if m.Stage == stageReady || m.Stage == stageFailed || m.Stage == stageAllocating {
				snapshot = nil
				return nil // 已结算,无需超时处理
			}
			m.Stage = stageFailed
			snapshot = cloneMatch(m)
			return nil
		}, u.matchTTL())
		if lerr != nil {
			plog.With(ctx).Warnw("msg", "expire_lock_failed", "match_id", mid, "err", lerr)
			u.removeActive(ctx, mid)
			continue
		}
		if snapshot == nil {
			u.removeActive(ctx, mid)
			continue
		}
		// 超时:无明确拒绝者,全部票据退回队列(rejecterID=0)
		u.onMatchFailed(ctx, snapshot, 0)
		plog.With(ctx).Infow("msg", "match_confirm_timeout", "match_id", mid)
	}
	return nil
}

// ── push 辅助 ─────────────────────────────────────────────────────────────────

// pushProgress 给 members 全体推同一阶段进度(battle 字段为空时不填)。
func (u *MatchUsecase) pushProgress(ctx context.Context, matchID uint64, stage matchv1.MatchStage, members []*matchv1.MatchMemberStorageRecord, dsAddr, _ string) {
	if u.pusher == nil || len(members) == 0 {
		return
	}
	now := time.Now().UnixMilli()
	for _, m := range members {
		prog := buildProgress(matchID, stage, members, dsAddr, "")
		u.pushOneProgress(ctx, m.PlayerId, prog, now)
	}
}

// pushOne 给单个玩家推 READY 进度(带其专属 battle_ticket)。
func (u *MatchUsecase) pushOne(ctx context.Context, playerID uint64, m *matchv1.MatchStorageRecord, dsAddr, battleTicket string, nowMs int64) {
	if u.pusher == nil {
		return
	}
	prog := buildProgress(m.MatchId, m.Stage, m.Members, dsAddr, battleTicket)
	u.pushOneProgress(ctx, playerID, prog, nowMs)
}

func (u *MatchUsecase) pushOneProgress(ctx context.Context, playerID uint64, prog *matchv1.MatchProgress, nowMs int64) {
	event := &matchv1.MatchProgressEvent{
		Progress:   prog,
		ToPlayerId: playerID,
		TsMs:       nowMs,
	}
	payload, err := proto.Marshal(event)
	if err != nil {
		plog.With(ctx).Warnw("msg", "match_push_marshal_failed", "err", err, "to_player_id", playerID)
		return
	}
	// 原则 3 例外:callerID=0 → 发给所有人(含发起方)
	if _, err := u.pusher.PushMatchProgress(ctx, 0, []uint64{playerID}, payload); err != nil {
		plog.With(ctx).Warnw("msg", "match_push_failed", "to_player_id", playerID, "err", err)
	}
}

// rollbackClaims 释放一批玩家的队列归属(SETNX 回滚)。
func (u *MatchUsecase) rollbackClaims(ctx context.Context, playerIDs []uint64) {
	for _, pid := range playerIDs {
		if err := u.repo.DeletePlayerIndex(ctx, pid); err != nil {
			plog.With(ctx).Warnw("msg", "rollback_claim_failed", "player_id", pid, "err", err)
		}
	}
}
