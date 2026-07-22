package service

import (
	"context"
	"testing"

	plog "github.com/luyuancpp/pandora/pkg/log"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	tradev1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/trade/v1"
)

// TestTradeServiceRejectsBodyIdentityWithoutJWT 防止旧协议里的 seller_id/player_id
// 被误当作可信身份：没有 JWT 注入的 caller 时，所有玩家接口都必须先拒绝。
func TestTradeServiceRejectsBodyIdentityWithoutJWT(t *testing.T) {
	svc := NewTradeService(nil)
	ctx := context.Background()

	tests := []struct {
		name string
		call func() commonv1.ErrCode
	}{
		{
			name: "创建订单伪造seller",
			call: func() commonv1.ErrCode {
				resp, err := svc.CreateOrder(ctx, &tradev1.CreateOrderRequest{SellerId: 999, BuyerId: 2})
				if err != nil {
					t.Fatalf("CreateOrder transport err: %v", err)
				}
				return resp.GetCode()
			},
		},
		{
			name: "确认订单伪造player",
			call: func() commonv1.ErrCode {
				resp, err := svc.ConfirmOrder(ctx, &tradev1.ConfirmOrderRequest{PlayerId: 999, OrderId: 1})
				if err != nil {
					t.Fatalf("ConfirmOrder transport err: %v", err)
				}
				return resp.GetCode()
			},
		},
		{
			name: "取消订单伪造player",
			call: func() commonv1.ErrCode {
				resp, err := svc.CancelOrder(ctx, &tradev1.CancelOrderRequest{PlayerId: 999, OrderId: 1})
				if err != nil {
					t.Fatalf("CancelOrder transport err: %v", err)
				}
				return resp.GetCode()
			},
		},
		{
			name: "查询订单伪造player",
			call: func() commonv1.ErrCode {
				resp, err := svc.ListMyOrders(ctx, &tradev1.ListMyOrdersRequest{PlayerId: 999})
				if err != nil {
					t.Fatalf("ListMyOrders transport err: %v", err)
				}
				return resp.GetCode()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.call(); got != commonv1.ErrCode_ERR_UNAUTHORIZED {
				t.Fatalf("无 JWT 时必须拒绝 body 身份: got=%s", got)
			}
		})
	}
}

// TestTradeServiceValidatesOrderIDBeforeUsecase 授权通过后，零订单号必须在 service
// 边界被拒，不能进入状态机或资源结算路径。
func TestTradeServiceValidatesOrderIDBeforeUsecase(t *testing.T) {
	svc := NewTradeService(nil)
	ctx := context.WithValue(context.Background(), plog.CtxKeyPlayerID, uint64(7))

	confirm, err := svc.ConfirmOrder(ctx, &tradev1.ConfirmOrderRequest{PlayerId: 999})
	if err != nil {
		t.Fatalf("ConfirmOrder transport err: %v", err)
	}
	if confirm.GetCode() != commonv1.ErrCode_ERR_INVALID_ARG {
		t.Fatalf("零 order_id 应拒绝: got=%s", confirm.GetCode())
	}

	cancel, err := svc.CancelOrder(ctx, &tradev1.CancelOrderRequest{PlayerId: 999})
	if err != nil {
		t.Fatalf("CancelOrder transport err: %v", err)
	}
	if cancel.GetCode() != commonv1.ErrCode_ERR_INVALID_ARG {
		t.Fatalf("零 order_id 应拒绝: got=%s", cancel.GetCode())
	}
}
