// inventory_test.go — InventoryUsecase 业务逻辑单测(W5 ③,2026-06-18)。
//
// 用内存版 fakeRepo 复刻 MySQL 幂等 / 扣减语义,无需真 DB;
// 验证 usable / sellable 规则裁决、幂等键去重、数量不足拦截。
package biz

import (
	"context"
	"fmt"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/services/economy/inventory/internal/conf"
	"github.com/luyuancpp/pandora/services/economy/inventory/internal/data"
)

// ledgerEntry 复刻 MySQL inventory_ledger 一行:记录首次执行的请求指纹 + 结果快照。
type ledgerEntry struct {
	fingerprint   string
	snapRemaining int64
	snapGold      int64
}

// escrowEntry 复刻 MySQL auction_escrow 一行(挂单托管资产)。
type escrowEntry struct {
	kind         data.EscrowKind
	itemConfigID uint32
	frozenQty    int64
	frozenGold   int64
	closed       bool
}

// fakeRepo 是 data.InventoryRepo 的内存实现(复刻 MySQL 幂等 / 扣减 / 指纹快照 / escrow 语义)。
type fakeRepo struct {
	gold   map[uint64]int64
	items  map[uint64]map[uint32]int64
	ledger map[string]ledgerEntry  // key=playerID|idempotencyKey
	escrow map[string]*escrowEntry // key=playerID|order:<orderID>
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		gold:   map[uint64]int64{},
		items:  map[uint64]map[uint32]int64{},
		ledger: map[string]ledgerEntry{},
		escrow: map[string]*escrowEntry{},
	}
}

func keyOf(pid uint64, k string) string {
	return string(rune(pid)) + "|" + k
}

func escrowKeyOf(pid, orderID uint64) string {
	return keyOf(pid, fmt.Sprintf("order:%d", orderID))
}

func (f *fakeRepo) GetInventory(_ context.Context, playerID uint64) (int64, []data.ItemStack, error) {
	var out []data.ItemStack
	for id, c := range f.items[playerID] {
		if c > 0 {
			out = append(out, data.ItemStack{ItemConfigID: id, Count: c})
		}
	}
	return f.gold[playerID], out, nil
}

func (f *fakeRepo) GrantItems(_ context.Context, playerID uint64, items []data.ItemGrant, gold int64, idempotencyKey, _ string) (int64, bool, error) {
	gk := keyOf(playerID, idempotencyKey)
	fp := data.GrantFingerprint(items, gold)
	if e, ok := f.ledger[gk]; ok {
		if e.fingerprint != fp {
			return 0, false, errcode.New(errcode.ErrInventoryIdempotencyConflict, "idempotency conflict")
		}
		return e.snapGold, true, nil
	}
	if f.items[playerID] == nil {
		f.items[playerID] = map[uint32]int64{}
	}
	for _, it := range items {
		f.items[playerID][it.ItemConfigID] += it.Count
	}
	f.gold[playerID] += gold
	f.ledger[gk] = ledgerEntry{fingerprint: fp, snapGold: f.gold[playerID]}
	return f.gold[playerID], false, nil
}

func (f *fakeRepo) UseItem(_ context.Context, playerID uint64, itemConfigID uint32, count int64, idempotencyKey, _ string) (int64, bool, error) {
	gk := keyOf(playerID, idempotencyKey)
	fp := data.UseFingerprint(itemConfigID, count)
	if e, ok := f.ledger[gk]; ok {
		if e.fingerprint != fp {
			return 0, false, errcode.New(errcode.ErrInventoryIdempotencyConflict, "idempotency conflict")
		}
		return e.snapRemaining, true, nil
	}
	have := f.items[playerID][itemConfigID]
	if have == 0 {
		return 0, false, errcode.New(errcode.ErrInventoryItemNotFound, "not found")
	}
	if have < count {
		return 0, false, errcode.New(errcode.ErrInventoryInsufficient, "insufficient")
	}
	f.items[playerID][itemConfigID] = have - count
	f.ledger[gk] = ledgerEntry{fingerprint: fp, snapRemaining: have - count}
	return have - count, false, nil
}

