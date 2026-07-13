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
	"sort"

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
	// AddMember 在事务里拉人入群:锁群行 → 复核 operatorID 仍是本群成员(邀请者权限,持锁复核
	// 消除「biz 检查通过后邀请者已退群仍能拉人」的 TOCTOU)→ 校验未在群、未超员 → 插成员 + member_count++。
	//   返回 alreadyIn=true 表示玩家已在群(幂等命中,未改动)。
	// maxGroups>0 时:新加入前校验该玩家所在群数未超上限(不变量 §9.18)。
	AddMember(ctx context.Context, groupID, operatorID, playerID uint64, maxMembers, maxGroups int) (alreadyIn bool, err error)
	// RemoveMember 在事务里删群成员 + member_count--(玩家本人退群用,幂等)。先锁群行且禁移除现任群主。
	RemoveMember(ctx context.Context, groupID, playerID uint64) error
	// KickMember 在事务里踢人:锁群行 → 持锁复核 operatorID 为现任群主、target 在群且非群主 → 删成员 + member_count--。
	// operatorID 权限在事务内权威复核,消除「biz 检查通过后群主被并发转让仍能踢人」的 TOCTOU(三审 P1-9)。
	KickMember(ctx context.Context, groupID, operatorID, targetID uint64) error
	// DisbandGroup 在事务里删群:锁群行 → 持锁复核 operatorID 仍是现任群主 → 删全部成员 + 删 group 行。
	DisbandGroup(ctx context.Context, groupID, operatorID uint64) error
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
		// reservePlayerGroupSlot 对 player_group_counts 该玩家计数行 FOR UPDATE 加锁,若按 owner +
		// 客户端原始成员顺序逐个加锁,两个成员集相反的并发建群会形成 A↔B 锁环致确定性死锁(五审 P2)。
		// 故先把 owner + 全部成员去重后按 player_id 升序排序,统一全局加锁序,消除 ABBA 死锁;
		// 配合 tx 的 1213/1205 有界重试兜底二级索引间隙锁偶发死锁。
		lockIDs := make([]uint64, 0, count)
		lockIDs = append(lockIDs, ownerID)
		lockIDs = append(lockIDs, memberIDs...)
		sort.Slice(lockIDs, func(i, j int) bool { return lockIDs[i] < lockIDs[j] })
		var prev uint64
		var seenPrev bool
		for _, pid := range lockIDs {
			if seenPrev && pid == prev {
				continue // 去重:同一玩家只需锁一次
			}
			prev, seenPrev = pid, true
			if err := reservePlayerGroupSlot(ctx, tx, pid, maxGroups); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO chat_groups (group_id, name, owner_id, member_count, max_members) VALUES (?, ?, ?, ?, ?)`,
			newGroupID, name, ownerID, count, maxMembers); err != nil {
			return dbErr(err, "insert group %d", newGroupID)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO chat_group_members (group_id, player_id, role) VALUES (?, ?, ?)`,
			newGroupID, ownerID, GroupRoleOwner); err != nil {
			return dbErr(err, "insert owner member")
		}
		for _, m := range memberIDs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO chat_group_members (group_id, player_id, role) VALUES (?, ?, ?)`,
				newGroupID, m, GroupRoleMember); err != nil {
				return dbErr(err, "insert member %d", m)
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

func (r *MySQLGroupRepo) AddMember(ctx context.Context, groupID, operatorID, playerID uint64, maxMembers, maxGroups int) (bool, error) {
	alreadyIn := false
	err := r.tx(ctx, func(tx *sql.Tx) error {
		var memberCount int32
		err := tx.QueryRowContext(ctx,
			`SELECT member_count FROM chat_groups WHERE group_id = ? FOR UPDATE`, groupID).Scan(&memberCount)
		if errors.Is(err, sql.ErrNoRows) {
			return errcode.New(errcode.ErrGroupNotFound, "group %d not found", groupID)
		}
		if err != nil {
			return dbErr(err, "lock group %d", groupID)
		}

		// 持群行锁复核邀请者仍在群内(权威),消除「biz GetGroupMember 通过后邀请者已退群 / 被踢
		// 仍能拉人」的 TOCTOU(三审 P1-9)。operatorID==0 时跳过(内部无邀请者场景不经此路径)。
		if operatorID != 0 {
			var x int
			oerr := tx.QueryRowContext(ctx,
				`SELECT 1 FROM chat_group_members WHERE group_id = ? AND player_id = ? LIMIT 1`,
				groupID, operatorID).Scan(&x)
			if errors.Is(oerr, sql.ErrNoRows) {
				return errcode.New(errcode.ErrGroupNotMember, "operator %d not in group %d", operatorID, groupID)
			}
			if oerr != nil {
				return dbErr(oerr, "check operator %d", operatorID)
			}
		}

		var x int
		err = tx.QueryRowContext(ctx,
			`SELECT 1 FROM chat_group_members WHERE group_id = ? AND player_id = ? LIMIT 1`,
			groupID, playerID).Scan(&x)
		if err == nil {
			if _, err := reconcilePlayerGroupCount(ctx, tx, playerID); err != nil {
				return err
			}
			alreadyIn = true
			return nil // 幂等命中也自愈旧 Pod 留下的计数漂移
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return dbErr(err, "check group member")
		}

		if int(memberCount) >= maxMembers {
			return errcode.New(errcode.ErrGroupFull, "group %d full (%d/%d)", groupID, memberCount, maxMembers)
		}
		// 新加入前校验目标玩家所在群数未超上限(不变量 §9.18)。
		if err := reservePlayerGroupSlot(ctx, tx, playerID, maxGroups); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO chat_group_members (group_id, player_id, role) VALUES (?, ?, ?)`,
			groupID, playerID, GroupRoleMember); err != nil {
			return dbErr(err, "insert group member %d", playerID)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE chat_groups SET member_count = member_count + 1 WHERE group_id = ?`, groupID); err != nil {
			return dbErr(err, "inc group member_count")
		}
		return nil
	})
	return alreadyIn, err
}

func (r *MySQLGroupRepo) RemoveMember(ctx context.Context, groupID, playerID uint64) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		// 先锁群行,与 TransferOwner 共用串行化点,并禁止移除现任群主:否则退群/踢人与转让
		// 交错会删掉刚晋升的新群主并留下悬空 owner_id(三审 P1-9 TOCTOU)。群主须先转让或解散。
		var curOwner uint64
		err := tx.QueryRowContext(ctx,
			`SELECT owner_id FROM chat_groups WHERE group_id = ? FOR UPDATE`, groupID).Scan(&curOwner)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // 群已解散,退群/踢人幂等成功
		}
		if err != nil {
			return dbErr(err, "lock group %d", groupID)
		}
		if curOwner == playerID {
			return errcode.New(errcode.ErrGroupNotOwner,
				"owner %d must transfer or disband before leaving group %d", playerID, groupID)
		}
		// 固定 count-row → detail-range 顺序；先锁住该玩家全部成员关系，再执行 DELETE。
		if _, err := reconcilePlayerGroupCount(ctx, tx, playerID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM chat_group_members WHERE group_id = ? AND player_id = ?`, groupID, playerID)
		if err != nil {
			return dbErr(err, "delete group member %d", playerID)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // 前置 reconcile 已完成幂等退群的计数自愈
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE chat_groups SET member_count = member_count - 1 WHERE group_id = ? AND member_count > 0`, groupID); err != nil {
			return dbErr(err, "dec group member_count")
		}
		// 释放该玩家一个「所在群」名额(计数表,§3.5)。
		if err := releasePlayerGroupSlot(ctx, tx, playerID); err != nil {
			return err
		}
		return nil
	})
}

func (r *MySQLGroupRepo) KickMember(ctx context.Context, groupID, operatorID, targetID uint64) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		// 先锁群行(与 TransferOwner/RemoveMember 共用串行化点)并持锁复核 operatorID 仍是现任群主、
		// target 在群且非群主:消除「biz 检查通过后群主被并发转让 / target 被转成群主仍被踢」的
		// TOCTOU(三审 P1-9)。
		var curOwner uint64
		err := tx.QueryRowContext(ctx,
			`SELECT owner_id FROM chat_groups WHERE group_id = ? FOR UPDATE`, groupID).Scan(&curOwner)
		if errors.Is(err, sql.ErrNoRows) {
			return errcode.New(errcode.ErrGroupNotFound, "group %d not found", groupID)
		}
		if err != nil {
			return dbErr(err, "lock group %d", groupID)
		}
		if curOwner != operatorID {
			return errcode.New(errcode.ErrGroupNotOwner,
				"player %d is not current owner of group %d (concurrent transfer?)", operatorID, groupID)
		}
		if targetID == curOwner {
			return errcode.New(errcode.ErrGroupNotOwner, "cannot kick the owner")
		}
		if _, err := reconcilePlayerGroupCount(ctx, tx, targetID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM chat_group_members WHERE group_id = ? AND player_id = ?`, groupID, targetID)
		if err != nil {
			return dbErr(err, "delete group member %d", targetID)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errcode.New(errcode.ErrGroupNotMember, "target %d not in group %d", targetID, groupID)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE chat_groups SET member_count = member_count - 1 WHERE group_id = ? AND member_count > 0`, groupID); err != nil {
			return dbErr(err, "dec group member_count")
		}
		// 释放被踢玩家一个「所在群」名额(计数表,§3.5)。
		if err := releasePlayerGroupSlot(ctx, tx, targetID); err != nil {
			return err
		}
		return nil
	})
}

func (r *MySQLGroupRepo) DisbandGroup(ctx context.Context, groupID, operatorID uint64) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		// 先锁群行并复核 operatorID 仍是现任群主:防旧群主转让后仍解散群(三审 P1-9)。
		var curOwner uint64
		err := tx.QueryRowContext(ctx,
			`SELECT owner_id FROM chat_groups WHERE group_id = ? FOR UPDATE`, groupID).Scan(&curOwner)
		if errors.Is(err, sql.ErrNoRows) {
			return errcode.New(errcode.ErrGroupNotFound, "group %d not found", groupID)
		}
		if err != nil {
			return dbErr(err, "lock group %d", groupID)
		}
		if curOwner != operatorID {
			return errcode.New(errcode.ErrGroupNotOwner,
				"player %d is not current owner of group %d (concurrent transfer?)", operatorID, groupID)
		}
		// 先读出全部成员 player_id 并升序排序,统一 player_group_counts 计数行加锁序(与 CreateGroup
		// 的升序 reserve 同序),减少并发解散 / 建群间的 ABBA 死锁,由 tx 有界重试兜底。
		rows, err := tx.QueryContext(ctx,
			`SELECT player_id FROM chat_group_members WHERE group_id = ?`, groupID)
		if err != nil {
			return dbErr(err, "list group members %d", groupID)
		}
		var memberIDs []uint64
		for rows.Next() {
			var pid uint64
			if serr := rows.Scan(&pid); serr != nil {
				_ = rows.Close()
				return dbErr(serr, "scan group member %d", groupID)
			}
			memberIDs = append(memberIDs, pid)
		}
		if rerr := rows.Err(); rerr != nil {
			_ = rows.Close()
			return dbErr(rerr, "iter group members %d", groupID)
		}
		_ = rows.Close()
		sort.Slice(memberIDs, func(i, j int) bool { return memberIDs[i] < memberIDs[j] })

		// 与 CreateGroup 相同，按 player_id 升序逐个取得 count-row → detail-range；不能先锁完
		// 所有 count row 再碰明细，否则会和逐玩家 reserve 形成 count(B) ↔ detail(A) 的 ABBA。
		for _, pid := range memberIDs {
			if _, err := reconcilePlayerGroupCount(ctx, tx, pid); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM chat_group_members WHERE group_id = ?`, groupID); err != nil {
			return dbErr(err, "delete group members %d", groupID)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM chat_groups WHERE group_id = ?`, groupID); err != nil {
			return dbErr(err, "delete group %d", groupID)
		}
		// 每个成员释放一个「所在群」名额(计数表,§3.5)。
		for _, pid := range memberIDs {
			if err := releasePlayerGroupSlot(ctx, tx, pid); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *MySQLGroupRepo) TransferOwner(ctx context.Context, groupID, oldOwnerID, newOwnerID uint64) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		// 先锁群行(串行化点)并确认旧群主仍是现任群主,防止并发两次转让产生双 OWNER
		// (三审 P1-9 TOCTOU)。lock 顺序统一为 chat_groups → chat_group_members,与 RemoveMember 一致。
		var curOwner uint64
		err := tx.QueryRowContext(ctx,
			`SELECT owner_id FROM chat_groups WHERE group_id = ? FOR UPDATE`, groupID).Scan(&curOwner)
		if errors.Is(err, sql.ErrNoRows) {
			return errcode.New(errcode.ErrGroupNotFound, "group %d not found", groupID)
		}
		if err != nil {
			return dbErr(err, "lock group %d", groupID)
		}
		if curOwner != oldOwnerID {
			return errcode.New(errcode.ErrGroupNotOwner,
				"player %d is not current owner of group %d (concurrent transfer?)", oldOwnerID, groupID)
		}
		var role int32
		err = tx.QueryRowContext(ctx,
			`SELECT role FROM chat_group_members WHERE group_id = ? AND player_id = ? FOR UPDATE`,
			groupID, newOwnerID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return errcode.New(errcode.ErrGroupNotMember, "target %d not in group %d", newOwnerID, groupID)
		}
		if err != nil {
			return dbErr(err, "lock target %d", newOwnerID)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE chat_group_members SET role = ? WHERE group_id = ? AND player_id = ?`,
			GroupRoleMember, groupID, oldOwnerID); err != nil {
			return dbErr(err, "demote old owner")
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE chat_group_members SET role = ? WHERE group_id = ? AND player_id = ?`,
			GroupRoleOwner, groupID, newOwnerID); err != nil {
			return dbErr(err, "promote new owner")
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE chat_groups SET owner_id = ? WHERE group_id = ?`, newOwnerID, groupID); err != nil {
			return dbErr(err, "update group owner")
		}
		return nil
	})
}

