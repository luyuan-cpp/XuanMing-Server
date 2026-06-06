// Package server — gRPC server 注册。
package server

import (
	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"

	"github.com/luyuancpp/pandora/pkg/grpcserver"
	pmw "github.com/luyuancpp/pandora/pkg/middleware"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"

	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/service"
)

// NewGRPCServer 构造 gRPC server 并注册 HubAllocatorService。
//
// 端口由 cfg.Server.Grpc.Addr 决定(默认 :50021)。
// 调用方为后端内部(login / 大厅 DS),pmw.AuthOptional() 保持与其它服一致;
// 本服 RPC 不依赖 player_id 授权(player_id 由 login 显式传入)。
func NewGRPCServer(cfg *conf.Config, svc *service.HubService) *kgrpc.Server {
	srv := grpcserver.MustNewServer(cfg.Server, pmw.AuthOptional())
	hubv1.RegisterHubAllocatorServiceServer(srv, svc)
	return srv
}
