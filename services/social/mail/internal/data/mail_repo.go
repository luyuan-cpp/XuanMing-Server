// Package data 是 mail 服务的数据层(MySQL 邮件存储,2026-06-29)。
//
// 库表(deploy/mysql-init/12-mail-tables.sql,pandora_social 库):
//
//	sys_mail            系统邮件一份(PK mail_id snowflake,channel 内递增)
//	guild_mail          公会邮件一份(PK mail_id;idx guild_id)
//	player_mail         个人收件箱(PK mail_id;idx player_id+status,写扩散)
//	player_mail_cursor  系统/公会拉取游标(PK player_id)
//	player_mail_claim   附件领取幂等(PK player_id+mail_id)
//
// 系统/公会邮件 = channel + watermark 拉取(零写扩散);个人邮件 = 写扩散(离线可达)。
// 邮件正文+附件序列化为 MailContentStorageRecord proto bytes 存 payload blob(CLAUDE.md §5.8)。
package data

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// 个人邮件状态(与 proto MailStatus 数值一致)。
const (
	StatusUnread  = 1
	StatusRead    = 2
	StatusClaimed = 3
)

// MailRow 是一行邮件(任意 channel,data → biz 内部结构)。payload 为存储 blob。
type MailRow struct {
	MailID    uint64
	Status    int32 // 仅个人邮件有意义;系统/公会拉取后由 biz 推导
	Claimed   bool
	CreatedMs int64
	ExpireMs  int64 // 个人邮件
	StartMs   int64 // 系统/公会邮件
	EndMs     int64 // 系统/公会邮件
	Payload   []byte
}

// ExpiredPersonalRow 是 sweep 捞出的过期个人邮件(含收件人,biz 解 payload 决定归档或直删)。
type ExpiredPersonalRow struct {
	MailID    uint64
	PlayerID  uint64
	Status    int32
	ExpireMs  int64
	CreatedMs int64
	Payload   []byte
}

