// Package server 负责把 PushService 实现挂到 gRPC server 上。
package server

import (
	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"

	"github.com/luyuancpp/pandora/pkg/grpcserver"
	pmw "github.com/luyuancpp/pandora/pkg/middleware"
	pushv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/push/v1"

	"github.com/luyuancpp/pandora/services/runtime/push/internal/conf"
	"github.com/luyuancpp/pandora/services/runtime/push/internal/service"
)

// NewGRPCServer 构造 gRPC server 并把 PushService 注册上去。
//
// 端口由 cfg.Server.Grpc.Addr 决定(默认 :50014,见 conf.Defaults)。
//
// ⚠️ Subscribe 是 server stream RPC,Kratos transport/grpc 会自动处理 stream 生命周期,
// 业务侧 PushService.Subscribe 收到的 stream.Context() 在 client 断开时自动 cancel。
//
// W3 ①(2026-06-05):加 pmw.AuthOptional() 中间件。
//
// ⚠️ 注意(2026-06-08 修正):AuthOptional 是 Kratos unary middleware,**只对 unary RPC 生效**。
// push 当前唯一 RPC Subscribe 是 server stream,Kratos 不在 unary 链上跑它,所以 AuthOptional
// 对 Subscribe 实际是 no-op;Subscribe 的 player_id 由 service 层 pmw.PlayerIDFromContext 直接
// 从 Kratos transport 的 x-pandora-player-id 头(Envoy jwt_authn 注入)读取。这里保留
// AuthOptional 仅为将来 push 若新增 unary RPC 时的鉴权占位,不影响 Subscribe。
// 网关侧鉴权强约束在 Envoy jwt_authn(prefix /pandora.push.v1.PushService/ requires JWT)。
func NewGRPCServer(cfg *conf.Config, svc *service.PushService) *kgrpc.Server {
	srv := grpcserver.MustNewServer(cfg.Server, pmw.AuthOptional())
	pushv1.RegisterPushServiceServer(srv, svc)
	return srv
}
