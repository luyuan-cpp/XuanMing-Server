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

// Trace 是 trace_id 注入 / 透传 middleware,server / client 都用同一份。
//
// Server 侧:从 incoming metadata 找 x-pandora-trace-id;没有则生成 UUID;塞进 ctx。
// Client 侧:从 ctx 取 trace_id;写到 outgoing metadata。
//
// 用法:
//
//	srv := kgrpc.NewServer(kgrpc.Middleware(middleware.Trace()))
func Trace() middleware.Middleware {
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			traceID := extractTraceID(ctx)
			if traceID == "" {
				traceID = uuid.NewString()
			}
			ctx = plog.WithTraceID(ctx, traceID)

			// Server 侧:把 trace_id 写到回程 metadata(给客户端日志关联用)
			if tr, ok := transport.FromServerContext(ctx); ok {
				tr.ReplyHeader().Set(MetadataKeyTraceID, traceID)
			}
			// Client 侧:把 trace_id 写到 outgoing metadata(给下游服务用)
			if tr, ok := transport.FromClientContext(ctx); ok {
				tr.RequestHeader().Set(MetadataKeyTraceID, traceID)
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
