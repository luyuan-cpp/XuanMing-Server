// Package server — gRPC server 注册。
package server

import (
	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"

	"github.com/luyuancpp/pandora/pkg/grpcserver"
	pmw "github.com/luyuancpp/pandora/pkg/middleware"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"

	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/service"
)

// NewGRPCServer 构造 gRPC server 并注册 DSAllocatorService。
//
// 端口由 cfg.Server.Grpc.Addr 决定(默认 :50020)。
// 调用方为后端内部(matchmaker / 战斗 DS),pmw.AuthOptional() 保持与其它服一致;
// 本服 RPC 不依赖 player_id 授权。
func NewGRPCServer(cfg *conf.Config, svc *service.AllocatorService) *kgrpc.Server {
	srv := grpcserver.MustNewServer(cfg.Server, pmw.AuthOptional())
	dsv1.RegisterDSAllocatorServiceServer(srv, svc)
	return srv
}
