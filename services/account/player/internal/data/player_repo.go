// Package data 是 player 服务的数据层(MySQL 玩家档案 / 段位 / 英雄池)。
//
// 库表(deploy/mysql-init/04-player-tables.sql):
//
//	pandora_player.players        玩家档案(PK player_id,uk nickname)
//	pandora_player.player_heroes  英雄解锁(uk player_id+hero_id)
//	pandora_player.mmr_history    MMR 变化历史 + 幂等键(uk player_id+idempotency_key)
//
// 幂等:ApplyMMRChange 在一个事务里 INSERT mmr_history;命中 1062 唯一键冲突 → 视为
// 已处理(already=true),读回该幂等键已记录的 new_mmr 返回,不重复改 players.mmr。
// players 表是结构化列(docs CLAUDE.md §5.9 不强制 proto 化),直接映射 proto 字段。
package data

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/luyuancpp/pandora/pkg/errcode"
	playerv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/player/v1"
)

// MMRChange 是一次 MMR 变更请求(biz 算好语义后传给 data 落库)。
type MMRChange struct {
	PlayerID       uint64
	IdempotencyKey string // 一般是 match_id 字符串
	Delta          int32
	Reason         string
	Floor          int  // MMR 下限(clamp 用)
	IncBattle      bool // 是否计一场对局(total_battles+1)
	IncWin         bool // 是否计一胜(total_wins+1)
}

// AttrAllocation 是一次加点请求里对某属性增加的点数(只增,Points>0)。
type AttrAllocation struct {
	Key    string
	Points int32
}

// AttrPoint 是某条属性的已分配点数。
type AttrPoint struct {
	Key    string
	Points int32
}

// PlayerRepo 是 player 数据层抽象。biz 层只依赖此接口,不依赖 *sql.DB。
type PlayerRepo interface {
	// EnsureProfile 确保玩家档案存在(INSERT IGNORE 默认档案),已存在则不动。
	EnsureProfile(ctx context.Context, playerID uint64, defaultNickname string, baseMMR int) error
	// GetProfile 读玩家档案。not found → (nil, false, nil)。
	GetProfile(ctx context.Context, playerID uint64) (*playerv1.PlayerProfile, bool, error)
	// UpdateNickname 改昵称。昵称被占用 → ErrPlayerNicknameTaken;玩家不存在 → ErrPlayerNotFound。
	UpdateNickname(ctx context.Context, playerID uint64, nickname string) error
	// ListHeroes 列出玩家已解锁英雄(配置表 hero_id,uint32)。
	ListHeroes(ctx context.Context, playerID uint64) ([]uint32, error)
	// UnlockHero 解锁英雄。已拥有 → (true, nil) 幂等命中。
	UnlockHero(ctx context.Context, playerID uint64, heroID uint32, source string) (already bool, err error)
	// GetMMR 读玩家当前 MMR。not found → (0, false, nil)。
	GetMMR(ctx context.Context, playerID uint64) (mmr int, found bool, err error)
	// ApplyMMRChange 幂等改 MMR + 战绩计数。命中幂等键 → (已记录 new_mmr, true, nil)。
	ApplyMMRChange(ctx context.Context, change MMRChange) (newMMR int, already bool, err error)

	// ── 出战养成 ──────────────────────────────────────────────────────────
	// IsHeroOwned 判断玩家是否已解锁该英雄。
	IsHeroOwned(ctx context.Context, playerID uint64, heroID uint32) (bool, error)
	// SetActiveHero 设定出战英雄(玩家须先 EnsureProfile;英雄拥有校验在 biz 层)。
	SetActiveHero(ctx context.Context, playerID uint64, heroID uint32) error
	// GetActiveHero 读出战英雄。未选定 / 未建档 → (0, nil)。
	GetActiveHero(ctx context.Context, playerID uint64) (uint32, error)
	// GrantAttributePoints 幂等授予可分配点。命中幂等键 → (当前 unspent, true, nil)。
	GrantAttributePoints(ctx context.Context, playerID uint64, points int32, idempotencyKey string) (unspent int, already bool, err error)
	// AllocateAttributePoints 分配点(事务:校验 unspent>=sum,扣减,累加 player_attributes)。
	// 点数不足 → ErrPlayerInsufficientPoints。
	AllocateAttributePoints(ctx context.Context, playerID uint64, allocs []AttrAllocation) (unspent int, err error)
	// ResetAttributes 洗点(已分配点全退回 unspent,清空 player_attributes)。
	ResetAttributes(ctx context.Context, playerID uint64) (unspent int, err error)
	// GetAttributes 读已分配属性点 + 未分配点。
	GetAttributes(ctx context.Context, playerID uint64) (attrs []AttrPoint, unspent int, err error)
}

