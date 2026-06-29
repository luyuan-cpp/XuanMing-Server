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
	"math/rand"
	"strings"

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

// IncomingRequestRow 是一条「发给本人且仍 pending」的好友请求(供 biz 组装 FriendRequestInfo)。
type IncomingRequestRow struct {
	RequestID   uint64
	RequesterID uint64
	CreatedMs   int64
}

// BlockRow 是一条黑名单条目(被拉黑玩家 + 拉黑时间,供 biz 组装 BlockInfo)。
type BlockRow struct {
	BlockedID uint64
	SinceMs   int64
}

// RecommendRow 是一条推荐好友候选(供 biz 组装 RecommendedFriendInfo)。
// Mutual 是与查询者的共同好友数(FOF 候选 >0;随机兜底候选为 0)。
type RecommendRow struct {
	CandidateID uint64
	Mutual      int
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
	// RejectRequest 在事务里拒绝好友请求:锁请求行(FOR UPDATE)→ 校验 target 本人 →
	// 确认仍 pending → 置 rejected。返回 rejected 表示本次是否真正把 pending→rejected:
	//   - rejected=true:本次完成;
	//   - rejected=false, err=nil:请求已被并发处理(已 accept / Block 改 rejected),biz 报找不到。
	RejectRequest(ctx context.Context, requestID, rejecterID uint64) (rejected bool, err error)
	// ListIncomingRequests 列出「发给 playerID 且仍 pending」的好友请求(离线补拉用)。
	ListIncomingRequests(ctx context.Context, playerID uint64) ([]IncomingRequestRow, error)
	// ListFriends 列出玩家的好友(friend_id + since_ms)。
	ListFriends(ctx context.Context, playerID uint64) ([]FriendRow, error)
	// RemoveFriend 删双向好友边(幂等:不存在也不报错)。不动黑名单 / 请求。
	RemoveFriend(ctx context.Context, playerID, targetID uint64) error
	// Block 在一个事务里:写黑名单 + 删双向好友边 + 取消两人之间的 pending 请求。
	Block(ctx context.Context, playerID, targetID uint64) error
	// Unblock 从黑名单移除(幂等:不存在也不报错)。不自动恢复好友关系。
	Unblock(ctx context.Context, playerID, targetID uint64) error
	// ListBlocks 列出玩家拉黑的人(blocked_id + since_ms)。
	ListBlocks(ctx context.Context, playerID uint64) ([]BlockRow, error)
	// RecommendByMutual 返回「好友的好友」候选,按共同好友数降序、同数随机,取 limit 个。
	// 已排除:自己 / 已是好友 / 任一方向拉黑 / 双方任一方向 pending 请求 / exclude 列表。
	RecommendByMutual(ctx context.Context, playerID uint64, exclude []uint64, limit int) ([]RecommendRow, error)
	// RecommendRandom 从好友图里的玩家随机兜底,返回至多 limit 个候选;尾部不足时可少于 limit。
	// 排除条件同上。
	RecommendRandom(ctx context.Context, playerID uint64, exclude []uint64, limit int) ([]RecommendRow, error)
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

func (r *MySQLFriendRepo) RejectRequest(ctx context.Context, requestID, rejecterID uint64) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 锁请求行,确认仍是 pending(防并发:accept / 另一次 reject / Block 改状态)
	var targetID uint64
	var status int32
	err = tx.QueryRowContext(ctx,
		`SELECT target_id, status FROM friend_requests
WHERE request_id = ? FOR UPDATE`, requestID).Scan(&targetID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, errcode.New(errcode.ErrFriendNotFound, "request not found: %d", requestID)
	}
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "lock request %d: %v", requestID, err)
	}
	// R5 权威校验:只有请求的 target 本人能拒绝(放进事务,杜绝 TOCTOU)
	if targetID != rejecterID {
		return false, errcode.New(errcode.ErrFriendNotFound, "request %d not for %d", requestID, rejecterID)
	}
	// 并发下请求已被处理(accept / Block)→ 本次未真正完成 pending→rejected
	if status != requestStatusPending {
		return false, nil
	}

	if _, uerr := tx.ExecContext(ctx,
		`UPDATE friend_requests SET status = ?, updated_at = NOW() WHERE request_id = ?`,
		requestStatusRejected, requestID); uerr != nil {
		return false, errcode.New(errcode.ErrInternal, "reject request %d: %v", requestID, uerr)
	}
	if cerr := tx.Commit(); cerr != nil {
		return false, errcode.New(errcode.ErrInternal, "commit reject %d: %v", requestID, cerr)
	}
	return true, nil
}

