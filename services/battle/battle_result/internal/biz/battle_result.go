// Package biz 是 battle_result 服务的业务逻辑层(W4 ③,2026-06-06)。
//
// 职责(docs/design/go-services.md §2.13):
//   - 消费 pandora.battle.result → 幂等落库(不变量 §2,unique match_id)
//   - MMR 在此算(Elo,DS 不可信,不变量 §6),落 battle_player_stats.mmr_delta
//   - 消费 pandora.ds.lifecycle 的 ABANDONED → 写 abandoned 补偿记录(不变量 §4)
//   - 落库后发 pandora.player.update 事件(player 服务上线后消费做幂等 UpdateMMR)
//   - 提供 GetMatchResult / ListPlayerHistory 查询 RPC
//
// 关键不变量:
//   - 幂等键 = match_id(SaveResult 命中唯一键 → alreadyRecorded,不重复写)
//   - MMR 覆盖 DS 上报值(只信对局胜负 winner_team,不信 DS 给的 mmr_delta)
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
// 弱依赖:实现内部 fail 静默(返回 error 仅记日志,不阻断落库)。
type PlayerUpdatePusher interface {
	PushPlayerUpdate(ctx context.Context, playerID uint64, payload []byte) error
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
	repo   data.BattleRepo
	mmr    MMRReader
	pusher PlayerUpdatePusher
	cfg    conf.BattleConf
}

// NewBattleResultUsecase 构造。pusher 可为 nil(kafka 不可用时静默丢弃 player.update)。
func NewBattleResultUsecase(repo data.BattleRepo, mmr MMRReader, pusher PlayerUpdatePusher, cfg conf.BattleConf) *BattleResultUsecase {
	if mmr == nil {
		mmr = NewStaticMMRReader(cfg.BaseMMR)
	}
	return &BattleResultUsecase{repo: repo, mmr: mmr, pusher: pusher, cfg: cfg}
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

	already, err := u.repo.SaveResult(ctx, result)
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

	// 落库成功才推 player.update(player 上线后据此幂等改段位)
	u.pushPlayerUpdates(ctx, result)
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

	already, err := u.repo.SaveResult(ctx, result)
	if err != nil {
		return err
	}
	if already {
		// 已有正常结算或已补偿过 → 不重复(不变量 §2)
		plog.With(ctx).Infow("msg", "abandoned_idempotent_hit", "match_id", matchID)
		return nil
	}
	plog.With(ctx).Infow("msg", "battle_abandoned_recorded", "match_id", matchID, "players", len(playerIDs))

	// 通知玩家段位回滚(delta=0:不掉段)
	for _, pid := range playerIDs {
		u.pushOne(ctx, pid, matchID, 0, "abandon", tsMs)
	}
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

// pushPlayerUpdates 给每个玩家发 player.update 携带其 mmr_delta。
func (u *BattleResultUsecase) pushPlayerUpdates(ctx context.Context, result *battlev1.BattleResult) {
	for _, s := range result.GetStats() {
		reason := reasonForTeam(s.GetTeam(), result.GetWinnerTeam())
		u.pushOne(ctx, s.GetPlayerId(), result.GetMatchId(), s.GetMmrDelta(), reason, result.GetEndedAtMs())
	}
}

// pushOne 发单个玩家的 player.update 事件(弱依赖:pusher nil 或失败仅记日志)。
func (u *BattleResultUsecase) pushOne(ctx context.Context, playerID, matchID uint64, mmrDelta int32, reason string, tsMs int64) {
	if u.pusher == nil {
		return
	}
	evt := &playerv1.PlayerUpdateEvent{
		PlayerId: playerID,
		MatchId:  matchID,
		MmrDelta: mmrDelta,
		Reason:   reason,
		TsMs:     tsMs,
	}
	payload, err := proto.Marshal(evt)
	if err != nil {
		plog.With(ctx).Warnw("msg", "player_update_marshal_failed", "player_id", playerID, "err", err)
		return
	}
	if perr := u.pusher.PushPlayerUpdate(ctx, playerID, payload); perr != nil {
		plog.With(ctx).Warnw("msg", "player_update_push_failed", "player_id", playerID, "match_id", matchID, "err", perr)
	}
}
