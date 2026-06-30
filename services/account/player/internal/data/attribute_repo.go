package data

import (
	"context"
	"database/sql"
	"errors"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// GrantAttributePoints 幂等授予可分配点(不变量 §2 风格:idempotency_key 防重复授予)。
//
// 流程:
//  1. INSERT attr_point_grants(命中 uk → 幂等:读回当前 unspent 返回 already=true,不重复加)
//  2. UPDATE players SET unspent_attr_points += points
//  3. 读回 unspent
func (r *MySQLPlayerRepo) GrantAttributePoints(ctx context.Context, playerID uint64, points int32, idempotencyKey string) (int, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	const insGrant = `INSERT INTO attr_point_grants (player_id, idempotency_key, points) VALUES (?, ?, ?)`
	if _, gerr := tx.ExecContext(ctx, insGrant, playerID, idempotencyKey, points); gerr != nil {
		if isDupErr(gerr) {
			// 幂等命中:读回当前 unspent,不重复授予
			var cur int
			qerr := tx.QueryRowContext(ctx, `SELECT unspent_attr_points FROM players WHERE player_id = ? LIMIT 1`, playerID).Scan(&cur)
			if errors.Is(qerr, sql.ErrNoRows) {
				return 0, false, errcode.New(errcode.ErrPlayerNotFound, "player not found: %d", playerID)
			}
			if qerr != nil {
				return 0, false, errcode.New(errcode.ErrInternal, "read unspent player=%d: %v", playerID, qerr)
			}
			return cur, true, nil
		}
		return 0, false, errcode.New(errcode.ErrInternal, "insert grant player=%d: %v", playerID, gerr)
	}

	const updPlayer = `UPDATE players SET unspent_attr_points = unspent_attr_points + ? WHERE player_id = ?`
	res, uerr := tx.ExecContext(ctx, updPlayer, points, playerID)
	if uerr != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "grant unspent player=%d: %v", playerID, uerr)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return 0, false, errcode.New(errcode.ErrPlayerNotFound, "player not found: %d", playerID)
	}

	var unspent int
	if qerr := tx.QueryRowContext(ctx, `SELECT unspent_attr_points FROM players WHERE player_id = ? LIMIT 1`, playerID).Scan(&unspent); qerr != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "read unspent player=%d: %v", playerID, qerr)
	}
	if cerr := tx.Commit(); cerr != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "commit grant player=%d: %v", playerID, cerr)
	}
	return unspent, false, nil
}

// AllocateAttributePoints 分配点(事务:锁 players 行校验 unspent>=sum,扣减,累加 player_attributes)。
func (r *MySQLPlayerRepo) AllocateAttributePoints(ctx context.Context, playerID uint64, allocs []AttrAllocation) (int, error) {
	var sum int32
	for _, a := range allocs {
		sum += a.Points
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	var unspent int
	err = tx.QueryRowContext(ctx, `SELECT unspent_attr_points FROM players WHERE player_id = ? FOR UPDATE`, playerID).Scan(&unspent)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errcode.New(errcode.ErrPlayerNotFound, "player not found: %d", playerID)
	}
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "lock player=%d: %v", playerID, err)
	}
	if int(sum) > unspent {
		return 0, errcode.New(errcode.ErrPlayerInsufficientPoints, "insufficient points player=%d need=%d have=%d", playerID, sum, unspent)
	}

	const upsert = `INSERT INTO player_attributes (player_id, attr_key, points) VALUES (?, ?, ?)
ON DUPLICATE KEY UPDATE points = points + VALUES(points)`
	for _, a := range allocs {
		if _, aerr := tx.ExecContext(ctx, upsert, playerID, a.Key, a.Points); aerr != nil {
			return 0, errcode.New(errcode.ErrInternal, "upsert attr player=%d key=%s: %v", playerID, a.Key, aerr)
		}
	}

	newUnspent := unspent - int(sum)
	if _, uerr := tx.ExecContext(ctx, `UPDATE players SET unspent_attr_points = ? WHERE player_id = ?`, newUnspent, playerID); uerr != nil {
		return 0, errcode.New(errcode.ErrInternal, "deduct unspent player=%d: %v", playerID, uerr)
	}
	if cerr := tx.Commit(); cerr != nil {
		return 0, errcode.New(errcode.ErrInternal, "commit allocate player=%d: %v", playerID, cerr)
	}
	return newUnspent, nil
}

// ResetAttributes 洗点(事务:锁 players 行,sum(已分配点)退回 unspent,清空 player_attributes)。
func (r *MySQLPlayerRepo) ResetAttributes(ctx context.Context, playerID uint64) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	var unspent int
	err = tx.QueryRowContext(ctx, `SELECT unspent_attr_points FROM players WHERE player_id = ? FOR UPDATE`, playerID).Scan(&unspent)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errcode.New(errcode.ErrPlayerNotFound, "player not found: %d", playerID)
	}
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "lock player=%d: %v", playerID, err)
	}

	var allocated int
	if qerr := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(points), 0) FROM player_attributes WHERE player_id = ?`, playerID).Scan(&allocated); qerr != nil {
		return 0, errcode.New(errcode.ErrInternal, "sum attr player=%d: %v", playerID, qerr)
	}
	if _, derr := tx.ExecContext(ctx, `DELETE FROM player_attributes WHERE player_id = ?`, playerID); derr != nil {
		return 0, errcode.New(errcode.ErrInternal, "clear attr player=%d: %v", playerID, derr)
	}

	newUnspent := unspent + allocated
	if _, uerr := tx.ExecContext(ctx, `UPDATE players SET unspent_attr_points = ? WHERE player_id = ?`, newUnspent, playerID); uerr != nil {
		return 0, errcode.New(errcode.ErrInternal, "restore unspent player=%d: %v", playerID, uerr)
	}
	if cerr := tx.Commit(); cerr != nil {
		return 0, errcode.New(errcode.ErrInternal, "commit reset player=%d: %v", playerID, cerr)
	}
	return newUnspent, nil
}

func (r *MySQLPlayerRepo) GetAttributes(ctx context.Context, playerID uint64) ([]AttrPoint, int, error) {
	const q = `SELECT attr_key, points FROM player_attributes WHERE player_id = ? ORDER BY attr_key`
	rows, err := r.db.QueryContext(ctx, q, playerID)
	if err != nil {
		return nil, 0, errcode.New(errcode.ErrInternal, "query attrs player=%d: %v", playerID, err)
	}
	defer func() { _ = rows.Close() }()

	var attrs []AttrPoint
	for rows.Next() {
		var a AttrPoint
		if serr := rows.Scan(&a.Key, &a.Points); serr != nil {
			return nil, 0, errcode.New(errcode.ErrInternal, "scan attr player=%d: %v", playerID, serr)
		}
		attrs = append(attrs, a)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, 0, errcode.New(errcode.ErrInternal, "iterate attrs player=%d: %v", playerID, rerr)
	}

	var unspent int
	uerr := r.db.QueryRowContext(ctx, `SELECT unspent_attr_points FROM players WHERE player_id = ? LIMIT 1`, playerID).Scan(&unspent)
	if errors.Is(uerr, sql.ErrNoRows) {
		return attrs, 0, nil
	}
	if uerr != nil {
		return nil, 0, errcode.New(errcode.ErrInternal, "read unspent player=%d: %v", playerID, uerr)
	}
	return attrs, unspent, nil
}
