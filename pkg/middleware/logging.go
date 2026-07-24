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

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
)

// inbandCoder 是所有携带统一 commonv1.ErrCode 的 gRPC 响应共同满足的接口
// (生成的 proto 响应都带 GetCode() commonv1.ErrCode)。中间件用它读出 handler
// 以 in-band 方式返回的错误码,而不需要感知具体服务类型。
type inbandCoder interface {
	GetCode() commonv1.ErrCode
}

// inbandServerFault 判定 handler 在 transport error 为 nil 时,是否通过响应 Code
// 字段返回了服务端内部 / 基础设施故障(见 errcode.IsServerFault)。返回 true 时中间件
// 升 ERROR,避免这类故障被记成 rpc_ok(DEBUG)在线上 info 级静默。
func inbandServerFault(resp any) (commonv1.ErrCode, bool) {
	c, ok := resp.(inbandCoder)
	if !ok {
		return commonv1.ErrCode_OK, false
	}
	code := c.GetCode()
	return code, errcode.IsServerFault(errcode.Code(code))
}

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
			isClient := false
			if tr, ok := transport.FromClientContext(ctx); ok {
				op = tr.Operation()
				kind = string(tr.Kind()) + "_client"
				isClient = true
			} else if tr, ok := transport.FromServerContext(ctx); ok {
				op = tr.Operation()
				kind = string(tr.Kind())
			}

			resp, err := handler(ctx, req)
			latency := time.Since(start)

			h := plog.With(ctx)
			inbandCode, inbandFault := inbandServerFault(resp)
			switch {
			case err != nil:
				h.Errorw(
					"msg", "rpc_failed",
					"transport", kind,
					"op", op,
					"latency_ms", latency.Milliseconds(),
					"code", errors.Code(err),
					"reason", errors.Reason(err),
					"err", err.Error(),
				)
			case !isClient && inbandFault:
				// handler 以 in-band Code 返回了服务端内部 / 基础设施故障(transport err=nil),
				// 否则会被下面的 rpc_ok 记成 DEBUG,在生产 info 级下静默(§16 禁止吞掉故障)。
				// 只在本次是 server hop 时打:client hop 的 resp 是下游响应,其故障归下游服务记。
				// 无 err 详情(已被转成 in-band Code),但 op + code + ctx 的 trace_id/player_id
				// 足以定位是哪个 RPC 在出内部故障并顺链下钻。
				h.Errorw(
					"msg", "rpc_inband_error",
					"transport", kind,
					"op", op,
					"latency_ms", latency.Milliseconds(),
					"code", int32(inbandCode),
				)
			case latency.Milliseconds() >= slowMs:
				// 慢请求升 WARN:生产 info 级下也能看到,直接指出慢在哪个 op
				h.Warnw(
					"msg", "rpc_slow",
					"transport", kind,
					"op", op,
					"latency_ms", latency.Milliseconds(),
					"slow_threshold_ms", slowMs,
				)
			default:
				// 正常成功请求(或预期业务拒绝码)降为 DEBUG:高 QPS 下 rpc_ok 是最大噪音源,
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
