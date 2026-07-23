// Package middleware — 会话现行性中间件(R5 复审 P0-1,INC-20260722-004,2026-07-22)。
//
// 背景:Envoy jwt_authn 只验签名与 exp;顶号后旧 JWT 在 exp 前(默认 24h)仍能过网关,
// 若业务服务只读 x-pandora-player-id,旧设备保留全部按 player_id 定向能力(好友申请/
// 交易确认/背包使用……),违反事故档案"旧 session 不得再获得按 player_id 定向能力"。
// 本中间件把 login/push 已有的会话现行性门推广到**所有客户端面 unary RPC**:
// 凡携带 Envoy 验签后重写的 x-pandora-jwt-payload 头的请求,其 jti 必须等于 login
// 会话权威(pandora:sess)当前一代,否则按语义可判别地拒绝。
//
// ⚠️ 只覆盖 unary:Kratos unary middleware 链对 server stream 不生效,流式服务
// (push.Subscribe)必须在 service 层自行过门(见 push AuthorizeAndRegister)。
package middleware

import (
	"context"

	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/transport"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/sessiongate"
)

// SessionCurrent 校验客户端面请求的会话现行性(jti == 会话权威当前一代)。
//
// 判定矩阵(证据 = x-pandora-jwt-payload 头,仅客户端面 :8443 可信:入站无条件剥离、
// 验签成功后由 Envoy 重写,客户端无法伪造;内部服务/DS 面直连不带该头):
//
//   - 无 transport / 无证据头        → 放行(非客户端面请求:内部调用、DS 回调、dev 直连。
//     客户端面必带该头——jwt_authn 对全部玩家服务路由 require——缺头即非客户端面,
//     require 档也不改变本行:强行拒会切断全部内部调用)
//   - 证据头存在但不可解 / player_id 缺失 → ErrUnauthorized(证据残缺不可能来自正常网关)
//   - gate 未装配                    → require ? ErrUnavailable(fail-closed) : 放行
//   - 权威查询失败                    → ErrUnavailable(fail-closed,禁止猜测)
//   - 无会话(登出/过期)             → ErrUnauthorized(客户端允许自动换新会话)
//   - jti ≠ 当前一代                 → ErrSessionSuperseded(→ gRPC ABORTED;被顶设备
//     只能转交互登录,不得自动完整 Login 反顶,INC-20260722-004 R4 P0 互踢循环)
//
// 错误经 errcode.ToGRPCError 显式转换,保证客户端看到可判别的标准 gRPC 状态码
// (ABORTED/UNAUTHENTICATED/UNAVAILABLE),而不是 UNKNOWN。
//
// 诚实边界:本门是"请求进入时"的现行性判定;判定通过后、业务写提交前发生的会话轮换
// 无法被跨存储(Redis 会话 vs MySQL 业务)原子拦截,残余窗口为毫秒级在途请求,由
// login 侧副作用终检(Login/SelectRole/IssueDSTicket 返回前复核)进一步收窄。
func SessionCurrent(gate sessiongate.Gate, require bool) middleware.Middleware {
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			// tr 缺失 = 进程内直调/测试路径,无请求头证据可判,交给下游(与 AuthOptional 同语义)。
			tr, ok := transport.FromServerContext(ctx)
			if !ok {
				return handler(ctx, req)
			}
			raw := tr.RequestHeader().Get(MetadataKeyJWTPayload)
			if raw == "" {
				return handler(ctx, req)
			}
			claims := ParseJWTPayloadClaims(raw)
			playerID := PlayerIDFromContext(ctx)
			if claims.JTI == "" || playerID == 0 {
				// 头存在但残缺:正常网关路径不可能出现,按证据不可信 fail-closed。
				return nil, errcode.ToGRPCError(errcode.New(errcode.ErrUnauthorized,
					"session payload malformed or player identity missing"))
			}
			if gate == nil {
				if require {
					// 强制档不允许在"无法判定现行性"的装配下对客户端面放行。
					return nil, errcode.ToGRPCError(errcode.New(errcode.ErrUnavailable,
						"session authority not wired; request rejected (fail-closed)"))
				}
				return handler(ctx, req)
			}
			cur, found, err := gate.CurrentJTI(ctx, playerID)
			if err != nil {
				// 权威不可达:fail-closed(§9.22 禁止把「查不了」当「仍现行」)。
				return nil, errcode.ToGRPCError(err)
			}
			if !found {
				return nil, errcode.ToGRPCError(errcode.New(errcode.ErrUnauthorized,
					"session expired or logged out; login again"))
			}
			if cur != claims.JTI {
				// 顶号专属码(→ gRPC ABORTED):与自然过期/登出可判别,防被顶设备自动反顶。
				plog.With(ctx).Warnw("msg", "session_superseded_rejected",
					"player_id", playerID, "op", tr.Operation())
				return nil, errcode.ToGRPCError(errcode.New(errcode.ErrSessionSuperseded,
					"session superseded by a newer login"))
			}
			return handler(ctx, req)
		}
	}
}
