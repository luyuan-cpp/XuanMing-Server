// inventory_sharding.go 是 inventory 侧拍卖成交跨人对转跨分片落点 + ledger 腿(leg)幂等键口径的
// 服务内纯逻辑(nil-safe 接线)。
//
// 背景(scale-cellular-20m.md §4.2 owner 不变量 + decision-revisit-auction-engine.md):背包 /
// 货币是玩家 owner 数据,同一 player_id 的背包必落同一 owner cell。拍卖成交(SettleAuctionMatch)
// 是跨人对转——卖家交付道具 + 收金币,买家付金币 + 收道具,买卖双方背包按 player_id 分片后落不同
// cell,跨 cell 无原子本地事务(现状"一个本地事务对转"只在双方同 cell 时成立)。分片落地时结算
// 须拆 Kafka 事件 + 幂等消费,每条腿在各自 owner cell 幂等写,match_id 贯穿各腿做对账主键
// (不变量 §9.2 战斗/成交幂等、§9.7 对转资源扣减原子 + 补偿幂等键)。
//
// 本文件只落服务内纯逻辑,不改现状 SettleAuctionMatch(单本地事务 + match_id 幂等键)实现:
//   - 统一拍卖结算 ledger 腿幂等键口径(AuctionLegKey),与现状幂等键 "auction:settle:<match_id>"
//     同源、再细分到 (player_id, leg),为分片落地各腿消费者提供单一口径,避免口径漂移。
//   - 用确定性 cellroute.Router 把买卖双方解析到各自 owner (region, cell),判定本笔成交是否
//     跨分片 / 跨 region,作为可观测信号,供分片上线评估跨 region 成交占比。
//
// 边界(AGENTS.md §11.1):真正的背包按 owner cell 分片 / Kafka 结算出箱消费者 / 跨 cell 资源
// 对转属基础设施,由 Codex/人接;本轮 router 为 nil(单 Cell)时行为不变,只在注入后打观测日志。
package biz

import (
	"context"
	"fmt"

	plog "github.com/luyuancpp/pandora/pkg/log"
)

// AuctionLeg 是拍卖成交对转账本的一条腿(资产转移方向)。分片下每条腿在各自 owner cell 幂等写。
type AuctionLeg string

const (
	// LegSellerDeliver 卖家 escrow 交付道具(扣卖家托管道具)。
	LegSellerDeliver AuctionLeg = "seller_deliver"
	// LegSellerReceive 卖家收金币。
	LegSellerReceive AuctionLeg = "seller_receive"
	// LegBuyerPay 买家 escrow 付金币(扣买家托管金币)。
	LegBuyerPay AuctionLeg = "buyer_pay"
	// LegBuyerReceive 买家收道具。
	LegBuyerReceive AuctionLeg = "buyer_receive"
)

// AuctionLegKey 是拍卖成交对转账本一条腿的幂等键口径(对账主键)。
//
// 口径统一:canonical "auction:settle:<match_id>:<player_id>:<leg>",与现状 SettleAuctionMatch
// 幂等键 "auction:settle:<match_id>" 同源,再细分到 (player_id, leg),分片落地时每条腿在各自 owner
// cell 幂等写,重复消费同一腿命中唯一键只对转一次(不变量 §9.2 / §9.7)。纯函数,确定性;
// match_id / player_id 为 0 时键含 0(调用方应先校验非 0)。
func AuctionLegKey(matchID, playerID uint64, leg AuctionLeg) string {
	return fmt.Sprintf("auction:settle:%d:%d:%s", matchID, playerID, leg)
}

// AuctionParties 是一笔拍卖成交买卖双方的 owner 分片落点(只取跨分片判定需要的维度)。
type AuctionParties struct {
	SellerRegionID uint32
	SellerCellID   uint32
	BuyerRegionID  uint32
	BuyerCellID    uint32
}

// CrossShardSettlement 判断一笔成交对转是否跨分片(买卖双方背包落不同 Cell)。
// 同 Cell(单分片本地对转,现状单事务成立)→ false。
func (p AuctionParties) CrossShardSettlement() bool {
	return p.SellerRegionID != p.BuyerRegionID || p.SellerCellID != p.BuyerCellID
}

// CrossRegionSettlement 判断一笔成交对转是否跨 region(买卖双方 owner region 不同)。
// 跨 region 成交按最小跨 region 通道异步对转,占比应低;同 region → false。
func (p AuctionParties) CrossRegionSettlement() bool {
	return p.SellerRegionID != p.BuyerRegionID
}

// auctionParties 解析一笔成交买卖双方的 owner 分片落点 (region, cell)。
// router 为 nil(单 Cell / dev)或任一方路由失败 / player_id 为 0 → 返回 (AuctionParties{}, false),
// 调用方退化为不做观测(单分片本地对转语义不变)。
func (u *InventoryUsecase) auctionParties(sellerID, buyerID uint64) (AuctionParties, bool) {
	if u.router == nil || sellerID == 0 || buyerID == 0 {
		return AuctionParties{}, false
	}
	sloc, err := u.router.Route(sellerID)
	if err != nil {
		return AuctionParties{}, false
	}
	bloc, err := u.router.Route(buyerID)
	if err != nil {
		return AuctionParties{}, false
	}
	return AuctionParties{
		SellerRegionID: sloc.RegionID,
		SellerCellID:   sloc.CellID,
		BuyerRegionID:  bloc.RegionID,
		BuyerCellID:    bloc.CellID,
	}, true
}

// logAuctionSettlementRouting 在 router 注入后,把一笔成交对转成功的跨分片落点打成观测日志。
//
// 仅可观测,不改对转路径:背包按 owner cell 分片 / Kafka 结算出箱消费者 / 跨 cell 资源对转属
// 基础设施(AGENTS.md §11.1,由 Codex/人接);本处只暴露「本笔成交是否跨分片 / 跨 region」信号,
// 供分片上线前评估跨 region 成交占比。router 为 nil(单 Cell)时不调用此路径,行为不变。
// 跨 region 成交额外带 sample_leg_key(AuctionLegKey 口径样例,卖家交付腿),作对账键排障锚点。
func (u *InventoryUsecase) logAuctionSettlementRouting(ctx context.Context, matchID, sellerID, buyerID uint64) {
	parties, ok := u.auctionParties(sellerID, buyerID)
	if !ok {
		return
	}
	if parties.CrossRegionSettlement() {
		plog.With(ctx).Infow("msg", "auction_settlement_routing",
			"match_id", matchID,
			"cross_shard", true,
			"cross_region", true,
			"seller_region", parties.SellerRegionID,
			"buyer_region", parties.BuyerRegionID,
			"sample_leg_key", AuctionLegKey(matchID, sellerID, LegSellerDeliver))
		return
	}
	plog.With(ctx).Debugw("msg", "auction_settlement_routing",
		"match_id", matchID,
		"cross_shard", parties.CrossShardSettlement(),
		"cross_region", false)
}
