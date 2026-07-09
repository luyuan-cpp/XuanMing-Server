// Package data 是 guild 服务的数据层(MySQL 公会 / 群成员关系,2026-06-27)。
//
// 库表(deploy/mysql-init/11-guild-tables.sql,pandora_social 库):
//
//	guilds              公会(PK guild_id snowflake,uk name)
//	guild_members       公会成员(PK player_id = 单归属:玩家只属一个公会)
//	guild_join_requests 加入申请(PK request_id snowflake,uk guild_id+player_id)
//
// 角色 / 状态取值与 proto 对齐:
//
//	role:   1 leader / 2 officer / 3 member(GuildRole)
//	status: 1 pending / 2 approved / 3 rejected(GuildJoinStatus)
//
// 成员关系是结构化列,直接映射(CLAUDE.md §5.9 关系型表不强制 proto bytes blob)。
// 复合一致性操作(审批 / 退会 / 踢人 / 转让 / 解散)在单 MySQL 事务内完成;
// 公会成员是 owner(guild_id)单键操作,无跨人事务(不撞 friend 跨人强一致难题)。
package data

import (
	"context"
	"database/sql"
	"errors"

	"github.com/go-sql-driver/mysql"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// 公会职位(与 proto GuildRole 数值一致)。
const (
	GuildRoleLeader  = 1
	GuildRoleOfficer = 2
	GuildRoleMember  = 3
)

// 加入申请状态(与 proto GuildJoinStatus 数值一致)。
const (
	joinStatusPending  = 1
	joinStatusApproved = 2
	joinStatusRejected = 3
)

// mysqlErrDupEntry 是 MySQL 唯一键冲突错误码(用于把 uk_name 冲突翻译成 ErrGuildNameTaken)。
const mysqlErrDupEntry = 1062

// GuildRow 是一行公会(data → biz 内部结构)。
type GuildRow struct {
	GuildID     uint64
	Name        string
	LeaderID    uint64
	MemberCount int32
	MaxMembers  int32
	CreatedMs   int64
}

// GuildMemberRow 是一行公会成员(含所属公会 + 职位 + 加入时间)。
type GuildMemberRow struct {
	PlayerID uint64
	GuildID  uint64
	Role     int32
	JoinedMs int64
}

// GuildJoinRequestRow 是一行加入申请。
type GuildJoinRequestRow struct {
	RequestID uint64
	GuildID   uint64
	PlayerID  uint64
	Status    int32
	CreatedMs int64
}

// GuildRepo 是公会数据层抽象。biz 只依赖此接口,不依赖 *sql.DB。
type GuildRepo interface {
	// CreateGuild 在事务里建公会:校验创建者未在任何公会(单归属)→ 插 guilds → 插 leader 成员。
	//   - 创建者已在公会 → ErrGuildAlreadyInGuild
	//   - 公会名已占用 → ErrGuildNameTaken
	CreateGuild(ctx context.Context, newGuildID, leaderID uint64, name string, maxMembers int) error
	// GetGuild 读公会;not found → (nil, false, nil)。
	GetGuild(ctx context.Context, guildID uint64) (*GuildRow, bool, error)
	// GetMyGuild 读玩家所在公会;不在任何公会 → (nil, false, nil)。
	GetMyGuild(ctx context.Context, playerID uint64) (*GuildRow, bool, error)
	// GetMember 读玩家的成员行(含 guild_id / role);不在任何公会 → (nil, false, nil)。
	GetMember(ctx context.Context, playerID uint64) (*GuildMemberRow, bool, error)
	// ListMembers 列公会成员(按 player_id 升序游标分页;cursor=0 首页,limit>0 限量)。
	ListMembers(ctx context.Context, guildID, cursor uint64, limit int) ([]GuildMemberRow, error)
	// CreateJoinRequest 创建 / 复用加入申请(pending);已 pending → 复用既有 request_id。
	// maxPending>0 时:事务内校验该公会 pending 申请数,新增 / 复开一条会超限则回
	// ErrGuildRequestLimit(不变量 §9.18)。
	CreateJoinRequest(ctx context.Context, newRequestID, guildID, playerID uint64, maxPending int) (requestID uint64, reused bool, err error)
	// GetRequest 读申请;not found → (nil, false, nil)。
	GetRequest(ctx context.Context, requestID uint64) (*GuildJoinRequestRow, bool, error)
	// ApproveJoin 在事务里审批通过:锁申请行 → 校验审批人在该公会且为 leader/officer →
	// 校验申请仍 pending、申请人未在公会、未超员 → 插成员 + 申请置 approved + member_count++。
	// 返回 approved=false,err=nil 表示申请已被并发处理(非 pending),biz 不报成功。
	ApproveJoin(ctx context.Context, requestID, approverID uint64, maxMembers int) (approved bool, err error)
	// RejectJoin 在事务里拒绝:锁申请行 → 校验审批人 leader/officer → 仍 pending → 置 rejected。
	RejectJoin(ctx context.Context, requestID, approverID uint64) (rejected bool, err error)
	// RemoveMember 在事务里删成员 + member_count--(退会 / 踢人共用,幂等:不存在不报错)。
	RemoveMember(ctx context.Context, guildID, playerID uint64) error
	// DisbandGuild 在事务里删公会:删全部成员 + 删全部申请 + 删 guild 行。
	DisbandGuild(ctx context.Context, guildID uint64) error
	// SetRole 设成员职位(任命 / 撤销官员)。
	SetRole(ctx context.Context, guildID, playerID uint64, role int32) error
	// TransferLeader 在事务里转让会长:旧会长降 member,新会长升 leader,更新 guilds.leader_id。
	TransferLeader(ctx context.Context, guildID, oldLeaderID, newLeaderID uint64) error
	// ListPendingRequests 列公会的挂起申请(按 request_id 升序游标分页)。
	ListPendingRequests(ctx context.Context, guildID, cursor uint64, limit int) ([]GuildJoinRequestRow, error)
}

// MySQLGuildRepo 是基于 database/sql 的 GuildRepo 实现。
type MySQLGuildRepo struct {
	db *sql.DB
}

// NewMySQLGuildRepo 构造。db 由 pkg/mysqlx.MustNewClient 提供(连 pandora_social 库)。
func NewMySQLGuildRepo(db *sql.DB) *MySQLGuildRepo {
	return &MySQLGuildRepo{db: db}
}

func isDupEntry(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == mysqlErrDupEntry
}

func (r *MySQLGuildRepo) CreateGuild(ctx context.Context, newGuildID, leaderID uint64, name string, maxMembers int) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		// 单归属:创建者不能已在任何公会。
		var x int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM guild_members WHERE player_id = ? LIMIT 1`, leaderID).Scan(&x)
		if err == nil {
			return errcode.New(errcode.ErrGuildAlreadyInGuild, "player %d already in a guild", leaderID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return errcode.New(errcode.ErrInternal, "check member %d: %v", leaderID, err)
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO guilds (guild_id, name, leader_id, member_count, max_members) VALUES (?, ?, ?, 1, ?)`,
			newGuildID, name, leaderID, maxMembers); err != nil {
			if isDupEntry(err) {
				return errcode.New(errcode.ErrGuildNameTaken, "guild name %q taken", name)
			}
			return errcode.New(errcode.ErrInternal, "insert guild %d: %v", newGuildID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO guild_members (player_id, guild_id, role) VALUES (?, ?, ?)`,
			leaderID, newGuildID, GuildRoleLeader); err != nil {
			return errcode.New(errcode.ErrInternal, "insert leader member %d: %v", leaderID, err)
		}
		return nil
	})
}

func (r *MySQLGuildRepo) GetGuild(ctx context.Context, guildID uint64) (*GuildRow, bool, error) {
	return r.scanGuild(ctx, r.db.QueryRowContext(ctx,
		`SELECT guild_id, name, leader_id, member_count, max_members,
		        CAST(UNIX_TIMESTAMP(created_at) * 1000 AS SIGNED)
		 FROM guilds WHERE guild_id = ?`, guildID))
}

func (r *MySQLGuildRepo) GetMyGuild(ctx context.Context, playerID uint64) (*GuildRow, bool, error) {
	return r.scanGuild(ctx, r.db.QueryRowContext(ctx,
		`SELECT g.guild_id, g.name, g.leader_id, g.member_count, g.max_members,
		        CAST(UNIX_TIMESTAMP(g.created_at) * 1000 AS SIGNED)
		 FROM guilds g JOIN guild_members m ON m.guild_id = g.guild_id
		 WHERE m.player_id = ?`, playerID))
}

func (r *MySQLGuildRepo) scanGuild(_ context.Context, row *sql.Row) (*GuildRow, bool, error) {
	var g GuildRow
	err := row.Scan(&g.GuildID, &g.Name, &g.LeaderID, &g.MemberCount, &g.MaxMembers, &g.CreatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "scan guild: %v", err)
	}
	return &g, true, nil
}

func (r *MySQLGuildRepo) GetMember(ctx context.Context, playerID uint64) (*GuildMemberRow, bool, error) {
	var m GuildMemberRow
	err := r.db.QueryRowContext(ctx,
		`SELECT player_id, guild_id, role, CAST(UNIX_TIMESTAMP(joined_at) * 1000 AS SIGNED)
		 FROM guild_members WHERE player_id = ?`, playerID).
		Scan(&m.PlayerID, &m.GuildID, &m.Role, &m.JoinedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "get member %d: %v", playerID, err)
	}
	return &m, true, nil
}

func (r *MySQLGuildRepo) ListMembers(ctx context.Context, guildID, cursor uint64, limit int) ([]GuildMemberRow, error) {
	q := `SELECT player_id, guild_id, role, CAST(UNIX_TIMESTAMP(joined_at) * 1000 AS SIGNED)
		 FROM guild_members WHERE guild_id = ? AND (? = 0 OR player_id > ?)
		 ORDER BY player_id ASC`
	args := []any{guildID, cursor, cursor}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "list members %d: %v", guildID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []GuildMemberRow
	for rows.Next() {
		var m GuildMemberRow
		if err := rows.Scan(&m.PlayerID, &m.GuildID, &m.Role, &m.JoinedMs); err != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan member: %v", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *MySQLGuildRepo) CreateJoinRequest(ctx context.Context, newRequestID, guildID, playerID uint64, maxPending int) (uint64, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingID uint64
	var status int32
	qerr := tx.QueryRowContext(ctx,
		`SELECT request_id, status FROM guild_join_requests WHERE guild_id = ? AND player_id = ? FOR UPDATE`,
		guildID, playerID).Scan(&existingID, &status)
	switch {
	case errors.Is(qerr, sql.ErrNoRows):
		// 新增一条 pending 前,先校验该公会 pending 申请未满(不变量 §9.18)。
		if cerr := checkGuildPendingLimit(ctx, tx, guildID, maxPending); cerr != nil {
			return 0, false, cerr
		}
		if _, ierr := tx.ExecContext(ctx,
			`INSERT INTO guild_join_requests (request_id, guild_id, player_id, status) VALUES (?, ?, ?, ?)`,
			newRequestID, guildID, playerID, joinStatusPending); ierr != nil {
			return 0, false, errcode.New(errcode.ErrInternal, "insert join request: %v", ierr)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return 0, false, errcode.New(errcode.ErrInternal, "commit: %v", commitErr)
		}
		return newRequestID, false, nil
	case qerr != nil:
		return 0, false, errcode.New(errcode.ErrInternal, "query join request: %v", qerr)
	}

	if status == joinStatusPending {
		if commitErr := tx.Commit(); commitErr != nil {
			return 0, false, errcode.New(errcode.ErrInternal, "commit: %v", commitErr)
		}
		return existingID, true, nil
	}
	// 历史 rejected → 复位 pending,复用 request_id;从非 pending 转 pending 同样占用一格,先校验上限。
	if cerr := checkGuildPendingLimit(ctx, tx, guildID, maxPending); cerr != nil {
		return 0, false, cerr
	}
	if _, uerr := tx.ExecContext(ctx,
		`UPDATE guild_join_requests SET status = ? WHERE request_id = ?`,
		joinStatusPending, existingID); uerr != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "reopen join request: %v", uerr)
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "commit: %v", commitErr)
	}
	return existingID, false, nil
}

// checkGuildPendingLimit 在事务内锁定读该公会的 pending 申请数,达到上限回 ErrGuildRequestLimit
// (不变量 §9.18)。maxPending<=0 关闭校验。FOR UPDATE 对 guild_id 索引区间加锁,消除并发新增
// pending 的幻读竞态。
func checkGuildPendingLimit(ctx context.Context, tx *sql.Tx, guildID uint64, maxPending int) error {
	if maxPending <= 0 {
		return nil
	}
	var cnt int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM guild_join_requests WHERE guild_id = ? AND status = ? FOR UPDATE`,
		guildID, joinStatusPending).Scan(&cnt); err != nil {
		return errcode.New(errcode.ErrInternal, "count pending requests guild=%d: %v", guildID, err)
	}
	if cnt >= maxPending {
		return errcode.New(errcode.ErrGuildRequestLimit,
			"pending join request limit reached for guild %d (max %d)", guildID, maxPending)
	}
	return nil
}

