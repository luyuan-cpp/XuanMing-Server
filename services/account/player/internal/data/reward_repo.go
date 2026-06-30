package data

import (
	"context"
	"database/sql"
	"errors"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// ── 领奖记录 ───────────────────────────────────────────────────────────────────

// LoadRewardClaims 读玩家领奖记录(player_reward_claims.record bytes + version 乐观锁)。
// 未建行 → (nil, 0, nil)。
func (r *MySQLPlayerRepo) LoadRewardClaims(ctx context.Context, playerID uint64) ([]byte, int32, error) {
	const q = `SELECT record, version FROM player_reward_claims WHERE player_id = ? LIMIT 1`
	var (
		record  []byte
		version int32
	)
	err := r.db.QueryRowContext(ctx, q, playerID).Scan(&record, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, errcode.New(errcode.ErrInternal, "load reward claims player=%d: %v", playerID, err)
	}
	return record, version, nil
}

// SaveRewardClaims 乐观锁写领奖记录。expectVersion==0 → INSERT;>0 → UPDATE ... WHERE version。
// 版本不匹配 / 并发冲突 → ErrPlayerVersionMismatch(由 biz 决定是否重试)。
func (r *MySQLPlayerRepo) SaveRewardClaims(ctx context.Context, playerID uint64, record []byte, expectVersion int32) error {
	if expectVersion == 0 {
		const ins = `INSERT INTO player_reward_claims (player_id, record, version) VALUES (?, ?, 1)`
		if _, err := r.db.ExecContext(ctx, ins, playerID, record); err != nil {
			if isDupErr(err) {
				return errcode.New(errcode.ErrPlayerVersionMismatch,
					"reward claims player=%d already exists (expect new)", playerID)
			}
			return errcode.New(errcode.ErrInternal, "insert reward claims player=%d: %v", playerID, err)
		}
		return nil
	}

	const upd = `UPDATE player_reward_claims SET record = ?, version = version + 1
WHERE player_id = ? AND version = ?`
	res, err := r.db.ExecContext(ctx, upd, record, playerID, expectVersion)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "update reward claims player=%d: %v", playerID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return errcode.New(errcode.ErrInternal, "rows affected reward claims player=%d: %v", playerID, err)
	}
	if rows == 0 {
		return errcode.New(errcode.ErrPlayerVersionMismatch,
			"reward claims player=%d version mismatch (expect %d)", playerID, expectVersion)
	}
	return nil
}
