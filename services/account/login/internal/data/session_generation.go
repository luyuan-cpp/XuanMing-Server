// session_generation.go — 会话代际 MySQL 持久化(R7 复审 P0-4,2026-07-23;R7 收口改单调代际)。
//
// 背景:角色写 fencing 此前依赖「MySQL 事务内 precommit 读 Redis 会话权威」,precommit
// 通过与 COMMIT 之间存在跨存储窗口——A 的 Redis 检查通过 → B 登录轮换 session → A COMMIT,
// 旧会话的角色写仍成为新会话看到的权威数据。
//
// R7 收口(并发 Login 定序):首版实现是「无条件覆盖 upsert」,并发登录 A/B 各自先写
// Redis 再写 MySQL 时,迟到的 A 会把 MySQL 回写成旧 jti(Redis=B、MySQL=A 撕裂),合法的
// B 反而被 SetRole 代际复核拒绝。现在 MySQL 是登录定序权威:本表增加单调 `generation`
// 列,PersistSessionJTI 在事务内原子 +1 并返回新代际;Login 拿到代际后再对 Redis 做
// 「仅更高代际可覆盖」的条件写(见 account.go RedisSessionRepo.Set)。任意交错下两个
// 存储最终都收敛到最高代际那次登录,输掉定序的登录直接失败,不交付凭据。
//
// SetRole 侧契约不变:同一 MySQL 事务内 SELECT ... FOR UPDATE 复核 sess_jti 后再 COMMIT,
// InnoDB 行锁保证与本表的代际写串行化(见 role.go)。
package data

import (
	"context"
	"database/sql"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// SessionGenerationRepo 持久化玩家当前会话代际(jti + 单调 generation)到 MySQL,
// 供 Login 定序与业务写事务同事务域 fencing。
type SessionGenerationRepo interface {
	// PersistSessionJTI 原子推进玩家会话代际并落当前 jti,返回本次登录分配到的
	// 单调 generation(首登=1)。Login 必须在 Redis 会话写入之前调用;失败必须使
	// 登录失败(fail-closed),否则定序权威落后于 Redis。
	PersistSessionJTI(ctx context.Context, playerID uint64, jti string) (uint64, error)

	// TombstoneSessionJTI 登出墓碑(R8 收口,P2):仅当行内 sess_jti 仍等于本次登出
	// 的 jti 时,把它推进一代并改写为哨兵值(永不命中真 jti)——登出后 MySQL 行
	// 不再与已失效的旧 jti 匹配。条件写(CAS)而非无条件覆盖:并发新登录可能已
	// 把行推到更新代际,无条件墓碑会毒化新会话。返回是否实际写入。
	TombstoneSessionJTI(ctx context.Context, playerID uint64, jti string) (bool, error)
}

// MySQLSessionGenerationRepo 基于 *sql.DB 的实现(pandora_account.player_session_generations)。
type MySQLSessionGenerationRepo struct {
	db *sql.DB
}

// NewMySQLSessionGenerationRepo 构造。db 与 AccountRepo/PlayerRoleRepo 共用连接池。
func NewMySQLSessionGenerationRepo(db *sql.DB) *MySQLSessionGenerationRepo {
	return &MySQLSessionGenerationRepo{db: db}
}

// PersistSessionJTI 事务内「upsert(generation+1) → 读回本行代际 → COMMIT」。
// upsert 持有行 X 锁直到 COMMIT,同事务读回的必然是本次分配的代际,并发登录在此串行化。
func (r *MySQLSessionGenerationRepo) PersistSessionJTI(ctx context.Context, playerID uint64, jti string) (uint64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "begin session generation tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	const upsert = `INSERT INTO player_session_generations(player_id, sess_jti, generation) VALUES (?, ?, 1)
ON DUPLICATE KEY UPDATE generation = generation + 1, sess_jti = VALUES(sess_jti)`
	if _, err := tx.ExecContext(ctx, upsert, playerID, jti); err != nil {
		return 0, errcode.New(errcode.ErrInternal, "mysql persist session jti: %v", err)
	}
	var gen uint64
	if err := tx.QueryRowContext(ctx,
		`SELECT generation FROM player_session_generations WHERE player_id = ?`,
		playerID).Scan(&gen); err != nil {
		return 0, errcode.New(errcode.ErrInternal, "mysql read session generation: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, errcode.New(errcode.ErrInternal, "commit session generation: %v", err)
	}
	return gen, nil
}

// sessionTombstoneJTI 登出墓碑哨兵值:非 uuid 格式,永不与真实 jti 碰撞。
const sessionTombstoneJTI = "logged-out"

// TombstoneSessionJTI 单语句条件 UPDATE:行仍持有本次登出的 jti 才推代际改哨兵,
// 并发新登录已轮换时 WHERE 不命中 = no-op(不毒化新会话)。
func (r *MySQLSessionGenerationRepo) TombstoneSessionJTI(ctx context.Context, playerID uint64, jti string) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE player_session_generations SET generation = generation + 1, sess_jti = ? WHERE player_id = ? AND sess_jti = ?`,
		sessionTombstoneJTI, playerID, jti)
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "mysql tombstone session jti: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "mysql tombstone rows affected: %v", err)
	}
	return n > 0, nil
}
