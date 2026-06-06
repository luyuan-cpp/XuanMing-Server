// Package data 是 battle_result 服务的数据层(MySQL 战斗结算落库)。
//
// 库表(deploy/mysql-init/03-battle-tables.sql):
//
//	pandora_battle.battles              对局结算头(PK match_id,幂等键,不变量 §2)
//	pandora_battle.battle_player_stats  玩家战绩 + MMR 变化(uk match_id+player_id)
//
// 幂等:SaveResult 在一个事务里 INSERT battles;命中 1062 唯一键冲突 → 视为已落库
// (alreadyRecorded=true),回滚不重复写 stats。MMR 在 biz 层算好后由本层落库。
package data

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	battlev1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/battle/v1"
)

// OutboxRecord 是一条待发布的 player.update 事务出箱记录(W4 ⑨,不变量 §4)。
//
// 落 battles + battle_player_stats 的同一事务里写入,二者原子提交;后台发布器轮询
// 出箱表逐条投递 Kafka,投递成功才删行。ID 仅 FetchOutbox 返回时填充。
type OutboxRecord struct {
	ID       int64  // 出箱行主键(SaveResult 入参时忽略,FetchOutbox 返回时填充)
	PlayerID uint64 // kafka key(不变量 §9 同玩家事件保序)
	Payload  []byte // player.v1.PlayerUpdateEvent proto bytes
}

// BattleRepo 是 battle_result 数据层抽象。biz 层只依赖此接口,不依赖 *sql.DB。
type BattleRepo interface {
	// SaveResult 事务写 battles + battle_player_stats + player_update_outbox。
	// 三者原子提交(不变量 §4:落库与待发布段位事件不会半成功)。
	// 幂等:match_id 已存在 → 返回 (true, nil),不重复写(出箱也不写)。
	SaveResult(ctx context.Context, result *battlev1.BattleResult, outbox []OutboxRecord) (alreadyRecorded bool, err error)
	// GetResult 读一场对局结算(含全部玩家战绩)。not found → (nil, false, nil)。
	GetResult(ctx context.Context, matchID uint64) (*battlev1.BattleResult, bool, error)
	// ListPlayerHistory 倒序列出玩家参与的对局(ended_at_ms < beforeMs,最多 limit 条)。
	// beforeMs<=0 表示从最新开始。
	ListPlayerHistory(ctx context.Context, playerID uint64, limit int, beforeMs int64) ([]*battlev1.BattleResult, error)
	// FetchOutbox 按 id 升序取最多 limit 条待发布出箱记录(FIFO 保序)。
	FetchOutbox(ctx context.Context, limit int) ([]OutboxRecord, error)
	// DeleteOutbox 删除已成功投递的出箱行。
	DeleteOutbox(ctx context.Context, id int64) error
}

// MySQLBattleRepo 是基于 database/sql 的 BattleRepo 实现。
type MySQLBattleRepo struct {
	db *sql.DB
}

// NewMySQLBattleRepo 构造。db 由 pkg/mysqlx.MustNewClient 提供(连 pandora_battle 库)。
func NewMySQLBattleRepo(db *sql.DB) *MySQLBattleRepo {
	return &MySQLBattleRepo{db: db}
}

func (r *MySQLBattleRepo) SaveResult(ctx context.Context, result *battlev1.BattleResult, outbox []OutboxRecord) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, errcode.New(errcode.ErrBattleResultDBWrite, "begin tx: %v", err)
	}
	// 任何提前 return 前回滚;Commit 成功后 Rollback 返回 ErrTxDone 可忽略。
	defer func() { _ = tx.Rollback() }()

	const insBattle = `INSERT INTO battles
(match_id, started_at_ms, ended_at_ms, winner_team, outcome, ds_pod_name, game_mode, map_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	_, err = tx.ExecContext(ctx, insBattle,
		result.GetMatchId(),
		result.GetStartedAtMs(),
		result.GetEndedAtMs(),
		result.GetWinnerTeam(),
		int32(result.GetOutcome()),
		result.GetDsPodName(),
		result.GetGameMode(),
		result.GetMapId(),
	)
	if err != nil {
		if isDupErr(err) {
			// 幂等命中:同 match_id 已落库,本次跳过(不变量 §2)
			return true, nil
		}
		return false, errcode.New(errcode.ErrBattleResultDBWrite, "insert battles match=%d: %v", result.GetMatchId(), err)
	}

	const insStat = `INSERT INTO battle_player_stats
(match_id, player_id, hero_id, team, kills, deaths, assists, damage_dealt, damage_taken, healing, gold, mmr_delta)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	for _, s := range result.GetStats() {
		if _, serr := tx.ExecContext(ctx, insStat,
			result.GetMatchId(),
			s.GetPlayerId(),
			s.GetHeroId(),
			s.GetTeam(),
			s.GetKills(),
			s.GetDeaths(),
			s.GetAssists(),
			s.GetDamageDealt(),
			s.GetDamageTaken(),
			s.GetHealing(),
			s.GetGold(),
			s.GetMmrDelta(),
		); serr != nil {
			return false, errcode.New(errcode.ErrBattleResultDBWrite, "insert stats match=%d player=%d: %v",
				result.GetMatchId(), s.GetPlayerId(), serr)
		}
	}

	// 同事务写 player.update 出箱(不变量 §4:落库与待发布段位事件原子提交)。
	const insOutbox = `INSERT INTO player_update_outbox
(match_id, player_id, payload, created_at_ms)
VALUES (?, ?, ?, ?)`
	nowMs := time.Now().UnixMilli()
	for _, o := range outbox {
		if _, oerr := tx.ExecContext(ctx, insOutbox,
			result.GetMatchId(), o.PlayerID, o.Payload, nowMs,
		); oerr != nil {
			return false, errcode.New(errcode.ErrBattleResultDBWrite, "insert outbox match=%d player=%d: %v",
				result.GetMatchId(), o.PlayerID, oerr)
		}
	}

	if cerr := tx.Commit(); cerr != nil {
		return false, errcode.New(errcode.ErrBattleResultDBWrite, "commit match=%d: %v", result.GetMatchId(), cerr)
	}
	return false, nil
}

