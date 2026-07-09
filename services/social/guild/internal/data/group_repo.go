// group_repo.go 是 guild 服务里临时群(GroupService)的数据层(2026-06-27)。
//
// 库表(deploy/mysql-init/11-guild-tables.sql,pandora_social 库):
//
//	chat_groups        临时群(PK group_id snowflake)
//	chat_group_members 群成员(PK group_id+player_id = 多归属:玩家可在多个群)
//
// 角色取值与 proto GroupRole 对齐:1 owner / 2 member。
// 与公会(单归属、有职位审批)区分:群组多归属、轻量、只有 owner / member 两级。
package data

import (
	"context"
	"database/sql"
	"errors"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// 群组职位(与 proto GroupRole 数值一致)。
const (
	GroupRoleOwner  = 1
	GroupRoleMember = 2
)

// groupListReadHardLimit 是群成员 / 我所在的群列表单次返回的防御性 SQL 上限(不变量 §9.18
// 读取侧「单次返回上限」兜底)。群成员受 max_group_members(默认 50)兜住,我所在的群受
// max_groups_per_player(默认 50)兜住;此处取更宽松硬上限,仅防历史脏数据下的无界返回。
const groupListReadHardLimit = 500

// GroupRow 是一行临时群。
type GroupRow struct {
	GroupID     uint64
	Name        string
	OwnerID     uint64
	MemberCount int32
	MaxMembers  int32
	CreatedMs   int64
}

// GroupMemberRow 是一行群成员。
type GroupMemberRow struct {
	GroupID  uint64
	PlayerID uint64
	Role     int32
	JoinedMs int64
}

// GroupRepo 是临时群数据层抽象。
type GroupRepo interface {
	// CreateGroup 在事务里建群:插 chat_groups → 插 owner 成员 → 插初始成员(去重 owner)。
	// memberIDs 已由 biz 去重并排除 owner;maxMembers 用于上限校验。
	// maxGroups>0 时:对 owner 及每个初始成员校验其所在群数未超上限(不变量 §9.18)。
	CreateGroup(ctx context.Context, newGroupID, ownerID uint64, name string, memberIDs []uint64, maxMembers, maxGroups int) error
	// GetGroup 读群;not found → (nil, false, nil)。
	GetGroup(ctx context.Context, groupID uint64) (*GroupRow, bool, error)
	// GetGroupMember 读群成员行;不在群 → (nil, false, nil)。
	GetGroupMember(ctx context.Context, groupID, playerID uint64) (*GroupMemberRow, bool, error)
	// ListGroupMembers 列群成员(owner 在前)。
	ListGroupMembers(ctx context.Context, groupID uint64) ([]GroupMemberRow, error)
	// ListMyGroups 列玩家所在的群。
	ListMyGroups(ctx context.Context, playerID uint64) ([]GroupRow, error)
	// AddMember 在事务里拉人入群:锁群行 → 校验未在群、未超员 → 插成员 + member_count++。
	//   返回 alreadyIn=true 表示玩家已在群(幂等命中,未改动)。
	// maxGroups>0 时:新加入前校验该玩家所在群数未超上限(不变量 §9.18)。
	AddMember(ctx context.Context, groupID, playerID uint64, maxMembers, maxGroups int) (alreadyIn bool, err error)
	// RemoveMember 在事务里删群成员 + member_count--(退群 / 踢人共用,幂等)。
	RemoveMember(ctx context.Context, groupID, playerID uint64) error
	// DisbandGroup 在事务里删群:删全部成员 + 删 group 行。
	DisbandGroup(ctx context.Context, groupID uint64) error
	// TransferOwner 在事务里转让群主:旧群主降 member,新群主升 owner,更新 chat_groups.owner_id。
	TransferOwner(ctx context.Context, groupID, oldOwnerID, newOwnerID uint64) error
}

// MySQLGroupRepo 是基于 database/sql 的 GroupRepo 实现(与 GuildRepo 共用同一 *sql.DB)。
type MySQLGroupRepo struct {
	db *sql.DB
}

// NewMySQLGroupRepo 构造。db 连 pandora_social 库。
func NewMySQLGroupRepo(db *sql.DB) *MySQLGroupRepo {
	return &MySQLGroupRepo{db: db}
}

func (r *MySQLGroupRepo) CreateGroup(ctx context.Context, newGroupID, ownerID uint64, name string, memberIDs []uint64, maxMembers, maxGroups int) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		count := 1 + len(memberIDs)
		if count > maxMembers {
			return errcode.New(errcode.ErrGroupFull, "group members %d > max %d", count, maxMembers)
		}
		// 每个入群玩家(owner + 初始成员)都不得超自身所在群上限(不变量 §9.18)。
		if err := checkPlayerGroupLimit(ctx, tx, ownerID, maxGroups); err != nil {
			return err
		}
		for _, m := range memberIDs {
			if err := checkPlayerGroupLimit(ctx, tx, m, maxGroups); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO chat_groups (group_id, name, owner_id, member_count, max_members) VALUES (?, ?, ?, ?, ?)`,
			newGroupID, name, ownerID, count, maxMembers); err != nil {
			return errcode.New(errcode.ErrInternal, "insert group %d: %v", newGroupID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO chat_group_members (group_id, player_id, role) VALUES (?, ?, ?)`,
			newGroupID, ownerID, GroupRoleOwner); err != nil {
			return errcode.New(errcode.ErrInternal, "insert owner member: %v", err)
		}
		for _, m := range memberIDs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO chat_group_members (group_id, player_id, role) VALUES (?, ?, ?)`,
				newGroupID, m, GroupRoleMember); err != nil {
				return errcode.New(errcode.ErrInternal, "insert member %d: %v", m, err)
			}
		}
		return nil
	})
}

func (r *MySQLGroupRepo) GetGroup(ctx context.Context, groupID uint64) (*GroupRow, bool, error) {
	var g GroupRow
	err := r.db.QueryRowContext(ctx,
		`SELECT group_id, name, owner_id, member_count, max_members,
		        CAST(UNIX_TIMESTAMP(created_at) * 1000 AS SIGNED)
		 FROM chat_groups WHERE group_id = ?`, groupID).
		Scan(&g.GroupID, &g.Name, &g.OwnerID, &g.MemberCount, &g.MaxMembers, &g.CreatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "get group %d: %v", groupID, err)
	}
	return &g, true, nil
}

func (r *MySQLGroupRepo) GetGroupMember(ctx context.Context, groupID, playerID uint64) (*GroupMemberRow, bool, error) {
	var m GroupMemberRow
	err := r.db.QueryRowContext(ctx,
		`SELECT group_id, player_id, role, CAST(UNIX_TIMESTAMP(joined_at) * 1000 AS SIGNED)
		 FROM chat_group_members WHERE group_id = ? AND player_id = ?`, groupID, playerID).
		Scan(&m.GroupID, &m.PlayerID, &m.Role, &m.JoinedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "get group member %d/%d: %v", groupID, playerID, err)
	}
	return &m, true, nil
}

func (r *MySQLGroupRepo) ListGroupMembers(ctx context.Context, groupID uint64) ([]GroupMemberRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT group_id, player_id, role, CAST(UNIX_TIMESTAMP(joined_at) * 1000 AS SIGNED)
		 FROM chat_group_members WHERE group_id = ? ORDER BY role ASC, joined_at ASC LIMIT ?`, groupID, groupListReadHardLimit)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "list group members %d: %v", groupID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []GroupMemberRow
	for rows.Next() {
		var m GroupMemberRow
		if err := rows.Scan(&m.GroupID, &m.PlayerID, &m.Role, &m.JoinedMs); err != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan group member: %v", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *MySQLGroupRepo) ListMyGroups(ctx context.Context, playerID uint64) ([]GroupRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT g.group_id, g.name, g.owner_id, g.member_count, g.max_members,
		        CAST(UNIX_TIMESTAMP(g.created_at) * 1000 AS SIGNED)
		 FROM chat_groups g JOIN chat_group_members m ON m.group_id = g.group_id
		 WHERE m.player_id = ? ORDER BY g.created_at DESC LIMIT ?`, playerID, groupListReadHardLimit)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "list my groups %d: %v", playerID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []GroupRow
	for rows.Next() {
		var g GroupRow
		if err := rows.Scan(&g.GroupID, &g.Name, &g.OwnerID, &g.MemberCount, &g.MaxMembers, &g.CreatedMs); err != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan group: %v", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (r *MySQLGroupRepo) AddMember(ctx context.Context, groupID, playerID uint64, maxMembers, maxGroups int) (bool, error) {
	alreadyIn := false
	err := r.tx(ctx, func(tx *sql.Tx) error {
		var memberCount int32
		err := tx.QueryRowContext(ctx,
			`SELECT member_count FROM chat_groups WHERE group_id = ? FOR UPDATE`, groupID).Scan(&memberCount)
		if errors.Is(err, sql.ErrNoRows) {
			return errcode.New(errcode.ErrGroupNotFound, "group %d not found", groupID)
		}
		if err != nil {
			return errcode.New(errcode.ErrInternal, "lock group %d: %v", groupID, err)
		}

		var x int
		err = tx.QueryRowContext(ctx,
			`SELECT 1 FROM chat_group_members WHERE group_id = ? AND player_id = ? LIMIT 1`,
			groupID, playerID).Scan(&x)
		if err == nil {
			alreadyIn = true
			return nil // 幂等命中
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return errcode.New(errcode.ErrInternal, "check group member: %v", err)
		}

		if int(memberCount) >= maxMembers {
			return errcode.New(errcode.ErrGroupFull, "group %d full (%d/%d)", groupID, memberCount, maxMembers)
		}
		// 新加入前校验目标玩家所在群数未超上限(不变量 §9.18)。
		if err := checkPlayerGroupLimit(ctx, tx, playerID, maxGroups); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO chat_group_members (group_id, player_id, role) VALUES (?, ?, ?)`,
			groupID, playerID, GroupRoleMember); err != nil {
			return errcode.New(errcode.ErrInternal, "insert group member %d: %v", playerID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE chat_groups SET member_count = member_count + 1 WHERE group_id = ?`, groupID); err != nil {
			return errcode.New(errcode.ErrInternal, "inc group member_count: %v", err)
		}
		return nil
	})
	return alreadyIn, err
}

func (r *MySQLGroupRepo) RemoveMember(ctx context.Context, groupID, playerID uint64) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM chat_group_members WHERE group_id = ? AND player_id = ?`, groupID, playerID)
		if err != nil {
			return errcode.New(errcode.ErrInternal, "delete group member %d: %v", playerID, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // 幂等
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE chat_groups SET member_count = member_count - 1 WHERE group_id = ? AND member_count > 0`, groupID); err != nil {
			return errcode.New(errcode.ErrInternal, "dec group member_count: %v", err)
		}
		return nil
	})
}

func (r *MySQLGroupRepo) DisbandGroup(ctx context.Context, groupID uint64) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM chat_group_members WHERE group_id = ?`, groupID); err != nil {
			return errcode.New(errcode.ErrInternal, "delete group members %d: %v", groupID, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM chat_groups WHERE group_id = ?`, groupID); err != nil {
			return errcode.New(errcode.ErrInternal, "delete group %d: %v", groupID, err)
		}
		return nil
	})
}

func (r *MySQLGroupRepo) TransferOwner(ctx context.Context, groupID, oldOwnerID, newOwnerID uint64) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		var role int32
		err := tx.QueryRowContext(ctx,
			`SELECT role FROM chat_group_members WHERE group_id = ? AND player_id = ? FOR UPDATE`,
			groupID, newOwnerID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return errcode.New(errcode.ErrGroupNotMember, "target %d not in group %d", newOwnerID, groupID)
		}
		if err != nil {
			return errcode.New(errcode.ErrInternal, "lock target %d: %v", newOwnerID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE chat_group_members SET role = ? WHERE group_id = ? AND player_id = ?`,
			GroupRoleMember, groupID, oldOwnerID); err != nil {
			return errcode.New(errcode.ErrInternal, "demote old owner: %v", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE chat_group_members SET role = ? WHERE group_id = ? AND player_id = ?`,
			GroupRoleOwner, groupID, newOwnerID); err != nil {
			return errcode.New(errcode.ErrInternal, "promote new owner: %v", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE chat_groups SET owner_id = ? WHERE group_id = ?`, newOwnerID, groupID); err != nil {
			return errcode.New(errcode.ErrInternal, "update group owner: %v", err)
		}
		return nil
	})
}

// checkPlayerGroupLimit 在事务内锁定读玩家当前所在群数,达到上限回 ErrGroupJoinLimit
// (不变量 §9.18)。maxGroups<=0 关闭校验。FOR UPDATE 对 player_id 索引区间加锁,消除并发
// 入群致「我所在的群」列表超限的幻读竞态。
func checkPlayerGroupLimit(ctx context.Context, tx *sql.Tx, playerID uint64, maxGroups int) error {
	if maxGroups <= 0 {
		return nil
	}
	var cnt int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chat_group_members WHERE player_id = ? FOR UPDATE`, playerID).Scan(&cnt); err != nil {
		return errcode.New(errcode.ErrInternal, "count player groups %d: %v", playerID, err)
	}
	if cnt >= maxGroups {
		return errcode.New(errcode.ErrGroupJoinLimit,
			"group join limit reached for %d (max %d)", playerID, maxGroups)
	}
	return nil
}

func (r *MySQLGroupRepo) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
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