func (r *MySQLGuildRepo) GetRequest(ctx context.Context, requestID uint64) (*GuildJoinRequestRow, bool, error) {
	var rq GuildJoinRequestRow
	err := r.db.QueryRowContext(ctx,
		`SELECT request_id, guild_id, player_id, status, CAST(UNIX_TIMESTAMP(created_at) * 1000 AS SIGNED)
		 FROM guild_join_requests WHERE request_id = ?`, requestID).
		Scan(&rq.RequestID, &rq.GuildID, &rq.PlayerID, &rq.Status, &rq.CreatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "get request %d: %v", requestID, err)
	}
	return &rq, true, nil
}

func (r *MySQLGuildRepo) ApproveJoin(ctx context.Context, requestID, approverID uint64, maxMembers int) (bool, error) {
	approved := false
	err := r.tx(ctx, func(tx *sql.Tx) error {
		// 1. 锁申请行。
		var guildID, applicantID uint64
		var status int32
		err := tx.QueryRowContext(ctx,
			`SELECT guild_id, player_id, status FROM guild_join_requests WHERE request_id = ? FOR UPDATE`,
			requestID).Scan(&guildID, &applicantID, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return errcode.New(errcode.ErrGuildRequestInvalid, "request %d not found", requestID)
		}
		if err != nil {
			return errcode.New(errcode.ErrInternal, "lock request %d: %v", requestID, err)
		}
		if status != joinStatusPending {
			return nil // 已被并发处理,approved 保持 false
		}

		// 2. 审批人须在该公会且为 leader/officer。
		var approverRole int32
		err = tx.QueryRowContext(ctx,
			`SELECT role FROM guild_members WHERE player_id = ? AND guild_id = ?`,
			approverID, guildID).Scan(&approverRole)
		if errors.Is(err, sql.ErrNoRows) {
			return errcode.New(errcode.ErrGuildNoPermission, "approver %d not in guild %d", approverID, guildID)
		}
		if err != nil {
			return errcode.New(errcode.ErrInternal, "check approver: %v", err)
		}
		if approverRole != GuildRoleLeader && approverRole != GuildRoleOfficer {
			return errcode.New(errcode.ErrGuildNoPermission, "approver %d not leader/officer", approverID)
		}

		// 3. 申请人不能已在任何公会(单归属)。
		var x int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM guild_members WHERE player_id = ? LIMIT 1`, applicantID).Scan(&x)
		if err == nil {
			return errcode.New(errcode.ErrGuildAlreadyInGuild, "applicant %d already in a guild", applicantID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return errcode.New(errcode.ErrInternal, "check applicant: %v", err)
		}

		// 4. 不超员(锁公会行读 member_count)。
		var memberCount int32
		if err := tx.QueryRowContext(ctx,
			`SELECT member_count FROM guilds WHERE guild_id = ? FOR UPDATE`, guildID).Scan(&memberCount); err != nil {
			return errcode.New(errcode.ErrInternal, "lock guild %d: %v", guildID, err)
		}
		if int(memberCount) >= maxMembers {
			return errcode.New(errcode.ErrGuildFull, "guild %d full (%d/%d)", guildID, memberCount, maxMembers)
		}

		// 5. 插成员 + 申请 approved + member_count++。
		// player_id 是主键(单归属硬约束):并发被另一个公会先批时 dup → 翻译成
		// 「已在公会」业务错误(步骤 3 的快照读拦不住这种竞态,靠 PK 兑底)。
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO guild_members (player_id, guild_id, role) VALUES (?, ?, ?)`,
			applicantID, guildID, GuildRoleMember); err != nil {
			if isDupEntry(err) {
				return errcode.New(errcode.ErrGuildAlreadyInGuild, "applicant %d already in a guild", applicantID)
			}
			return errcode.New(errcode.ErrInternal, "insert member %d: %v", applicantID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE guild_join_requests SET status = ? WHERE request_id = ?`,
			joinStatusApproved, requestID); err != nil {
			return errcode.New(errcode.ErrInternal, "mark approved: %v", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE guilds SET member_count = member_count + 1 WHERE guild_id = ?`, guildID); err != nil {
			return errcode.New(errcode.ErrInternal, "inc member_count: %v", err)
		}
		approved = true
		return nil
	})
	return approved, err
}

