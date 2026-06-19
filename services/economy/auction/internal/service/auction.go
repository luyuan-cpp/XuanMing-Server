// Package service 是 auction 服务的 gRPC service 层(2026-06-19)。
//
// 职责:
//   - 实现 auctionv1.AuctionServiceServer
//   - 从 ctx 取 JWT player_id(R5:override request,防伪造他人身份)
//   - proto Request/Response ↔ biz 入参/出参互转
//   - errcode.Code → commonv1.ErrCode 1:1 映射
//
// 协议原则(R5):PlaceOrder 的 seller、Bid 的 buyer、Cancel/List 的 player 一律以 ctx 中的
// JWT player_id 为准,忽略请求体里的对应字段;player_id=0 → ERR_UNAUTHORIZED。
package service

import (
	"context"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	auctionv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/auction/v1"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"

	"github.com/luyuancpp/pandora/services/economy/auction/internal/biz"
	"github.com/luyuancpp/pandora/services/economy/auction/internal/data"
)

// AuctionService 实现 auctionv1.AuctionServiceServer。
type AuctionService struct {
	auctionv1.UnimplementedAuctionServiceServer
	uc *biz.AuctionUsecase
}

// NewAuctionService 构造。
func NewAuctionService(uc *biz.AuctionUsecase) *AuctionService {
	return &AuctionService{uc: uc}
}

// PlaceOrder 卖家挂单。seller 以 JWT ctx 为准(R5)。
func (s *AuctionService) PlaceOrder(ctx context.Context, req *auctionv1.PlaceOrderRequest) (*auctionv1.PlaceOrderResponse, error) {
	ownerID := callerID(ctx)
	if ownerID == 0 {
		return &auctionv1.PlaceOrderResponse{Code: commonv1.ErrCode_ERR_UNAUTHORIZED}, nil
	}
	order, err := s.uc.PlaceOrder(ctx, ownerID, req.GetMarketId(), req.GetItemConfigId(), req.GetQuantity(), req.GetPrice(), req.GetIdempotencyKey())
	if err != nil {
		return &auctionv1.PlaceOrderResponse{Code: toProtoCode(err)}, nil
	}
	return &auctionv1.PlaceOrderResponse{
		Code:           commonv1.ErrCode_OK,
		OrderId:        order.GetOrderId(),
		Status:         order.GetStatus(),
		FilledQuantity: order.GetFilledQuantity(),
	}, nil
}

// Bid 买家出价。buyer 以 JWT ctx 为准(R5)。
func (s *AuctionService) Bid(ctx context.Context, req *auctionv1.BidRequest) (*auctionv1.BidResponse, error) {
	ownerID := callerID(ctx)
	if ownerID == 0 {
		return &auctionv1.BidResponse{Code: commonv1.ErrCode_ERR_UNAUTHORIZED}, nil
	}
	order, err := s.uc.Bid(ctx, ownerID, req.GetMarketId(), req.GetItemConfigId(), req.GetQuantity(), req.GetPrice(), req.GetIdempotencyKey())
	if err != nil {
		return &auctionv1.BidResponse{Code: toProtoCode(err)}, nil
	}
	return &auctionv1.BidResponse{
		Code:           commonv1.ErrCode_OK,
		OrderId:        order.GetOrderId(),
		Status:         order.GetStatus(),
		FilledQuantity: order.GetFilledQuantity(),
	}, nil
}

// CancelOrder 撤单。player 以 JWT ctx 为准(R5)。
func (s *AuctionService) CancelOrder(ctx context.Context, req *auctionv1.CancelOrderRequest) (*auctionv1.CancelOrderResponse, error) {
	ownerID := callerID(ctx)
	if ownerID == 0 {
		return &auctionv1.CancelOrderResponse{Code: commonv1.ErrCode_ERR_UNAUTHORIZED}, nil
	}
	if req.GetMarketId() == 0 || req.GetOrderId() == 0 {
		return &auctionv1.CancelOrderResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	if err := s.uc.CancelOrder(ctx, ownerID, req.GetMarketId(), req.GetOrderId()); err != nil {
		return &auctionv1.CancelOrderResponse{Code: toProtoCode(err)}, nil
	}
	return &auctionv1.CancelOrderResponse{Code: commonv1.ErrCode_OK}, nil
}

// ListMarket 看市场订单簿。
func (s *AuctionService) ListMarket(ctx context.Context, req *auctionv1.ListMarketRequest) (*auctionv1.ListMarketResponse, error) {
	if callerID(ctx) == 0 {
		return &auctionv1.ListMarketResponse{Code: commonv1.ErrCode_ERR_UNAUTHORIZED}, nil
	}
	orders, err := s.uc.ListMarket(ctx, req.GetMarketId(), data.Side(req.GetSide()), int(req.GetLimit()))
	if err != nil {
		return &auctionv1.ListMarketResponse{Code: toProtoCode(err)}, nil
	}
	return &auctionv1.ListMarketResponse{Code: commonv1.ErrCode_OK, Orders: orders}, nil
}

// ListMyOrders 看自己的挂单 / 出价。player 以 JWT ctx 为准(R5)。
func (s *AuctionService) ListMyOrders(ctx context.Context, req *auctionv1.ListMyOrdersRequest) (*auctionv1.ListMyOrdersResponse, error) {
	ownerID := callerID(ctx)
	if ownerID == 0 {
		return &auctionv1.ListMyOrdersResponse{Code: commonv1.ErrCode_ERR_UNAUTHORIZED}, nil
	}
	orders, err := s.uc.ListMyOrders(ctx, ownerID, req.GetActiveOnly())
	if err != nil {
		return &auctionv1.ListMyOrdersResponse{Code: toProtoCode(err)}, nil
	}
	return &auctionv1.ListMyOrdersResponse{Code: commonv1.ErrCode_OK, Orders: orders}, nil
}

// ── 辅助 ──────────────────────────────────────────────────────────────────────

// callerID 从 ctx 取 JWT 注入的 player_id。
func callerID(ctx context.Context) uint64 {
	id, _ := ctx.Value(plog.CtxKeyPlayerID).(uint64)
	return id
}

// toProtoCode 把 pkg/errcode 1:1 映射成 proto enum(数值相同)。
func toProtoCode(err error) commonv1.ErrCode {
	return commonv1.ErrCode(errcode.As(err))
}
