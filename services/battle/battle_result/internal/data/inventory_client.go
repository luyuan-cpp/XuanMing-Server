// inventory_client.go — battle_result 调 inventory.GrantInstances 把战斗装备掉落落库(W5 ④,2026-07-08)。
//
// 接线对齐 mail/trade/auction 的 gRPC granter:内网 insecure 直连,无 JWT(系统接口)。
// inventory.GrantInstances 要求 callerID==0(系统内部直连),故必须走 MustDialInsecure。
// 幂等键 = battle_drop:{match_id}:{player_id},同对局同玩家的掉落只入账一次(资产不变量)。
package data

import (
	"context"

	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/grpcclient"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	inventoryv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/inventory/v1"
)

// GrpcInstanceGranter 用 inventory 服务 gRPC client 实现 biz.InstanceGranter。
type GrpcInstanceGranter struct {
	conn *grpc.ClientConn
	cli  inventoryv1.InventoryServiceClient
}

// NewGrpcInstanceGranter 直连 inventory 服务 endpoint(host:port,内网 insecure)。
func NewGrpcInstanceGranter(inventoryAddr string) *GrpcInstanceGranter {
	conn := grpcclient.MustDialInsecure(inventoryAddr)
	return &GrpcInstanceGranter{conn: conn, cli: inventoryv1.NewInventoryServiceClient(conn)}
}

// Close 关闭底层连接。
func (g *GrpcInstanceGranter) Close() error {
	if g.conn != nil {
		return g.conn.Close()
	}
	return nil
}

// GrantInstances 调 inventory.GrantInstances 幂等发放战斗装备掉落;
// 返回非 OK 透传错误(发布器保留出箱行下轮重试),gRPC 错误原样返回。
func (g *GrpcInstanceGranter) GrantInstances(ctx context.Context, playerID uint64, itemConfigIDs []uint32, idempotencyKey string) error {
	resp, err := g.cli.GrantInstances(ctx, &inventoryv1.GrantInstancesRequest{
		PlayerId:       playerID,
		ItemConfigIds:  itemConfigIDs,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return err
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return errcode.New(errcode.Code(resp.GetCode()), "inventory grant instances code=%d", resp.GetCode())
	}
	return nil
}
