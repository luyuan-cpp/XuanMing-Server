// Package biz 是 battle_result 服务的业务逻辑层(W4 ③,2026-06-06)。
//
// 职责(docs/design/go-services.md §2.13):
//   - 消费 pandora.battle.result → 幂等落库(不变量 §2,unique match_id)
//   - MMR 在此算(Elo,DS 不可信,不变量 §6),落 battle_player_stats.mmr_delta
//   - 消费 pandora.ds.lifecycle 的 ABANDONED → 写 abandoned 补偿记录(不变量 §4)
//   - 落库同事务写 player.update 出箱 → 后台发布器可靠投递(不变量 §4)
//   - 提供 GetMatchResult / ListPlayerHistory 查询 RPC
//
// 关键不变量:
//   - 幂等键 = match_id(SaveResult 命中唯一键 → alreadyRecorded,不重复写)
//   - MMR 覆盖 DS 上报值(只信对局胜负 winner_team,不信 DS 给的 mmr_delta)
//
// W4 ⑨ 可靠补偿(事务出箱,HANDOFF §3 Step 2):
//
//	W4 ③ 落库后直接发 player.update 是 best-effort 弱依赖,Kafka 不可用时事件直接丢
//	→ 玩家段位永不更新。W4 ⑨ 改为:落 battles + stats 的同一事务里再写 player.update
//	出箱行(原子提交);后台 RunOutboxPublisher 轮询出箱逐条投递 Kafka,成功才删行。
//	配合 player 服务幂等消费(W4 ④ mmr_history uk),整条段位写链是 at-least-once
//	可靠闭环,可穿越 Kafka 临时不可用。
package biz

import (
	"context"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	battlev1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/battle/v1"
	playerv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/player/v1"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/data"
)

// MMRReader 读玩家当前 MMR(算 Elo 期望胜率用)。
//
// W4 ③ player 服务未上线 → StaticMMRReader 全返 BaseMMR;player 上线后换 gRPC 实现。
type MMRReader interface {
	GetMMR(ctx context.Context, playerID uint64) (int, error)
}

// PlayerUpdatePusher 发 pandora.player.update 事件(kafka key=player_id,不变量 §9)。
//
// W4 ⑨ 起由后台 RunOutboxPublisher 调用:投递失败 → 返回 error → 出箱行保留下轮重试
// (不再是 best-effort 静默丢)。
type PlayerUpdatePusher interface {
	PushPlayerUpdate(ctx context.Context, playerID uint64, payload []byte) error
}

// MatchReleaser 通知 matchmaker 释放一场已结算/废弃对局的撮合状态(内部 RPC)。
//
// 修复:对局走完 READY → 进战斗 → 结算后,matchmaker 故意保留的 player→ticket 归属
// (SETNX claim)+ 票据 + match 镜像本只能等 30min TTL 自然过期;期间玩家回 Hub 再次
// StartMatch 会撞上残留 claim 报 ErrMatchAlreadyMatching(4002)。battle_result 落库后
// 主动调此接口让 matchmaker 彻底释放,玩家回 Hub 即可立刻再次匹配。
//
// best-effort 弱依赖:实现可为 nil(matchmaker 地址未配),调用失败仅 Warn 不影响结算落库。
type MatchReleaser interface {
	ReleaseMatch(ctx context.Context, matchID uint64, playerIDs []uint64) error
}

// StaticMMRReader 是固定返回 base 的 MMRReader(player 服务未上线时兜底)。
type StaticMMRReader struct {
	base int
}

// NewStaticMMRReader 构造。
func NewStaticMMRReader(base int) *StaticMMRReader { return &StaticMMRReader{base: base} }

// GetMMR 恒返 base。
func (s *StaticMMRReader) GetMMR(_ context.Context, _ uint64) (int, error) { return s.base, nil }

// BattleResultUsecase 是 battle_result 业务逻辑核心。
type BattleResultUsecase struct {
	repo     data.BattleRepo
	mmr      MMRReader
	pusher   PlayerUpdatePusher
	releaser MatchReleaser
	cfg      conf.BattleConf
}

