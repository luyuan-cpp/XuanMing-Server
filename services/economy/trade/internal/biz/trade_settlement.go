// trade_settlement.go 是交易结算跨分片落点 + ledger 腿(leg)幂等键口径的服务内纯逻辑(nil-safe 接线)。
//
// 背景(decision-revisit-trade-storage.md §4/§5):交易结算是跨人写(买家扣金币 / 卖家收金币 /
// 托管物品转买家背包),按 player_id 分片后买卖双方资源落不同分片 / slot,跨 slot 无原子事务
// (§3 撞 CROSSSLOT)。结算拆成 Kafka 事件 + 幂等消费,ledger 每笔挂幂等键 uk(player_id, order_id, leg)
// `INSERT IGNORE`(§5 表),order_id 贯穿各腿与补偿,是跨服务对账主键(§5.2 铁律:
// 任何扣钱/扣物/发货/退款都必须挂幂等键落库)。
//
// 本文件只落服务内纯逻辑,不改现状 Redis WATCH/MULTI/EXEC + ResourceLedger(order_id 幂等)实现:
//   - 统一结算 ledger 腿幂等键口径(SettlementLegKey),与 §5 表 uk(player_id, order_id, leg) 对齐,
//     避免分片落地时各腿(扣款 / 收款 / 发货 / 退款)消费者口径漂移。
//   - 用确定性 cellroute.Router 把买卖双方解析到各自 owner (region, cell),判定本笔结算是否
//     跨分片 / 跨 region(CrossShardSettlement / CrossRegionSettlement),作为可观测信号。
//
// 边界(AGENTS.md §11.1):真正的分片 MySQL/TiDB / Kafka 结算出箱消费者 / 跨分片资源对转属
// 基础设施,由 Codex/人接;本轮 router 为 nil(单 Cell)时行为不变,只在注入后打观测日志。
package biz

import (
	"context"
	"fmt"

	plog "github.com/luyuancpp/pandora/pkg/log"
)

// SettlementLeg 是结算账本的一条腿(资源转移方向)。分片下每条腿在各自 owner 分片幂等写。
type SettlementLeg string

const (
	// LegBuyerDebit 买家分片扣金币。
	LegBuyerDebit SettlementLeg = "buyer_debit"
	// LegSellerCredit 卖家分片收金币。
	LegSellerCredit SettlementLeg = "seller_credit"
	// LegItemTransfer 托管物品转买家背包。
	LegItemTransfer SettlementLeg = "item_transfer"
	// LegRefund 补偿 / 撤单退款(单独腿,退款只退一次,§5 表)。
	LegRefund SettlementLeg = "refund"
)

// SettlementLegKey 是结算账本一条腿的幂等键口径(对账主键)。
//
// 口径统一:canonical "order_id:player_id:leg",与 decision-revisit-trade-storage.md §5 表
// 「ledger 每笔 uk(player_id, order_id, leg) INSERT IGNORE」同维度。重复消费同一腿命中唯一键
// → 只转移一次(不变量 §9.7)。纯函数,确定性;order_id / player_id 为 0 时键含 0
// (调用方应先校验非 0)。
func SettlementLegKey(orderID, playerID uint64, leg SettlementLeg) string {
	return fmt.Sprintf("%d:%d:%s", orderID, playerID, leg)
}

// TradeParties 是一笔交易买卖双方的 owner 分片落点(只取跨分片判定需要的维度)。
type TradeParties struct {
	BuyerRegionID  uint32
	BuyerCellID    uint32
	SellerRegionID uint32
	SellerCellID   uint32
}

// CrossShardSettlement 判断一笔结算是否跨分片(买卖双方落不同 Cell)。
// 同 Cell(单分片本地结算)→ false。
func (p TradeParties) CrossShardSettlement() bool {
	return p.BuyerRegionID != p.SellerRegionID || p.BuyerCellID != p.SellerCellID
}

// CrossRegionSettlement 判断一笔结算是否跨 region(买卖双方 owner region 不同)。
// 跨 region 交易按 §4.4「最小跨 region 通道」处理,占比应极低;同 region → false。
func (p TradeParties) CrossRegionSettlement() bool {
	return p.BuyerRegionID != p.SellerRegionID
}

// tradeParties 解析一笔交易买卖双方的 owner 分片落点 (region, cell)。
// router 为 nil(单 Cell / dev)或任一方路由失败 / player_id 为 0 → 返回 (TradeParties{}, false),
// 调用方退化为不做观测(单分片本地结算语义不变)。
func (u *TradeUsecase) tradeParties(buyerID, sellerID uint64) (TradeParties, bool) {
	if u.router == nil || buyerID == 0 || sellerID == 0 {
		return TradeParties{}, false
	}
	bloc, err := u.router.Route(buyerID)
	if err != nil {
		return TradeParties{}, false
	}
	sloc, err := u.router.Route(sellerID)
	if err != nil {
		return TradeParties{}, false
	}
	return TradeParties{
		BuyerRegionID:  bloc.RegionID,
		BuyerCellID:    bloc.CellID,
		SellerRegionID: sloc.RegionID,
		SellerCellID:   sloc.CellID,
	}, true
}

// logSettlementRouting 在 router 注入后,把一笔结算成功的跨分片落点打成观测日志。
//
// 仅可观测,不改结算路径:分片 MySQL/TiDB / Kafka 结算出箱消费者 / 跨分片资源对转属基础设施
// (AGENTS.md §11.1,由 Codex/人接);本处只暴露「本笔结算是否跨分片 / 跨 region」信号,供分片
// 上线前评估跨 region 交易占比。router 为 nil(单 Cell)时不调用此路径,行为不变。
// 跨 region 结算额外带 sample_leg_key(SettlementLegKey 口径样例,买家扣款腿),作对账键排障锚点。
func (u *TradeUsecase) logSettlementRouting(ctx context.Context, orderID, buyerID, sellerID uint64) {
	parties, ok := u.tradeParties(buyerID, sellerID)
	if !ok {
		return
	}
	if parties.CrossRegionSettlement() {
		plog.With(ctx).Infow("msg", "trade_settlement_routing",
			"order_id", orderID,
			"cross_shard", true,
			"cross_region", true,
			"buyer_region", parties.BuyerRegionID,
			"seller_region", parties.SellerRegionID,
			"sample_leg_key", SettlementLegKey(orderID, buyerID, LegBuyerDebit))
		return
	}
	plog.With(ctx).Debugw("msg", "trade_settlement_routing",
		"order_id", orderID,
		"cross_shard", parties.CrossShardSettlement(),
		"cross_region", false)
}
