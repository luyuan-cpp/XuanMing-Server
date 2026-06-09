// Package middleware — Auth middleware
//
// W2 简化版:只校验 metadata 中的 player_id 是否存在 + 注入 ctx。
// W3+ 接入真实 JWT 解析(login 服务签发 token,Envoy 用 jwt_authn filter 校验,
// 校验通过后把 player_id 注入到 header,这里只读 header 不重复校验)。
//
// 即:
//   - Envoy(对外)负责 JWT 签名校验,把 player_id 提取出来放进 header
//   - 业务服(Kratos)用本 middleware 从 header 读 player_id 注入 ctx
//   - 业务代码用 ctx.Value(...) 拿 player_id,不用碰 JWT
package middleware

import (
	"context"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/middleware"

	plog "github.com/luyuancpp/pandora/pkg/log"
)

// AuthRequired 校验请求必须带 player_id(从 header 来,由 Envoy / gateway 鉴权后注入)。
// 没有 player_id 返回 Unauthenticated 错误。
//
// 用法:
//
//	srv := kgrpc.NewServer(kgrpc.Middleware(middleware.AuthRequired()))
//
// W3+:Envoy 配 jwt_authn filter + extract_claim 把 sub claim 写到 header:
//   x-pandora-player-id: 1001
func AuthRequired() middleware.Middleware {
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			playerID := extractPlayerID(ctx)
			if playerID == 0 {
				return nil, errors.New(401, "AUTH_REQUIRED", "missing or invalid player_id")
			}
			ctx = plog.WithPlayerID(ctx, playerID)
			return handler(ctx, req)
		}
	}
}

// AuthOptional 跟 AuthRequired 类似,但 player_id 缺失时不报错(供 Login 这种登录前 RPC 用)。
// 有 player_id 就注入 ctx,没有就 pass。
func AuthOptional() middleware.Middleware {
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			playerID := extractPlayerID(ctx)
			if playerID > 0 {
				ctx = plog.WithPlayerID(ctx, playerID)
			}
			return handler(ctx, req)
		}
	}
}

// PlayerIDFromContext 取 player_id,兼容 unary 与 server stream 两条路径。
//
// ⚠️ Kratos v2 的 unary middleware 链(AuthRequired / AuthOptional)**只对 unary RPC 生效**;
// server stream RPC(如 push.Subscribe)走 Kratos 独立的 stream 拦截器,不跑这条链,
// 所以 ctx 里不会有中间件注入的 player_id。但 stream 拦截器**会建立 transport context**,
// Envoy jwt_authn 注入的 x-pandora-player-id 头能从 transport.RequestHeader 读到。
//
// 取值优先级:
//  1. ctx.Value(player_id)——unary 路径,中间件已注入(已鉴权)
//  2. transport RequestHeader 的 x-pandora-player-id——stream 路径,Envoy 鉴权后注入
//
// 取不到返回 0(直连内网端口联调时无网关注入,按匿名处理)。
func PlayerIDFromContext(ctx context.Context) uint64 {
	if v := ctx.Value(plog.CtxKeyPlayerID); v != nil {
		if id, ok := v.(uint64); ok && id > 0 {
			return id
		}
	}
	return extractPlayerID(ctx)
}