// NewBattleResultUsecase 构造。pusher 可为 nil:player.update 已写事务出箱,
// pusher/producer 不可用时出箱积压不丢,等 producer 可用后由发布器补发(当前需重启/重配)。
// releaser 可为 nil:matchmaker 地址未配时跳过对局状态释放(best-effort 弱依赖)。
func NewBattleResultUsecase(repo data.BattleRepo, mmr MMRReader, pusher PlayerUpdatePusher, releaser MatchReleaser, cfg conf.BattleConf) *BattleResultUsecase {
	if mmr == nil {
		mmr = NewStaticMMRReader(cfg.BaseMMR)
	}
	return &BattleResultUsecase{repo: repo, mmr: mmr, pusher: pusher, releaser: releaser, cfg: cfg}
}

// releaseMatch 落库成功后通知 matchmaker 释放本局撮合状态。best-effort:实现缺省 / 失败
// 仅 Warn,绝不影响结算落库(弱依赖,不变量:结算是权威路径,释放只是清残留)。
func (u *BattleResultUsecase) releaseMatch(ctx context.Context, result *battlev1.BattleResult) {
	if u.releaser == nil {
		return
	}
	playerIDs := make([]uint64, 0, len(result.GetStats()))
	for _, s := range result.GetStats() {
		if pid := s.GetPlayerId(); pid != 0 {
			playerIDs = append(playerIDs, pid)
		}
	}
	if err := u.releaser.ReleaseMatch(ctx, result.GetMatchId(), playerIDs); err != nil {
		plog.With(ctx).Warnw("msg", "match_release_failed", "match_id", result.GetMatchId(), "err", err)
	}
}

// ── ReportResult:幂等落库 + MMR ─────────────────────────────────────────────

// ReportResult 落一场对局结算(消费 battle.result / 同步 RPC 共用)。
// 返回 alreadyRecorded:true 表示幂等命中,本次跳过(不算错误)。
func (u *BattleResultUsecase) ReportResult(ctx context.Context, result *battlev1.BattleResult) (bool, error) {
	if result == nil || result.GetMatchId() == 0 {
		return false, errcode.New(errcode.ErrInvalidArg, "match_id required")
	}
	if len(result.GetStats()) == 0 {
		return false, errcode.New(errcode.ErrInvalidArg, "stats required for match %d", result.GetMatchId())
	}

	// 正常结算:outcome 缺省补 NORMAL
	if result.GetOutcome() == battlev1.BattleOutcome_BATTLE_OUTCOME_UNSPECIFIED {
		result.Outcome = battlev1.BattleOutcome_BATTLE_OUTCOME_NORMAL
	}

	// MMR 仅对正常结算计算(不变量 §6,覆盖 DS 上报的 mmr_delta)。
	// ABANDONED 是补偿语义:权威路径是 ds.lifecycle → HandleAbandoned(delta 全 0,不掉段)。
	// 此处兜底:若 battle.result 误报 / 伪造 Outcome=ABANDONED,强制 delta 全 0,
	// 防止 DS 不可信地通过 abandoned 改玩家段位(不变量 §4/§6)。
	if result.GetOutcome() == battlev1.BattleOutcome_BATTLE_OUTCOME_ABANDONED {
		for _, s := range result.GetStats() {
			s.MmrDelta = 0
		}
	} else {
		u.assignMMR(ctx, result)
	}

	// MMR 算完才组装出箱(携带最终 mmr_delta);与落库同事务原子提交(不变量 §4)。
	abandoned := result.GetOutcome() == battlev1.BattleOutcome_BATTLE_OUTCOME_ABANDONED
	outbox, err := u.buildOutbox(result, abandoned)
	if err != nil {
		return false, err
	}

	already, err := u.repo.SaveResult(ctx, result, outbox)
	if err != nil {
		return false, err
	}
	if already {
		plog.With(ctx).Infow("msg", "battle_result_idempotent_hit", "match_id", result.GetMatchId())
		return true, nil
	}

	plog.With(ctx).Infow("msg", "battle_result_recorded",
		"match_id", result.GetMatchId(), "winner_team", result.GetWinnerTeam(),
		"outcome", result.GetOutcome().String(), "players", len(result.GetStats()))

	// 结算落库成功 → 通知 matchmaker 释放本局撮合状态(玩家回 Hub 可立刻再匹配)。
	u.releaseMatch(ctx, result)
	return false, nil
}

