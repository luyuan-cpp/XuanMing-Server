// transfer.go — 邮件 transfer 附件实例托管 RPC(2026-07-22,bag-domain.md §7.1)。
//
// 三个均为系统接口(仅后端内部直连,不在 Envoy 暴露):经 Envoy 的客户端调用必带 JWT
// (callerID>0)→ 一律拒绝,杜绝玩家自助托管/领取/释放他人实例(鉴权同 GrantItems)。
package service

import (
	"context"

	pmw "github.com/luyuancpp/pandora/pkg/middleware"
	bagv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/bag/v1"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	inventoryv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/inventory/v1"

	"github.com/luyuancpp/pandora/services/economy/inventory/internal/data"
)

// toBagItem 把托管快照转 BagItem(TransferAttachment.item 形状:count 恒 1,slot 无意义留 0)。
func toBagItem(row data.EscrowedInstance) *bagv1.BagItem {
	attrs := make([]*bagv1.BagItemAttribute, 0, len(row.Attributes))
	for _, a := range row.Attributes {
		attrs = append(attrs, &bagv1.BagItemAttribute{AttrId: a.AttrID, Value: a.Value})
	}
	return &bagv1.BagItem{
		ItemConfigId: row.ItemConfigID,
		Count:        1,
		InstanceId:   row.InstanceID,
		Identified:   row.Identified,
		Attrs:        attrs,
	}
}

// EscrowOutInstances 托管扣出(系统接口,仅后端内部直连)。
func (s *InventoryService) EscrowOutInstances(ctx context.Context, req *inventoryv1.EscrowOutInstancesRequest) (*inventoryv1.EscrowOutInstancesResponse, error) {
	if pmw.PlayerIDFromContext(ctx) != 0 {
		return &inventoryv1.EscrowOutInstancesResponse{Code: commonv1.ErrCode_ERR_PERMISSION_DENY}, nil
	}
	rows, err := s.uc.EscrowOutInstances(ctx,
		req.GetSourcePlayerId(), req.GetToPlayerId(), req.GetInstanceIds(), req.GetEscrowKey())
	if err != nil {
		return &inventoryv1.EscrowOutInstancesResponse{Code: toProtoCode(err)}, nil
	}
	items := make([]*bagv1.BagItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, toBagItem(row))
	}
	return &inventoryv1.EscrowOutInstancesResponse{Code: commonv1.ErrCode_OK, Items: items}, nil
}

// ClaimTransferInstances 领取托管实例(系统接口,mail 服务内网直连)。
func (s *InventoryService) ClaimTransferInstances(ctx context.Context, req *inventoryv1.ClaimTransferInstancesRequest) (*inventoryv1.ClaimTransferInstancesResponse, error) {
	if pmw.PlayerIDFromContext(ctx) != 0 {
		return &inventoryv1.ClaimTransferInstancesResponse{Code: commonv1.ErrCode_ERR_PERMISSION_DENY}, nil
	}
	items := make([]data.TransferClaimItem, 0, len(req.GetItems()))
	for _, it := range req.GetItems() {
		items = append(items, data.TransferClaimItem{InstanceID: it.GetInstanceId(), ItemConfigID: it.GetItemConfigId()})
	}
	if err := s.uc.ClaimTransferInstances(ctx, req.GetToPlayerId(), items, req.GetIdempotencyKey()); err != nil {
		return &inventoryv1.ClaimTransferInstancesResponse{Code: toProtoCode(err)}, nil
	}
	return &inventoryv1.ClaimTransferInstancesResponse{Code: commonv1.ErrCode_OK}, nil
}

// ReleaseTransferEscrow 托管释放回源(系统接口,发信 saga 补偿)。
func (s *InventoryService) ReleaseTransferEscrow(ctx context.Context, req *inventoryv1.ReleaseTransferEscrowRequest) (*inventoryv1.ReleaseTransferEscrowResponse, error) {
	if pmw.PlayerIDFromContext(ctx) != 0 {
		return &inventoryv1.ReleaseTransferEscrowResponse{Code: commonv1.ErrCode_ERR_PERMISSION_DENY}, nil
	}
	if err := s.uc.ReleaseTransferEscrow(ctx, req.GetInstanceIds()); err != nil {
		return &inventoryv1.ReleaseTransferEscrowResponse{Code: toProtoCode(err)}, nil
	}
	return &inventoryv1.ReleaseTransferEscrowResponse{Code: commonv1.ErrCode_OK}, nil
}

// ConsumeTransferEscrow 消托管行不物化(系统接口,bag phase 2 DS 领取链;mail 服务内网直连)。
func (s *InventoryService) ConsumeTransferEscrow(ctx context.Context, req *inventoryv1.ConsumeTransferEscrowRequest) (*inventoryv1.ConsumeTransferEscrowResponse, error) {
	if pmw.PlayerIDFromContext(ctx) != 0 {
		return &inventoryv1.ConsumeTransferEscrowResponse{Code: commonv1.ErrCode_ERR_PERMISSION_DENY}, nil
	}
	if err := s.uc.ConsumeTransferEscrow(ctx, req.GetToPlayerId(), req.GetInstanceIds()); err != nil {
		return &inventoryv1.ConsumeTransferEscrowResponse{Code: toProtoCode(err)}, nil
	}
	return &inventoryv1.ConsumeTransferEscrowResponse{Code: commonv1.ErrCode_OK}, nil
}
