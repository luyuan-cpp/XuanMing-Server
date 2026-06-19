// Package biz 是 inventory 服务的业务逻辑层(W5 ③,2026-06-18)。
//
// 职责(docs/design/go-services.md §2.9 economy 域):
//   - 背包道具持有 / 货币余额读
//   - 系统驱动幂等发放(GrantItems:战后掉落 / 活动 / 购买到账)
//   - 大厅态道具使用(UseItem:开箱 / 经验书)与出售换金币(SellItem)
//
// 边界(ds-arch.md §0.1):战斗内即时用道具 / 出装 / 购买道具走 UE GAS,不经 gRPC。
//
// 关键不变量(CLAUDE.md §9.7):发放 / 扣减必须原子 + 幂等键;校验数量在 data 层
// SELECT ... FOR UPDATE 锁行内做,避免并发超扣。usable / sellable 规则在 biz 层用配置裁决。
package biz

import (
	"context"
	"fmt"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"

	"github.com/luyuancpp/pandora/services/economy/inventory/internal/conf"
	"github.com/luyuancpp/pandora/services/economy/inventory/internal/data"
)

// InventoryUsecase 是 inventory 服务业务逻辑核心。
type InventoryUsecase struct {
	repo data.InventoryRepo
	cfg  conf.InventoryConf
}

// NewInventoryUsecase 构造。
func NewInventoryUsecase(repo data.InventoryRepo, cfg conf.InventoryConf) *InventoryUsecase {
	return &InventoryUsecase{repo: repo, cfg: cfg}
}

// GetInventory 读玩家背包(货币 + 道具堆叠)。
func (u *InventoryUsecase) GetInventory(ctx context.Context, playerID uint64) (int64, []data.ItemStack, error) {
	if playerID == 0 {
		return 0, nil, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	return u.repo.GetInventory(ctx, playerID)
}

// GrantItems 幂等发放道具 + 货币(系统驱动,idempotency_key 防重复入账)。
func (u *InventoryUsecase) GrantItems(ctx context.Context, playerID uint64, items []data.ItemGrant, gold int64, idempotencyKey string) (int64, error) {
	if playerID == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if idempotencyKey == "" {
		return 0, errcode.New(errcode.ErrInvalidArg, "idempotency_key required")
	}
	if len(items) == 0 && gold == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "nothing to grant")
	}
	if gold < 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "gold must not be negative")
	}
	for _, it := range items {
		if it.ItemConfigID == 0 {
			return 0, errcode.New(errcode.ErrInvalidArg, "item_config_id required")
		}
		if it.Count <= 0 {
			return 0, errcode.New(errcode.ErrInvalidArg, "count must be positive: item=%d", it.ItemConfigID)
		}
	}
	detail := fmt.Sprintf("grant items=%d gold=%d", len(items), gold)
	newGold, already, err := u.repo.GrantItems(ctx, playerID, items, gold, idempotencyKey, detail)
	if err != nil {
		return 0, err
	}
	if already {
		plog.With(ctx).Infow("msg", "grant_items_idempotent_hit",
			"player_id", playerID, "idempotency_key", idempotencyKey, "gold", newGold)
	}
	return newGold, nil
}

// UseItem 大厅态使用消耗品(不可大厅使用 → ErrInventoryItemNotUsable;数量不足 → ErrInventoryInsufficient)。
func (u *InventoryUsecase) UseItem(ctx context.Context, playerID uint64, itemConfigID uint32, count int64, idempotencyKey string) (int64, error) {
	if playerID == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if itemConfigID == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "item_config_id required")
	}
	if count <= 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "count must be positive")
	}
	if idempotencyKey == "" {
		return 0, errcode.New(errcode.ErrInvalidArg, "idempotency_key required")
	}
	rule := u.cfg.RuleOf(itemConfigID)
	if rule == nil || !rule.Usable {
		return 0, errcode.New(errcode.ErrInventoryItemNotUsable, "item not usable in lobby: %d", itemConfigID)
	}
	detail := fmt.Sprintf("use item=%d count=%d", itemConfigID, count)
	remaining, already, err := u.repo.UseItem(ctx, playerID, itemConfigID, count, idempotencyKey, detail)
	if err != nil {
		return 0, err
	}
	if already {
		plog.With(ctx).Infow("msg", "use_item_idempotent_hit",
			"player_id", playerID, "idempotency_key", idempotencyKey, "item", itemConfigID, "remaining", remaining)
	}
	return remaining, nil
}

