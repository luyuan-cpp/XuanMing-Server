package service

import (
	"context"
	"testing"

	plog "github.com/luyuancpp/pandora/pkg/log"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	inventoryv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/inventory/v1"
)

func TestEnsureAuctionEscrowRejectsPlayerCaller(t *testing.T) {
	ctx := plog.WithPlayerID(context.Background(), 1001)
	svc := &InventoryService{}
	resp, err := svc.EnsureAuctionEscrow(ctx, &inventoryv1.EnsureAuctionEscrowRequest{
		PlayerId:          1001,
		OrderId:           2001,
		Side:              inventoryv1.EscrowSide_ESCROW_SIDE_SELL,
		ItemConfigId:      7001,
		RemainingQuantity: 1,
		UnitPrice:         100,
	})
	if err != nil {
		t.Fatalf("玩家调用应以业务码拒绝, got transport err: %v", err)
	}
	if resp.GetCode() != commonv1.ErrCode_ERR_PERMISSION_DENY {
		t.Fatalf("玩家调用 code=%v want ERR_PERMISSION_DENY", resp.GetCode())
	}
}