func (f *fakeRepo) SellItem(_ context.Context, playerID uint64, itemConfigID uint32, count, gold int64, idempotencyKey, _ string) (int64, int64, bool, error) {
	gk := keyOf(playerID, idempotencyKey)
	fp := data.SellFingerprint(itemConfigID, count, gold)
	if e, ok := f.ledger[gk]; ok {
		if e.fingerprint != fp {
			return 0, 0, false, errcode.New(errcode.ErrInventoryIdempotencyConflict, "idempotency conflict")
		}
		return e.snapRemaining, e.snapGold, true, nil
	}
	have := f.items[playerID][itemConfigID]
	if have == 0 {
		return 0, 0, false, errcode.New(errcode.ErrInventoryItemNotFound, "not found")
	}
	if have < count {
		return 0, 0, false, errcode.New(errcode.ErrInventoryInsufficient, "insufficient")
	}
	f.items[playerID][itemConfigID] = have - count
	f.gold[playerID] += gold
	f.ledger[gk] = ledgerEntry{fingerprint: fp, snapRemaining: have - count, snapGold: f.gold[playerID]}
	return have - count, f.gold[playerID], false, nil
}

func (f *fakeRepo) SettleAuctionMatch(_ context.Context, _, sellerID, buyerID, sellOrderID, buyOrderID uint64, itemConfigID uint32, quantity, totalGold int64, idempotencyKey, _ string) (bool, error) {
	fp := data.AuctionSettleFingerprint(sellerID, buyerID, itemConfigID, quantity, totalGold)
	sk := keyOf(sellerID, idempotencyKey)
	bk := keyOf(buyerID, idempotencyKey)
	// 幂等命中:任一方流水已存(指纹一致)→ already 回放;指纹不一致 → 冲突。
	if e, ok := f.ledger[sk]; ok {
		if e.fingerprint != fp {
			return false, errcode.New(errcode.ErrInventoryIdempotencyConflict, "idempotency conflict")
		}
		return true, nil
	}
	if e, ok := f.ledger[bk]; ok {
		if e.fingerprint != fp {
			return false, errcode.New(errcode.ErrInventoryIdempotencyConflict, "idempotency conflict")
		}
		return true, nil
	}
	// 从双方 escrow 消费(资产已在 FreezeForOrder 冻结)。
	se := f.escrow[escrowKeyOf(sellerID, sellOrderID)]
	if se == nil || se.closed || se.kind != data.EscrowKindItem || se.frozenQty < quantity {
		return false, errcode.New(errcode.ErrInventoryInsufficient, "seller item escrow insufficient")
	}
	be := f.escrow[escrowKeyOf(buyerID, buyOrderID)]
	if be == nil || be.closed || be.kind != data.EscrowKindGold || be.frozenGold < totalGold {
		return false, errcode.New(errcode.ErrInventoryInsufficient, "buyer gold escrow insufficient")
	}
	se.frozenQty -= quantity
	be.frozenGold -= totalGold
	// 入账对手:卖家加金币,买家加道具。
	f.gold[sellerID] += totalGold
	if f.items[buyerID] == nil {
		f.items[buyerID] = map[uint32]int64{}
	}
	f.items[buyerID][itemConfigID] += quantity
	f.ledger[sk] = ledgerEntry{fingerprint: fp}
	f.ledger[bk] = ledgerEntry{fingerprint: fp}
	return false, nil
}

