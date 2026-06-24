// Package service 是 inventory 服务的 gRPC service 层(W5 ③,2026-06-18)。
//
// 职责:
//   - 实现 inventoryv1.InventoryServiceServer 接口
//   - proto Request/Response ↔ biz 入参/出参互转
//   - errcode.Code → commonv1.ErrCode 1:1 映射
//
// 鉴权边界(2026-06-17 安全审查修复):
//   - 客户端 RPC(GetInventory / UseItem / SellItem):以 Envoy jwt_authn 注入的调用者身份为准
//     (pmw.PlayerIDFromContext),**不信任请求体 player_id**;请求体 player_id 与调用者不一致直接拒,
//     防止伪造 player_id 读 / 用 / 卖他人背包。
//   - 系统 RPC(GrantItems:战后掉落 / 活动 / 购买到账):只允许后端内部直连(无 JWT,callerID==0);
//     带玩家 JWT 的客户端调用一律拒绝,杜绝玩家自助发道具。并且不在 Envoy 暴露 GrantItems 路由。
package service

import (
	"context"

	"github.com/luyuancpp/pandora/pkg/errcode"
	pmw "github.com/luyuancpp/pandora/pkg/middleware"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	inventoryv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/inventory/v1"

	"github.com/luyuancpp/pandora/services/economy/inventory/internal/biz"
	"github.com/luyuancpp/pandora/services/economy/inventory/internal/data"
)

// callerPlayerID 取经鉴权的调用者身份并校验请求体 player_id 一致性(客户端 RPC 用)。
//
//	未鉴权(callerID==0,直连内网无网关注入) → ERR_UNAUTHORIZED
//	请求体 player_id 与调用者不一致           → ERR_PERMISSION_DENY
//
// 返回权威 player_id(= 调用者身份),后续业务一律用它,不信任 req.PlayerId。
func callerPlayerID(ctx context.Context, reqPlayerID uint64) (uint64, commonv1.ErrCode) {
	callerID := pmw.PlayerIDFromContext(ctx)
	if callerID == 0 {
		return 0, commonv1.ErrCode_ERR_UNAUTHORIZED
	}
	if reqPlayerID != 0 && reqPlayerID != callerID {
		return 0, commonv1.ErrCode_ERR_PERMISSION_DENY
	}
	return callerID, commonv1.ErrCode_OK
}

// InventoryService 实现 inventoryv1.InventoryServiceServer。
type InventoryService struct {
	inventoryv1.UnimplementedInventoryServiceServer
	uc *biz.InventoryUsecase
}

// NewInventoryService 构造。
func NewInventoryService(uc *biz.InventoryUsecase) *InventoryService {
	return &InventoryService{uc: uc}
}

// GetInventory 读玩家背包(货币 + 道具堆叠)。以调用者身份为准。
func (s *InventoryService) GetInventory(ctx context.Context, req *inventoryv1.GetInventoryRequest) (*inventoryv1.GetInventoryResponse, error) {
	playerID, code := callerPlayerID(ctx, req.GetPlayerId())
	if code != commonv1.ErrCode_OK {
		return &inventoryv1.GetInventoryResponse{Code: code}, nil
	}
	gold, items, err := s.uc.GetInventory(ctx, playerID)
	if err != nil {
		return &inventoryv1.GetInventoryResponse{Code: toProtoCode(err)}, nil
	}
	out := make([]*inventoryv1.ItemStack, 0, len(items))
	for _, it := range items {
		out = append(out, &inventoryv1.ItemStack{ItemConfigId: it.ItemConfigID, Count: it.Count})
	}
	return &inventoryv1.GetInventoryResponse{
		Code:      commonv1.ErrCode_OK,
		Inventory: &inventoryv1.Inventory{PlayerId: playerID, Gold: gold, Items: out},
	}, nil
}

