// Package middleware — KillSwitch middleware
//
// 把 pkg/killswitch 的「RPC 级临时关停」接到 Kratos middleware 链上:
// 命中关停规则的 RPC 直接短路返回 ErrServiceDisabled,不进业务 handler。
//
// 设计要点:
//   - operation 取自 transport.Operation()(形如 "/pandora.login.v1.LoginService/Login"),
//     server / client 都能拿到;本 middleware 只在 server 侧拦截。
//   - 放在默认链 Trace 之后:被关的 RPC 仍有 trace_id / metrics 可观测(便于看"关了多少次")。
//   - fail-open:killswitch.Default 为 nil(未装配)时一律放行,绝不因开关系统故障拖垮服务。
//   - server stream RPC(如 push.Subscribe)走 Kratos 独立的 stream 拦截器,不跑 unary 链,
//     需要在 stream 入口自行调用 killswitch.Disabled(见 KillSwitchStreamCheck)。
package middleware

import (
	"context"

	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/transport"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/killswitch"
)

// KillSwitch 返回一个拦截被关停 RPC 的 middleware。
//
// 用法(已内置进 pkg/grpcserver.MustNewServer 默认链,业务通常无需手动加):
//
//	srv := kgrpc.NewServer(kgrpc.Middleware(middleware.KillSwitch()))
func KillSwitch() middleware.Middleware {
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			op := operationOf(ctx)
			if op != "" {
				if disabled, reason := killswitch.Disabled(op); disabled {
					return nil, errcode.New(errcode.ErrServiceDisabled, "%s", reason)
				}
			}
			return handler(ctx, req)
		}
	}
}

// KillSwitchStreamCheck 给 server stream RPC 在入口处手动判定是否被关停。
// 命中返回 ErrServiceDisabled,业务应直接 return 该错误终止 stream 建立。
//
// 用法(push.Subscribe 这类 stream handler 第一行):
//
//	if err := pmw.KillSwitchStreamCheck(ctx); err != nil {
//	    return err
//	}
func KillSwitchStreamCheck(ctx context.Context) error {
	op := operationOf(ctx)
	if op == "" {
		return nil
	}
	if disabled, reason := killswitch.Disabled(op); disabled {
		return errcode.New(errcode.ErrServiceDisabled, "%s", reason)
	}
	return nil
}

// operationOf 取当前 RPC 的 full operation(server 侧)。取不到返回 ""。
func operationOf(ctx context.Context) string {
	if tr, ok := transport.FromServerContext(ctx); ok {
		return tr.Operation()
	}
	return ""
}
