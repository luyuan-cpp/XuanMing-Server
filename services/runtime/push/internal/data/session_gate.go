// session_gate.go — 会话现行性权威的只读视图(push.Subscribe P0 修复 2026-07-22,
// INC-20260722-004)。
//
// 权威本体由 login 维护:Redis `pandora:sess:<player_id>` hash(字段 jti = 当前会话
// 代际,顶号/重登轮换,Logout compare-delete;见 login internal/data/account.go,
// key 格式是跨服务契约,两侧同步改)。push 只读:JWT 验签只证明"曾经登录过",
// 不证明"未被顶号"——旧 token 在 exp 前仍能过 Envoy jwt_authn,必须再过现行性门
// (CLAUDE.md §9.23 会话 fencing)。
package data

import (
	"context"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// SessionGate 查询玩家当前会话代际(jti)。
type SessionGate interface {
	// CurrentJTI 返回当前会话 jti;found=false = 无会话(已登出/过期)。
	// err 非 nil = 权威不可达,调用方必须 fail-closed(拒建流/关流重试),
	// 禁止按"无会话"或"仍现行"猜测(§9.22)。
	CurrentJTI(ctx context.Context, playerID uint64) (jti string, found bool, err error)
}

// RedisSessionGate 是基于共享 Redis 的 SessionGate 实现。
type RedisSessionGate struct {
	rdb redis.UniversalClient
}

// NewRedisSessionGate 构造(rdb 与 login 指向同一 Redis 实例,infra 单实例部署契约)。
func NewRedisSessionGate(rdb redis.UniversalClient) *RedisSessionGate {
	return &RedisSessionGate{rdb: rdb}
}

func sessionKey(playerID uint64) string {
	return fmt.Sprintf("pandora:sess:%d", playerID)
}

// CurrentJTI 实现 SessionGate。
func (g *RedisSessionGate) CurrentJTI(ctx context.Context, playerID uint64) (string, bool, error) {
	jti, err := g.rdb.HGet(ctx, sessionKey(playerID), "jti").Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, errcode.New(errcode.ErrUnavailable, "session authority unavailable: %v", err)
	}
	return jti, jti != "", nil
}