// MailRepo 是邮件数据层抽象。biz 只依赖此接口。
type MailRepo interface {
	GetCursor(ctx context.Context, playerID uint64) (lastSys, lastGuild uint64, err error)
	GetPlayerGuild(ctx context.Context, playerID uint64) (guildID uint64, ok bool, err error)

	// ListPersonal 倒序拉个人邮件;beforeID=0 取首页,>0 取 mail_id<beforeID;limit>0 限量。
	ListPersonal(ctx context.Context, playerID uint64, nowMs int64, beforeID uint64, limit int) ([]MailRow, error)
	ListSysSince(ctx context.Context, lastSys uint64, nowMs int64) ([]MailRow, error)
	ListGuildSince(ctx context.Context, guildID, lastGuild uint64, nowMs int64) ([]MailRow, error)
	AdvanceCursor(ctx context.Context, playerID, sysMax, guildMax uint64) error

	SetPersonalStatus(ctx context.Context, playerID, mailID uint64, status int32) error
	DeletePersonal(ctx context.Context, playerID, mailID uint64) error
	// GetClaimablePayload 取邮件正文用于领取,并按 channel 校验领取人有权访问:
	//   - 个人邮件:必须 player_id == 收件人
	//   - 公会邮件:必须 player_id 当前属于该公会
	//   - 系统邮件:任意玩家可领
	// 未生效(start_ms 未到)、已过期(end_ms 已过)或越权 → found=false。
	GetClaimablePayload(ctx context.Context, playerID, mailID uint64, nowMs int64) (payload []byte, found bool, err error)
	HasClaimed(ctx context.Context, playerID, mailID uint64) (bool, error)
	RecordClaim(ctx context.Context, playerID, mailID uint64) (firstTime bool, err error)

	// ── DS 三段式领取(bag phase 2,2026-07-22;行复用 player_mail_claim:
	// claimed=0+intent_payload=意图,claimed=1=终态;HasClaimed 只认终态)──

	// GetClaimState 返回领取状态:claimed=终态已领;intentOpen=意图行存在且未终态。
	GetClaimState(ctx context.Context, playerID, mailID uint64) (claimed, intentOpen bool, err error)
	// GetClaimIntent 读意图行 payload(仅 claimed=0 行;终态/无行 → found=false)。
	GetClaimIntent(ctx context.Context, playerID, mailID uint64) (payload []byte, found bool, err error)
	// CreateClaimIntent 建意图行(claimed=0;INSERT IGNORE):已有任何行 → created=false,
	// 调用方重读状态决策(并发/重放安全,不覆盖既有意图或终态)。
	CreateClaimIntent(ctx context.Context, playerID, mailID uint64, payload []byte) (created bool, err error)
	// MarkClaimed 意图行置终态(claimed=1;已终态幂等 no-op)。无行 → found=false。
	MarkClaimed(ctx context.Context, playerID, mailID uint64) (found bool, err error)

	InsertSysMail(ctx context.Context, mailID uint64, startMs, endMs int64, payload []byte) error
	InsertGuildMail(ctx context.Context, mailID, guildID uint64, startMs, endMs int64, payload []byte) error
	// InsertPersonalMail 写收件箱,事务内原子校验单玩家行数上限(§9 不变量 18):
	// 满时先驱逐最旧的已领(status=claimed)邮件,仍满返回 ErrMailBoxFull。
	InsertPersonalMail(ctx context.Context, mailID, playerID uint64, expireMs int64, payload []byte, maxInbox int) error

	// ── sweep 清理(全部幂等,多副本并发安全,单批 limit 有界)──────────────────────

	// ListExpiredPersonal 捞过期个人邮件(expire_ms ∈ (0, expireBeforeMs]),按过期时间升序。
	ListExpiredPersonal(ctx context.Context, expireBeforeMs int64, limit int) ([]ExpiredPersonalRow, error)
	// ArchiveAndDeletePersonal 同事务:archive 行移入 player_mail_archive(INSERT IGNORE 幂等),
	// deleteIDs(须含 archive 行的 mail_id)从 player_mail 删除。
	ArchiveAndDeletePersonal(ctx context.Context, archive []ExpiredPersonalRow, deleteIDs []uint64) error
	// DeleteSysMailEndedBefore / DeleteGuildMailEndedBefore 删失效系统/公会邮件(end_ms ∈ (0, endBeforeMs])。
	DeleteSysMailEndedBefore(ctx context.Context, endBeforeMs int64, limit int) (int64, error)
	DeleteGuildMailEndedBefore(ctx context.Context, endBeforeMs int64, limit int) (int64, error)
	// DeleteClaimsBefore 删 mail_id < maxMailID 的领取记录(雪花 ID 时间段单调,等价按创建时间截断)。
	DeleteClaimsBefore(ctx context.Context, maxMailID uint64, limit int) (int64, error)
	// PurgeArchiveBefore 删归档超保留期的行(归档表自身有界)。
	PurgeArchiveBefore(ctx context.Context, retentionDays, limit int) (int64, error)
}

// MySQLMailRepo 实现 MailRepo。
type MySQLMailRepo struct {
	db *sql.DB
}

// NewMySQLMailRepo 构造。
func NewMySQLMailRepo(db *sql.DB) *MySQLMailRepo {
	return &MySQLMailRepo{db: db}
}

func (r *MySQLMailRepo) GetCursor(ctx context.Context, playerID uint64) (uint64, uint64, error) {
	var s, g uint64
	err := r.db.QueryRowContext(ctx,
		`SELECT last_sys_mail_id, last_guild_mail_id FROM player_mail_cursor WHERE player_id = ?`,
		playerID).Scan(&s, &g)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, errcode.New(errcode.ErrInternal, "get cursor %d: %v", playerID, err)
	}
	return s, g, nil
}

func (r *MySQLMailRepo) GetPlayerGuild(ctx context.Context, playerID uint64) (uint64, bool, error) {
	var g uint64
	err := r.db.QueryRowContext(ctx,
		`SELECT guild_id FROM guild_members WHERE player_id = ?`, playerID).Scan(&g)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "get player guild %d: %v", playerID, err)
	}
	return g, true, nil
}