// ── HandleAbandoned:DS 崩溃补偿 ───────────────────────────────────────────────

// HandleAbandoned 处理 ds_allocator 发来的 ABANDONED 事件(不变量 §4)。
// 写一条 outcome=ABANDONED、mmr_delta 全 0 的补偿记录(幂等),并通知 player 段位回滚。
func (u *BattleResultUsecase) HandleAbandoned(ctx context.Context, matchID uint64, playerIDs []uint64, mapID uint32, gameMode string, tsMs int64) error {
	if matchID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "match_id required")
	}
	if tsMs <= 0 {
		tsMs = time.Now().UnixMilli()
	}

	stats := make([]*battlev1.PlayerStats, 0, len(playerIDs))
	for _, pid := range playerIDs {
		stats = append(stats, &battlev1.PlayerStats{PlayerId: pid, MmrDelta: 0})
	}
	result := &battlev1.BattleResult{
		MatchId:    matchID,
		EndedAtMs:  tsMs,
		WinnerTeam: winnerTeamDraw,
		Outcome:    battlev1.BattleOutcome_BATTLE_OUTCOME_ABANDONED,
		GameMode:   gameMode,
		MapId:      mapID,
		Stats:      stats,
	}

	// 出箱携 delta=0(不掉段)+ reason=abandon;与补偿记录同事务提交。
	outbox, err := u.buildOutbox(result, true)
	if err != nil {
		return err
	}

	already, err := u.repo.SaveResult(ctx, result, outbox)
	if err != nil {
		return err
	}
	if already {
		// 已有正常结算或已补偿过 → 不重复(不变量 §2)
		plog.With(ctx).Infow("msg", "abandoned_idempotent_hit", "match_id", matchID)
		return nil
	}
	plog.With(ctx).Infow("msg", "battle_abandoned_recorded", "match_id", matchID, "players", len(playerIDs))

	// 废弃对局补偿落库成功 → 同样释放撮合状态(玩家可立刻再匹配)。
	u.releaseMatch(ctx, result)
	return nil
}

// ── 查询 RPC ──────────────────────────────────────────────────────────────────

// GetMatchResult 读一场对局结算。
func (u *BattleResultUsecase) GetMatchResult(ctx context.Context, matchID uint64) (*battlev1.BattleResult, bool, error) {
	if matchID == 0 {
		return nil, false, errcode.New(errcode.ErrInvalidArg, "match_id required")
	}
	return u.repo.GetResult(ctx, matchID)
}

// ListPlayerHistory 倒序列出玩家战绩历史。
func (u *BattleResultUsecase) ListPlayerHistory(ctx context.Context, playerID uint64, limit int, beforeMs int64) ([]*battlev1.BattleResult, error) {
	if playerID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	return u.repo.ListPlayerHistory(ctx, playerID, limit, beforeMs)
}

// ── 辅助 ──────────────────────────────────────────────────────────────────────

// assignMMR 按两队当前 MMR 均值算 Elo delta,写回每个 stat.MmrDelta(不变量 §6)。
func (u *BattleResultUsecase) assignMMR(ctx context.Context, result *battlev1.BattleResult) {
	var sum0, n0, sum1, n1 int
	for _, s := range result.GetStats() {
		m, err := u.mmr.GetMMR(ctx, s.GetPlayerId())
		if err != nil {
			m = u.cfg.BaseMMR
			plog.With(ctx).Warnw("msg", "mmr_read_failed_fallback_base", "player_id", s.GetPlayerId(), "err", err)
		}
		if s.GetTeam() == winnerTeamA {
			sum0 += m
			n0++
		} else {
			sum1 += m
			n1++
		}
	}
	avgA := u.cfg.BaseMMR
	if n0 > 0 {
		avgA = sum0 / n0
	}
	avgB := u.cfg.BaseMMR
	if n1 > 0 {
		avgB = sum1 / n1
	}
	deltaA, deltaB := eloDeltas(avgA, avgB, u.cfg.EloKFactor, result.GetWinnerTeam())
	for _, s := range result.GetStats() {
		if s.GetTeam() == winnerTeamA {
			s.MmrDelta = int32(deltaA)
		} else {
			s.MmrDelta = int32(deltaB)
		}
	}
}

