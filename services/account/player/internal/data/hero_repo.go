package data

import (
	"context"
	"database/sql"
	"errors"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

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
