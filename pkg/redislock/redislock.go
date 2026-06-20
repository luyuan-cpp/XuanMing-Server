// Package redislock 提供基于 Redis 的分布式锁(SetNX + Lua 校验解锁)。
//
// 来自 mmorpg/go/db/internal/locker/。改动:
//   - package locker → redislock
//   - 默认 prefix 由 "distributed:lock:" 改为 "pandora:lock:"
//     (对齐 docs/design/infra.md §3.2 命名规范)
//
// 使用约束:
//   - lock TTL ≤ 30s(infra.md §3.3),业务必须主动释放,不能依赖 TTL。
//   - 用 UUID 作为 lockValue,Release / Extend 用 Lua 脚本校验 owner,
//     防止误释放别人的锁。
package redislock

import (
	"context"
	"fmt"
	"time"

	klog "github.com/go-kratos/kratos/v2/log"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// 默认 key 前缀,跟 docs/design/infra.md §3.2 命名规范保持一致。
const DefaultPrefix = "pandora:lock:"

// RedisLocker 是基于 Redis 的分布式锁实现。
type RedisLocker struct {
	redisClient redis.Cmdable
	prefix      string
}

// TryLockResult 是单次 TryLock 返回的句柄,用于后续 Release / Extend。
type TryLockResult struct {
	locked    bool
	lockKey   string
	lockValue string // 唯一 owner 标识,防止误释放
	redis     redis.Cmdable
}

// NewRedisLocker 创建 RedisLocker。如果不传 prefix 用 DefaultPrefix。
func NewRedisLocker(redisClient redis.Cmdable, prefix ...string) *RedisLocker {
	lockPrefix := DefaultPrefix
	if len(prefix) > 0 && prefix[0] != "" {
		lockPrefix = prefix[0]
	}
	return &RedisLocker{
		redisClient: redisClient,
		prefix:      lockPrefix,
	}
}

// TryLock 非阻塞尝试加锁。失败返回 locked=false,err 是 redis 错误。
func (rl *RedisLocker) TryLock(ctx context.Context, key string, ttl time.Duration) (*TryLockResult, error) {
	finalLockKey := rl.prefix + key
	lockValue := uuid.NewString()

	setCmd := rl.redisClient.SetNX(ctx, finalLockKey, lockValue, ttl)
	success, err := setCmd.Result()
	if err != nil {
		klog.Errorf("Redis TryLock failed | key=%s | err=%v", finalLockKey, err)
		return &TryLockResult{locked: false}, fmt.Errorf("redis setnx failed: %w", err)
	}

	return &TryLockResult{
		locked:    success,
		lockKey:   finalLockKey,
		lockValue: lockValue,
		redis:     rl.redisClient,
	}, nil
}

// IsLocked 返回是否成功持锁。
func (tlr *TryLockResult) IsLocked() bool { return tlr.locked }

// Release 释放锁,Lua 校验 owner。
// 返回 (释放成功?, 错误)。锁已过期或被别人接管 → (false, nil)。
func (tlr *TryLockResult) Release(ctx context.Context) (bool, error) {
	if !tlr.locked {
		// 未持锁时调 Release 是调用方正常幂等路径,不是错误
		klog.Debugf("Release lock skipped: not holding lock | key=%s", tlr.lockKey)
		return false, nil
	}

	luaScript := `
		local currentValue = redis.call('GET', KEYS[1])
		if currentValue == ARGV[1] then
			return redis.call('DEL', KEYS[1])
		else
			return 0
		end
	`
	delResult, err := tlr.redis.
		Eval(ctx, luaScript, []string{tlr.lockKey}, tlr.lockValue).
		Int64()
	if err != nil {
		klog.Errorf("Release lock Redis error | key=%s | err=%v", tlr.lockKey, err)
		return false, fmt.Errorf("redis eval release script failed: %w", err)
	}

	if delResult == 1 {
		klog.Debugf("Release lock success | key=%s", tlr.lockKey)
		tlr.locked = false
		return true, nil
	}
	// 已过期或被接管(TTL 到期 / 被别人重新加锁)是预期内分支,不是错误
	klog.Debugf("Release lock no-op: lock expired or not owned | key=%s", tlr.lockKey)
	tlr.locked = false
	return false, nil
}

// Extend 续锁,Lua 校验 owner。锁已过期或被接管 → (false, nil)。
func (tlr *TryLockResult) Extend(ctx context.Context, extendTTL time.Duration) (bool, error) {
	if !tlr.locked {
		// 未持锁时调 Extend 是调用方正常幂等路径,不是错误
		klog.Debugf("Extend lock skipped: not holding lock | key=%s", tlr.lockKey)
		return false, nil
	}

	luaScript := `
		local currentValue = redis.call('GET', KEYS[1])
		if currentValue == ARGV[1] then
			return redis.call('EXPIRE', KEYS[1], ARGV[2])
		else
			return 0
		end
	`
	expireResult, err := tlr.redis.
		Eval(ctx, luaScript, []string{tlr.lockKey}, tlr.lockValue, int(extendTTL.Seconds())).
		Int64()
	if err != nil {
		klog.Errorf("Extend lock Redis error | key=%s | err=%v", tlr.lockKey, err)
		return false, fmt.Errorf("redis eval extend script failed: %w", err)
	}

	return expireResult == 1, nil
}
