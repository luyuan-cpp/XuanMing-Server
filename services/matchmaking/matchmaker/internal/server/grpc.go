// Package server — gRPC server 注册。
package server

import (
	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"

	"github.com/luyuancpp/pandora/pkg/grpcserver"
	pmw "github.com/luyuancpp/pandora/pkg/middleware"
	configv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/config/v1"
	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"

	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/conf"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/service"
)

// NewGRPCServer 构造 gRPC server 并注册 MatchService。
//
// 端口由 cfg.Server.Grpc.Addr 决定(默认 :50011)。
// pmw.AuthOptional() 从 Envoy 注入的 x-pandora-player-id header 读 player_id 注入 ctx。
// Envoy jwt_authn 已在路由层 require JWT;service 层再做 callerID==0 拦截兜底。
//
// ctAdmin 非 nil(config_table.dir 已配置)时同端口挂配置表热更入口:
// 内部接口,Envoy 无该 service 路由,玩家流量到不了;service 层再做 callerID!=0 拒绝兜底。
func NewGRPCServer(cfg *conf.Config, svc *service.MatchService, ctAdmin *service.ConfigTableAdminService) *kgrpc.Server {
	srv := grpcserver.MustNewServer(cfg.Server, pmw.AuthOptional())
	matchv1.RegisterMatchServiceServer(srv, svc)
	if ctAdmin != nil {
		configv1.RegisterConfigTableAdminServiceServer(srv, ctAdmin)
	}
	return srv
}
