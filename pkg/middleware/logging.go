// Package middleware — Logging middleware
//
// 按 docs/design/infra.md §11 字段约定输出 access log:
//
//	{ts, level, service, trace_id, player_id, op, latency_ms, code, err}
//
// 跟 Kratos 自带 logging.Server() 的区别:
//   - 复用 pandora/pkg/log 的 ctx 字段(trace_id / player_id / match_id 自动带)
//   - 成功请求默认 DEBUG 级(生产 info 级下不出;LOG_LEVEL=debug 可临时全量打开),
//     慢请求(≥ LOG_SLOW_RPC_MS,默认 500ms)升 WARN 级 rpc_slow
//   - 失败请求 ERROR 级 rpc_failed,打印 err 详情,不打印 req/resp 内容(避免日志爆炸)
package middleware

import (
	"context"
	"os"
	"strconv"
	"time"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/transport"

	plog "github.com/luyuancpp/pandora/pkg/log"
)

// Logging 打印 access log。
//
// 用法:
//
//	srv := kgrpc.NewServer(kgrpc.Middleware(
//	    middleware.Trace(),
//	    middleware.Logging(),
//	    middleware.Metrics(),
//	))
//
// 注意 middleware 顺序:Trace 必须在 Logging 之前(否则 access log 拿不到 trace_id)。
func Logging() middleware.Middleware {
	slowMs := slowRPCThresholdMs()
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			start := time.Now()

			// 方向判定(§16.7):client transport 存在即本次是 client hop(它是本次调用
			// 刚放进 ctx 的);server transport 可能是 handler 继承的,只作兜底,
			// 避免 handler 内发起的下游调用被错标成外层 server op。
			op := ""
			kind := ""
			if tr, ok := transport.FromClientContext(ctx); ok {
				op = tr.Operation()
				kind = string(tr.Kind()) + "_client"
			} else if tr, ok := transport.FromServerContext(ctx); ok {
				op = tr.Operation()
				kind = string(tr.Kind())
			}

			resp, err := handler(ctx, req)
			latency := time.Since(start)

			h := plog.With(ctx)
			if err != nil {
				h.Errorw(
					"msg", "rpc_failed",
					"transport", kind,
					"op", op,
					"latency_ms", latency.Milliseconds(),
					"code", errors.Code(err),
					"reason", errors.Reason(err),
					"err", err.Error(),
				)
			} else if latency.Milliseconds() >= slowMs {
				// 慢请求升 WARN:生产 info 级下也能看到,直接指出慢在哪个 op
				h.Warnw(
					"msg", "rpc_slow",
					"transport", kind,
					"op", op,
					"latency_ms", latency.Milliseconds(),
					"slow_threshold_ms", slowMs,
				)
			} else {
				// 正常成功请求降为 DEBUG:高 QPS 下 rpc_ok 是最大噪音源,
				// 排障需要全量 access log 时设 LOG_LEVEL=debug 即可打开
				h.Debugw(
					"msg", "rpc_ok",
					"transport", kind,
					"op", op,
					"latency_ms", latency.Milliseconds(),
				)
			}

			return resp, err
		}
	}
}

// slowRPCThresholdMs 从 LOG_SLOW_RPC_MS 环境变量取慢请求阈值(毫秒),默认 500。
func slowRPCThresholdMs() int64 {
	if v := os.Getenv("LOG_SLOW_RPC_MS"); v != "" {
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil && ms > 0 {
			return ms
		}
	}
	return 500
}
