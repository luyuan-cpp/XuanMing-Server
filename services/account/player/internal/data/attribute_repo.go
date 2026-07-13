package data

import (
	"context"
	"database/sql"
	"errors"
	"math"

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
//
// repo 自守(不依赖上层限制):逐项 checked-add 累计,拒非正点数,校验请求总和、单属性列
// 「当前值 + 增量」与 unspent 均不越有符号 INT 列上界(MaxInt32);任一越界返回业务错误且零写入。
func (r *MySQLPlayerRepo) AllocateAttributePoints(ctx context.Context, playerID uint64, allocs []AttrAllocation) (int, error) {
	// 1) 请求级 checked 累计:按 attr_key 归并增量,单值 <= MaxInt32,累加前必 < 2*MaxInt32,不溢出。
	if len(allocs) == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "allocations required")
	}
	perKey := make(map[string]int64, len(allocs))
	var sum int64
	for _, a := range allocs {
		if a.Key == "" {
			return 0, errcode.New(errcode.ErrInvalidArg, "attr_key must not be empty")
		}
		if a.Points <= 0 {
			return 0, errcode.New(errcode.ErrInvalidArg, "points must be positive: %s", a.Key)
		}
		perKey[a.Key] += int64(a.Points)
		if perKey[a.Key] > math.MaxInt32 {
			return 0, errcode.New(errcode.ErrInvalidArg, "attr %s allocation out of range", a.Key)
		}
		sum += int64(a.Points)
		if sum > math.MaxInt32 {
			return 0, errcode.New(errcode.ErrPlayerInsufficientPoints, "total allocation out of range")
		}
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
	// 2) 总和不得超过可分配点(sum <= MaxInt32 已保证,unspent 为 INT,int64 比较安全)。
	if sum > int64(unspent) {
		return 0, errcode.New(errcode.ErrPlayerInsufficientPoints, "insufficient points player=%d need=%d have=%d", playerID, sum, unspent)
	}

	// 3) 权威列上界:锁定并读取受影响属性当前值,校验「当前值 + 增量」不越 INT 列上界(MaxInt32)。
	//    在任何写入前完成校验,越界直接返回(defer Rollback 保证零写入)。
	existing := make(map[string]int64, len(perKey))
	rows, qerr := tx.QueryContext(ctx, `SELECT attr_key, points FROM player_attributes WHERE player_id = ? FOR UPDATE`, playerID)
	if qerr != nil {
		return 0, errcode.New(errcode.ErrInternal, "lock attrs player=%d: %v", playerID, qerr)
	}
	for rows.Next() {
		var k string
		var p int64
		if serr := rows.Scan(&k, &p); serr != nil {
			_ = rows.Close()
			return 0, errcode.New(errcode.ErrInternal, "scan attr player=%d: %v", playerID, serr)
		}
		existing[k] = p
	}
	if rerr := rows.Err(); rerr != nil {
		_ = rows.Close()
		return 0, errcode.New(errcode.ErrInternal, "iterate attrs player=%d: %v", playerID, rerr)
	}
	_ = rows.Close()
	for k, delta := range perKey {
		if existing[k]+delta > math.MaxInt32 {
			return 0, errcode.New(errcode.ErrInvalidArg, "attr %s cumulative points out of range player=%d", k, playerID)
		}
	}

	// 4) 写入:按归并后的增量逐属性 upsert(等价原逐条累加,消除重复 key 的列溢出隐患)。
	const upsert = `INSERT INTO player_attributes (player_id, attr_key, points) VALUES (?, ?, ?)
ON DUPLICATE KEY UPDATE points = points + VALUES(points)`
	for k, delta := range perKey {
		if _, aerr := tx.ExecContext(ctx, upsert, playerID, k, delta); aerr != nil {
			return 0, errcode.New(errcode.ErrInternal, "upsert attr player=%d key=%s: %v", playerID, k, aerr)
		}
	}

	newUnspent := unspent - int(sum) // sum <= unspent,newUnspent >= 0
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