func (r *MySQLMailRepo) ListPersonal(ctx context.Context, playerID uint64, nowMs int64, beforeID uint64, limit int) ([]MailRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT mail_id, status, claimed, expire_ms,
		        CAST(UNIX_TIMESTAMP(created_at) * 1000 AS SIGNED), payload
		 FROM player_mail
		 WHERE player_id = ? AND (expire_ms = 0 OR expire_ms > ?)
		       AND (? = 0 OR mail_id < ?)
		 ORDER BY mail_id DESC
		 LIMIT ?`, playerID, nowMs, beforeID, beforeID, limit)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "list personal %d: %v", playerID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []MailRow
	for rows.Next() {
		var m MailRow
		var claimed int
		if err := rows.Scan(&m.MailID, &m.Status, &claimed, &m.ExpireMs, &m.CreatedMs, &m.Payload); err != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan personal: %v", err)
		}
		m.Claimed = claimed != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *MySQLMailRepo) ListSysSince(ctx context.Context, lastSys uint64, nowMs int64) ([]MailRow, error) {
	return r.listChannelSince(ctx,
		`SELECT mail_id, start_ms, end_ms, CAST(UNIX_TIMESTAMP(created_at)*1000 AS SIGNED), payload
		 FROM sys_mail
		 WHERE mail_id > ? AND (start_ms = 0 OR start_ms <= ?) AND (end_ms = 0 OR end_ms > ?)
		 ORDER BY mail_id`, lastSys, nowMs)
}

func (r *MySQLMailRepo) ListGuildSince(ctx context.Context, guildID, lastGuild uint64, nowMs int64) ([]MailRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT mail_id, start_ms, end_ms, CAST(UNIX_TIMESTAMP(created_at)*1000 AS SIGNED), payload
		 FROM guild_mail
		 WHERE guild_id = ? AND mail_id > ? AND (start_ms = 0 OR start_ms <= ?) AND (end_ms = 0 OR end_ms > ?)
		 ORDER BY mail_id`, guildID, lastGuild, nowMs, nowMs)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "list guild mail %d: %v", guildID, err)
	}
	return scanChannelRows(rows)
}

func (r *MySQLMailRepo) listChannelSince(ctx context.Context, q string, last uint64, nowMs int64) ([]MailRow, error) {
	rows, err := r.db.QueryContext(ctx, q, last, nowMs, nowMs)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "list channel mail: %v", err)
	}
	return scanChannelRows(rows)
}

func scanChannelRows(rows *sql.Rows) ([]MailRow, error) {
	defer func() { _ = rows.Close() }()
	var out []MailRow
	for rows.Next() {
		var m MailRow
		if err := rows.Scan(&m.MailID, &m.StartMs, &m.EndMs, &m.CreatedMs, &m.Payload); err != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan channel mail: %v", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *MySQLMailRepo) AdvanceCursor(ctx context.Context, playerID, sysMax, guildMax uint64) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO player_mail_cursor (player_id, last_sys_mail_id, last_guild_mail_id)
		 VALUES (?, ?, ?)
		 ON DUPLICATE KEY UPDATE
		   last_sys_mail_id = GREATEST(last_sys_mail_id, VALUES(last_sys_mail_id)),
		   last_guild_mail_id = GREATEST(last_guild_mail_id, VALUES(last_guild_mail_id))`,
		playerID, sysMax, guildMax)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "advance cursor %d: %v", playerID, err)
	}
	return nil
}

func (r *MySQLMailRepo) SetPersonalStatus(ctx context.Context, playerID, mailID uint64, status int32) error {
	// claimed 列随 status=claimed 同步置 1(只置不清),供客户端视图与清理/驱逐判定
	_, err := r.db.ExecContext(ctx,
		`UPDATE player_mail SET status = ?, claimed = IF(? = 3, 1, claimed) WHERE mail_id = ? AND player_id = ?`,
		status, status, mailID, playerID)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "set status %d: %v", mailID, err)
	}
	return nil
}

func (r *MySQLMailRepo) DeletePersonal(ctx context.Context, playerID, mailID uint64) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM player_mail WHERE mail_id = ? AND player_id = ?`, mailID, playerID)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "delete mail %d: %v", mailID, err)
	}
	return nil
}

