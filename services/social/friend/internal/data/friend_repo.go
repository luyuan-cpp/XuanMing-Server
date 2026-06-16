// Package data 是 friend 服务的数据层(MySQL 好友图 / 好友请求 / 黑名单,2026-06-15)。
//
// 库表(deploy/mysql-init/06-social-tables.sql,pandora_social 库):
//
//	friendships      双向好友边(每对好友落两行,player_id↔friend_id 各一行,便于 ListFriends)
//	friend_requests  好友请求(PK request_id snowflake,uk requester_id+target_id)
//	blocks           黑名单(uk player_id+blocked_id)
//
// 三张表都是结构化列,直接映射(CLAUDE.md §5.9 关系型表不强制 proto bytes blob)。
// FriendRequestStatus 取值与 proto pandora.friend.v1.FriendRequestStatus 对齐:
// 1=pending / 2=accepted / 3=rejected / 4=expired。
package data

import (
	"context"
	"database/sql"
	"errors"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// 好友请求状态(与 proto FriendRequestStatus 数值一致)。
const (
	requestStatusPending  = 1
	requestStatusAccepted = 2
	requestStatusRejected = 3
)

// FriendRequestRow 是一行好友请求(data → biz 内部结构,不外泄客户端)。
type FriendRequestRow struct {
	RequestID   uint64
	RequesterID uint64
	TargetID    uint64
	Status      int32
}

// FriendRow 是一条好友关系(friend_id + 成为好友时间,供 biz 组装 FriendInfo)。
type FriendRow struct {
	FriendID uint64
	SinceMs  int64
}

// FriendRepo 是 friend 数据层抽象。biz 层只依赖此接口,不依赖 *sql.DB。
type FriendRepo interface {
	// AreFriends 判断 a / b 是否已是好友(查一行即可,双向落库)。
	AreFriends(ctx context.Context, a, b uint64) (bool, error)
	// IsBlocked 判断 a / b 之间是否存在任一方向的拉黑。
	IsBlocked(ctx context.Context, a, b uint64) (bool, error)
	// CountFriends 统计玩家当前好友数(AddFriend 提前失败用,非权威)。
	CountFriends(ctx context.Context, playerID uint64) (int, error)
	// CreateRequest 创建 / 复用好友请求。
	//   - 无历史 → 用 newRequestID 插入 pending,返回 (newRequestID, false)
	//   - 已有 pending → 复用,返回 (已存在 request_id, true)
	//   - 已有 rejected/expired → 重置为 pending,返回 (已存在 request_id, false)
	CreateRequest(ctx context.Context, newRequestID, requesterID, targetID uint64) (requestID uint64, reused bool, err error)
	// GetRequest 读好友请求;not found → (nil, false, nil)。
	GetRequest(ctx context.Context, requestID uint64) (*FriendRequestRow, bool, error)
	// AcceptRequest 在一个事务里完成「接受好友请求」的全部权威校验与写入:
	//   1. 锁请求行(FOR UPDATE),确认仍是 pending;
	//   2. R5 校验:只有请求的 target 本人(accepterID)能接受;
	//   3. block 校验:双方任一方向已拉黑则拒绝;
	//   4. maxFriends > 0 时对 requester / target 双方做好友上限校验;
	//   5. 标记 accepted + 写双向好友边(幂等 INSERT IGNORE)。
	// 返回 accepted 表示本次调用是否真正把 pending→accepted 并建边:
	//   - accepted=true:本次完成,biz 应推送 REQUEST_ACCEPTED;
	//   - accepted=false, err=nil:请求已被并发处理(Block 改 rejected / 另一次 accept),
	//     biz 不得推送、不得报"成功"(避免假成功)。
	AcceptRequest(ctx context.Context, requestID, accepterID uint64, maxFriends int) (accepted bool, err error)
	// ListFriends 列出玩家的好友(friend_id + since_ms)。
	ListFriends(ctx context.Context, playerID uint64) ([]FriendRow, error)
	// Block 在一个事务里:写黑名单 + 删双向好友边 + 取消两人之间的 pending 请求。
	Block(ctx context.Context, playerID, targetID uint64) error
}

// MySQLFriendRepo 是基于 database/sql 的 FriendRepo 实现。
type MySQLFriendRepo struct {
	db *sql.DB
}

// NewMySQLFriendRepo 构造。db 由 pkg/mysqlx.MustNewClient 提供(连 pandora_social 库)。
func NewMySQLFriendRepo(db *sql.DB) *MySQLFriendRepo {
	return &MySQLFriendRepo{db: db}
}

func (r *MySQLFriendRepo) AreFriends(ctx context.Context, a, b uint64) (bool, error) {
	const q = `SELECT 1 FROM friendships WHERE player_id = ? AND friend_id = ? LIMIT 1`
	var x int
	err := r.db.QueryRowContext(ctx, q, a, b).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "query friendship %d-%d: %v", a, b, err)
	}
	return true, nil
}