// SellItem 出售道具换金币(不可出售 → ErrInventoryNotSellable;数量不足 → ErrInventoryInsufficient)。
func (u *InventoryUsecase) SellItem(ctx context.Context, playerID uint64, itemConfigID uint32, count int64, idempotencyKey string) (int64, int64, error) {
	if playerID == 0 {
		return 0, 0, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if itemConfigID == 0 {
		return 0, 0, errcode.New(errcode.ErrInvalidArg, "item_config_id required")
	}
	if count <= 0 {
		return 0, 0, errcode.New(errcode.ErrInvalidArg, "count must be positive")
	}
	if idempotencyKey == "" {
		return 0, 0, errcode.New(errcode.ErrInvalidArg, "idempotency_key required")
	}
	rule := u.cfg.RuleOf(itemConfigID)
	if rule == nil || !rule.Sellable {
		return 0, 0, errcode.New(errcode.ErrInventoryNotSellable, "item not sellable: %d", itemConfigID)
	}
	// 防御:可出售必须单价 > 0(启动时已校验,此处兜底防配置漂移/负价扣币)。
	if rule.SellUnitPrice <= 0 {
		return 0, 0, errcode.New(errcode.ErrInventoryNotSellable, "item not sellable (non-positive price): %d", itemConfigID)
	}
	// 防御:单价 * 数量 int64 溢出会变负数进而少扣/反加金币,溢出直接拒。
	gold, ok := safeMulInt64(rule.SellUnitPrice, count)
	if !ok || gold <= 0 {
		return 0, 0, errcode.New(errcode.ErrInvalidArg, "sell amount overflow item=%d price=%d count=%d", itemConfigID, rule.SellUnitPrice, count)
	}
	detail := fmt.Sprintf("sell item=%d count=%d gold=%d", itemConfigID, count, gold)
	remaining, newGold, already, err := u.repo.SellItem(ctx, playerID, itemConfigID, count, gold, idempotencyKey, detail)
	if err != nil {
		return 0, 0, err
	}
	if already {
		plog.With(ctx).Infow("msg", "sell_item_idempotent_hit",
			"player_id", playerID, "idempotency_key", idempotencyKey, "item", itemConfigID, "remaining", remaining, "gold", newGold)
	}
	return remaining, newGold, nil
}

// SettleAuctionMatch 原子结算一笔拍卖成交(系统驱动,幂等键基于 match_id)。
//
// 在 inventory 一个本地事务里完成卖↔买双方资产对转:
//   - 卖家:扣 quantity 个 itemConfigID,加 quantity*unitPrice 金币;
//   - 买家:扣 quantity*unitPrice 金币,加 quantity 个 itemConfigID。
//
// 幂等键 = "auction:settle:<match_id>",同一成交重复结算只生效一次(不变量 §9.2 / §9.7)。
// 卖家道具不足 / 买家金币不足 → ErrInventoryInsufficient,整笔回滚。
func (u *InventoryUsecase) SettleAuctionMatch(ctx context.Context, matchID, sellerID, buyerID uint64, itemConfigID uint32, quantity, unitPrice int64) error {
	if matchID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "match_id required")
	}
	if sellerID == 0 || buyerID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "seller_id / buyer_id required")
	}
	if sellerID == buyerID {
		// 自成交净额为零且会让同一玩家同 key 写两条流水冲突;视为非法,撮合侧应避免自撮合。
		return errcode.New(errcode.ErrInvalidArg, "seller and buyer must differ: %d", sellerID)
	}
	if itemConfigID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "item_config_id required")
	}
	if quantity <= 0 {
		return errcode.New(errcode.ErrInvalidArg, "quantity must be positive")
	}
	if unitPrice <= 0 {
		return errcode.New(errcode.ErrInvalidArg, "unit_price must be positive")
	}
	totalGold, ok := safeMulInt64(unitPrice, quantity)
	if !ok || totalGold <= 0 {
		return errcode.New(errcode.ErrInvalidArg, "settle amount overflow match=%d price=%d qty=%d", matchID, unitPrice, quantity)
	}
	idempotencyKey := fmt.Sprintf("auction:settle:%d", matchID)
	detail := fmt.Sprintf("auction settle match=%d item=%d qty=%d gold=%d", matchID, itemConfigID, quantity, totalGold)
	already, err := u.repo.SettleAuctionMatch(ctx, matchID, sellerID, buyerID, itemConfigID, quantity, totalGold, idempotencyKey, detail)
	if err != nil {
		return err
	}
	if already {
		plog.With(ctx).Infow("msg", "auction_settle_idempotent_hit",
			"match_id", matchID, "seller_id", sellerID, "buyer_id", buyerID, "item", itemConfigID, "qty", quantity, "gold", totalGold)
	}
	return nil
}

// safeMulInt64 做溢出安全的 int64 乘法(a,b 均已保证为正)。溢出返回 (0, false)。
func safeMulInt64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	c := a * b
	if c/b != a {
		return 0, false
	}
	return c, true
}