// GetClaimablePayload 取邮件正文并按 channel 校验领取人权限 + 生效区间。
// 越权 / 未生效 / 已过期 / 不存在 → (nil, false, nil)。
func (r *MySQLMailRepo) GetClaimablePayload(ctx context.Context, playerID, mailID uint64, nowMs int64) ([]byte, bool, error) {
	// 1) 个人邮件:仅收件人本人,过期不可领
	var payload []byte
	err := r.db.QueryRowContext(ctx,
		`SELECT payload FROM player_mail
		 WHERE mail_id = ? AND player_id = ? AND (expire_ms = 0 OR expire_ms > ?)`,
		mailID, playerID, nowMs).Scan(&payload)
	if err == nil {
		return payload, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, errcode.New(errcode.ErrInternal, "get personal payload %d: %v", mailID, err)
	}

	// 2) 系统邮件:任意玩家可领,须已生效未过期
	err = r.db.QueryRowContext(ctx,
		`SELECT payload FROM sys_mail
		 WHERE mail_id = ? AND (start_ms = 0 OR start_ms <= ?) AND (end_ms = 0 OR end_ms > ?)`,
		mailID, nowMs, nowMs).Scan(&payload)
	if err == nil {
		return payload, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, errcode.New(errcode.ErrInternal, "get sys payload %d: %v", mailID, err)
	}

	// 3) 公会邮件:领取人须当前属于该邮件的公会,且已生效未过期
	err = r.db.QueryRowContext(ctx,
		`SELECT gm.payload FROM guild_mail gm
		 JOIN guild_members m ON m.guild_id = gm.guild_id
		 WHERE gm.mail_id = ? AND m.player_id = ?
		   AND (gm.start_ms = 0 OR gm.start_ms <= ?) AND (gm.end_ms = 0 OR gm.end_ms > ?)`,
		mailID, playerID, nowMs, nowMs).Scan(&payload)
	if err == nil {
		return payload, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, errcode.New(errcode.ErrInternal, "get guild payload %d: %v", mailID, err)
	}
	return nil, false, nil
}

func (r *MySQLMailRepo) HasClaimed(ctx context.Context, playerID, mailID uint64) (bool, error) {
	// 只认终态(claimed=1);DS 领取意图行(claimed=0)不算已领——它由 ClaimMail 的
	// 互斥检查(GetClaimState.intentOpen → 9607)单独处理。
	var x int
	err := r.db.QueryRowContext(ctx,
		`SELECT 1 FROM player_mail_claim WHERE player_id = ? AND mail_id = ? AND claimed = 1`, playerID, mailID).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "has claimed: %v", err)
	}
	return true, nil
}

// GetClaimState 读领取状态(终态 / 意图进行中)。
func (r *MySQLMailRepo) GetClaimState(ctx context.Context, playerID, mailID uint64) (bool, bool, error) {
	var claimedI8 int8
	err := r.db.QueryRowContext(ctx,
		`SELECT claimed FROM player_mail_claim WHERE player_id = ? AND mail_id = ?`, playerID, mailID).Scan(&claimedI8)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, errcode.New(errcode.ErrInternal, "get claim state: %v", err)
	}
	if claimedI8 != 0 {
		return true, false, nil
	}
	return false, true, nil
}

// GetClaimIntent 读意图行 payload(仅未终态行)。
func (r *MySQLMailRepo) GetClaimIntent(ctx context.Context, playerID, mailID uint64) ([]byte, bool, error) {
	var payload []byte
	err := r.db.QueryRowContext(ctx,
		`SELECT intent_payload FROM player_mail_claim WHERE player_id = ? AND mail_id = ? AND claimed = 0`,
		playerID, mailID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "get claim intent: %v", err)
	}
	return payload, true, nil
}

