// Package middleware — Metrics middleware
//
// 按 docs/design/infra.md §10 命名规范导出 Prometheus 指标:
//   pandora_rpc_duration_seconds{service,method,code}
//   pandora_rpc_total{service,method,code}
//
// 同时兼容 gRPC server / HTTP server / gRPC client(Kratos transport.Transporter 统一抽象)。
package middleware

import (
	"context"
	"strings"
	"time"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/transport"
	"github.com/prometheus/client_golang/prometheus"

	pmetrics "github.com/luyuancpp/pandora/pkg/metrics"
)

var (
	rpcDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "pandora",
		Subsystem: "rpc",
		Name:      "duration_seconds",
		Help:      "RPC handler duration in seconds.",
		Buckets:   pmetrics.StandardBuckets,
	}, []string{"service", "method", "code"})

	rpcTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "pandora",
		Subsystem: "rpc",
		Name:      "total",
		Help:      "RPC call count.",
	}, []string{"service", "method", "code"})
)

func init() {
	pmetrics.Register(rpcDurationSeconds)
	pmetrics.Register(rpcTotal)
}

// Metrics 记录 RPC duration + total。
//
// service / method label 从 transport.Operation() 解析,例如:
//   /pandora.login.v1.LoginService/Login → service="LoginService", method="Login"
//
// code label 是粗粒度错误分类(低基数,避免 prometheus 高 cardinality)。
func Metrics() middleware.Middleware {
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			start := time.Now()
			service, method := splitOperation(ctx)

			resp, err := handler(ctx, req)

			code := codeLabel(err)
			elapsed := time.Since(start).Seconds()
			rpcDurationSeconds.WithLabelValues(service, method, code).Observe(elapsed)
			rpcTotal.WithLabelValues(service, method, code).Inc()

			return resp, err
		}
	}
}

// splitOperation 把 "/pandora.login.v1.LoginService/Login" 切成 ("LoginService", "Login")。
//
// 方向判定(§16.7):client transport 是本次调用刚放进 ctx 的,存在即说明这是 client hop;
// server transport 可能是 server handler 继承下来的,只能作兜底。不得反过来优先 server,
// 否则 handler 内(含异步 goroutine)发起的下游调用会被错标成外层 server op。
func splitOperation(ctx context.Context) (service, method string) {
	tr, ok := transport.FromClientContext(ctx)
	if !ok {
		if tr, ok = transport.FromServerContext(ctx); !ok {
			return "unknown", "unknown"
		}
	}
	op := tr.Operation()
	parts := strings.Split(strings.TrimPrefix(op, "/"), "/")
	if len(parts) != 2 {
		return "unknown", op
	}
	svc := parts[0]
	if dot := strings.LastIndex(svc, "."); dot >= 0 {
		svc = svc[dot+1:]
	}
	return svc, parts[1]
}

// codeLabel 把 error 转成稳定的 label 值(避免高基数)。
//
// Kratos errors.Code(err) 返回 HTTP-style code,我们映射成几个粗粒度桶。
// 业务错误码段(login 1000-1999 / team 3000-3999 等)按段位映射,
// 避免 prometheus label 爆炸。
func codeLabel(err error) string {
	if err == nil {
		return "ok"
	}
	code := errors.Code(err)
	switch {
	case code == 0:
		return "ok"
	case code == 401 || code == 403:
		return "unauthorized"
	case code >= 400 && code < 500:
		return "client_err"
	case code == 504:
		return "timeout"
	case code >= 500:
		return "server_err"
	default:
		return "other"
	}
}
