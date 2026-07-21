package data

import (
	"context"
	"database/sql"
	"errors"

	"github.com/luyuancpp/pandora/pkg/errcode"
	playerv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/player/v1"
)

func (r *MySQLPlayerRepo) EnsureProfile(ctx context.Context, playerID uint64, defaultNickname string, baseMMR int) error {
	const q = `INSERT IGNORE INTO players (player_id, nickname, level, mmr, avatar, total_battles, total_wins)
VALUES (?, ?, 1, ?, '', 0, 0)`
	if _, err := r.db.ExecContext(ctx, q, playerID, defaultNickname, baseMMR); err != nil {
		return errcode.New(errcode.ErrInternal, "ensure profile player=%d: %v", playerID, err)
	}
	return nil
}

func (r *MySQLPlayerRepo) GetProfile(ctx context.Context, playerID uint64) (*playerv1.PlayerProfile, bool, error) {
	const q = `SELECT nickname, level, mmr, avatar,
UNIX_TIMESTAMP(created_at)*1000, UNIX_TIMESTAMP(last_seen_at)*1000, total_battles, total_wins, exp
FROM players WHERE player_id = ? LIMIT 1`
	p := &playerv1.PlayerProfile{PlayerId: playerID}
	err := r.db.QueryRowContext(ctx, q, playerID).Scan(
		&p.Nickname, &p.Level, &p.Mmr, &p.Avatar,
		&p.CreatedAtMs, &p.LastSeenMs, &p.TotalBattles, &p.TotalWins, &p.ExpInLevel)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "query profile player=%d: %v", playerID, err)
	}
	return p, true, nil
}

func (r *MySQLPlayerRepo) UpdateNickname(ctx context.Context, playerID uint64, nickname string) error {
	const q = `UPDATE players SET nickname = ? WHERE player_id = ?`
	res, err := r.db.ExecContext(ctx, q, nickname, playerID)
	if err != nil {
		if isDupErr(err) {
			return errcode.New(errcode.ErrPlayerNicknameTaken, "nickname taken: %s", nickname)
		}
		return errcode.New(errcode.ErrInternal, "update nickname player=%d: %v", playerID, err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		// 0 行受影响有两种可能:玩家不存在,或昵称未变。确认玩家是否存在以区分。
		var exists int
		qerr := r.db.QueryRowContext(ctx, `SELECT 1 FROM players WHERE player_id = ? LIMIT 1`, playerID).Scan(&exists)
		if errors.Is(qerr, sql.ErrNoRows) {
			return errcode.New(errcode.ErrPlayerNotFound, "player not found: %d", playerID)
		}
		if qerr != nil {
			return errcode.New(errcode.ErrInternal, "check player exists %d: %v", playerID, qerr)
		}
		// 玩家存在但昵称未变 → 幂等成功
	}
	return nil
}
