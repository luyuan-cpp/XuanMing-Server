// Package data 是 login 服务的"数据层"(repository)。
//
// W3 ②(2026-06-05)真实化:
//   - MySQL: pandora_account.accounts / account_devices / account_bans 三表
//   - Redis: pandora:sess:<player_id>      (hash, TTL 24h)        session 状态
//   - Redis: pandora:ticket:<jti>          (string, TTL 5min)     DSTicket 防重放(SETNX)
//
// W2 MockAccountRepo 留作旁路:cfg.Node.MySQLClient.DSN 为空时 fallback,
// 便于不带 mysql 跑 push 联调。
package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
)

// AccountRepo 是账号数据访问接口。biz 层依赖本接口,而不是具体实现,
// 方便在 mock / mysql 实现之间切换不动 biz/service。
type AccountRepo interface {
	// FindByAccount 根据账号名查 player_id + bcrypt 哈希后的密码。
	// 找不到返回 ErrLoginAccountNotFound。
	FindByAccount(ctx context.Context, account string) (playerID uint64, passwordHash string, err error)

	// CreateAccount 新建账号(snowflake 分配的 playerID 传入)。
	// 账号已存在返回 ErrAlreadyExists。
	CreateAccount(ctx context.Context, playerID uint64, account, bcryptHash string) error

	// CheckBanned 检查账号 / 设备是否在有效封禁期内(account_bans 表 expires_at>now 或 NULL)。
	CheckBanned(ctx context.Context, playerID uint64, deviceID string) (banned bool, err error)

	// TouchDevice 记录最近一次登录设备(account_devices upsert)。失败由 biz 层只记日志。
	TouchDevice(ctx context.Context, playerID uint64, deviceID string) error
}

// =====================================================================
// MockAccountRepo:W2 mock 实现(yaml 没配 mysql DSN 时 fallback)。
// =====================================================================

// MockAccountRepo 固定账号 + 固定 bcrypt 哈希,不接 mysql。
type MockAccountRepo struct {
	Account      string
	PasswordHash string
	PlayerID     uint64
}

// NewMockAccountRepo 构造 mock。bcryptHash 必须是 pkg/passwd.Hash 结果(60 字节)。
//
// W3 ②:为了跟真实 MySQL 实现行为一致,main 启动时调一次 passwd.Hash(MockPasswordHash) 转换。
func NewMockAccountRepo(account, bcryptHash string, playerID uint64) *MockAccountRepo {
	return &MockAccountRepo{
		Account:      account,
		PasswordHash: bcryptHash,
		PlayerID:     playerID,
	}
}

func (m *MockAccountRepo) FindByAccount(_ context.Context, account string) (uint64, string, error) {
	if account != m.Account {
		return 0, "", errcode.New(errcode.ErrLoginAccountNotFound, "account=%s not found", account)
	}
	return m.PlayerID, m.PasswordHash, nil
}

func (m *MockAccountRepo) CreateAccount(_ context.Context, _ uint64, _, _ string) error {
	return errcode.New(errcode.ErrAlreadyExists, "mock repo does not support create")
}

func (m *MockAccountRepo) CheckBanned(_ context.Context, _ uint64, _ string) (bool, error) {
	return false, nil
}

func (m *MockAccountRepo) TouchDevice(_ context.Context, _ uint64, _ string) error {
	return nil
}

// =====================================================================
// MySQLAccountRepo:W3 ② 真实实现。
// =====================================================================

// MySQLAccountRepo 基于 *sql.DB 的账号仓储。
type MySQLAccountRepo struct {
	db *sql.DB
}

// NewMySQLAccountRepo 构造。db 由 pkg/mysqlx.MustNewClient 提供。
func NewMySQLAccountRepo(db *sql.DB) *MySQLAccountRepo {
	return &MySQLAccountRepo{db: db}
}

