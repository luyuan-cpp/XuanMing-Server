// inventory_client.go — mail 服务调 inventory.GrantItems 把附件入账(2026-06-29)。
//
// 接线对齐 trade/auction 的 GrpcResourceLedger:内网 insecure 直连,无 JWT。
// 幂等键 = mail:{mail_id}:{player_id},同封邮件对同一玩家只入账一次(资产不变量)。
package data

import (
	"context"

	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/grpcclient"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	inventoryv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/inventory/v1"
	mailv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/mail/v1"
)

// GrpcItemGranter 用 inventory 服务 gRPC client 实现 biz.ItemGranter。
type GrpcItemGranter struct {
	conn *grpc.ClientConn
	cli  inventoryv1.InventoryServiceClient
}

// NewGrpcItemGranter 直连 inventory 服务 endpoint(host:port,内网 insecure)。
func NewGrpcItemGranter(inventoryAddr string) *GrpcItemGranter {
	conn := grpcclient.MustDialInsecure(inventoryAddr)
	return &GrpcItemGranter{conn: conn, cli: inventoryv1.NewInventoryServiceClient(conn)}
}

// Close 关闭底层连接。
func (g *GrpcItemGranter) Close() error {
	if g.conn != nil {
		return g.conn.Close()
	}
	return nil
}

// Grant 调 inventory.GrantItems 幂等发放附件;返回非 OK 透传错误,gRPC 错误原样返回。
func (g *GrpcItemGranter) Grant(ctx context.Context, playerID uint64, atts []*mailv1.MailAttachment, idempotencyKey string) error {
	items := make([]*inventoryv1.ItemGrant, 0, len(atts))
	for _, a := range atts {
		items = append(items, &inventoryv1.ItemGrant{ItemConfigId: a.GetItemConfigId(), Count: int64(a.GetCount())})
	}
	resp, err := g.cli.GrantItems(ctx, &inventoryv1.GrantItemsRequest{
		PlayerId:       playerID,
		Items:          items,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return err
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return errcode.New(errcode.Code(resp.GetCode()), "inventory grant code=%d", resp.GetCode())
	}
	return nil
}

// GrantInstances 调 inventory.GrantInstances 幂等铸造装备实例(装备型附件领取用)。
// itemConfigIDs 为逐件展开的配置 ID(count 份 → count 个元素);inventory 侧按幂等键去重。
func (g *GrpcItemGranter) GrantInstances(ctx context.Context, playerID uint64, itemConfigIDs []uint32, idempotencyKey string) error {
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
