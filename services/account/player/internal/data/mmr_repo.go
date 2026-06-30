package data

import (
	"context"
	"database/sql"
	"errors"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

func (r *MySQLPlayerRepo) GetMMR(ctx context.Context, playerID uint64) (int, bool, error) {
	const q = `SELECT mmr FROM players WHERE player_id = ? LIMIT 1`
	var mmr int
	err := r.db.QueryRowContext(ctx, q, playerID).Scan(&mmr)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "query mmr player=%d: %v", playerID, err)
	}
	return mmr, true, nil
}

// ApplyMMRChange 在一个事务里幂等改 MMR(不变量 §2)。
//
// 流程:
//  1. SELECT mmr FOR UPDATE 锁玩家行(玩家须先 EnsureProfile 存在)
//  2. INSERT mmr_history(命中 uk → 幂等:读回已记录 new_mmr 返回,不重复改 players)
//  3. UPDATE players SET mmr=clamp(old+delta), total_battles/total_wins 按语义累加
func (r *MySQLPlayerRepo) ApplyMMRChange(ctx context.Context, change MMRChange) (int, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	var oldMMR int
	err = tx.QueryRowContext(ctx, `SELECT mmr FROM players WHERE player_id = ? FOR UPDATE`, change.PlayerID).Scan(&oldMMR)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, errcode.New(errcode.ErrPlayerNotFound, "player not found: %d", change.PlayerID)
	}
	if err != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "lock player=%d: %v", change.PlayerID, err)
	}

	newMMR := oldMMR + int(change.Delta)
	if newMMR < change.Floor {
		newMMR = change.Floor
	}

	const insHist = `INSERT INTO mmr_history (player_id, idempotency_key, delta, reason, old_mmr, new_mmr)
VALUES (?, ?, ?, ?, ?, ?)`
	if _, herr := tx.ExecContext(ctx, insHist,
		change.PlayerID, change.IdempotencyKey, change.Delta, change.Reason, oldMMR, newMMR); herr != nil {
		if isDupErr(herr) {
			// 幂等命中:读回该 idempotency_key 已记录的 new_mmr(不重复改 players)
			var recordedNew int
			qerr := tx.QueryRowContext(ctx,
				`SELECT new_mmr FROM mmr_history WHERE player_id = ? AND idempotency_key = ? LIMIT 1`,
				change.PlayerID, change.IdempotencyKey).Scan(&recordedNew)
			if qerr != nil {
				return 0, false, errcode.New(errcode.ErrInternal, "read idem mmr player=%d key=%s: %v",
					change.PlayerID, change.IdempotencyKey, qerr)
			}
			return recordedNew, true, nil
		}
		return 0, false, errcode.New(errcode.ErrInternal, "insert mmr_history player=%d: %v", change.PlayerID, herr)
	}

	battleInc := 0
	if change.IncBattle {
		battleInc = 1
	}
	winInc := 0
	if change.IncWin {
		winInc = 1
	}
	const updPlayer = `UPDATE players SET mmr = ?, total_battles = total_battles + ?, total_wins = total_wins + ?
WHERE player_id = ?`
	if _, uerr := tx.ExecContext(ctx, updPlayer, newMMR, battleInc, winInc, change.PlayerID); uerr != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "update player=%d mmr: %v", change.PlayerID, uerr)
	}

	if cerr := tx.Commit(); cerr != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "commit player=%d: %v", change.PlayerID, cerr)
	}
	return newMMR, false, nil
}
