// Package server 负责把 LoginService 实现挂到 gRPC server 上。
package server

import (
	"github.com/luyuancpp/pandora/pkg/grpcserver"
	"github.com/luyuancpp/pandora/services/account/login/internal/conf"
	"github.com/luyuancpp/pandora/services/account/login/internal/service"

	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	loginv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/login/v1"
)

// NewGRPCServer 构造 gRPC server 并把 LoginService 注册上去。
//
// 端口由 cfg.Server.Grpc.Addr 决定(默认 :50001,见 conf.Defaults)。
func NewGRPCServer(cfg *conf.Config, svc *service.LoginService) *kgrpc.Server {
	srv := grpcserver.MustNewServer(cfg.Server)
	loginv1.RegisterLoginServiceServer(srv, svc)
	return srv
}
