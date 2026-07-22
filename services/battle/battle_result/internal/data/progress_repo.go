// progress_repo.go — 战斗中实时进度水位去重 + 进度发放事务出箱(实时成长,2026-07-20)。
//
// 库表(deploy/mysql-init/05-battle-outbox.sql / tools/migrate pandora_battle 000005+000006):
//
//	pandora_battle.battle_progress_stream 每场进度水位(PK match_id;last_applied_seq 单调推进,
//	                                      settled_at_ms>0 = 对局已结算,后续进度一律拒 = 僵尸 DS fencing)
//	pandora_battle.battle_progress_outbox 进度发放事务出箱(uk match+seq+player+kind;
//	                                      exp → player.AddExperience / item → inventory.GrantInstances)
//
// 幂等 / 原子:水位推进(乐观 CAS:WHERE last_applied_seq=expected AND settled_at_ms=0)、
// 单场/单玩家累计上限判定与出箱行写入同一 MySQL 事务;CAS 失败(并发写者 / 已结算)或
// 超限整批回滚,DS 按错误语义重试、丢批或停流。
package data

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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
	// Stopped 实时通道已停流(未知事实/违纪混版持久标记,后续进度一律拒;
	// 审计 P1:无持久标记时,违纪 DS 停流后再发只含已知事实的批会被重新接受)。
	Stopped bool
	// Existed 水位行是否存在(= 本场已走实时通道;killswitch 中途关闭不影响已开流对局)。
	Existed bool
}

// ProgressPlayerTotals 是本场单个玩家的累计入账快照(单玩家上限封顶依据,审计 P1:
// 只按场累计时失陷 DS 可把全场额度灌给一人)。
type ProgressPlayerTotals struct {
	TotalExp   uint64
	TotalItems uint32
	TotalKills uint32
}

// ProgressPlayerDelta 是本批某玩家的新增累计(与水位 CAS 同事务 upsert 到 battle_progress_player)。
type ProgressPlayerDelta struct {
	PlayerID uint64
	Exp      uint64
	Items    uint32
	Kills    uint32
}

// ProgressCaps 单场 / 单场单玩家累计上限(biz 从配置注入;各项必须 >0,由
// conf *OrDefault 取值保证)。判定在 ApplyProgress 事务内的一致快照上进行:
// 事务外"先读累计再判上限"与水位 CAS 分属不同快照,重试请求可能读到旧水位 +
// 新累计,把同批 delta 重复计入后返回永久 ErrInvalidArg(审计 P1:DS 据契约
// 丢批并释放拾取认领,而首请求出箱已提交 → 重新拾取可重复发放)。
type ProgressCaps struct {
	MatchExp    uint64
	MatchItems  uint32
	PlayerExp   uint64
	PlayerItems uint32
	PlayerKills uint32
}