// CreateClaimIntent 建意图行(INSERT IGNORE:已有行不覆盖,created=false 由调用方重读)。
func (r *MySQLMailRepo) CreateClaimIntent(ctx context.Context, playerID, mailID uint64, payload []byte) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`INSERT IGNORE INTO player_mail_claim (player_id, mail_id, claimed, intent_payload) VALUES (?, ?, 0, ?)`,
		playerID, mailID, payload)
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "create claim intent: %v", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkClaimed 意图行置终态(幂等:已终态 no-op)。
func (r *MySQLMailRepo) MarkClaimed(ctx context.Context, playerID, mailID uint64) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE player_mail_claim SET claimed = 1 WHERE player_id = ? AND mail_id = ?`, playerID, mailID)
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "mark claimed: %v", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	// RowsAffected=0:行不存在,或已是 claimed=1(MySQL 不计未变更行)——补一次存在性判定。
	claimed, intentOpen, serr := r.GetClaimState(ctx, playerID, mailID)
	if serr != nil {
		return false, serr
	}
	return claimed || intentOpen, nil
}

func (r *MySQLMailRepo) RecordClaim(ctx context.Context, playerID, mailID uint64) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`INSERT IGNORE INTO player_mail_claim (player_id, mail_id) VALUES (?, ?)`, playerID, mailID)
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "record claim: %v", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *MySQLMailRepo) InsertSysMail(ctx context.Context, mailID uint64, startMs, endMs int64, payload []byte) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO sys_mail (mail_id, start_ms, end_ms, payload) VALUES (?, ?, ?, ?)`,
		mailID, startMs, endMs, payload)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "insert sys mail: %v", err)
	}
	return nil
}

