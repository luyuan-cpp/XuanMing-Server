// role.go — 玩家已选角色仓储(选角权威化,2026-07-08)。
//
// 设计(docs 综述):
//   - login 是"角色数据权威":player_roles 表存玩家当前已选角色(覆盖式,每次登录可重选)。
//   - hub_allocator 是"hub 票据签发权威":login 只把 role_id 透传给 AssignHub,
//     由 allocator 签进票据 claim,自己不另签(保持单一签发权威)。
//   - 弱依赖纪律:Login 读角色失败只记日志按 0(未选角)处理,不阻断登录;
//     SelectRole 写库失败必须报错(权威数据没落库就不能发票)。
package data

import (
	"context"
	"database/sql"
	"errors"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// PlayerRoleRepo 是玩家已选角色数据访问接口。biz 依赖接口,方便单测 fake。
type PlayerRoleRepo interface {
	// GetRole 查玩家当前已选角色。从未选过返回 (0, nil)(不是错误——登录首次就是这个态)。
	GetRole(ctx context.Context, playerID uint64) (roleID uint32, err error)

	// SetRole 覆盖式 upsert 玩家已选角色。幂等:重复设置同角色也成功。
	SetRole(ctx context.Context, playerID uint64, roleID uint32) error
}

// MySQLPlayerRoleRepo 基于 *sql.DB 的实现(pandora_account.player_roles)。
type MySQLPlayerRoleRepo struct {
	db *sql.DB
}

// NewMySQLPlayerRoleRepo 构造。db 由 pkg/mysqlx.MustNewClient 提供(与 AccountRepo 共用连接池)。
func NewMySQLPlayerRoleRepo(db *sql.DB) *MySQLPlayerRoleRepo {
	return &MySQLPlayerRoleRepo{db: db}
}

func (r *MySQLPlayerRoleRepo) GetRole(ctx context.Context, playerID uint64) (uint32, error) {
	const q = `SELECT role_id FROM player_roles WHERE player_id = ? LIMIT 1`
	var roleID uint32
	err := r.db.QueryRowContext(ctx, q, playerID).Scan(&roleID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil // 从未选过角,合法态
	}
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "mysql get player role: %v", err)
	}
	return roleID, nil
}

func (r *MySQLPlayerRoleRepo) SetRole(ctx context.Context, playerID uint64, roleID uint32) error {
	const q = `INSERT INTO player_roles(player_id, role_id) VALUES (?, ?)
ON DUPLICATE KEY UPDATE role_id = VALUES(role_id)`
	if _, err := r.db.ExecContext(ctx, q, playerID, roleID); err != nil {
		return errcode.New(errcode.ErrInternal, "mysql set player role: %v", err)
	}
	return nil
}
