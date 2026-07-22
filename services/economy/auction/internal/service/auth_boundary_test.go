package service

import (
	"context"
	"testing"

	plog "github.com/luyuancpp/pandora/pkg/log"
	auctionv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/auction/v1"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
)

// TestAuctionServiceRequiresJWTForEveryPlayerRPC 固化拍卖所有入口的统一鉴权边界。
// nil usecase 可确保未授权请求在触达业务状态机、市场锁和资产托管前就返回。
func TestAuctionServiceRequiresJWTForEveryPlayerRPC(t *testing.T) {
	svc := NewAuctionService(nil)
	ctx := context.Background()

	tests := []struct {
		name string
		call func() commonv1.ErrCode
	}{
		{
			name: "挂单",
			call: func() commonv1.ErrCode {
				resp, err := svc.PlaceOrder(ctx, &auctionv1.PlaceOrderRequest{})
				if err != nil {
					t.Fatalf("PlaceOrder transport err: %v", err)
				}
				return resp.GetCode()
			},
		},
		{
			name: "出价",
			call: func() commonv1.ErrCode {
				resp, err := svc.Bid(ctx, &auctionv1.BidRequest{})
				if err != nil {
					t.Fatalf("Bid transport err: %v", err)
				}
				return resp.GetCode()
			},
		},
		{
			name: "撤单",
			call: func() commonv1.ErrCode {
				resp, err := svc.CancelOrder(ctx, &auctionv1.CancelOrderRequest{MarketId: 1, OrderId: 1})
				if err != nil {
					t.Fatalf("CancelOrder transport err: %v", err)
				}
				return resp.GetCode()
			},
		},
		{
			name: "市场列表",
			call: func() commonv1.ErrCode {
				resp, err := svc.ListMarket(ctx, &auctionv1.ListMarketRequest{})
				if err != nil {
					t.Fatalf("ListMarket transport err: %v", err)
				}
				return resp.GetCode()
			},
		},
		{
			name: "个人订单",
			call: func() commonv1.ErrCode {
				resp, err := svc.ListMyOrders(ctx, &auctionv1.ListMyOrdersRequest{})
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
				t.Fatalf("无 JWT 时必须拒绝: got=%s", got)
			}
		})
	}
}

// TestAuctionServiceValidatesCancelIdentity 授权后，撤单必须同时带 market_id 与
// order_id；任一缺失都不能进入市场锁和托管释放路径。
func TestAuctionServiceValidatesCancelIdentity(t *testing.T) {
	svc := NewAuctionService(nil)
	ctx := context.WithValue(context.Background(), plog.CtxKeyPlayerID, uint64(7))

	for _, req := range []*auctionv1.CancelOrderRequest{
		{OrderId: 1},
		{MarketId: 1},
		{},
	} {
		resp, err := svc.CancelOrder(ctx, req)
		if err != nil {
			t.Fatalf("CancelOrder transport err: %v", err)
		}
		if resp.GetCode() != commonv1.ErrCode_ERR_INVALID_ARG {
			t.Fatalf("不完整撤单标识应拒绝: req=%+v code=%s", req, resp.GetCode())
		}
	}
}