func (r *MySQLAccountRepo) FindByAccount(ctx context.Context, account string) (uint64, string, error) {
	const q = `SELECT player_id, password_hash FROM accounts WHERE account = ? LIMIT 1`
	var (
		playerID uint64
		hash     string
	)
	err := r.db.QueryRowContext(ctx, q, account).Scan(&playerID, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", errcode.New(errcode.ErrLoginAccountNotFound, "account=%s not found", account)
	}
	if err != nil {
		return 0, "", errcode.New(errcode.ErrInternal, "mysql find account: %v", err)
	}
	return playerID, hash, nil
}

func (r *MySQLAccountRepo) CreateAccount(ctx context.Context, playerID uint64, account, bcryptHash string) error {
	const q = `INSERT INTO accounts(player_id, account, password_hash) VALUES (?, ?, ?)`
	_, err := r.db.ExecContext(ctx, q, playerID, account, bcryptHash)
	if err != nil {
		// 1062 = ER_DUP_ENTRY,字符串匹配避免强依赖 mysql driver 错误类型
		if isDupErr(err) {
			return errcode.New(errcode.ErrAlreadyExists, "account=%s already exists", account)
		}
		return errcode.New(errcode.ErrInternal, "mysql create account: %v", err)
	}
	return nil
}

func (r *MySQLAccountRepo) CheckBanned(ctx context.Context, playerID uint64, deviceID string) (bool, error) {
	const q = `SELECT COUNT(*) FROM account_bans
WHERE (expires_at IS NULL OR expires_at > UTC_TIMESTAMP())
  AND ((player_id IS NOT NULL AND player_id = ?) OR (device_id IS NOT NULL AND device_id = ?))`
	var cnt int
	if err := r.db.QueryRowContext(ctx, q, playerID, deviceID).Scan(&cnt); err != nil {
		return false, errcode.New(errcode.ErrInternal, "mysql check banned: %v", err)
	}
	return cnt > 0, nil
}

func (r *MySQLAccountRepo) TouchDevice(ctx context.Context, playerID uint64, deviceID string) error {
	if deviceID == "" {
		return nil
	}
	const q = `INSERT INTO account_devices(player_id, device_id, last_login_at)
VALUES (?, ?, UTC_TIMESTAMP())
ON DUPLICATE KEY UPDATE last_login_at = UTC_TIMESTAMP()`
	if _, err := r.db.ExecContext(ctx, q, playerID, deviceID); err != nil {
		return errcode.New(errcode.ErrInternal, "mysql touch device: %v", err)
	}
	return nil
}

// isDupErr 粗略判断 MySQL 唯一键冲突,不依赖 mysql driver 强类型。
func isDupErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "1062") || strings.Contains(s, "Duplicate entry")
}

// =====================================================================
// SessionRepo:Redis 上的玩家 session 状态。
// =====================================================================

// SessionRepo 维护 pandora:sess:<player_id> hash + TTL。
//
// hash 字段:
//
//	token      string  当前签的 session JWT(全文,debug 用)
//	jti        string  session JWT 的 jti(便于将来 jti 黑名单)
//	device_id  string  当前设备
//	exp_ms     int64   session 过期 unix ms
type SessionRepo interface {
	Set(ctx context.Context, playerID uint64, token, jti, deviceID string, ttl time.Duration) error
	Delete(ctx context.Context, playerID uint64) error
}

// RedisSessionRepo 基于 go-redis/v9 的 SessionRepo 实现。
type RedisSessionRepo struct {
	rdb *redis.Client
}

// NewRedisSessionRepo 构造。
func NewRedisSessionRepo(rdb *redis.Client) *RedisSessionRepo {
	return &RedisSessionRepo{rdb: rdb}
}

func sessKey(playerID uint64) string {
	return fmt.Sprintf("pandora:sess:%d", playerID)
}

