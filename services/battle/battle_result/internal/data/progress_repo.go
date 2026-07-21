// progress_repo.go — 战斗中实时进度水位去重 + 进度发放事务出箱(实时成长,2026-07-20)。
//
// 库表(deploy/mysql-init/05-battle-outbox.sql / tools/migrate pandora_battle 000005):
//
//	pandora_battle.battle_progress_stream 每场进度水位(PK match_id;last_applied_seq 单调推进,
//	                                      settled_at_ms>0 = 对局已结算,后续进度一律拒 = 僵尸 DS fencing)
//	pandora_battle.battle_progress_outbox 进度发放事务出箱(uk match+seq+player+kind;
//	                                      exp → player.AddExperience / item → inventory.GrantInstances)
//
// 幂等 / 原子:水位推进(乐观 CAS:WHERE last_applied_seq=expected AND settled_at_ms=0)与
// 出箱行写入同一 MySQL 事务;CAS 失败(并发写者 / 已结算)整批回滚,DS 按错误语义重试或停流。
package data

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// ProgressGrantKind 是进度出箱行的发放类型。
type ProgressGrantKind uint8

const (
	// ProgressGrantExp 经验入账(player.AddExperience)。
	ProgressGrantExp ProgressGrantKind = 1
	// ProgressGrantItem 掉落发放(inventory.GrantInstances,每 ID 一件未鉴定实例)。
	ProgressGrantItem ProgressGrantKind = 2
)

// ProgressOutboxRecord 是一条待发放的进度出箱记录。
// 幂等键 = progress:{match_id}:{seq}:{player_id}:{kind 名}。
// Seq:exp 行 = 批末事件 seq(批内按玩家聚合);item 行 = 该拾取事实自身的 seq
// (每事实一行,天然不超 CSV 列宽,合法掉落永不截断 —— 审计 P1)。
type ProgressOutboxRecord struct {
	ID            int64
	MatchID       uint64
	Seq           uint64
	PlayerID      uint64
	Kind          ProgressGrantKind
	ExpDelta      uint64   // Kind=Exp 时有效
	ItemConfigIDs []uint32 // Kind=Item 时有效(CSV 存储,复用 drop 出箱编码)
}

// ProgressWatermark 是一场对局的进度水位快照(单场模式 / 去重 / 累计上限的权威依据)。
type ProgressWatermark struct {
	// LastAppliedSeq 已应用批末 seq(0 = 尚未入账任何批)。
	LastAppliedSeq uint64
	// TotalExp 本场已累计入账经验(事实换算后,累计上限依据,审计 P1)。
	TotalExp uint64
	// TotalItems 本场已累计入账掉落件数。
	TotalItems uint32
	// Settled 对局已结算(终局标记,迟到进度一律拒)。
	Settled bool
	// Existed 水位行是否存在(= 本场已走实时通道;killswitch 中途关闭不影响已开流对局)。
	Existed bool
}

