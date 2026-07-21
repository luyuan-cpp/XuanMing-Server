// Package middleware 提供 Pandora 自研的 Kratos middleware。
//
// 跟 Kratos 自带 middleware(recovery / tracing / logging / metadata)的区别:
// 这里的 middleware 跟 Pandora 业务约定耦合,比如:
//   - trace.go     从 Pandora metadata key 提取 / 注入 trace_id(跟 mmorpg 风格对齐)
//   - auth.go      JWT 解析 + 注入 player_id 到 ctx
//   - metrics.go   Prometheus 指标命名按 docs/design/infra.md §10 规范
//   - logging.go   access log 字段约定按 docs/design/infra.md §11
//
// 设计上 gRPC server / HTTP server / gRPC client 都能复用同一个 middleware
// (Kratos middleware.Middleware 是协议无关的)。
package middleware

import (
	"context"
	"strconv"

	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/transport"
	"github.com/google/uuid"

	plog "github.com/luyuancpp/pandora/pkg/log"
)

// MetadataKeyTraceID 是 Pandora 跨服务传递的 trace_id metadata key。
//
// gRPC 走 grpc metadata,HTTP 走 header(Kratos transport 统一抽象)。
// 命名大小写不敏感,跟 mmorpg 风格对齐:`x-pandora-trace-id`。
const MetadataKeyTraceID = "x-pandora-trace-id"

// MetadataKeyPlayerID 是 player_id metadata key,Envoy / gateway 鉴权后注入。
const MetadataKeyPlayerID = "x-pandora-player-id"

// MetadataKeyJWTPayload 是 Envoy jwt_authn 验签成功后透传的 JWT payload 头
// (forward_payload_header,base64url JSON)。客户端面入站第一时间无条件剥离本头,
// 只有验签成功才由 Envoy 重写,因此其中的 jti/sub 在 :8443 面可信
// (deploy/envoy/envoy.yaml header_mutation + jwt_authn 说明)。
// 该常量只统一可信请求头名称,不表示任意调用方自行写入的同名头都可信。
const MetadataKeyJWTPayload = "x-pandora-jwt-payload"

// Trace 是 trace_id 注入 / 透传 middleware,server / client 都用同一份。
//
// Server 侧:从 incoming metadata 找 x-pandora-trace-id;没有则生成 UUID;塞进 ctx + 回程 header。
// Client 侧:从 ctx(plog)取 trace_id;没有则生成 UUID;只写 outgoing metadata。
//
// 方向判定按「本次调用的 transport」二选一,client 分支不得触碰 server transport:
// server handler 内(含由请求 ctx 派生的异步 goroutine)发起下游调用时,ctx 里同时带着
// 入站请求的 server transport 和本次调用的 client transport;此时若再走
// FromServerContext 写 ReplyHeader,就会与 gRPC 正在发送响应时对同一 metadata Map 的
// 遍历并发(grpc metadata.Join / SetHeader),触发
// fatal error: concurrent map iteration and map write(2026-07-21 ds_allocator Heartbeat 崩溃)。
//
// 用法:
//
//	srv := kgrpc.NewServer(kgrpc.Middleware(middleware.Trace()))
func Trace() middleware.Middleware {
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			// Client 侧:本次调用存在 client transport ⇒ 这是 client hop。
			// trace_id 只从 ctx 值取(server 侧 middleware 已放入),不读 server transport。
			if tr, ok := transport.FromClientContext(ctx); ok {
				traceID, _ := ctx.Value(plog.CtxKeyTraceID).(string)
				if traceID == "" {
					traceID = uuid.NewString()
					ctx = plog.WithTraceID(ctx, traceID)
				}
				tr.RequestHeader().Set(MetadataKeyTraceID, traceID)
				return handler(ctx, req)
			}

			// Server 侧:从 incoming metadata 提取 / 生成,写 ctx + 回程 header。
			// 此时 handler 尚未开始组装响应,写 ReplyHeader 与响应发送不并发。
			traceID := extractTraceID(ctx)
			if traceID == "" {
				traceID = uuid.NewString()
			}
			ctx = plog.WithTraceID(ctx, traceID)
			if tr, ok := transport.FromServerContext(ctx); ok {
				tr.ReplyHeader().Set(MetadataKeyTraceID, traceID)
			}
			return handler(ctx, req)
		}
	}
}

// extractTraceID 从 Kratos transport 抽象中拿 trace_id(server 入站方向)。
func extractTraceID(ctx context.Context) string {
	if tr, ok := transport.FromServerContext(ctx); ok {
		if v := tr.RequestHeader().Get(MetadataKeyTraceID); v != "" {
			return v
		}
	}
	return ""
}

// extractPlayerID 从 metadata 拿 player_id(Envoy / gateway 鉴权后注入到 header)。
//
// Returns 0 if not present.
func extractPlayerID(ctx context.Context) uint64 {
	tr, ok := transport.FromServerContext(ctx)
	if !ok {
		return 0
	}
	v := tr.RequestHeader().Get(MetadataKeyPlayerID)
	if v == "" {
		return 0
	}
	id, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0
	}
	return id
}