func (r *RedisSessionRepo) Set(ctx context.Context, playerID uint64, token, jti, deviceID string, ttl time.Duration) error {
	key := sessKey(playerID)
	pipe := r.rdb.TxPipeline()
	pipe.HSet(ctx, key,
		"token", token,
		"jti", jti,
		"device_id", deviceID,
		"exp_ms", time.Now().Add(ttl).UnixMilli(),
	)
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return errcode.New(errcode.ErrInternal, "redis sess set: %v", err)
	}
	return nil
}

func (r *RedisSessionRepo) Delete(ctx context.Context, playerID uint64) error {
	if err := r.rdb.Del(ctx, sessKey(playerID)).Err(); err != nil && !errors.Is(err, redis.Nil) {
		return errcode.New(errcode.ErrInternal, "redis sess del: %v", err)
	}
	return nil
}

// =====================================================================
// TicketJTIRepo:DSTicket 防重放(Verify 时 SETNX)。
// =====================================================================

// TicketJTIRepo 维护 pandora:ticket:<jti> 短期标记。
//
// 语义:首次 Verify 时 SETNX 成功 → 票据可用;再次 SETNX 失败 → ErrLoginTicketReplayed。
type TicketJTIRepo interface {
	MarkUsed(ctx context.Context, jti string, ttl time.Duration) error
}

// RedisTicketJTIRepo 基于 go-redis/v9 的 TicketJTIRepo 实现。
type RedisTicketJTIRepo struct {
	rdb *redis.Client
}

// NewRedisTicketJTIRepo 构造。
func NewRedisTicketJTIRepo(rdb *redis.Client) *RedisTicketJTIRepo {
	return &RedisTicketJTIRepo{rdb: rdb}
}

func ticketKey(jti string) string {
	return fmt.Sprintf("pandora:ticket:%s", jti)
}

func (r *RedisTicketJTIRepo) MarkUsed(ctx context.Context, jti string, ttl time.Duration) error {
	if jti == "" {
		return errcode.New(errcode.ErrInvalidArg, "empty jti")
	}
	ok, err := r.rdb.SetNX(ctx, ticketKey(jti), 1, ttl).Result()
	if err != nil {
		return errcode.New(errcode.ErrInternal, "redis ticket setnx: %v", err)
	}
	if !ok {
		return errcode.New(errcode.ErrLoginTicketReplayed, "ticket jti=%s already used", jti)
	}
	return nil
}

// =====================================================================
// SeedAccount:开发期 mock_account 自动注册(避免每次手动 INSERT)。
// =====================================================================

// SeedAccount 在 accounts 表里查/建一条种子账号。
//
// bcryptHash 必须由调用方用 pkg/passwd.Hash 算好(避免在不同 cost 下产出不同哈希)。
// 返回 (playerID, created, err):
//   - 已存在:created=false,playerID=表中现存的
//   - 新建:created=true,playerID=传入的 fallbackPlayerID
func SeedAccount(ctx context.Context, db *sql.DB, account, bcryptHash string, fallbackPlayerID uint64) (uint64, bool, error) {
	repo := &MySQLAccountRepo{db: db}

	// 1. 先查
	id, _, e := repo.FindByAccount(ctx, account)
	if e == nil {
		return id, false, nil
	}
	var ce *errcode.Error
	if !errors.As(e, &ce) || ce.Code != errcode.ErrLoginAccountNotFound {
		return 0, false, e
	}

	// 2. 不存在则建
	if err := repo.CreateAccount(ctx, fallbackPlayerID, account, bcryptHash); err != nil {
		// 并发种了 → 回查
		var ce2 *errcode.Error
		if errors.As(err, &ce2) && ce2.Code == errcode.ErrAlreadyExists {
			if id2, _, e2 := repo.FindByAccount(ctx, account); e2 == nil {
				return id2, false, nil
			}
		}
		return 0, false, err
	}
	plog.With(ctx).Infow("msg", "seed_account_created", "account", account, "player_id", fallbackPlayerID)
	return fallbackPlayerID, true, nil
}