func (r *MySQLFriendRepo) ListIncomingRequests(ctx context.Context, playerID uint64) ([]IncomingRequestRow, error) {
	const q = `SELECT request_id, requester_id, UNIX_TIMESTAMP(created_at)*1000
FROM friend_requests WHERE target_id = ? AND status = ? ORDER BY created_at DESC`
	rows, err := r.db.QueryContext(ctx, q, playerID, requestStatusPending)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "query incoming requests player=%d: %v", playerID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []IncomingRequestRow
	for rows.Next() {
		var rr IncomingRequestRow
		if serr := rows.Scan(&rr.RequestID, &rr.RequesterID, &rr.CreatedMs); serr != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan incoming request player=%d: %v", playerID, serr)
		}
		out = append(out, rr)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate incoming requests player=%d: %v", playerID, rerr)
	}
	return out, nil
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

func (r *MySQLFriendRepo) RemoveFriend(ctx context.Context, playerID, targetID uint64) error {
	// 删双向好友边(幂等:删不到行不报错)。单条 DELETE 覆盖两个方向,天然原子。
	if _, derr := r.db.ExecContext(ctx,
		`DELETE FROM friendships WHERE (player_id = ? AND friend_id = ?) OR (player_id = ? AND friend_id = ?)`,
		playerID, targetID, targetID, playerID); derr != nil {
		return errcode.New(errcode.ErrInternal, "remove friendship %d-%d: %v", playerID, targetID, derr)
	}
	return nil
}

func (r *MySQLFriendRepo) Unblock(ctx context.Context, playerID, targetID uint64) error {
	// 从黑名单移除(幂等:删不到行不报错)。不自动恢复好友关系。
	if _, derr := r.db.ExecContext(ctx,
		`DELETE FROM blocks WHERE player_id = ? AND blocked_id = ?`, playerID, targetID); derr != nil {
		return errcode.New(errcode.ErrInternal, "unblock %d->%d: %v", playerID, targetID, derr)
	}
	return nil
}

func (r *MySQLFriendRepo) ListBlocks(ctx context.Context, playerID uint64) ([]BlockRow, error) {
	const q = `SELECT blocked_id, UNIX_TIMESTAMP(created_at)*1000
FROM blocks WHERE player_id = ? ORDER BY created_at DESC`
	rows, err := r.db.QueryContext(ctx, q, playerID)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "query blocks player=%d: %v", playerID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []BlockRow
	for rows.Next() {
		var br BlockRow
		if serr := rows.Scan(&br.BlockedID, &br.SinceMs); serr != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan block player=%d: %v", playerID, serr)
		}
		out = append(out, br)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate blocks player=%d: %v", playerID, rerr)
	}
	return out, nil
}

// excludeClause 把 exclude 列表拼成 ` AND <col> NOT IN (?,?,...)`,并返回对应参数。
// exclude 为空时返回空字符串 + nil,调用方原样拼到 SQL 即可。
func excludeClause(col string, exclude []uint64) (string, []any) {
	if len(exclude) == 0 {
		return "", nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(exclude)), ",")
	args := make([]any, 0, len(exclude))
	for _, id := range exclude {
		args = append(args, id)
	}
	return " AND " + col + " NOT IN (" + ph + ")", args
}