// reservePlayerGroupSlot 在事务内为玩家占用一个「所在群」名额。固定锁顺序是：
// player_group_counts 单行 → chat_group_members(player_id) 明细范围。前者串行化 TiDB 新/新写，
// 后者复用 MySQL 旧版 COUNT...FOR UPDATE 的索引锁，使旧/新混跑时新版必看到旧写提交后的明细。
// 明细 COUNT 是权威，计数行只作为 TiDB 串行化点和读优化值；调用方须继续按 player_id 升序。
func reservePlayerGroupSlot(ctx context.Context, tx *sql.Tx, playerID uint64, maxGroups int) error {
	cnt, err := reconcilePlayerGroupCount(ctx, tx, playerID)
	if err != nil {
		return err
	}
	if maxGroups > 0 && cnt >= maxGroups {
		return errcode.New(errcode.ErrGroupJoinLimit,
			"group join limit reached for %d (max %d)", playerID, maxGroups)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE player_group_counts SET group_count = ? WHERE player_id = ?`, cnt+1, playerID); err != nil {
		return dbErr(err, "set reserved group count %d", playerID)
	}
	return nil
}

// reconcilePlayerGroupCount 先取得 per-player 计数行锁，再用旧版同款明细锁定读取实际群数并
// 绝对值回写。TiDB 不靠明细范围锁防幻读，而靠第一把计数行锁串行所有兼容新版；旧版只能在
// MySQL 混跑，那里其 COUNT...FOR UPDATE / 明细 INSERT、DELETE 会与第二把锁互斥。
func reconcilePlayerGroupCount(ctx context.Context, tx *sql.Tx, playerID uint64) (int, error) {
	// 惰性建行：空更新 + 显式 FOR UPDATE 保证已有行和首次创建都成为新版本唯一串行化点。
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO player_group_counts (player_id, group_count) VALUES (?, 0)
		 ON DUPLICATE KEY UPDATE player_id = player_id`, playerID); err != nil {
		return 0, dbErr(err, "ensure group count row %d", playerID)
	}
	var stored int
	if err := tx.QueryRowContext(ctx,
		`SELECT group_count FROM player_group_counts WHERE player_id = ? FOR UPDATE`, playerID).Scan(&stored); err != nil {
		return 0, dbErr(err, "lock group count %d", playerID)
	}
	var actual int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chat_group_members WHERE player_id = ? FOR UPDATE`, playerID).Scan(&actual); err != nil {
		return 0, dbErr(err, "count player groups %d", playerID)
	}
	if stored != actual {
		if _, err := tx.ExecContext(ctx,
			`UPDATE player_group_counts SET group_count = ? WHERE player_id = ?`, actual, playerID); err != nil {
			return 0, dbErr(err, "reconcile group count %d", playerID)
		}
	}
	return actual, nil
}

// releasePlayerGroupSlot 在调用方已按 count-row → detail-range 顺序锁定并完成明细 DELETE 后重算，
// 不对可能被旧 Pod 留脏的计数做 -1。
func releasePlayerGroupSlot(ctx context.Context, tx *sql.Tx, playerID uint64) error {
	_, err := reconcilePlayerGroupCount(ctx, tx, playerID)
	return err
}

// tx 是带死锁重试的事务封装(复用 guild_repo.go 同包的 txMaxRetries / isRetryableTxErr / dbErr)。
// chat_groups 群行是每群操作的第一把锁(串行化点);CreateGroup 另按 player_id 升序依次锁
// 计数行→明细范围。此重试兜底二级索引间隙锁等偶发 1213/1205;fn 必须可安全重放。
func (r *MySQLGroupRepo) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	var lastErr error
	for attempt := 0; attempt <= txMaxRetries; attempt++ {
		lastErr = r.txOnce(ctx, fn)
		if lastErr == nil || !isRetryableTxErr(lastErr) || ctx.Err() != nil {
			return lastErr
		}
	}
	return lastErr
}

// txOnce 执行单次事务:fn 返回 error 则回滚,nil 则提交。
func (r *MySQLGroupRepo) txOnce(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "begin tx: %v", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return dbErr(err, "commit tx")
	}
	return nil
}