// buildOutbox 把一场结算的每个玩家组装成 player.update 出箱记录(待发布,与落库同事务)。
//
//	abandoned=true → reason 全 "abandon"(delta 已置 0,不掉段)
//	abandoned=false → 按胜负 win/lose/draw
func (u *BattleResultUsecase) buildOutbox(result *battlev1.BattleResult, abandoned bool) ([]data.OutboxRecord, error) {
	recs := make([]data.OutboxRecord, 0, len(result.GetStats()))
	for _, s := range result.GetStats() {
		reason := "abandon"
		if !abandoned {
			reason = reasonForTeam(s.GetTeam(), result.GetWinnerTeam())
		}
		evt := &playerv1.PlayerUpdateEvent{
			PlayerId: s.GetPlayerId(),
			MatchId:  result.GetMatchId(),
			MmrDelta: s.GetMmrDelta(),
			Reason:   reason,
			TsMs:     result.GetEndedAtMs(),
		}
		payload, err := proto.Marshal(evt)
		if err != nil {
			return nil, errcode.New(errcode.ErrInternal, "marshal player.update player=%d: %v", s.GetPlayerId(), err)
		}
		recs = append(recs, data.OutboxRecord{PlayerID: s.GetPlayerId(), Payload: payload})
	}
	return recs, nil
}

// ── player.update 事务出箱发布器(W4 ⑨,不变量 §4)─────────────────────────────

// RunOutboxPublisher 启动后台 player.update 出箱发布循环,直到 ctx 取消。
//
// 每轮取一批待发布出箱行(FIFO 按 id),逐条投递 Kafka;投递成功才删行。投递失败 →
// 本批中断、保留出箱行,下一轮重试(同玩家 key 有序,不变量 §9)。配合 player 服务幂等
// 消费(W4 ④ mmr_history uk),整条段位写链是 at-least-once 可靠闭环,可穿越 Kafka 临时不可用。
func (u *BattleResultUsecase) RunOutboxPublisher(ctx context.Context) {
	interval := u.cfg.OutboxPublishInterval.Std()
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	plog.With(ctx).Infow("msg", "outbox_publisher_started", "interval", interval.String(), "batch", u.outboxBatchSize())
	for {
		select {
		case <-ctx.Done():
			plog.With(ctx).Infow("msg", "outbox_publisher_stopped")
			return
		case <-ticker.C:
			if n, err := u.publishOutboxBatch(ctx); err != nil {
				plog.With(ctx).Warnw("msg", "outbox_publish_batch_failed", "published", n, "err", err)
			}
		}
	}
}

// publishOutboxBatch 取一批出箱记录投递,返回本轮成功投递并删除的条数。
// 投递失败立即中断本轮(保留出箱行下轮重试),保证同玩家事件按 id 顺序投递。
func (u *BattleResultUsecase) publishOutboxBatch(ctx context.Context) (int, error) {
	if u.pusher == nil {
		// kafka 未配置:出箱无法投递。出箱行已落库不丢,等 producer 可用后重启再发。
		return 0, nil
	}
	recs, err := u.repo.FetchOutbox(ctx, u.outboxBatchSize())
	if err != nil {
		return 0, err
	}
	published := 0
	for _, r := range recs {
		if perr := u.pusher.PushPlayerUpdate(ctx, r.PlayerID, r.Payload); perr != nil {
			return published, perr // 本轮中断,保留出箱行下轮重试
		}
		if derr := u.repo.DeleteOutbox(ctx, r.ID); derr != nil {
			return published, derr
		}
		published++
	}
	if published > 0 {
		plog.With(ctx).Infow("msg", "outbox_published", "count", published)
	}
	return published, nil
}

// outboxBatchSize 返回每轮发布批大小(配置缺省 128)。
func (u *BattleResultUsecase) outboxBatchSize() int {
	if u.cfg.OutboxBatchSize > 0 {
		return u.cfg.OutboxBatchSize
	}
	return 128
}
