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

// Grant 调 inventory.GrantItems 幂等发放 stack 形态附件;返回非 OK 透传错误,gRPC 错误原样返回。
// 调用方(biz.ClaimMail)已按 oneof 分组只传 stack;混入其它形态说明分组逻辑被破坏,报错不静默跳过。
func (g *GrpcItemGranter) Grant(ctx context.Context, playerID uint64, atts []*mailv1.MailAttachment, idempotencyKey string) error {
	items := make([]*inventoryv1.ItemGrant, 0, len(atts))
	for _, a := range atts {
		s := a.GetStack()
		if s == nil {
			return errcode.New(errcode.ErrMailAttachmentUnsupported, "non-stack attachment in stack grant")
		}
		items = append(items, &inventoryv1.ItemGrant{ItemConfigId: s.GetItemConfigId(), Count: int64(s.GetCount())})
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

// ClaimTransfers 调 inventory.ClaimTransferInstances 交付 transfer 形态附件(既存实例
// 托管转移只改归属,bag-domain.md §7.1)。请求只带 instance_id+config 核对项——领取内容
// 以 inventory 托管行为权威,附件快照不参与写入;inventory 侧按幂等键去重。
// 调用方(biz.ClaimMail)已按 oneof 分组只传 transfer;混入其它形态说明分组被破坏,报错不静默跳过。
func (g *GrpcItemGranter) ClaimTransfers(ctx context.Context, playerID uint64, atts []*mailv1.MailAttachment, idempotencyKey string) error {
	items := make([]*inventoryv1.TransferClaimItem, 0, len(atts))
	for _, a := range atts {
		xfer := a.GetTransfer()
		if xfer == nil {
			return errcode.New(errcode.ErrMailAttachmentUnsupported, "non-transfer attachment in transfer claim")
		}
		items = append(items, &inventoryv1.TransferClaimItem{
			InstanceId:   xfer.GetItem().GetInstanceId(),
			ItemConfigId: xfer.GetItem().GetItemConfigId(),
		})
	}
	resp, err := g.cli.ClaimTransferInstances(ctx, &inventoryv1.ClaimTransferInstancesRequest{
		ToPlayerId:     playerID,
		Items:          items,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return err
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return errcode.New(errcode.Code(resp.GetCode()), "inventory claim transfers code=%d", resp.GetCode())
	}
	return nil
}

// ConsumeTransferEscrow 调 inventory.ConsumeTransferEscrow 消托管行不物化
// (bag phase 2 DS 领取链:资产已经 bag journal 入包,托管行只删防双持;幂等)。
func (g *GrpcItemGranter) ConsumeTransferEscrow(ctx context.Context, playerID uint64, instanceIDs []uint64) error {
	resp, err := g.cli.ConsumeTransferEscrow(ctx, &inventoryv1.ConsumeTransferEscrowRequest{
		ToPlayerId:  playerID,
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return err
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return errcode.New(errcode.Code(resp.GetCode()), "inventory consume escrow code=%d", resp.GetCode())
	}
	return nil
}
