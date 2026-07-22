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
	"encoding/base64"
	"encoding/json"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/transport"

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
//
//	x-pandora-player-id: 1001
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

// SessionJTIFromContext 从 Envoy jwt_authn 验签后透传的 x-pandora-jwt-payload 头
// 提取会话 JWT 的 jti(会话现行性门用,防顶号后旧设备继续拿票,CLAUDE.md §9.23)。
//
// 只在客户端面(:8443)可信:该头在入站被无条件剥离、仅验签成功后由 Envoy 重写。
// 请求体不带 token 的鉴权 RPC(SelectRole)靠它读 jti;取不到返回 ""(直连内网端口
// 联调 / DS 面无 jwt_authn),由业务侧按 profile 决定 fail-closed 还是放行。
func SessionJTIFromContext(ctx context.Context) string {
	// tr 是 Kratos 写入 ctx 的服务端 transport,ok 表示当前调用确实带有可读的请求头上下文。
	tr, ok := transport.FromServerContext(ctx)
	// 无 transport 时不能猜测会话身份,统一返回空值交给业务 profile 决定拒绝或兼容放行。
	if !ok {
		return ""
	}
	// 这里只读取 Envoy 重写后的可信 payload 头,具体解码与异常归一化由纯函数负责。
	return ParseJWTPayloadJTI(tr.RequestHeader().Get(MetadataKeyJWTPayload))
}

// SessionPayloadClaims 是从 Envoy 验签后 payload 头提取的会话声明子集
// (长连接会话现行性门用,push.Subscribe P0 修复 2026-07-22)。
type SessionPayloadClaims struct {
	JTI   string // 会话代际标识(与 login pandora:sess 的 jti 现行性判定)
	ExpMs int64  // 会话 JWT 到期毫秒时间戳(0 = 未携带;长连接须在流内自查到期)
}

// SessionClaimsFromContext 同 SessionJTIFromContext,但同时取 exp(长连接建立后
// JWT 会持续"有效",exp 只在建流时被 Envoy 校验一次;流内到期收口靠业务自查)。
func SessionClaimsFromContext(ctx context.Context) SessionPayloadClaims {
	tr, ok := transport.FromServerContext(ctx)
	if !ok {
		return SessionPayloadClaims{}
	}
	return ParseJWTPayloadClaims(tr.RequestHeader().Get(MetadataKeyJWTPayload))
}

// ParseJWTPayloadJTI 解析 Envoy forward_payload_header 的 base64url JSON,取 jti。
// 纯函数供测试;任何解码 / 结构异常都返回 ""(调用方按"头缺失"同等处理)。
func ParseJWTPayloadJTI(payload string) string {
	return ParseJWTPayloadClaims(payload).JTI
}

// ParseJWTPayloadClaims 解析 Envoy forward_payload_header 的 base64url JSON,取
// jti + exp。纯函数供测试;任何解码 / 结构异常都返回零值(调用方按"头缺失"同等处理)。
// payload 是完整 JWT payload 的 base64url 文本,不是原始 JWT,也不在这里重复做签名校验。
func ParseJWTPayloadClaims(payload string) SessionPayloadClaims {
	// 空字符串表示网关没有提供可信 payload 头,无需进入解码流程。
	if payload == "" {
		return SessionPayloadClaims{}
	}
	// raw 保存解码后的 JSON 字节;err 仅描述当前 base64url 编码形式是否可解。
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		// Envoy 输出无 padding;兼容带 padding 的实现差异后仍失败才放弃。
		if raw, err = base64.URLEncoding.DecodeString(payload); err != nil {
			return SessionPayloadClaims{}
		}
	}
	// claims 只声明当前逻辑需要的字段,其余 JWT claim 由 json.Unmarshal 安全忽略。
	var claims struct {
		JTI string `json:"jti"`
		Exp int64  `json:"exp"` // JWT 标准秒级时间戳
	}
	// JSON 结构异常与头缺失使用同一零值语义,禁止向调用方暴露半解析结果。
	if json.Unmarshal(raw, &claims) != nil {
		return SessionPayloadClaims{}
	}
	return SessionPayloadClaims{JTI: claims.JTI, ExpMs: claims.Exp * 1000}
}
