// Package grpcserver 提供 Pandora 服务的 gRPC server 包装(基于 Kratos transport/grpc)。
//
// 设计:
//   - 包装 Kratos transport/grpc.NewServer(...)
//   - 默认挂接 Pandora middleware 链(Recovery → Trace → Logging → Metrics)
//   - 业务侧:`grpcserver.MustNewServer(cfg.Server)` + `pb.RegisterXxxServer(srv, impl)` + `kratos.New(...).Run()`
//
// 跟之前 go-zero 版本的区别(2026-06-04 重写):
//   - go-zero zrpc.MustNewServer → Kratos transport/grpc.NewServer
//   - go-zero unary interceptor → Kratos middleware(协议无关,gRPC + HTTP 共用)
//   - 新增 server stream / bidi 支持(go-zero 不支持,Kratos 原生)
package grpcserver

import (
	"github.com/go-kratos/kratos/v2/middleware"
	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"

	"github.com/luyuancpp/pandora/pkg/config"
	pmw "github.com/luyuancpp/pandora/pkg/middleware"
)

// MustNewServer 创建并配置一个 Kratos gRPC Server。
//
// 默认 middleware 链(从外到内):
//   1. Recovery     最外层捕 panic
//   2. Trace        trace_id 注入 / 透传
//   3. Logging      access log
//   4. Metrics      Prometheus 指标
//
// 业务可通过 customMW 追加自定义 middleware(顺序在默认之后):
//
//	srv := grpcserver.MustNewServer(cfg.Server,
//	    pmw.AuthRequired(),  // 业务级鉴权
//	)
//	pb.RegisterLoginServiceServer(srv, loginsvc.New(...))
//	app := kratos.New(kratos.Server(srv))
//	app.Run()
func MustNewServer(c config.Server, customMW ...middleware.Middleware) *kgrpc.Server {
	mws := append([]middleware.Middleware{
		pmw.Recovery(),
		pmw.Trace(),
		pmw.Logging(),
		pmw.Metrics(),
	}, customMW...)

	opts := []kgrpc.ServerOption{
		kgrpc.Address(c.Grpc.Addr),
		kgrpc.Middleware(mws...),
	}
	if c.Grpc.Network != "" {
		opts = append(opts, kgrpc.Network(c.Grpc.Network))
	}
	if c.Grpc.Timeout > 0 {
		opts = append(opts, kgrpc.Timeout(c.Grpc.Timeout.Std()))
	}

	// gRPC reflection 开关化(W3 ③,2026-06-05):
	//   - Kratos transport/grpc.NewServer 默认开 reflection(v1 + v1alpha)。
	//   - dev:cfg.Server.Grpc.EnableReflection = true,保留默认行为,
	//     `grpcurl :50001 list` / `describe pandora.login.v1.LoginService` 可用。
	//   - prod(默认 false):调 kgrpc.DisableReflection() 关闭,
	//     避免 schema 泄露 / 攻击面扩大。
	if !c.Grpc.EnableReflection {
		opts = append(opts, kgrpc.DisableReflection())
	}
	return kgrpc.NewServer(opts...)
}