func (r *MySQLMailRepo) InsertGuildMail(ctx context.Context, mailID, guildID uint64, startMs, endMs int64, payload []byte) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO guild_mail (mail_id, guild_id, start_ms, end_ms, payload) VALUES (?, ?, ?, ?, ?)`,
		mailID, guildID, startMs, endMs, payload)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "insert guild mail: %v", err)
	}
	return nil
}

// InsertPersonalMail 事务内原子校验收件箱上限后写入(§9 不变量 18,防 TOCTOU):
// COUNT(*) FOR UPDATE 锁住该玩家的 idx_player_status 索引范围,并发同玩家写入串行化;
// 满时驱逐最旧的已领邮件(附件已落袋,删除无损),仍满回滚返回 ErrMailBoxFull。
// 过期未清但未领的行仍占名额(不在写路径解 payload 判附件,留给 sweep 归档后自然释放);
// 调用方(battle_result 掉落出箱)靠补扫重试,旧邮件过期被清后发送自然成功。
func (r *MySQLMailRepo) InsertPersonalMail(ctx context.Context, mailID, playerID uint64, expireMs int64, payload []byte, maxInbox int) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "insert personal mail begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if maxInbox > 0 {
		var cnt int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM player_mail WHERE player_id = ? FOR UPDATE`, playerID).Scan(&cnt); err != nil {
			return errcode.New(errcode.ErrInternal, "insert personal mail count %d: %v", playerID, err)
		}
		if cnt >= maxInbox {
			res, err := tx.ExecContext(ctx,
				`DELETE FROM player_mail WHERE player_id = ? AND status = ? ORDER BY mail_id LIMIT ?`,
				playerID, StatusClaimed, cnt-maxInbox+1)
			if err != nil {
				return errcode.New(errcode.ErrInternal, "insert personal mail evict %d: %v", playerID, err)
			}
			evicted, _ := res.RowsAffected()
			if cnt-int(evicted) >= maxInbox {
				return errcode.New(errcode.ErrMailBoxFull, "player %d inbox full (%d/%d)", playerID, cnt, maxInbox)
			}
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO player_mail (mail_id, player_id, status, expire_ms, payload) VALUES (?, ?, 1, ?, ?)`,
		mailID, playerID, expireMs, payload); err != nil {
		return errcode.New(errcode.ErrInternal, "insert personal mail: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return errcode.New(errcode.ErrInternal, "insert personal mail commit: %v", err)
	}
	return nil
}

// ── sweep 清理实现(幂等删除,多副本并发安全:同行只会被一方删成功)──────────────────

func (r *MySQLMailRepo) ListExpiredPersonal(ctx context.Context, expireBeforeMs int64, limit int) ([]ExpiredPersonalRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT mail_id, player_id, status, expire_ms,
		        CAST(UNIX_TIMESTAMP(created_at) * 1000 AS SIGNED), payload
		 FROM player_mail
		 WHERE expire_ms > 0 AND expire_ms <= ?
		 ORDER BY expire_ms
		 LIMIT ?`, expireBeforeMs, limit)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "list expired personal: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ExpiredPersonalRow
	for rows.Next() {
		var m ExpiredPersonalRow
		if err := rows.Scan(&m.MailID, &m.PlayerID, &m.Status, &m.ExpireMs, &m.CreatedMs, &m.Payload); err != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan expired personal: %v", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *MySQLMailRepo) ArchiveAndDeletePersonal(ctx context.Context, archive []ExpiredPersonalRow, deleteIDs []uint64) error {
	if len(deleteIDs) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "archive personal begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if len(archive) > 0 {
		q := strings.Builder{}
		q.WriteString(`INSERT IGNORE INTO player_mail_archive (mail_id, player_id, status, expire_ms, created_ms, payload) VALUES `)
		args := make([]any, 0, len(archive)*6)
		for i, m := range archive {
			if i > 0 {
				q.WriteString(",")
			}
			q.WriteString("(?, ?, ?, ?, ?, ?)")
			args = append(args, m.MailID, m.PlayerID, m.Status, m.ExpireMs, m.CreatedMs, m.Payload)
		}
		if _, err := tx.ExecContext(ctx, q.String(), args...); err != nil {
			return errcode.New(errcode.ErrInternal, "archive personal insert: %v", err)
		}
	}

	q := `DELETE FROM player_mail WHERE mail_id IN (?` + strings.Repeat(",?", len(deleteIDs)-1) + `)`
	args := make([]any, len(deleteIDs))
	for i, id := range deleteIDs {
		args[i] = id
	}
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return errcode.New(errcode.ErrInternal, "archive personal delete: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return errcode.New(errcode.ErrInternal, "archive personal commit: %v", err)
	}
	return nil
}

func (r *MySQLMailRepo) DeleteSysMailEndedBefore(ctx context.Context, endBeforeMs int64, limit int) (int64, error) {
	return r.execAffected(ctx, "delete sys mail",
		`DELETE FROM sys_mail WHERE end_ms > 0 AND end_ms <= ? LIMIT ?`, endBeforeMs, limit)
}

func (r *MySQLMailRepo) DeleteGuildMailEndedBefore(ctx context.Context, endBeforeMs int64, limit int) (int64, error) {
	return r.execAffected(ctx, "delete guild mail",
		`DELETE FROM guild_mail WHERE end_ms > 0 AND end_ms <= ? LIMIT ?`, endBeforeMs, limit)
}

func (r *MySQLMailRepo) DeleteClaimsBefore(ctx context.Context, maxMailID uint64, limit int) (int64, error) {
	return r.execAffected(ctx, "delete claims",
		`DELETE FROM player_mail_claim WHERE mail_id < ? LIMIT ?`, maxMailID, limit)
}

func (r *MySQLMailRepo) PurgeArchiveBefore(ctx context.Context, retentionDays, limit int) (int64, error) {
	return r.execAffected(ctx, "purge archive",
		`DELETE FROM player_mail_archive WHERE archived_at < DATE_SUB(NOW(), INTERVAL ? DAY) LIMIT ?`, retentionDays, limit)
}

func (r *MySQLMailRepo) execAffected(ctx context.Context, op, q string, args ...any) (int64, error) {
	res, err := r.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "%s: %v", op, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
