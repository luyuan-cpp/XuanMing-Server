// Package grpcclient 提供 Pandora 服务调用其它 gRPC 服务的客户端包装(基于 Kratos transport/grpc)。
//
// 设计:
//   - 包装 Kratos transport/grpc.Dial / DialInsecure
//   - 默认挂接 Pandora client middleware(Trace 透传 + Metrics)
//   - 服务发现:Kratos registry/etcd(W3+ 接入)/ 直连 endpoint(W2 简化版)
//
// 用法(直连):
//
//	conn := grpcclient.MustDial("127.0.0.1:50001")
//	defer conn.Close()
//	cli := loginpb.NewLoginServiceClient(conn)
//
// 用法(经服务发现):
//
//	conn := grpcclient.MustDialDiscovery("discovery:///pandora.login", reg)
//	cli := loginpb.NewLoginServiceClient(conn)
package grpcclient

import (
	"context"
	"time"

	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/registry"
	"github.com/go-kratos/kratos/v2/selector"
	"github.com/go-kratos/kratos/v2/selector/wrr"
	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	"google.golang.org/grpc"

	pmw "github.com/luyuancpp/pandora/pkg/middleware"
)

// DefaultTimeout 是单次 RPC 默认超时(可被 ctx.WithTimeout 覆盖)。
const DefaultTimeout = 15 * time.Second

func init() {
	// 设置全局默认负载均衡为加权轮询(WRR)
	selector.SetGlobalSelector(wrr.NewBuilder())
}

// MustDial 直连指定 endpoint(host:port),不走服务发现。
// W2 简化版用,W3+ 切到 MustDialDiscovery。
//
// 默认挂载 Trace + Metrics middleware,默认 15s 超时。
func MustDial(endpoint string, customMW ...middleware.Middleware) *grpc.ClientConn {
	return mustDial(false, endpoint, nil, customMW...)
}

// MustDialDiscovery 经服务发现连接(target 形如 "discovery:///pandora.login")。
// reg 是 Kratos registry.Discovery 实现(etcd / consul / nacos)。
func MustDialDiscovery(endpoint string, reg registry.Discovery, customMW ...middleware.Middleware) *grpc.ClientConn {
	return mustDial(false, endpoint, reg, customMW...)
}

// MustDialInsecure 同 MustDial,但显式声明 insecure(不强制 TLS)。
// 内网服务间通信用这个;Envoy 入站才用 TLS。
func MustDialInsecure(endpoint string, customMW ...middleware.Middleware) *grpc.ClientConn {
	return mustDial(true, endpoint, nil, customMW...)
}

func mustDial(insecure bool, endpoint string, reg registry.Discovery, customMW ...middleware.Middleware) *grpc.ClientConn {
	// 默认 client middleware:Trace 透传 + Metrics + 第 4 层熔断(SRE breaker)。
	// 熔断挂在 client 侧:下游故障时快速失败,避免雪崩拖垮调用方。
	mws := append([]middleware.Middleware{
		pmw.Trace(),
		pmw.Metrics(),
		pmw.CircuitBreaker(),
	}, customMW...)

	opts := []kgrpc.ClientOption{
		kgrpc.WithEndpoint(endpoint),
		kgrpc.WithTimeout(DefaultTimeout),
		kgrpc.WithMiddleware(mws...),
	}
	if reg != nil {
		opts = append(opts, kgrpc.WithDiscovery(reg))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var (
		conn *grpc.ClientConn
		err  error
	)
	if insecure {
		conn, err = kgrpc.DialInsecure(ctx, opts...)
	} else {
		conn, err = kgrpc.Dial(ctx, opts...)
	}
	if err != nil {
		panic("grpcclient.MustDial " + endpoint + ": " + err.Error())
	}
	return conn
}

// WithTimeout 是给业务侧用的便捷函数,在 ctx 上设默认超时(如果 ctx 已有 deadline 则不覆盖)。
func WithTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	if _, ok := parent.Deadline(); ok {
		return parent, func() {}
	}
	return context.WithTimeout(parent, DefaultTimeout)
}
