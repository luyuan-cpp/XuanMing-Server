package data

import (
	"context"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// ── 出战装备预设 / 天赋树 ──────────────────────────────────────────────────────

// SetEquipment 全量替换出战装备预设(事务:删旧 + 按 slot 插新;uk_player_slot 保证 slot 唯一)。
func (r *MySQLPlayerRepo) SetEquipment(ctx context.Context, playerID uint64, slots []EquipmentSlot) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, derr := tx.ExecContext(ctx, `DELETE FROM player_equipment WHERE player_id = ?`, playerID); derr != nil {
		return errcode.New(errcode.ErrInternal, "clear equipment player=%d: %v", playerID, derr)
	}
	const ins = `INSERT INTO player_equipment (player_id, slot, item_config_id) VALUES (?, ?, ?)`
	for _, s := range slots {
		if _, ierr := tx.ExecContext(ctx, ins, playerID, s.Slot, s.ItemConfigID); ierr != nil {
			if isDupErr(ierr) {
				return errcode.New(errcode.ErrInvalidArg, "duplicate equipment slot player=%d slot=%d", playerID, s.Slot)
			}
			return errcode.New(errcode.ErrInternal, "insert equipment player=%d slot=%d: %v", playerID, s.Slot, ierr)
		}
	}
	if cerr := tx.Commit(); cerr != nil {
		return errcode.New(errcode.ErrInternal, "commit equipment player=%d: %v", playerID, cerr)
	}
	return nil
}

func (r *MySQLPlayerRepo) GetEquipment(ctx context.Context, playerID uint64) ([]EquipmentSlot, error) {
	const q = `SELECT slot, item_config_id FROM player_equipment WHERE player_id = ? ORDER BY slot`
	rows, err := r.db.QueryContext(ctx, q, playerID)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "query equipment player=%d: %v", playerID, err)
	}
	defer func() { _ = rows.Close() }()

	var slots []EquipmentSlot
	for rows.Next() {
		var s EquipmentSlot
		if serr := rows.Scan(&s.Slot, &s.ItemConfigID); serr != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan equipment player=%d: %v", playerID, serr)
		}
		slots = append(slots, s)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate equipment player=%d: %v", playerID, rerr)
	}
	return slots, nil
}