func (r *MySQLGuildRepo) RejectJoin(ctx context.Context, requestID, approverID uint64) (bool, error) {
	rejected := false
	err := r.tx(ctx, func(tx *sql.Tx) error {
		var guildID, applicantID uint64
		var status int32
		err := tx.QueryRowContext(ctx,
			`SELECT guild_id, player_id, status FROM guild_join_requests WHERE request_id = ? FOR UPDATE`,
			requestID).Scan(&guildID, &applicantID, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return errcode.New(errcode.ErrGuildRequestInvalid, "request %d not found", requestID)
		}
		if err != nil {
			return errcode.New(errcode.ErrInternal, "lock request %d: %v", requestID, err)
		}
		if status != joinStatusPending {
			return nil
		}
		var approverRole int32
		err = tx.QueryRowContext(ctx,
			`SELECT role FROM guild_members WHERE player_id = ? AND guild_id = ?`,
			approverID, guildID).Scan(&approverRole)
		if errors.Is(err, sql.ErrNoRows) {
			return errcode.New(errcode.ErrGuildNoPermission, "approver %d not in guild %d", approverID, guildID)
		}
		if err != nil {
			return errcode.New(errcode.ErrInternal, "check approver: %v", err)
		}
		if approverRole != GuildRoleLeader && approverRole != GuildRoleOfficer {
			return errcode.New(errcode.ErrGuildNoPermission, "approver %d not leader/officer", approverID)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE guild_join_requests SET status = ? WHERE request_id = ?`,
			joinStatusRejected, requestID); err != nil {
			return errcode.New(errcode.ErrInternal, "mark rejected: %v", err)
		}
		rejected = true
		return nil
	})
	return rejected, err
}

func (r *MySQLGuildRepo) RemoveMember(ctx context.Context, guildID, playerID uint64) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM guild_members WHERE guild_id = ? AND player_id = ?`, guildID, playerID)
		if err != nil {
			return errcode.New(errcode.ErrInternal, "delete member %d: %v", playerID, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return nil // 幂等:本就不在
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE guilds SET member_count = member_count - 1 WHERE guild_id = ? AND member_count > 0`, guildID); err != nil {
			return errcode.New(errcode.ErrInternal, "dec member_count: %v", err)
		}
		return nil
	})
}

func (r *MySQLGuildRepo) DisbandGuild(ctx context.Context, guildID uint64) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM guild_members WHERE guild_id = ?`, guildID); err != nil {
			return errcode.New(errcode.ErrInternal, "delete members of %d: %v", guildID, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM guild_join_requests WHERE guild_id = ?`, guildID); err != nil {
			return errcode.New(errcode.ErrInternal, "delete requests of %d: %v", guildID, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM guilds WHERE guild_id = ?`, guildID); err != nil {
			return errcode.New(errcode.ErrInternal, "delete guild %d: %v", guildID, err)
		}
		return nil
	})
}

func (r *MySQLGuildRepo) SetRole(ctx context.Context, guildID, playerID uint64, role int32) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE guild_members SET role = ? WHERE guild_id = ? AND player_id = ?`, role, guildID, playerID)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "set role %d: %v", playerID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errcode.New(errcode.ErrGuildNotMember, "player %d not in guild %d", playerID, guildID)
	}
	return nil
}

func (r *MySQLGuildRepo) TransferLeader(ctx context.Context, guildID, oldLeaderID, newLeaderID uint64) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		// 新会长须为本公会成员。
		var role int32
		err := tx.QueryRowContext(ctx,
			`SELECT role FROM guild_members WHERE player_id = ? AND guild_id = ? FOR UPDATE`,
			newLeaderID, guildID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return errcode.New(errcode.ErrGuildNotMember, "target %d not in guild %d", newLeaderID, guildID)
		}
		if err != nil {
			return errcode.New(errcode.ErrInternal, "lock target %d: %v", newLeaderID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE guild_members SET role = ? WHERE guild_id = ? AND player_id = ?`,
			GuildRoleMember, guildID, oldLeaderID); err != nil {
			return errcode.New(errcode.ErrInternal, "demote old leader: %v", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE guild_members SET role = ? WHERE guild_id = ? AND player_id = ?`,
			GuildRoleLeader, guildID, newLeaderID); err != nil {
			return errcode.New(errcode.ErrInternal, "promote new leader: %v", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE guilds SET leader_id = ? WHERE guild_id = ?`, newLeaderID, guildID); err != nil {
			return errcode.New(errcode.ErrInternal, "update guild leader: %v", err)
		}
		return nil
	})
}

func (r *MySQLGuildRepo) ListPendingRequests(ctx context.Context, guildID, cursor uint64, limit int) ([]GuildJoinRequestRow, error) {
	q := `SELECT request_id, guild_id, player_id, status, CAST(UNIX_TIMESTAMP(created_at) * 1000 AS SIGNED)
		 FROM guild_join_requests WHERE guild_id = ? AND status = ? AND (? = 0 OR request_id > ?)
		 ORDER BY request_id ASC`
	args := []any{guildID, joinStatusPending, cursor, cursor}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "list requests %d: %v", guildID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []GuildJoinRequestRow
	for rows.Next() {
		var rq GuildJoinRequestRow
		if err := rows.Scan(&rq.RequestID, &rq.GuildID, &rq.PlayerID, &rq.Status, &rq.CreatedMs); err != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan request: %v", err)
		}
		out = append(out, rq)
	}
	return out, rows.Err()
}

// tx 是事务封装:fn 返回 error 则回滚,nil 则提交。
func (r *MySQLGuildRepo) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "begin tx: %v", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return errcode.New(errcode.ErrInternal, "commit tx: %v", err)
	}
	return nil
}