func (r *MySQLFriendRepo) IsBlocked(ctx context.Context, a, b uint64) (bool, error) {
	const q = `SELECT 1 FROM blocks
WHERE (player_id = ? AND blocked_id = ?) OR (player_id = ? AND blocked_id = ?) LIMIT 1`
	var x int
	err := r.db.QueryRowContext(ctx, q, a, b, b, a).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "query block %d-%d: %v", a, b, err)
	}
	return true, nil
}

func (r *MySQLFriendRepo) CountFriends(ctx context.Context, playerID uint64) (int, error) {
	const q = `SELECT COUNT(*) FROM friendships WHERE player_id = ?`
	var n int
	if err := r.db.QueryRowContext(ctx, q, playerID).Scan(&n); err != nil {
		return 0, errcode.New(errcode.ErrInternal, "count friends %d: %v", playerID, err)
	}
	return n, nil
}

func (r *MySQLFriendRepo) CreateRequest(ctx context.Context, newRequestID, requesterID, targetID uint64) (uint64, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingID uint64
	var status int32
	err = tx.QueryRowContext(ctx,
		`SELECT request_id, status FROM friend_requests
WHERE requester_id = ? AND target_id = ? FOR UPDATE`, requesterID, targetID).Scan(&existingID, &status)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 无历史请求 → 插入新 pending
		if _, ierr := tx.ExecContext(ctx,
			`INSERT INTO friend_requests (request_id, requester_id, target_id, status)
VALUES (?, ?, ?, ?)`, newRequestID, requesterID, targetID, requestStatusPending); ierr != nil {
			return 0, false, errcode.New(errcode.ErrInternal, "insert request %d->%d: %v", requesterID, targetID, ierr)
		}
		if cerr := tx.Commit(); cerr != nil {
			return 0, false, errcode.New(errcode.ErrInternal, "commit: %v", cerr)
		}
		return newRequestID, false, nil

	case err != nil:
		return 0, false, errcode.New(errcode.ErrInternal, "lock request %d->%d: %v", requesterID, targetID, err)

	default:
		// 已有历史请求
		if status == requestStatusPending {
			// 复用现有 pending,不改库
			if cerr := tx.Commit(); cerr != nil {
				return 0, false, errcode.New(errcode.ErrInternal, "commit: %v", cerr)
			}
			return existingID, true, nil
		}
		// rejected/expired/accepted → 重置为 pending 再发起
		if _, uerr := tx.ExecContext(ctx,
			`UPDATE friend_requests SET status = ?, updated_at = NOW() WHERE request_id = ?`,
			requestStatusPending, existingID); uerr != nil {
			return 0, false, errcode.New(errcode.ErrInternal, "reset request %d: %v", existingID, uerr)
		}
		if cerr := tx.Commit(); cerr != nil {
			return 0, false, errcode.New(errcode.ErrInternal, "commit: %v", cerr)
		}
		return existingID, false, nil
	}
}

func (r *MySQLFriendRepo) GetRequest(ctx context.Context, requestID uint64) (*FriendRequestRow, bool, error) {
	const q = `SELECT request_id, requester_id, target_id, status
FROM friend_requests WHERE request_id = ? LIMIT 1`
	row := &FriendRequestRow{}
	err := r.db.QueryRowContext(ctx, q, requestID).Scan(
		&row.RequestID, &row.RequesterID, &row.TargetID, &row.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "query request %d: %v", requestID, err)
	}
	return row, true, nil
}