func (f *fakeRepo) FreezeForOrder(_ context.Context, playerID, orderID uint64, kind data.EscrowKind, itemConfigID uint32, quantity, frozenGold int64) (bool, error) {
	ek := escrowKeyOf(playerID, orderID)
	if _, ok := f.escrow[ek]; ok {
		return true, nil // 幂等:已冻结。
	}
	switch kind {
	case data.EscrowKindItem:
		if f.items[playerID] == nil || f.items[playerID][itemConfigID] < quantity {
			return false, errcode.New(errcode.ErrInventoryInsufficient, "freeze item insufficient")
		}
		f.items[playerID][itemConfigID] -= quantity
		f.escrow[ek] = &escrowEntry{kind: kind, itemConfigID: itemConfigID, frozenQty: quantity}
	case data.EscrowKindGold:
		if f.gold[playerID] < frozenGold {
			return false, errcode.New(errcode.ErrInventoryInsufficient, "freeze gold insufficient")
		}
		f.gold[playerID] -= frozenGold
		f.escrow[ek] = &escrowEntry{kind: kind, itemConfigID: itemConfigID, frozenGold: frozenGold}
	default:
		return false, errcode.New(errcode.ErrInvalidArg, "unknown escrow kind")
	}
	return false, nil
}

func (f *fakeRepo) ReleaseEscrow(_ context.Context, playerID, orderID uint64) (bool, error) {
	e := f.escrow[escrowKeyOf(playerID, orderID)]
	if e == nil || e.closed {
		return true, nil // 幂等 no-op。
	}
	if e.kind == data.EscrowKindItem && e.frozenQty > 0 {
		if f.items[playerID] == nil {
			f.items[playerID] = map[uint32]int64{}
		}
		f.items[playerID][e.itemConfigID] += e.frozenQty
	}
	if e.kind == data.EscrowKindGold && e.frozenGold > 0 {
		f.gold[playerID] += e.frozenGold
	}
	e.frozenQty, e.frozenGold, e.closed = 0, 0, true
	return false, nil
}

func newUC(repo data.InventoryRepo) *InventoryUsecase {
	return NewInventoryUsecase(repo, conf.InventoryConf{
		ItemRules: []conf.ItemRule{
			{ItemConfigID: 2001, Usable: true},
			{ItemConfigID: 3001, Sellable: true, SellUnitPrice: 10},
		},
	})
}

func TestGrantItems_Idempotent(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	first, err := uc.GrantItems(context.Background(), 100, []data.ItemGrant{{ItemConfigID: 2001, Count: 3}}, 50, "drop-m1")
	if err != nil {
		t.Fatalf("first grant err: %v", err)
	}
	if first != 50 {
		t.Fatalf("first grant gold want 50, got %d", first)
	}
	second, err := uc.GrantItems(context.Background(), 100, []data.ItemGrant{{ItemConfigID: 2001, Count: 3}}, 50, "drop-m1")
	if err != nil {
		t.Fatalf("second grant err: %v", err)
	}
	if second != 50 {
		t.Fatalf("idempotent grant should not double-add gold, want 50, got %d", second)
	}
	if repo.items[100][2001] != 3 {
		t.Fatalf("idempotent grant should not double-add items, want 3, got %d", repo.items[100][2001])
	}
}

func TestGrantItems_Validation(t *testing.T) {
	uc := newUC(newFakeRepo())
	if _, err := uc.GrantItems(context.Background(), 100, nil, 0, "k"); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("nothing to grant should be ErrInvalidArg, got %v", err)
	}
	if _, err := uc.GrantItems(context.Background(), 100, []data.ItemGrant{{ItemConfigID: 2001, Count: 0}}, 0, "k"); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("non-positive count should be ErrInvalidArg, got %v", err)
	}
	if _, err := uc.GrantItems(context.Background(), 100, nil, 5, ""); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("empty key should be ErrInvalidArg, got %v", err)
	}
}

func TestUseItem_NotUsable(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	// 3001 是 sellable 但非 usable。
	if _, err := uc.GrantItems(context.Background(), 100, []data.ItemGrant{{ItemConfigID: 3001, Count: 5}}, 0, "g1"); err != nil {
		t.Fatalf("grant err: %v", err)
	}
	_, err := uc.UseItem(context.Background(), 100, 3001, 1, "use1")
	if errcode.As(err) != errcode.ErrInventoryItemNotUsable {
		t.Fatalf("non-usable item should be ErrInventoryItemNotUsable, got %v", err)
	}
}

