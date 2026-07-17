// Package data 是 login 服务的"数据层"(repository)。
//
// W3 ②(2026-06-05)真实化:
//   - MySQL: pandora_account.accounts / account_devices / account_bans 三表
//   - Redis: pandora:sess:<player_id>      (hash, TTL 24h)        session 状态
//   - Redis: pandora:ticket:<jti>          (string, TTL 5min)     DSTicket 防重放(SETNX)
package data

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/errcode"
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
	// GetJTI 读当前 session 的 jti(P0 修复 2026-07-15,session 现行性门)。
	// found=false = 无 session(已登出/过期/从未登录)。
	GetJTI(ctx context.Context, playerID uint64) (jti string, found bool, err error)
	// DeleteIfJTI 仅当当前 session 的 jti 匹配时才删除(CAS)。
	// 防止旧设备的迟到 Logout 误删新登录的 session(顶号后新设备被踢)。
	DeleteIfJTI(ctx context.Context, playerID uint64, jti string) (deleted bool, err error)
}

// RedisSessionRepo 基于 go-redis/v9 的 SessionRepo 实现。
type RedisSessionRepo struct {
	rdb redis.UniversalClient
}

// NewRedisSessionRepo 构造。
func NewRedisSessionRepo(rdb redis.UniversalClient) *RedisSessionRepo {
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

func (r *RedisSessionRepo) GetJTI(ctx context.Context, playerID uint64) (string, bool, error) {
	jti, err := r.rdb.HGet(ctx, sessKey(playerID), "jti").Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, errcode.New(errcode.ErrInternal, "redis sess get jti: %v", err)
	}
	return jti, jti != "", nil
}

// deleteIfJTIScript:hash 字段 jti 与参数相等才 DEL(原子 CAS,防迟到 Logout 误删新 session)。
var deleteIfJTIScript = redis.NewScript(`
if redis.call("HGET", KEYS[1], "jti") == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

func (r *RedisSessionRepo) DeleteIfJTI(ctx context.Context, playerID uint64, jti string) (bool, error) {
	n, err := deleteIfJTIScript.Run(ctx, r.rdb, []string{sessKey(playerID)}, jti).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		return false, errcode.New(errcode.ErrInternal, "redis sess del-if-jti: %v", err)
	}
	return n > 0, nil
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

// AdmissionMarkerStatus 区分 marker 不存在、首次创建、同 attempt 已存在与冲突。
type AdmissionMarkerStatus uint8

const (
	AdmissionMarkerMissing AdmissionMarkerStatus = iota
	AdmissionMarkerCreated
	AdmissionMarkerExisting
	AdmissionMarkerConflict
)

// AdmissionTicketJTIRepo 是 Redis authority 在线准入的幂等消费扩展。
// attemptOwner 跨同实例普通 token 轮换稳定；acceptedCredentialHash 永久记录首次接受
// 的完整 active tuple。Peek 与原子 Mark 分开，使 TicketUsecase 能按 missing/existing
// 选择严格首次绑定或稳定身份重认，且绝不先覆盖已有 owner。
type AdmissionTicketJTIRepo interface {
	PeekAdmission(ctx context.Context, jti, attemptOwner string) (AdmissionMarkerStatus, error)
	MarkUsedByAdmission(ctx context.Context, jti, attemptOwner, acceptedCredentialHash string, ttl time.Duration) (AdmissionMarkerStatus, error)
}

// RedisTicketJTIRepo 基于 go-redis/v9 的 TicketJTIRepo 实现。
type RedisTicketJTIRepo struct {
	rdb                   redis.UniversalClient
	admissionReplayWindow time.Duration
}

// NewRedisTicketJTIRepo 构造。
func NewRedisTicketJTIRepo(rdb redis.UniversalClient) *RedisTicketJTIRepo {
	return &RedisTicketJTIRepo{rdb: rdb, admissionReplayWindow: 30 * time.Second}
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

var peekTicketAdmissionScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if not current then return 0 end
local version, attempt, credential, accepted_at, replay_until = string.match(
  current, '^([^|]+)|([^|]+)|([^|]+)|([^|]+)|([^|]+)$')
if version ~= 'admission-v4' or string.len(attempt or '') ~= 64 or not string.match(attempt, '^[0-9a-f]+$') or
   string.len(credential or '') ~= 64 or not string.match(credential, '^[0-9a-f]+$') or
   not tonumber(accepted_at) or not tonumber(replay_until) or tonumber(replay_until) < tonumber(accepted_at) then
  return 3
end
local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
if attempt == ARGV[1] and now_ms <= tonumber(replay_until) then
  return 2
end
return 3
`)

var markTicketAdmissionScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
if not current then
  local replay_until = now_ms + tonumber(ARGV[4])
  local marker = 'admission-v4|' .. ARGV[1] .. '|' .. ARGV[2] .. '|' .. now_ms .. '|' .. replay_until
  redis.call('PSETEX', KEYS[1], ARGV[3], marker)
  return 1
end
local version, attempt, credential, accepted_at, replay_until = string.match(
  current, '^([^|]+)|([^|]+)|([^|]+)|([^|]+)|([^|]+)$')
if version == 'admission-v4' and string.len(attempt or '') == 64 and string.match(attempt, '^[0-9a-f]+$') and
   string.len(credential or '') == 64 and string.match(credential, '^[0-9a-f]+$') and
   tonumber(accepted_at) and tonumber(replay_until) and tonumber(replay_until) >= tonumber(accepted_at) and
   attempt == ARGV[1] and now_ms <= tonumber(replay_until) then
  return 2
end
return 0
`)

const admissionMarkerVersion = "admission-v4"

func validAdmissionDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// PeekAdmission 只读现有 marker。legacy value="1"、坏格式与不同 attempt 一律返回
// Conflict（安全 replay），Redis 故障返回 Unavailable。
func (r *RedisTicketJTIRepo) PeekAdmission(ctx context.Context, jti, attemptOwner string) (AdmissionMarkerStatus, error) {
	if jti == "" || !validAdmissionDigest(attemptOwner) {
		return AdmissionMarkerConflict, errcode.New(errcode.ErrInvalidArg, "invalid admission marker lookup")
	}
	if r == nil || r.rdb == nil {
		return AdmissionMarkerConflict, errcode.New(errcode.ErrUnavailable, "ticket admission replay authority unavailable")
	}
	result, err := peekTicketAdmissionScript.Run(ctx, r.rdb, []string{ticketKey(jti)}, attemptOwner).Int64()
	if err != nil {
		return AdmissionMarkerConflict, errcode.NewCause(errcode.ErrUnavailable, err, "redis ticket admission lookup failed")
	}
	switch result {
	case 0:
		return AdmissionMarkerMissing, nil
	case 2:
		return AdmissionMarkerExisting, nil
	default:
		return AdmissionMarkerConflict, nil
	}
}

// MarkUsedByAdmission 在单条 Redis Lua 中完成 absent→versioned marker；同 attempt_owner
// 只确认已存在，绝不覆盖首次 accepted_credential_hash。不同 owner、legacy "1"、坏格式均冲突。
func (r *RedisTicketJTIRepo) MarkUsedByAdmission(
	ctx context.Context,
	jti, attemptOwner, acceptedCredentialHash string,
	ttl time.Duration,
) (AdmissionMarkerStatus, error) {
	if jti == "" || ttl <= 0 || !validAdmissionDigest(attemptOwner) || !validAdmissionDigest(acceptedCredentialHash) {
		return AdmissionMarkerConflict, errcode.New(errcode.ErrInvalidArg, "invalid admission ticket marker")
	}
	if r == nil || r.rdb == nil {
		return AdmissionMarkerConflict, errcode.New(errcode.ErrUnavailable, "ticket admission replay authority unavailable")
	}
	replayWindow := r.admissionReplayWindow
	if replayWindow <= 0 {
		replayWindow = 30 * time.Second
	}
	result, err := markTicketAdmissionScript.Run(ctx, r.rdb, []string{ticketKey(jti)},
		attemptOwner, acceptedCredentialHash, ttl.Milliseconds(), replayWindow.Milliseconds()).Int64()
	if err != nil {
		return AdmissionMarkerConflict, errcode.NewCause(errcode.ErrUnavailable, err, "redis ticket admission mark failed")
	}
	switch result {
	case 1:
		return AdmissionMarkerCreated, nil
	case 2:
		return AdmissionMarkerExisting, nil
	default:
		return AdmissionMarkerConflict, errcode.New(errcode.ErrLoginTicketReplayed, "ticket jti=%s already used by another admission", jti)
	}
}