func (r *MySQLFriendRepo) AcceptRequest(ctx context.Context, requestID, accepterID uint64, maxFriends int) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 锁请求行,确认仍是 pending(防并发重复 accept / Block 在预检后改状态)
	var requesterID, targetID uint64
	var status int32
	err = tx.QueryRowContext(ctx,
		`SELECT requester_id, target_id, status FROM friend_requests
WHERE request_id = ? FOR UPDATE`, requestID).Scan(&requesterID, &targetID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, errcode.New(errcode.ErrFriendNotFound, "request not found: %d", requestID)
	}
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "lock request %d: %v", requestID, err)
	}
	// R5 权威校验:只有请求的 target 本人能接受(放进事务,杜绝 biz 预检后的 TOCTOU)
	if targetID != accepterID {
		return false, errcode.New(errcode.ErrFriendNotFound, "request %d not for %d", requestID, accepterID)
	}
	// 并发下请求已被处理(Block 改 rejected / 另一次 accept 改 accepted)→ 本次未真正完成
	// pending→accepted,返回 accepted=false,由 biz 决定不推送、不报"成功"。
	if status != requestStatusPending {
		return false, nil
	}

	// block 权威校验(放进事务):请求发出后任一方可能拉黑,事务内 SELECT 防 TOCTOU
	var blockedX int
	berr := tx.QueryRowContext(ctx,
		`SELECT 1 FROM blocks
WHERE (player_id = ? AND blocked_id = ?) OR (player_id = ? AND blocked_id = ?) LIMIT 1`,
		accepterID, requesterID, requesterID, accepterID).Scan(&blockedX)
	if berr == nil {
		return false, errcode.New(errcode.ErrFriendBlocked, "blocked between %d and %d", accepterID, requesterID)
	}
	if !errors.Is(berr, sql.ErrNoRows) {
		return false, errcode.New(errcode.ErrInternal, "query block %d-%d: %v", accepterID, requesterID, berr)
	}

	// 好友上限原子校验:在 accept 事务内对双方分别统计已建立的好友边。
	// 请求行 FOR UPDATE 串行化了「同一请求」的并发 accept;统计在锁内进行,
	// 与同一请求的重复 accept 互斥。残留极窄竞态见 AcceptRequest 接口注释。
	if maxFriends > 0 {
		for _, pid := range [...]uint64{requesterID, targetID} {
			var cnt int
			if cerr := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM friendships WHERE player_id = ?`, pid).Scan(&cnt); cerr != nil {
				return false, errcode.New(errcode.ErrInternal, "count friends %d: %v", pid, cerr)
			}
			if cnt >= maxFriends {
				return false, errcode.New(errcode.ErrFriendLimit,
					"friend limit reached for %d (max %d)", pid, maxFriends)
			}
		}
	}

	if _, uerr := tx.ExecContext(ctx,
		`UPDATE friend_requests SET status = ?, updated_at = NOW() WHERE request_id = ?`,
		requestStatusAccepted, requestID); uerr != nil {
		return false, errcode.New(errcode.ErrInternal, "accept request %d: %v", requestID, uerr)
	}

	// 写双向好友边(幂等:重复 accept 不报错)
	const insFriend = `INSERT IGNORE INTO friendships (player_id, friend_id) VALUES (?, ?)`
	if _, ferr := tx.ExecContext(ctx, insFriend, requesterID, targetID); ferr != nil {
		return false, errcode.New(errcode.ErrInternal, "insert friendship %d->%d: %v", requesterID, targetID, ferr)
	}
	if _, ferr := tx.ExecContext(ctx, insFriend, targetID, requesterID); ferr != nil {
		return false, errcode.New(errcode.ErrInternal, "insert friendship %d->%d: %v", targetID, requesterID, ferr)
	}

	if cerr := tx.Commit(); cerr != nil {
		return false, errcode.New(errcode.ErrInternal, "commit request %d: %v", requestID, cerr)
	}
	return true, nil
}

func (r *MySQLFriendRepo) ListFriends(ctx context.Context, playerID uint64) ([]FriendRow, error) {
	const q = `SELECT friend_id, UNIX_TIMESTAMP(created_at)*1000
FROM friendships WHERE player_id = ? ORDER BY created_at DESC`
	rows, err := r.db.QueryContext(ctx, q, playerID)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "query friends player=%d: %v", playerID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []FriendRow
	for rows.Next() {
		var fr FriendRow
		if serr := rows.Scan(&fr.FriendID, &fr.SinceMs); serr != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan friend player=%d: %v", playerID, serr)
		}
		out = append(out, fr)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate friends player=%d: %v", playerID, rerr)
	}
	return out, nil
}

func (r *MySQLFriendRepo) Block(ctx context.Context, playerID, targetID uint64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. 写黑名单(幂等)
	if _, berr := tx.ExecContext(ctx,
		`INSERT IGNORE INTO blocks (player_id, blocked_id) VALUES (?, ?)`, playerID, targetID); berr != nil {
		return errcode.New(errcode.ErrInternal, "insert block %d->%d: %v", playerID, targetID, berr)
	}

	// 2. 删双向好友边
	if _, derr := tx.ExecContext(ctx,
		`DELETE FROM friendships WHERE (player_id = ? AND friend_id = ?) OR (player_id = ? AND friend_id = ?)`,
		playerID, targetID, targetID, playerID); derr != nil {
		return errcode.New(errcode.ErrInternal, "delete friendship %d-%d: %v", playerID, targetID, derr)
	}

	// 3. 取消两人之间的 pending 请求(任一方向)
	if _, rerr := tx.ExecContext(ctx,
		`UPDATE friend_requests SET status = ?, updated_at = NOW()
WHERE status = ? AND ((requester_id = ? AND target_id = ?) OR (requester_id = ? AND target_id = ?))`,
		requestStatusRejected, requestStatusPending, playerID, targetID, targetID, playerID); rerr != nil {
		return errcode.New(errcode.ErrInternal, "cancel pending requests %d-%d: %v", playerID, targetID, rerr)
	}

	if cerr := tx.Commit(); cerr != nil {
		return errcode.New(errcode.ErrInternal, "commit block %d->%d: %v", playerID, targetID, cerr)
	}
	return nil
}
