// Package server — gRPC server 注册。
package server

import (
	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"

	"github.com/luyuancpp/pandora/pkg/grpcserver"
	pmw "github.com/luyuancpp/pandora/pkg/middleware"
	inventoryv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/inventory/v1"

	"github.com/luyuancpp/pandora/services/economy/inventory/internal/conf"
	"github.com/luyuancpp/pandora/services/economy/inventory/internal/service"
)

// NewGRPCServer 构造 gRPC server 并注册 InventoryService。
//
// 端口由 cfg.Server.Grpc.Addr 决定(默认 :50015)。
// pmw.AuthOptional() 从 Envoy 注入的 x-pandora-player-id header 读 player_id 注入 ctx。
// 用 AuthOptional 而非 AuthRequired:GrantItems 是后端内部直连(无 JWT,callerID==0)需放行;
// 客户端 RPC(GetInventory/UseItem/SellItem)在 service 层用 callerPlayerID 强制鉴权 +
// 校验请求体 player_id == 调用者,GrantItems 在 service 层拒绝 callerID>0 的客户端调用。
func NewGRPCServer(cfg *conf.Config, svc *service.InventoryService) *kgrpc.Server {
	srv := grpcserver.MustNewServer(cfg.Server, pmw.AuthOptional())
	inventoryv1.RegisterInventoryServiceServer(srv, svc)
	return srv
}
