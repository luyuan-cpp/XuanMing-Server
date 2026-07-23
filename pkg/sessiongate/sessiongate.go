// Package sessiongate 提供玩家会话现行性权威(login 维护的 Redis `pandora:sess:<pid>`)
// 的共享只读视图(R5 复审 P0-1,INC-20260722-004,2026-07-22)。
//
// 背景:Envoy jwt_authn 只验签名与 exp,JWT 验签只证明"曾经登录过",不证明"未被顶号"。
// 顶号后旧 token 在 exp 前(默认 24h)仍能过网关,若业务服务只信 x-pandora-player-id,
// 旧设备就保留了全部按 player_id 定向的能力(好友申请/交易/背包……)。事故档案要求
// "旧 session 不得再获得按 player_id 定向能力",因此**所有客户端面服务**都必须对携带
// 会话证据(x-pandora-jwt-payload)的请求做现行性判定,而不是只在 login/push 入口。
//
// 本包由 push internal/data/session_gate.go 提升而来(该处保留 SessionGate 接口作为
// biz 端口,实现统一指到这里),供 pkg/middleware.SessionCurrent 与各服务装配复用。
//
// 权威本体由 login 维护:Redis `pandora:sess:<player_id>` hash(字段 jti = 当前会话
// 代际,顶号/重登轮换,Logout compare-delete;见 login internal/data/account.go,
// key 格式是跨服务契约,两侧同步改)。本包只读,不承担任何写入。
package sessiongate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/redisx"
)

// Gate 查询玩家当前会话代际(jti)。
type Gate interface {
	// CurrentJTI 返回当前会话 jti;found=false = 无会话(已登出/过期)。
	// err 非 nil = 权威不可达,调用方必须 fail-closed(拒请求/拒建流/关流重试),
	// 禁止按"无会话"或"仍现行"猜测(§9.22)。
	CurrentJTI(ctx context.Context, playerID uint64) (jti string, found bool, err error)
}

// RedisGate 是基于共享 Redis 的 Gate 实现。
type RedisGate struct {
	rdb redis.UniversalClient
}

// NewRedisGate 构造(rdb 必须与 login 会话权威指向同一 Redis;infra 部署契约,
// 多 Redis 拆分时各服务 node.redis_client 必须仍可达会话权威实例)。
func NewRedisGate(rdb redis.UniversalClient) *RedisGate {
	return &RedisGate{rdb: rdb}
}

func sessionKey(playerID uint64) string {
	return fmt.Sprintf("pandora:sess:%d", playerID)
}

// CurrentJTI 实现 Gate。
func (g *RedisGate) CurrentJTI(ctx context.Context, playerID uint64) (string, bool, error) {
	jti, err := g.rdb.HGet(ctx, sessionKey(playerID), "jti").Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, errcode.New(errcode.ErrUnavailable, "session authority unavailable: %v", err)
	}
	return jti, jti != "", nil
}

// MustBuild 按 node.redis_client 配置构造 RedisGate,返回 (gate, closer)。
//
// 端点未配置(host 与 addrs 皆空):
//   - require=true  → panic:prod 强制档漏配会话权威 = 部署错误,fail-fast 拒启,
//     不允许服务在"无法判定会话现行性"的状态下对客户端面开门(fail-closed);
//   - require=false → 返回 (nil, noop):dev 直连联调,中间件对无证据请求本就放行。
//
// require=true 时启动做一次带超时 Ping,连不上 panic(对齐 push/svc 强依赖语义);
// require=false 跳过 Ping(dev 容忍 Redis 后起,运行期查询失败仍按 fail-closed 拒请求)。
func MustBuild(rc config.RedisConf, require bool) (Gate, func()) {
	if rc.Host == "" && len(rc.Addrs) == 0 {
		if require {
			panic("sessiongate.MustBuild: session authority redis endpoint required " +
				"(set node.redis_client.host or addrs); require=true refuses to start without it")
		}
		return nil, func() {}
	}
	rdb := redisx.NewUniversalClient(rc)
	if require {
		pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := rdb.Ping(pingCtx).Err()
		cancel()
		if err != nil {
			_ = rdb.Close()
			panic(fmt.Sprintf("sessiongate.MustBuild: session authority redis ping failed host=%s addrs=%v: %v",
				rc.Host, rc.Addrs, err))
		}
	}
	return NewRedisGate(rdb), func() { _ = rdb.Close() }
}