// GetProgressWatermark 读一场对局的进度水位。行不存在 → 零值(Existed=false)。
func (r *MySQLBattleRepo) GetProgressWatermark(ctx context.Context, matchID uint64) (ProgressWatermark, error) {
	var (
		wm        ProgressWatermark
		settledMs int64
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT last_applied_seq, total_exp, total_items, settled_at_ms FROM battle_progress_stream WHERE match_id = ? LIMIT 1`,
		matchID,
	).Scan(&wm.LastAppliedSeq, &wm.TotalExp, &wm.TotalItems, &settledMs)
	if errors.Is(err, sql.ErrNoRows) {
		return ProgressWatermark{}, nil
	}
	if err != nil {
		return ProgressWatermark{}, errcode.New(errcode.ErrUnavailable, "query progress watermark match=%d: %v", matchID, err)
	}
	wm.Settled = settledMs > 0
	wm.Existed = true
	return wm, nil
}

// ApplyProgress 原子推进水位并写进度出箱(同一事务)。
//
//	expectedSeq 是调用方读到的水位(乐观 CAS 期望值);newSeq 是本批批末 seq(> expectedSeq)。
//	addExp / addItems 是本批新入账的经验 / 掉落件数,与水位同一 CAS 行累计
//	(biz 在 CAS 保护下先读后判上限,写入无竞态,§16.1)。
//	CAS 失败(并发写者抢先 / 对局已结算)→ ErrUnavailable(瞬时,DS 单飞行批下几乎不会发生,
//	重试后按新水位去重收敛);行不存在时首批 INSERT,撞 PK 同样按 ErrUnavailable 重试收敛。
func (r *MySQLBattleRepo) ApplyProgress(ctx context.Context, matchID, expectedSeq, newSeq uint64, addExp uint64, addItems uint32, rows []ProgressOutboxRecord) error {
	if matchID == 0 || newSeq <= expectedSeq {
		return errcode.New(errcode.ErrInvalidArg, "apply progress requires match/seq advance")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return errcode.New(errcode.ErrUnavailable, "begin progress tx match=%d: %v", matchID, err)
	}
	defer func() { _ = tx.Rollback() }()

	nowMs := time.Now().UnixMilli()
	if expectedSeq == 0 {
		// 首批:INSERT 创建水位行(已存在 → 并发写者已推进 → 重试收敛)。
		// 已结算对局的行永远存在(SaveResult 落终局标记),INSERT 撞 PK 即被拒,天然 fail-closed。
		if _, ierr := tx.ExecContext(ctx,
			`INSERT INTO battle_progress_stream (match_id, last_applied_seq, total_exp, total_items, final_seq, settled_at_ms, updated_at_ms)
VALUES (?, ?, ?, ?, 0, 0, ?)`, matchID, newSeq, addExp, addItems, nowMs); ierr != nil {
			if isDupErr(ierr) {
				return errcode.New(errcode.ErrUnavailable, "progress watermark contended match=%d", matchID)
			}
			return errcode.New(errcode.ErrUnavailable, "insert progress watermark match=%d: %v", matchID, ierr)
		}
	} else {
		res, uerr := tx.ExecContext(ctx,
			`UPDATE battle_progress_stream SET last_applied_seq = ?, total_exp = total_exp + ?, total_items = total_items + ?, updated_at_ms = ?
WHERE match_id = ? AND last_applied_seq = ? AND settled_at_ms = 0`,
			newSeq, addExp, addItems, nowMs, matchID, expectedSeq)
		if uerr != nil {
			return errcode.New(errcode.ErrUnavailable, "advance progress watermark match=%d: %v", matchID, uerr)
		}
		if affected, _ := res.RowsAffected(); affected == 0 {
			// 期望水位不匹配或已结算:让调用方重读水位。已结算场景重读后会拿到明确的
			// ErrInvalidState(biz 判 settled),不会无限重试。
			return errcode.New(errcode.ErrUnavailable, "progress watermark moved or settled match=%d", matchID)
		}
	}

	const insRow = `INSERT INTO battle_progress_outbox
(match_id, seq, player_id, kind, exp_delta, item_config_ids, created_at_ms)
VALUES (?, ?, ?, ?, ?, ?, ?)`
	for _, row := range rows {
		if _, rerr := tx.ExecContext(ctx, insRow,
			matchID, row.Seq, row.PlayerID, uint8(row.Kind),
			row.ExpDelta, encodeConfigIDs(row.ItemConfigIDs), nowMs,
		); rerr != nil {
			return errcode.New(errcode.ErrUnavailable, "insert progress outbox match=%d player=%d: %v",
				matchID, row.PlayerID, rerr)
		}
	}

	if cerr := tx.Commit(); cerr != nil {
		return errcode.New(errcode.ErrUnavailable, "commit progress match=%d: %v", matchID, cerr)
	}
	return nil
}

// FetchProgressOutbox 按 id 升序取最多 limit 条**已到重试时点**的待发放进度出箱记录。
// next_attempt_at_ms 过滤 + DeferProgressOutbox 退避,保证个别永久失败行(坏数据 /
// granter 未配)不会长期占满首批饿死后续正常行(审计 P1 队首阻塞)。
func (r *MySQLBattleRepo) FetchProgressOutbox(ctx context.Context, limit int) ([]ProgressOutboxRecord, error) {
	if limit <= 0 {
		limit = 128
	}
	const q = `SELECT id, match_id, seq, player_id, kind, exp_delta, item_config_ids
FROM battle_progress_outbox WHERE next_attempt_at_ms <= ? ORDER BY id ASC LIMIT ?`
	rows, err := r.db.QueryContext(ctx, q, time.Now().UnixMilli(), limit)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "query progress outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]ProgressOutboxRecord, 0, limit)
	for rows.Next() {
		var (
			rec  ProgressOutboxRecord
			kind uint8
			csv  string
		)
		if serr := rows.Scan(&rec.ID, &rec.MatchID, &rec.Seq, &rec.PlayerID, &kind, &rec.ExpDelta, &csv); serr != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan progress outbox: %v", serr)
		}
		rec.Kind = ProgressGrantKind(kind)
		rec.ItemConfigIDs = decodeConfigIDs(csv)
		out = append(out, rec)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate progress outbox: %v", rerr)
	}
	return out, nil
}

// DeleteProgressOutbox 删除已成功发放的进度出箱行。
func (r *MySQLBattleRepo) DeleteProgressOutbox(ctx context.Context, id int64) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM battle_progress_outbox WHERE id = ?`, id); err != nil {
		return errcode.New(errcode.ErrInternal, "delete progress outbox id=%d: %v", id, err)
	}
	return nil
}