// GetProgressWatermark 读一场对局的进度水位。行不存在 → 零值(Existed=false)。
func (r *MySQLBattleRepo) GetProgressWatermark(ctx context.Context, matchID uint64) (ProgressWatermark, error) {
	var (
		wm        ProgressWatermark
		settledMs int64
		stoppedMs int64
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT last_applied_seq, total_exp, total_items, settled_at_ms, stopped_at_ms FROM battle_progress_stream WHERE match_id = ? LIMIT 1`,
		matchID,
	).Scan(&wm.LastAppliedSeq, &wm.TotalExp, &wm.TotalItems, &settledMs, &stoppedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return ProgressWatermark{}, nil
	}
	if err != nil {
		return ProgressWatermark{}, errcode.New(errcode.ErrUnavailable, "query progress watermark match=%d: %v", matchID, err)
	}
	wm.Settled = settledMs > 0
	wm.Stopped = stoppedMs > 0
	wm.Existed = true
	return wm, nil
}

// ClaimProgressLegacy 实现 BattleRepo.ClaimProgressLegacy:行不存在才创建停流标记
// (固化"本场 legacy 结算模式");行已存在时零修改(INSERT IGNORE 撞 PK 即输掉认领,
// 审计 R4 #11:不得用 upsert 把开启副本并发刚开的流停掉)。
func (r *MySQLBattleRepo) ClaimProgressLegacy(ctx context.Context, matchID uint64) (bool, error) {
	nowMs := time.Now().UnixMilli()
	res, err := r.db.ExecContext(ctx, `
INSERT IGNORE INTO battle_progress_stream (match_id, last_applied_seq, total_exp, total_items, final_seq, settled_at_ms, stopped_at_ms, updated_at_ms)
VALUES (?, 0, 0, 0, 0, 0, ?, ?)`,
		matchID, nowMs, nowMs)
	if err != nil {
		return false, errcode.New(errcode.ErrUnavailable, "claim progress legacy match=%d: %v", matchID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, errcode.New(errcode.ErrUnavailable, "claim progress legacy rows match=%d: %v", matchID, err)
	}
	return n == 1, nil
}

// MarkProgressStopped 持久化停流标记(幂等:只记录首次停流时间)。行不存在时创建
// (首批就含未知事实的场景也必须留标记,否则后续已知批会开流)。
// ⚠️ upsert 语义,仅供"流内确定停流"(未知事实)使用;通道关闭固化走 ClaimProgressLegacy。
func (r *MySQLBattleRepo) MarkProgressStopped(ctx context.Context, matchID uint64) error {
	nowMs := time.Now().UnixMilli()
	if _, err := r.db.ExecContext(ctx, `
INSERT INTO battle_progress_stream (match_id, last_applied_seq, total_exp, total_items, final_seq, settled_at_ms, stopped_at_ms, updated_at_ms)
VALUES (?, 0, 0, 0, 0, 0, ?, ?)
ON DUPLICATE KEY UPDATE stopped_at_ms = IF(stopped_at_ms = 0, VALUES(stopped_at_ms), stopped_at_ms),
updated_at_ms = VALUES(updated_at_ms)`,
		matchID, nowMs, nowMs); err != nil {
		return errcode.New(errcode.ErrUnavailable, "mark progress stopped match=%d: %v", matchID, err)
	}
	return nil
}

// ApplyProgress 原子推进水位、判定累计上限并写进度出箱(同一事务)。
//
//	expectedSeq 是调用方读到的水位(乐观 CAS 期望值);newSeq 是本批批末 seq(> expectedSeq)。
//	addExp / addItems 是本批新入账的经验 / 掉落件数,与水位同一 CAS 行累计。
//	caps 是单场 / 单玩家累计上限:入账后在**本事务一致快照**上判定(水位行已被本事务
//	写锁定,并发批次被 CAS 串行化),超限返回 ErrInvalidArg 并整体回滚(零副作用)。
//	上限判定不得放在事务外(§16.1 TOCTOU;混合快照误判会把可重试竞争放大成永久拒,审计 P1)。
//	CAS 失败(并发写者抢先 / 对局已结算)→ ErrUnavailable(瞬时,DS 单飞行批下几乎不会发生,
//	重试后按新水位去重收敛);行不存在时首批 INSERT,撞 PK 同样按 ErrUnavailable 重试收敛。
func (r *MySQLBattleRepo) ApplyProgress(ctx context.Context, matchID, expectedSeq, newSeq uint64, addExp uint64, addItems uint32, playerDeltas []ProgressPlayerDelta, rows []ProgressOutboxRecord, caps ProgressCaps) error {
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
		// 已结算 / 已停流对局的行永远存在(SaveResult 落终局标记 / MarkProgressStopped
		// 落停流标记),INSERT 撞 PK 即被拒 → 调用方重读水位看到 Settled/Stopped,
		// 天然 fail-closed;非首批 UPDATE 由 settled_at_ms=0 AND stopped_at_ms=0 条件
		// fencing(审计 P1:停流与正常批的 CAS 竞态——正常批读到停流前旧快照后,
		// 不得再推进水位写出箱)。
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
WHERE match_id = ? AND last_applied_seq = ? AND settled_at_ms = 0 AND stopped_at_ms = 0`,
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

	// 单场累计上限判定:水位行已被本事务写锁定,读回的是入账后的权威累计(一致快照)。
	// 超限 → ErrInvalidArg + 整体回滚(零副作用);永久拒只可能来自真实超限的新批,
	// 重放批在水位 CAS 处就以 ErrUnavailable 收敛,不会走到这里。
	var (
		curExp   uint64
		curItems uint32
	)
	if qerr := tx.QueryRowContext(ctx,
		`SELECT total_exp, total_items FROM battle_progress_stream WHERE match_id = ?`,
		matchID).Scan(&curExp, &curItems); qerr != nil {
		return errcode.New(errcode.ErrUnavailable, "read progress totals match=%d: %v", matchID, qerr)
	}
	if curExp > caps.MatchExp {
		return errcode.New(errcode.ErrInvalidArg,
			"match %d cumulative exp %d exceeds per-match cap %d (batch +%d)", matchID, curExp, caps.MatchExp, addExp)
	}
	if curItems > caps.MatchItems {
		return errcode.New(errcode.ErrInvalidArg,
			"match %d cumulative items %d exceeds per-match cap %d (batch +%d)", matchID, curItems, caps.MatchItems, addItems)
	}

	// 单玩家累计与水位同事务推进(CAS 保护下 upsert 累加无竞态,单玩家上限判定依据)。
	const upsertPlayer = `INSERT INTO battle_progress_player
(match_id, player_id, total_exp, total_items, total_kills, updated_at_ms)
VALUES (?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE total_exp = total_exp + VALUES(total_exp),
total_items = total_items + VALUES(total_items),
total_kills = total_kills + VALUES(total_kills),
updated_at_ms = VALUES(updated_at_ms)`
	for _, d := range playerDeltas {
		if _, perr := tx.ExecContext(ctx, upsertPlayer,
			matchID, d.PlayerID, d.Exp, d.Items, d.Kills, nowMs); perr != nil {
			return errcode.New(errcode.ErrUnavailable, "upsert progress player totals match=%d player=%d: %v",
				matchID, d.PlayerID, perr)
		}
	}

	// 单玩家累计上限判定(同一事务一致快照,理由同上)。
	if len(playerDeltas) > 0 {
		query := `SELECT player_id, total_exp, total_items, total_kills FROM battle_progress_player
WHERE match_id = ? AND player_id IN (?` + strings.Repeat(",?", len(playerDeltas)-1) + `)`
		args := make([]any, 0, len(playerDeltas)+1)
		args = append(args, matchID)
		for _, d := range playerDeltas {
			args = append(args, d.PlayerID)
		}
		prows, perr := tx.QueryContext(ctx, query, args...)
		if perr != nil {
			return errcode.New(errcode.ErrUnavailable, "query progress player totals match=%d: %v", matchID, perr)
		}
		var capErr error
		for prows.Next() {
			var (
				pid uint64
				t   ProgressPlayerTotals
			)
			if serr := prows.Scan(&pid, &t.TotalExp, &t.TotalItems, &t.TotalKills); serr != nil {
				_ = prows.Close()
				return errcode.New(errcode.ErrUnavailable, "scan progress player totals match=%d: %v", matchID, serr)
			}
			switch {
			case t.TotalExp > caps.PlayerExp:
				capErr = errcode.New(errcode.ErrInvalidArg,
					"match %d player %d cumulative exp %d exceeds per-player cap %d", matchID, pid, t.TotalExp, caps.PlayerExp)
			case t.TotalItems > caps.PlayerItems:
				capErr = errcode.New(errcode.ErrInvalidArg,
					"match %d player %d cumulative items %d exceeds per-player cap %d", matchID, pid, t.TotalItems, caps.PlayerItems)
			case t.TotalKills > caps.PlayerKills:
				capErr = errcode.New(errcode.ErrInvalidArg,
					"match %d player %d cumulative kills %d exceeds per-player cap %d", matchID, pid, t.TotalKills, caps.PlayerKills)
			}
			if capErr != nil {
				break
			}
		}
		if cerr := prows.Close(); cerr != nil && capErr == nil {
			return errcode.New(errcode.ErrUnavailable, "close progress player totals match=%d: %v", matchID, cerr)
		}
		if capErr != nil {
			return capErr
		}
		if rerr := prows.Err(); rerr != nil {
			return errcode.New(errcode.ErrUnavailable, "iterate progress player totals match=%d: %v", matchID, rerr)
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

// ValidateProgressSchema 启动时探测实时进度三表(stream/outbox/player,含累计上限 /
// 退避列;000005 + 000006 两个迁移的产物),缺失即失败:
// settleProgressStreamTx 在**每次结算**都会无条件访问水位表,不能等首个结算才炸(§16.4)。
func (r *MySQLBattleRepo) ValidateProgressSchema(ctx context.Context) error {
	checks := []string{
		`SELECT match_id, last_applied_seq, total_exp, total_items, final_seq, settled_at_ms, stopped_at_ms, updated_at_ms FROM battle_progress_stream LIMIT 0`,
		`SELECT id, match_id, seq, player_id, kind, exp_delta, item_config_ids, next_attempt_at_ms, attempt_count, created_at_ms FROM battle_progress_outbox LIMIT 0`,
		`SELECT match_id, player_id, total_exp, total_items, total_kills, updated_at_ms FROM battle_progress_player LIMIT 0`,
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
	// stopped_at_ms 列级契约(审计 P2:只探测列存在时,错类型/可空/坏默认值的手工漂移
	// 列也会通过——停流 fencing 依赖 "缺省 0 = 未停流" 的语义)。
	var (
		dataType   string
		isNullable string
		colDefault sql.NullString
	)
	if err := r.db.QueryRowContext(ctx,
		`SELECT DATA_TYPE, IS_NULLABLE, COLUMN_DEFAULT FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'battle_progress_stream' AND COLUMN_NAME = 'stopped_at_ms'`,
	).Scan(&dataType, &isNullable, &colDefault); err != nil {
		return errcode.New(errcode.ErrInternal, "probe stopped_at_ms column contract: %v", err)
	}
	if dataType != "bigint" || isNullable != "NO" || !colDefault.Valid || colDefault.String != "0" {
		return errcode.New(errcode.ErrInternal,
			"battle_progress_stream.stopped_at_ms contract violated (type=%s nullable=%s default=%v, want bigint/NO/0): stop fencing semantics broken",
			dataType, isNullable, colDefault.String)
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
