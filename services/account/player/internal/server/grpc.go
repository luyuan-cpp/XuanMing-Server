// Package server — gRPC server 注册。
package server

import (
	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"

	"github.com/luyuancpp/pandora/pkg/grpcserver"
	pmw "github.com/luyuancpp/pandora/pkg/middleware"
	configv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/config/v1"
	playerv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/player/v1"

	"github.com/luyuancpp/pandora/services/account/player/internal/conf"
	"github.com/luyuancpp/pandora/services/account/player/internal/service"
)

// NewGRPCServer 构造 gRPC server 并注册 PlayerService。
//
// 端口由 cfg.Server.Grpc.Addr 决定(默认 :50002)。
// 调用方为后端内部(battle_result)/ 经 Envoy 的客户端,pmw.AuthOptional() 与其它服一致。
func NewGRPCServer(cfg *conf.Config, svc *service.PlayerService, ctAdmin configv1.ConfigTableAdminServiceServer) *kgrpc.Server {
	srv := grpcserver.MustNewServer(cfg.Server, pmw.AuthOptional())
	playerv1.RegisterPlayerServiceServer(srv, svc)
	if ctAdmin != nil {
		configv1.RegisterConfigTableAdminServiceServer(srv, ctAdmin)
	}
	return srv
}