func (r *MySQLFriendRepo) RecommendByMutual(ctx context.Context, playerID uint64, exclude []uint64, limit int) ([]RecommendRow, error) {
	// 好友的好友(FOF):f1 是我的好友,f2.friend_id 是我好友的好友。
	// 排除:我自己 / 已是我好友 / 双向拉黑 / 双向 pending 请求 / exclude 列表。
	// 按共同好友数降序、同数随机,取 limit 个。
	exCl, exArgs := excludeClause("f2.friend_id", exclude)
	q := `SELECT f2.friend_id, COUNT(*) AS mutual
FROM friendships f1
JOIN friendships f2 ON f1.friend_id = f2.player_id
WHERE f1.player_id = ?
  AND f2.friend_id <> ?
  AND f2.friend_id NOT IN (SELECT friend_id FROM friendships WHERE player_id = ?)
  AND NOT EXISTS (SELECT 1 FROM blocks b
        WHERE (b.player_id = ? AND b.blocked_id = f2.friend_id)
           OR (b.player_id = f2.friend_id AND b.blocked_id = ?))
  AND NOT EXISTS (SELECT 1 FROM friend_requests r
        WHERE r.status = 1
          AND ((r.requester_id = ? AND r.target_id = f2.friend_id)
            OR (r.requester_id = f2.friend_id AND r.target_id = ?)))` + exCl + `
GROUP BY f2.friend_id
ORDER BY mutual DESC, RAND()
LIMIT ?`
	args := []any{playerID, playerID, playerID, playerID, playerID, playerID, playerID}
	args = append(args, exArgs...)
	args = append(args, limit)
	return r.scanRecommend(ctx, "fof", playerID, q, args)
}

func (r *MySQLFriendRepo) RecommendRandom(ctx context.Context, playerID uint64, exclude []uint64, limit int) ([]RecommendRow, error) {
	// 兜底:FOF 不足时尽量补候选。绝不全表扫,且 pivot 必须落进真实 id 区间。
	// player_id 是雪花 ID(集中在当前高位窗口),直接 rand.Int63() 大概率落在现有最大 id 之后;
	// 先用索引 MIN/MAX(MySQL 直接取 idx_player 两端,O(1))取真实区间,
	// pivot ∈ [min,max] 保证 player_id>=pivot 必有行;尾部不满返回偏少可接受。
	var minID, maxID sql.Null[uint64]
	err := r.db.QueryRowContext(ctx,
		`SELECT MIN(player_id), MAX(player_id) FROM friendships`).Scan(&minID, &maxID)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "recommend range player=%d: %v", playerID, err)
	}
	if !maxID.Valid || maxID.V == 0 {
		return nil, nil // 空表,无兜底候选
	}
	lo, hi := minID.V, maxID.V
	pivot := lo
	if hi > lo {
		pivot = lo + uint64(rand.Int63n(int64(hi-lo+1)))
	}
	return r.recommendAnchor(ctx, playerID, exclude, pivot, limit)
}

// recommendAnchor 沿 player_id 索引从 pivot 正向扫,LIMIT limit 命中即止(有界索引区间,绝不全表)。
// 排除:自己 / 已是好友 / 双向拉黑 / 双向 pending / exclude;mutual 恒 0。
func (r *MySQLFriendRepo) recommendAnchor(ctx context.Context, playerID uint64, exclude []uint64, pivot uint64, limit int) ([]RecommendRow, error) {
	exCl, exArgs := excludeClause("player_id", exclude)
	q := `SELECT player_id, 0 AS mutual
FROM friendships
WHERE player_id >= ?
  AND player_id <> ?
  AND player_id NOT IN (SELECT friend_id FROM friendships WHERE player_id = ?)
  AND NOT EXISTS (SELECT 1 FROM blocks b
        WHERE (b.player_id = ? AND b.blocked_id = friendships.player_id)
           OR (b.player_id = friendships.player_id AND b.blocked_id = ?))
  AND NOT EXISTS (SELECT 1 FROM friend_requests r
        WHERE r.status = 1
          AND ((r.requester_id = ? AND r.target_id = friendships.player_id)
            OR (r.requester_id = friendships.player_id AND r.target_id = ?)))` + exCl + `
GROUP BY player_id
ORDER BY player_id
LIMIT ?`
	args := []any{pivot, playerID, playerID, playerID, playerID, playerID, playerID}
	args = append(args, exArgs...)
	args = append(args, limit)
	return r.scanRecommend(ctx, "random", playerID, q, args)
}

func (r *MySQLFriendRepo) scanRecommend(ctx context.Context, kind string, playerID uint64, q string, args []any) ([]RecommendRow, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "recommend %s player=%d: %v", kind, playerID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []RecommendRow
	for rows.Next() {
		var rr RecommendRow
		if serr := rows.Scan(&rr.CandidateID, &rr.Mutual); serr != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan recommend %s player=%d: %v", kind, playerID, serr)
		}
		out = append(out, rr)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate recommend %s player=%d: %v", kind, playerID, rerr)
	}
	return out, nil
}