// FetchOutbox 按 id 升序取最多 limit 条待发布出箱记录(FIFO 保序)。
func (r *MySQLBattleRepo) FetchOutbox(ctx context.Context, limit int) ([]OutboxRecord, error) {
	if limit <= 0 {
		limit = 128
	}
	const q = `SELECT id, player_id, payload FROM player_update_outbox ORDER BY id ASC LIMIT ?`
	rows, err := r.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "query outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]OutboxRecord, 0, limit)
	for rows.Next() {
		var rec OutboxRecord
		if serr := rows.Scan(&rec.ID, &rec.PlayerID, &rec.Payload); serr != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan outbox: %v", serr)
		}
		out = append(out, rec)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate outbox: %v", rerr)
	}
	return out, nil
}

// DeleteOutbox 删除已成功投递的出箱行。
func (r *MySQLBattleRepo) DeleteOutbox(ctx context.Context, id int64) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM player_update_outbox WHERE id = ?`, id); err != nil {
		return errcode.New(errcode.ErrInternal, "delete outbox id=%d: %v", id, err)
	}
	return nil
}

func (r *MySQLBattleRepo) GetResult(ctx context.Context, matchID uint64) (*battlev1.BattleResult, bool, error) {
	const q = `SELECT started_at_ms, ended_at_ms, winner_team, outcome, ds_pod_name, game_mode, map_id
FROM battles WHERE match_id = ? LIMIT 1`
	var (
		startedAt, endedAt  int64
		winnerTeam, outcome int32
		dsPod, gameMode     string
		mapID               uint32
	)
	err := r.db.QueryRowContext(ctx, q, matchID).Scan(
		&startedAt, &endedAt, &winnerTeam, &outcome, &dsPod, &gameMode, &mapID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "query battle match=%d: %v", matchID, err)
	}

	stats, err := r.loadStats(ctx, matchID)
	if err != nil {
		return nil, false, err
	}
	return &battlev1.BattleResult{
		MatchId:     matchID,
		StartedAtMs: startedAt,
		EndedAtMs:   endedAt,
		WinnerTeam:  winnerTeam,
		Outcome:     battlev1.BattleOutcome(outcome),
		DsPodName:   dsPod,
		GameMode:    gameMode,
		MapId:       mapID,
		Stats:       stats,
	}, true, nil
}

func (r *MySQLBattleRepo) ListPlayerHistory(ctx context.Context, playerID uint64, limit int, beforeMs int64) ([]*battlev1.BattleResult, error) {
	if limit <= 0 {
		limit = 20
	}
	// 先取玩家参与的 match_id(按结束时间倒序,游标分页),再逐场 load 完整结算。
	const q = `SELECT b.match_id FROM battle_player_stats s
JOIN battles b ON b.match_id = s.match_id
WHERE s.player_id = ? AND (? <= 0 OR b.ended_at_ms < ?)
ORDER BY b.ended_at_ms DESC
LIMIT ?`
	rows, err := r.db.QueryContext(ctx, q, playerID, beforeMs, beforeMs, limit)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "query history player=%d: %v", playerID, err)
	}
	defer func() { _ = rows.Close() }()

	matchIDs := make([]uint64, 0, limit)
	for rows.Next() {
		var mid uint64
		if serr := rows.Scan(&mid); serr != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan history player=%d: %v", playerID, serr)
		}
		matchIDs = append(matchIDs, mid)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate history player=%d: %v", playerID, rerr)
	}

	out := make([]*battlev1.BattleResult, 0, len(matchIDs))
	for _, mid := range matchIDs {
		res, found, gerr := r.GetResult(ctx, mid)
		if gerr != nil {
			return nil, gerr
		}
		if found {
			out = append(out, res)
		}
	}
	return out, nil
}

// loadStats 读一场对局的全部玩家战绩。
func (r *MySQLBattleRepo) loadStats(ctx context.Context, matchID uint64) ([]*battlev1.PlayerStats, error) {
	const q = `SELECT player_id, hero_id, team, kills, deaths, assists,
damage_dealt, damage_taken, healing, gold, mmr_delta
FROM battle_player_stats WHERE match_id = ? ORDER BY team, player_id`
	rows, err := r.db.QueryContext(ctx, q, matchID)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "query stats match=%d: %v", matchID, err)
	}
	defer func() { _ = rows.Close() }()

	var stats []*battlev1.PlayerStats
	for rows.Next() {
		s := &battlev1.PlayerStats{}
		if serr := rows.Scan(
			&s.PlayerId, &s.HeroId, &s.Team, &s.Kills, &s.Deaths, &s.Assists,
			&s.DamageDealt, &s.DamageTaken, &s.Healing, &s.Gold, &s.MmrDelta,
		); serr != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan stats match=%d: %v", matchID, serr)
		}
		stats = append(stats, s)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate stats match=%d: %v", matchID, rerr)
	}
	return stats, nil
}

// isDupErr 判断是否 MySQL 唯一键冲突(1062 ER_DUP_ENTRY)。
// 字符串匹配避免强依赖 driver 错误类型(对齐 login data/account.go)。
func isDupErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "1062") || strings.Contains(msg, "Duplicate entry")
}