// GrantItems 幂等发放道具 + 货币(系统接口,仅后端内部可调)。
func (s *InventoryService) GrantItems(ctx context.Context, req *inventoryv1.GrantItemsRequest) (*inventoryv1.GrantItemsResponse, error) {
	// 系统接口:经 Envoy 的客户端调用必带 JWT(callerID>0)→ 一律拒绝,杜绝玩家自助发道具。
	// 合法调用者是后端内部服务直连(无 x-pandora-player-id 头 → callerID==0)。
	if pmw.PlayerIDFromContext(ctx) != 0 {
		return &inventoryv1.GrantItemsResponse{Code: commonv1.ErrCode_ERR_PERMISSION_DENY}, nil
	}
	if req.GetPlayerId() == 0 {
		return &inventoryv1.GrantItemsResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	items := make([]data.ItemGrant, 0, len(req.GetItems()))
	for _, it := range req.GetItems() {
		items = append(items, data.ItemGrant{ItemConfigID: it.GetItemConfigId(), Count: it.GetCount()})
	}
	gold, err := s.uc.GrantItems(ctx, req.GetPlayerId(), items, req.GetGold(), req.GetIdempotencyKey())
	if err != nil {
		return &inventoryv1.GrantItemsResponse{Code: toProtoCode(err)}, nil
	}
	return &inventoryv1.GrantItemsResponse{Code: commonv1.ErrCode_OK, Gold: gold}, nil
}

// UseItem 大厅态使用消耗品。以调用者身份为准。
func (s *InventoryService) UseItem(ctx context.Context, req *inventoryv1.UseItemRequest) (*inventoryv1.UseItemResponse, error) {
	playerID, code := callerPlayerID(ctx, req.GetPlayerId())
	if code != commonv1.ErrCode_OK {
		return &inventoryv1.UseItemResponse{Code: code}, nil
	}
	remaining, err := s.uc.UseItem(ctx, playerID, req.GetItemConfigId(), req.GetCount(), req.GetIdempotencyKey())
	if err != nil {
		return &inventoryv1.UseItemResponse{Code: toProtoCode(err)}, nil
	}
	return &inventoryv1.UseItemResponse{Code: commonv1.ErrCode_OK, Remaining: remaining}, nil
}

// SellItem 出售道具换金币。以调用者身份为准。
func (s *InventoryService) SellItem(ctx context.Context, req *inventoryv1.SellItemRequest) (*inventoryv1.SellItemResponse, error) {
	playerID, code := callerPlayerID(ctx, req.GetPlayerId())
	if code != commonv1.ErrCode_OK {
		return &inventoryv1.SellItemResponse{Code: code}, nil
	}
	remaining, gold, err := s.uc.SellItem(ctx, playerID, req.GetItemConfigId(), req.GetCount(), req.GetIdempotencyKey())
	if err != nil {
		return &inventoryv1.SellItemResponse{Code: toProtoCode(err)}, nil
	}
	return &inventoryv1.SellItemResponse{Code: commonv1.ErrCode_OK, Remaining: remaining, Gold: gold}, nil
}

// SettleAuctionMatch 原子结算拍卖成交(系统接口,仅后端内部直连)。
//
// 鉴权同 GrantItems:经 Envoy 的客户端调用必带 JWT(callerID>0)→ 一律拒绝,杜绝玩家自助结算
// 套现 / 刷道具;合法调用者是 auction 服务内网直连(无 x-pandora-player-id 头 → callerID==0)。
func (s *InventoryService) SettleAuctionMatch(ctx context.Context, req *inventoryv1.SettleAuctionMatchRequest) (*inventoryv1.SettleAuctionMatchResponse, error) {
	if pmw.PlayerIDFromContext(ctx) != 0 {
		return &inventoryv1.SettleAuctionMatchResponse{Code: commonv1.ErrCode_ERR_PERMISSION_DENY}, nil
	}
	err := s.uc.SettleAuctionMatch(ctx,
		req.GetMatchId(), req.GetSellerId(), req.GetBuyerId(), req.GetSellOrderId(), req.GetBuyOrderId(),
		req.GetItemConfigId(), req.GetQuantity(), req.GetUnitPrice())
	if err != nil {
		return &inventoryv1.SettleAuctionMatchResponse{Code: toProtoCode(err)}, nil
	}
	return &inventoryv1.SettleAuctionMatchResponse{Code: commonv1.ErrCode_OK}, nil
}

// FreezeForOrder 挂单冻结资产(系统接口,仅后端内部直连)。鉴权同 SettleAuctionMatch:
// 经 Envoy 的客户端调用(callerID>0)一律拒绝,合法调用者是 auction 服务内网直连。
func (s *InventoryService) FreezeForOrder(ctx context.Context, req *inventoryv1.FreezeForOrderRequest) (*inventoryv1.FreezeForOrderResponse, error) {
	if pmw.PlayerIDFromContext(ctx) != 0 {
		return &inventoryv1.FreezeForOrderResponse{Code: commonv1.ErrCode_ERR_PERMISSION_DENY}, nil
	}
	err := s.uc.FreezeForOrder(ctx,
		req.GetPlayerId(), req.GetOrderId(), biz.EscrowSide(req.GetSide()),
		req.GetItemConfigId(), req.GetQuantity(), req.GetUnitPrice())
	if err != nil {
		return &inventoryv1.FreezeForOrderResponse{Code: toProtoCode(err)}, nil
	}
	return &inventoryv1.FreezeForOrderResponse{Code: commonv1.ErrCode_OK}, nil
}

// ReleaseEscrow 退还挂单 escrow 残余(系统接口,仅后端内部直连)。鉴权同 SettleAuctionMatch。
func (s *InventoryService) ReleaseEscrow(ctx context.Context, req *inventoryv1.ReleaseEscrowRequest) (*inventoryv1.ReleaseEscrowResponse, error) {
	if pmw.PlayerIDFromContext(ctx) != 0 {
		return &inventoryv1.ReleaseEscrowResponse{Code: commonv1.ErrCode_ERR_PERMISSION_DENY}, nil
	}
	err := s.uc.ReleaseEscrow(ctx, req.GetPlayerId(), req.GetOrderId())
	if err != nil {
		return &inventoryv1.ReleaseEscrowResponse{Code: toProtoCode(err)}, nil
	}
	return &inventoryv1.ReleaseEscrowResponse{Code: commonv1.ErrCode_OK}, nil
}
func toProtoCode(err error) commonv1.ErrCode {
	return commonv1.ErrCode(errcode.As(err))
}