func TestUseItem_Insufficient(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	if _, err := uc.GrantItems(context.Background(), 100, []data.ItemGrant{{ItemConfigID: 2001, Count: 1}}, 0, "g1"); err != nil {
		t.Fatalf("grant err: %v", err)
	}
	_, err := uc.UseItem(context.Background(), 100, 2001, 5, "use1")
	if errcode.As(err) != errcode.ErrInventoryInsufficient {
		t.Fatalf("over-use should be ErrInventoryInsufficient, got %v", err)
	}
}

func TestUseItem_Success(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	if _, err := uc.GrantItems(context.Background(), 100, []data.ItemGrant{{ItemConfigID: 2001, Count: 3}}, 0, "g1"); err != nil {
		t.Fatalf("grant err: %v", err)
	}
	remaining, err := uc.UseItem(context.Background(), 100, 2001, 2, "use1")
	if err != nil {
		t.Fatalf("use err: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("after use 2 of 3, remaining want 1, got %d", remaining)
	}
}

func TestSellItem_NotSellable(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	if _, err := uc.GrantItems(context.Background(), 100, []data.ItemGrant{{ItemConfigID: 2001, Count: 5}}, 0, "g1"); err != nil {
		t.Fatalf("grant err: %v", err)
	}
	_, _, err := uc.SellItem(context.Background(), 100, 2001, 1, "sell1")
	if errcode.As(err) != errcode.ErrInventoryNotSellable {
		t.Fatalf("non-sellable item should be ErrInventoryNotSellable, got %v", err)
	}
}

func TestSellItem_SuccessGivesGold(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	if _, err := uc.GrantItems(context.Background(), 100, []data.ItemGrant{{ItemConfigID: 3001, Count: 5}}, 0, "g1"); err != nil {
		t.Fatalf("grant err: %v", err)
	}
	remaining, gold, err := uc.SellItem(context.Background(), 100, 3001, 2, "sell1")
	if err != nil {
		t.Fatalf("sell err: %v", err)
	}
	if remaining != 3 {
		t.Fatalf("after sell 2 of 5, remaining want 3, got %d", remaining)
	}
	if gold != 20 {
		t.Fatalf("sell 2 @ 10 should give 20 gold, got %d", gold)
	}
}

func TestSellItem_Idempotent(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	if _, err := uc.GrantItems(context.Background(), 100, []data.ItemGrant{{ItemConfigID: 3001, Count: 5}}, 0, "g1"); err != nil {
		t.Fatalf("grant err: %v", err)
	}
	if _, _, err := uc.SellItem(context.Background(), 100, 3001, 2, "sell1"); err != nil {
		t.Fatalf("first sell err: %v", err)
	}
	remaining, gold, err := uc.SellItem(context.Background(), 100, 3001, 2, "sell1")
	if err != nil {
		t.Fatalf("second sell err: %v", err)
	}
	if remaining != 3 || gold != 20 {
		t.Fatalf("idempotent sell should not double-apply, want remaining=3 gold=20, got remaining=%d gold=%d", remaining, gold)
	}
}

func TestGrantItems_IdempotencyConflict(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	if _, err := uc.GrantItems(context.Background(), 100, []data.ItemGrant{{ItemConfigID: 2001, Count: 3}}, 50, "drop-m1"); err != nil {
		t.Fatalf("first grant err: %v", err)
	}
	// 同 idempotency_key 不同请求参数 → 冲突,而非静默回放旧结果。
	_, err := uc.GrantItems(context.Background(), 100, []data.ItemGrant{{ItemConfigID: 2001, Count: 999}}, 50, "drop-m1")
	if errcode.As(err) != errcode.ErrInventoryIdempotencyConflict {
		t.Fatalf("same key different request should be ErrInventoryIdempotencyConflict, got %v", err)
	}
	if repo.items[100][2001] != 3 {
		t.Fatalf("conflict must not apply second request, want 3, got %d", repo.items[100][2001])
	}
}

func TestSellItem_ReplayReturnsSnapshot(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	if _, err := uc.GrantItems(context.Background(), 100, []data.ItemGrant{{ItemConfigID: 3001, Count: 5}}, 0, "g1"); err != nil {
		t.Fatalf("grant err: %v", err)
	}
	if _, _, err := uc.SellItem(context.Background(), 100, 3001, 2, "sell1"); err != nil {
		t.Fatalf("first sell err: %v", err)
	}
	// 首次卖后再卖 1 个(不同 key),改变当前库存/金币;随后回放 sell1 必须返回首次快照,而非当前状态。
	if _, _, err := uc.SellItem(context.Background(), 100, 3001, 1, "sell2"); err != nil {
		t.Fatalf("second sell err: %v", err)
	}
	remaining, gold, err := uc.SellItem(context.Background(), 100, 3001, 2, "sell1")
	if err != nil {
		t.Fatalf("replay sell err: %v", err)
	}
	if remaining != 3 || gold != 20 {
		t.Fatalf("replay must return first-time snapshot remaining=3 gold=20, got remaining=%d gold=%d", remaining, gold)
	}
}

func TestSettleAuctionMatch_Success(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	ctx := context.Background()
	// 卖家(10)持 5 个道具 7001;买家(20)持 1000 金币。
	if _, err := uc.GrantItems(ctx, 10, []data.ItemGrant{{ItemConfigID: 7001, Count: 5}}, 0, "seed-seller"); err != nil {
		t.Fatalf("seed seller err: %v", err)
	}
	if _, err := uc.GrantItems(ctx, 20, nil, 1000, "seed-buyer"); err != nil {
		t.Fatalf("seed buyer err: %v", err)
	}
	// 卖家挂单冻结 3 个道具(sell order 501);买家出价冻结 3*100 金币(buy order 601)。
	if err := uc.FreezeForOrder(ctx, 10, 501, EscrowSideSell, 7001, 3, 100); err != nil {
		t.Fatalf("freeze seller err: %v", err)
	}
	if err := uc.FreezeForOrder(ctx, 20, 601, EscrowSideBuy, 7001, 3, 100); err != nil {
		t.Fatalf("freeze buyer err: %v", err)
	}
	// 冻结后活跃余额已扣减。
	if repo.items[10][7001] != 2 {
		t.Fatalf("after freeze seller active item want 2, got %d", repo.items[10][7001])
	}
	if repo.gold[20] != 700 {
		t.Fatalf("after freeze buyer active gold want 700, got %d", repo.gold[20])
	}
	// 成交:卖家交付 3 个 @ 单价 100 = 300 金币。
	if err := uc.SettleAuctionMatch(ctx, 999, 10, 20, 501, 601, 7001, 3, 100); err != nil {
		t.Fatalf("settle err: %v", err)
	}
	if repo.items[20][7001] != 3 {
		t.Fatalf("buyer item want 3, got %d", repo.items[20][7001])
	}
	if repo.gold[10] != 300 {
		t.Fatalf("seller gold want 300, got %d", repo.gold[10])
	}
	// 买家金币 = 700(冻结后剩余),300 已从 escrow 付给卖家。
	if repo.gold[20] != 700 {
		t.Fatalf("buyer gold want 700, got %d", repo.gold[20])
	}
}

func TestSettleAuctionMatch_Idempotent(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	ctx := context.Background()
	if _, err := uc.GrantItems(ctx, 10, []data.ItemGrant{{ItemConfigID: 7001, Count: 5}}, 0, "seed-seller"); err != nil {
		t.Fatalf("seed seller err: %v", err)
	}
	if _, err := uc.GrantItems(ctx, 20, nil, 1000, "seed-buyer"); err != nil {
		t.Fatalf("seed buyer err: %v", err)
	}
	if err := uc.FreezeForOrder(ctx, 10, 501, EscrowSideSell, 7001, 3, 100); err != nil {
		t.Fatalf("freeze seller err: %v", err)
	}
	if err := uc.FreezeForOrder(ctx, 20, 601, EscrowSideBuy, 7001, 3, 100); err != nil {
		t.Fatalf("freeze buyer err: %v", err)
	}
	if err := uc.SettleAuctionMatch(ctx, 999, 10, 20, 501, 601, 7001, 3, 100); err != nil {
		t.Fatalf("first settle err: %v", err)
	}
	// 重复结算同一 match_id:资产不可二次转移。
	if err := uc.SettleAuctionMatch(ctx, 999, 10, 20, 501, 601, 7001, 3, 100); err != nil {
		t.Fatalf("idempotent settle err: %v", err)
	}
	if repo.items[20][7001] != 3 || repo.gold[10] != 300 || repo.gold[20] != 700 {
		t.Fatalf("idempotent settle must not double-transfer: buyerItem=%d sellerGold=%d buyerGold=%d",
			repo.items[20][7001], repo.gold[10], repo.gold[20])
	}
}

func TestFreezeForOrder_ItemInsufficient(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	ctx := context.Background()
	// 卖家只有 1 个,挂 3 个 → 冻结失败(挂单阶段就拦下,不会进簿)。
	if _, err := uc.GrantItems(ctx, 10, []data.ItemGrant{{ItemConfigID: 7001, Count: 1}}, 0, "seed-seller"); err != nil {
		t.Fatalf("seed seller err: %v", err)
	}
	if err := uc.FreezeForOrder(ctx, 10, 501, EscrowSideSell, 7001, 3, 100); errcode.As(err) != errcode.ErrInventoryInsufficient {
		t.Fatalf("freeze item insufficient should be ErrInventoryInsufficient, got %v", err)
	}
	// 失败后活跃余额未被扣。
	if repo.items[10][7001] != 1 {
		t.Fatalf("active item must be untouched on freeze failure, got %d", repo.items[10][7001])
	}
}

func TestFreezeForOrder_GoldInsufficient(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	ctx := context.Background()
	if _, err := uc.GrantItems(ctx, 20, nil, 100, "seed-buyer"); err != nil {
		t.Fatalf("seed buyer err: %v", err)
	}
	// 出价冻结需要 300,只有 100 → 失败。
	if err := uc.FreezeForOrder(ctx, 20, 601, EscrowSideBuy, 7001, 3, 100); errcode.As(err) != errcode.ErrInventoryInsufficient {
		t.Fatalf("freeze gold insufficient should be ErrInventoryInsufficient, got %v", err)
	}
	if repo.gold[20] != 100 {
		t.Fatalf("active gold must be untouched on freeze failure, got %d", repo.gold[20])
	}
}

func TestFreezeForOrder_Idempotent(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	ctx := context.Background()
	if _, err := uc.GrantItems(ctx, 10, []data.ItemGrant{{ItemConfigID: 7001, Count: 5}}, 0, "seed-seller"); err != nil {
		t.Fatalf("seed seller err: %v", err)
	}
	if err := uc.FreezeForOrder(ctx, 10, 501, EscrowSideSell, 7001, 3, 100); err != nil {
		t.Fatalf("first freeze err: %v", err)
	}
	// 重复冻结同一 order:只扣一次。
	if err := uc.FreezeForOrder(ctx, 10, 501, EscrowSideSell, 7001, 3, 100); err != nil {
		t.Fatalf("idempotent freeze err: %v", err)
	}
	if repo.items[10][7001] != 2 {
		t.Fatalf("idempotent freeze must deduct once: active item want 2, got %d", repo.items[10][7001])
	}
}

func TestReleaseEscrow_RefundsRemaining(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	ctx := context.Background()
	if _, err := uc.GrantItems(ctx, 10, []data.ItemGrant{{ItemConfigID: 7001, Count: 5}}, 0, "seed-seller"); err != nil {
		t.Fatalf("seed seller err: %v", err)
	}
	// 冻 3 个道具(活跃剩 2)。
	if err := uc.FreezeForOrder(ctx, 10, 501, EscrowSideSell, 7001, 3, 100); err != nil {
		t.Fatalf("freeze err: %v", err)
	}
	// 撤单退还 → 活跃恢复 5。
	if err := uc.ReleaseEscrow(ctx, 10, 501); err != nil {
		t.Fatalf("release err: %v", err)
	}
	if repo.items[10][7001] != 5 {
		t.Fatalf("after release active item want 5, got %d", repo.items[10][7001])
	}
	// 重复退还幂等:不二次返还。
	if err := uc.ReleaseEscrow(ctx, 10, 501); err != nil {
		t.Fatalf("idempotent release err: %v", err)
	}
	if repo.items[10][7001] != 5 {
		t.Fatalf("idempotent release must not double-refund, got %d", repo.items[10][7001])
	}
}

func TestReleaseEscrow_BuyerPriceImprovement(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	ctx := context.Background()
	if _, err := uc.GrantItems(ctx, 10, []data.ItemGrant{{ItemConfigID: 7001, Count: 5}}, 0, "seed-seller"); err != nil {
		t.Fatalf("seed seller err: %v", err)
	}
	if _, err := uc.GrantItems(ctx, 20, nil, 1000, "seed-buyer"); err != nil {
		t.Fatalf("seed buyer err: %v", err)
	}
	// 卖家挂卖单单价 80;买家出价单价 100 冻 3*100=300 金币(活跃剩 700)。
	if err := uc.FreezeForOrder(ctx, 10, 501, EscrowSideSell, 7001, 3, 80); err != nil {
		t.Fatalf("freeze seller err: %v", err)
	}
	if err := uc.FreezeForOrder(ctx, 20, 601, EscrowSideBuy, 7001, 3, 100); err != nil {
		t.Fatalf("freeze buyer err: %v", err)
	}
	// 成交价 = 被动卖单价 80。买家实付 3*80=240,escrow 残余 300-240=60。
	if err := uc.SettleAuctionMatch(ctx, 999, 10, 20, 501, 601, 7001, 3, 80); err != nil {
		t.Fatalf("settle err: %v", err)
	}
	if repo.gold[10] != 240 {
		t.Fatalf("seller gold want 240, got %d", repo.gold[10])
	}
	// 买单完全成交后退还价差 60 → 买家活跃金币 700+60=760。
	if err := uc.ReleaseEscrow(ctx, 20, 601); err != nil {
		t.Fatalf("release buyer err: %v", err)
	}
	if repo.gold[20] != 760 {
		t.Fatalf("buyer gold after price-improvement refund want 760, got %d", repo.gold[20])
	}
}

func TestSettleAuctionMatch_Validation(t *testing.T) {
	uc := newUC(newFakeRepo())
	ctx := context.Background()
	if err := uc.SettleAuctionMatch(ctx, 0, 10, 20, 501, 601, 7001, 1, 1); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("zero match_id should be ErrInvalidArg, got %v", err)
	}
	if err := uc.SettleAuctionMatch(ctx, 1, 10, 10, 501, 601, 7001, 1, 1); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("self-trade should be ErrInvalidArg, got %v", err)
	}
	if err := uc.SettleAuctionMatch(ctx, 1, 10, 20, 0, 601, 7001, 1, 1); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("zero sell_order_id should be ErrInvalidArg, got %v", err)
	}
	if err := uc.SettleAuctionMatch(ctx, 1, 10, 20, 501, 601, 7001, 0, 1); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("zero quantity should be ErrInvalidArg, got %v", err)
	}
	if err := uc.SettleAuctionMatch(ctx, 1, 10, 20, 501, 601, 7001, 1, 0); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("zero unit_price should be ErrInvalidArg, got %v", err)
	}
}
