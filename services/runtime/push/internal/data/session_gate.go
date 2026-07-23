// session_gate.go — 会话现行性权威的只读视图端口(push.Subscribe P0 修复 2026-07-22,
// INC-20260722-004)。
//
// 权威本体由 login 维护:Redis `pandora:sess:<player_id>` hash(字段 jti = 当前会话
// 代际,顶号/重登轮换,Logout compare-delete;见 login internal/data/account.go,
// key 格式是跨服务契约,两侧同步改)。push 只读:JWT 验签只证明"曾经登录过",
// 不证明"未被顶号"——旧 token 在 exp 前仍能过 Envoy jwt_authn,必须再过现行性门
// (CLAUDE.md §9.23 会话 fencing)。
//
// R5 复审 P0-1(2026-07-22):Redis 实现已提升为共享 pkg/sessiongate.RedisGate
// (全部客户端面服务经 pmw.SessionCurrent 复用同一权威),本文件只保留 biz 依赖的
// 端口接口;main.go 装配 sessiongate.NewRedisGate(rdb)(结构化满足本接口)。
package data

import "context"

// SessionGate 查询玩家当前会话代际(jti)。
type SessionGate interface {
	// CurrentJTI 返回当前会话 jti;found=false = 无会话(已登出/过期)。
	// err 非 nil = 权威不可达,调用方必须 fail-closed(拒建流/关流重试),
	// 禁止按"无会话"或"仍现行"猜测(§9.22)。
	CurrentJTI(ctx context.Context, playerID uint64) (jti string, found bool, err error)
}