// MySQLPlayerRepo 是基于 database/sql 的 PlayerRepo 实现。
type MySQLPlayerRepo struct {
	db *sql.DB
}

// NewMySQLPlayerRepo 构造。db 由 pkg/mysqlx.MustNewClient 提供(连 pandora_player 库)。
func NewMySQLPlayerRepo(db *sql.DB) *MySQLPlayerRepo {
	return &MySQLPlayerRepo{db: db}
}

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
UNIX_TIMESTAMP(created_at)*1000, UNIX_TIMESTAMP(last_seen_at)*1000, total_battles, total_wins
FROM players WHERE player_id = ? LIMIT 1`
	p := &playerv1.PlayerProfile{PlayerId: playerID}
	err := r.db.QueryRowContext(ctx, q, playerID).Scan(
		&p.Nickname, &p.Level, &p.Mmr, &p.Avatar,
		&p.CreatedAtMs, &p.LastSeenMs, &p.TotalBattles, &p.TotalWins)
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

func (r *MySQLPlayerRepo) ListHeroes(ctx context.Context, playerID uint64) ([]uint32, error) {
	const q = `SELECT hero_id FROM player_heroes WHERE player_id = ? ORDER BY hero_id`
	rows, err := r.db.QueryContext(ctx, q, playerID)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "query heroes player=%d: %v", playerID, err)
	}
	defer func() { _ = rows.Close() }()

	var heroes []uint32
	for rows.Next() {
		var h uint32
		if serr := rows.Scan(&h); serr != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan hero player=%d: %v", playerID, serr)
		}
		heroes = append(heroes, h)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate heroes player=%d: %v", playerID, rerr)
	}
	return heroes, nil
}

func (r *MySQLPlayerRepo) UnlockHero(ctx context.Context, playerID uint64, heroID uint32, source string) (bool, error) {
	const q = `INSERT INTO player_heroes (player_id, hero_id, source) VALUES (?, ?, ?)`
	_, err := r.db.ExecContext(ctx, q, playerID, heroID, source)
	if err != nil {
		if isDupErr(err) {
			// 已拥有 → 幂等命中
			return true, nil
		}
		return false, errcode.New(errcode.ErrInternal, "unlock hero player=%d hero=%d: %v", playerID, heroID, err)
	}
	return false, nil
}

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

// ── 出战养成 ──────────────────────────────────────────────────────────────────

func (r *MySQLPlayerRepo) IsHeroOwned(ctx context.Context, playerID uint64, heroID uint32) (bool, error) {
	const q = `SELECT 1 FROM player_heroes WHERE player_id = ? AND hero_id = ? LIMIT 1`
	var x int
	err := r.db.QueryRowContext(ctx, q, playerID, heroID).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "check hero owned player=%d hero=%d: %v", playerID, heroID, err)
	}
	return true, nil
}

func (r *MySQLPlayerRepo) SetActiveHero(ctx context.Context, playerID uint64, heroID uint32) error {
	const q = `UPDATE players SET active_hero_id = ? WHERE player_id = ?`
	if _, err := r.db.ExecContext(ctx, q, heroID, playerID); err != nil {
		return errcode.New(errcode.ErrInternal, "set active hero player=%d hero=%d: %v", playerID, heroID, err)
	}
	return nil
}

func (r *MySQLPlayerRepo) GetActiveHero(ctx context.Context, playerID uint64) (uint32, error) {
	const q = `SELECT active_hero_id FROM players WHERE player_id = ? LIMIT 1`
	var heroID uint32
	err := r.db.QueryRowContext(ctx, q, playerID).Scan(&heroID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "get active hero player=%d: %v", playerID, err)
	}
	return heroID, nil
}

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

// isDupErr 判断是否 MySQL 1062 唯一键冲突(go-sql-driver 错误串含 "Error 1062")。
func isDupErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1062")
}
