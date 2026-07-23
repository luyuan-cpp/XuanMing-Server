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
	//
	// expectedSessJTI 非空时(R7 复审 P0-4):在**同一 MySQL 事务内**,UPSERT 之后、COMMIT
	// 之前对 player_session_generations 行做 SELECT ... FOR UPDATE,持久化会话代际与调用方
	// jti 不一致 → 整个事务 ROLLBACK,角色写永不可见。行锁把「角色写」与「登录轮换代际写」
	// 放进同一 InnoDB 串行化域:新登录把新 jti 落库(Login fail-closed 保证先于凭据交付)后,
	// 旧会话的角色写事务必然读到新 jti 而回滚——不再存在 R6 版「Redis precommit 通过与
	// COMMIT 之间被轮换」的跨存储窗口。
	// 兼容窗:该玩家行不存在(部署前登录的存量会话,尚未经过一次新 Login 落库)时,退化为
	// 仅 precommit 复核(与 R6 行为一致);下一次登录后即进入强 fencing。
	//
	// precommit 非 nil 时同样在事务内、COMMIT 之前执行(读 Redis 会话权威的既有防线,保留
	// 作为纵深);返回错误则 ROLLBACK。
	SetRole(ctx context.Context, playerID uint64, roleID uint32, expectedSessJTI string, precommit func(context.Context) error) error
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

func (r *MySQLPlayerRoleRepo) SetRole(ctx context.Context, playerID uint64, roleID uint32, expectedSessJTI string, precommit func(context.Context) error) error {
	const q = `INSERT INTO player_roles(player_id, role_id) VALUES (?, ?)
ON DUPLICATE KEY UPDATE role_id = VALUES(role_id)`
	// 两道防线都未启用时保持单语句路径(dev 裸跑/无会话权威),行为与历史一致。
	if expectedSessJTI == "" && precommit == nil {
		if _, err := r.db.ExecContext(ctx, q, playerID, roleID); err != nil {
			return errcode.New(errcode.ErrInternal, "mysql set player role: %v", err)
		}
		return nil
	}
	// 事务内「UPSERT → 同事务代际复核(FOR UPDATE) → precommit 复核 → COMMIT」:
	// 任一复核失败 ROLLBACK,角色写永不落地;崩溃在复核与 COMMIT 之间由事务自动回滚兜底。
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "begin set role tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, q, playerID, roleID); err != nil {
		return errcode.New(errcode.ErrInternal, "mysql set player role: %v", err)
	}
	if expectedSessJTI != "" {
		// R7 复审 P0-4:同事务域会话代际 fencing。FOR UPDATE 行锁保证与 Login 的代际
		// upsert 串行化——新登录代际已落库则此处必然读到并回滚;此处先持锁提交,则
		// 新登录的代际写在本事务之后,本次角色写序在轮换之前,语义等价「登录前写入」。
		var currentJTI string
		serr := tx.QueryRowContext(ctx,
			`SELECT sess_jti FROM player_session_generations WHERE player_id = ? FOR UPDATE`,
			playerID).Scan(&currentJTI)
		switch {
		case errors.Is(serr, sql.ErrNoRows):
			// 兼容窗:部署前登录的存量会话尚无代际行,退化为 precommit(Redis)复核。
		case serr != nil:
			return errcode.New(errcode.ErrInternal, "mysql read session generation: %v", serr)
		case currentJTI != expectedSessJTI:
			return errcode.New(errcode.ErrSessionSuperseded,
				"session superseded; role write rolled back")
		}
	}
	if precommit != nil {
		if err := precommit(ctx); err != nil {
			return err // ROLLBACK by defer:检查不过,写不落地
		}
	}
	if err := tx.Commit(); err != nil {
		return errcode.New(errcode.ErrInternal, "commit set player role: %v", err)
	}
	return nil
}