// DeferProgressOutbox 发放失败后把出箱行推迟到指数退避后的下一次尝试
// (2s·2^n,封顶 5min;行永不丢弃 —— 上限封顶后持续告警由人工介入,at-least-once 不变)。
func (r *MySQLBattleRepo) DeferProgressOutbox(ctx context.Context, id int64) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE battle_progress_outbox
SET attempt_count = attempt_count + 1,
    next_attempt_at_ms = ? + LEAST(2000 * POW(2, LEAST(attempt_count, 7)), 300000)
WHERE id = ?`, time.Now().UnixMilli(), id); err != nil {
		return errcode.New(errcode.ErrInternal, "defer progress outbox id=%d: %v", id, err)
	}
	return nil
}

// ValidateProgressSchema 启动时探测实时进度两表(含累计上限 / 退避列),缺失即失败:
// settleProgressStreamTx 在**每次结算**都会无条件访问水位表,不能等首个结算才炸(§16.4)。
func (r *MySQLBattleRepo) ValidateProgressSchema(ctx context.Context) error {
	checks := []string{
		`SELECT match_id, last_applied_seq, total_exp, total_items, final_seq, settled_at_ms, updated_at_ms FROM battle_progress_stream LIMIT 0`,
		`SELECT id, match_id, seq, player_id, kind, exp_delta, item_config_ids, next_attempt_at_ms, attempt_count, created_at_ms FROM battle_progress_outbox LIMIT 0`,
	}
	for _, query := range checks {
		rows, err := r.db.QueryContext(ctx, query)
		if err != nil {
			return errcode.New(errcode.ErrInternal, "battle progress schema invalid: %v", err)
		}
		if err := rows.Close(); err != nil {
			return errcode.New(errcode.ErrInternal, "close battle progress schema probe: %v", err)
		}
	}
	return nil
}

// ProgressSettleInfo 是 SaveResult 事务内对实时进度通道的结算收口结果。
type ProgressSettleInfo struct {
	// StreamExisted 结算时水位行是否已存在(= 本场走过实时通道)。
	StreamExisted bool
	// LastAppliedSeq 结算时的已应用水位(与 DS 上报 final_progress_seq 对账)。
	LastAppliedSeq uint64
	// DropsSuppressed true = 掉落发放权已归实时通道,结算路径的 dropped_item_config_ids
	// 只作对账不再发放(单一权威路径,realtime-progression.md §5)。
	DropsSuppressed bool
}

// settleProgressStreamTx 在结算事务内收口实时进度通道:
//  1. 锁定(或创建)水位行并打终局标记(settled_at_ms>0)→ 之后任何 ReportProgress 一律拒
//     (僵尸 / 分区恢复 DS fencing;ABANDONED 同样收口);
//  2. 返回水位信息,SaveResult 据 last_applied_seq>0 决定是否抑制结算路径掉落发放。
//
// 判定依据是服务端自己的水位表,不信 DS 声明:恶意 DS 两头上报也只有一条路径发放。
// 幂等:已打过终局标记的行原样返回不再改写(重复结算 / battles 幂等重放分支也会调本函数,
// 首次结算的 settled_at_ms / final_seq 是权威审计值,不被重放覆盖)。
func settleProgressStreamTx(ctx context.Context, tx *sql.Tx, matchID uint64, finalSeq uint64, nowMs int64) (ProgressSettleInfo, error) {
	var (
		lastSeq   uint64
		settledMs int64
	)
	err := tx.QueryRowContext(ctx,
		`SELECT last_applied_seq, settled_at_ms FROM battle_progress_stream WHERE match_id = ? FOR UPDATE`,
		matchID,
	).Scan(&lastSeq, &settledMs)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 本场未走实时通道:插入终局标记行,封死迟到进度(水位 0,掉落走结算路径)。
		if _, ierr := tx.ExecContext(ctx,
			`INSERT INTO battle_progress_stream (match_id, last_applied_seq, final_seq, settled_at_ms, updated_at_ms)
VALUES (?, 0, ?, ?, ?)`, matchID, finalSeq, nowMs, nowMs); ierr != nil {
			return ProgressSettleInfo{}, ierr
		}
		return ProgressSettleInfo{}, nil
	case err != nil:
		return ProgressSettleInfo{}, err
	}
	info := ProgressSettleInfo{
		StreamExisted:   true,
		LastAppliedSeq:  lastSeq,
		DropsSuppressed: lastSeq > 0,
	}
	if settledMs > 0 {
		return info, nil // 已收口(幂等重放),不改写首次结算标记
	}
	if _, uerr := tx.ExecContext(ctx,
		`UPDATE battle_progress_stream SET settled_at_ms = ?, final_seq = ?, updated_at_ms = ? WHERE match_id = ?`,
		nowMs, finalSeq, nowMs, matchID); uerr != nil {
		return ProgressSettleInfo{}, uerr
	}
	return info, nil
}
