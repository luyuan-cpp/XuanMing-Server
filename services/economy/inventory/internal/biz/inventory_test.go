// inventory_test.go — InventoryUsecase 业务逻辑单测(W5 ③,2026-06-18)。
//
// 用内存版 fakeRepo 复刻 MySQL 幂等 / 扣减语义,无需真 DB;
// 验证 usable / sellable 规则裁决、幂等键去重、数量不足拦截。
package biz

import (
	"context"
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

// fakeRepo 是 data.InventoryRepo 的内存实现(复刻 MySQL 幂等 / 扣减 / 指纹快照语义)。
type fakeRepo struct {
	gold   map[uint64]int64
	items  map[uint64]map[uint32]int64
	ledger map[string]ledgerEntry // key=playerID|idempotencyKey
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		gold:   map[uint64]int64{},
		items:  map[uint64]map[uint32]int64{},
		ledger: map[string]ledgerEntry{},
	}
}

func keyOf(pid uint64, k string) string {
	return string(rune(pid)) + "|" + k
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

func (f *fakeRepo) SettleAuctionMatch(_ context.Context, _, sellerID, buyerID uint64, itemConfigID uint32, quantity, totalGold int64, idempotencyKey, _ string) (bool, error) {
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
	// 卖家道具校验。
	if f.items[sellerID] == nil || f.items[sellerID][itemConfigID] < quantity {
		return false, errcode.New(errcode.ErrInventoryInsufficient, "seller item insufficient")
	}
	// 买家金币校验。
	if f.gold[buyerID] < totalGold {
		return false, errcode.New(errcode.ErrInventoryInsufficient, "buyer gold insufficient")
	}
	// 资产对转。
	f.items[sellerID][itemConfigID] -= quantity
	f.gold[sellerID] += totalGold
	f.gold[buyerID] -= totalGold
	if f.items[buyerID] == nil {
		f.items[buyerID] = map[uint32]int64{}
	}
	f.items[buyerID][itemConfigID] += quantity
	f.ledger[sk] = ledgerEntry{fingerprint: fp}
	f.ledger[bk] = ledgerEntry{fingerprint: fp}
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
	// 卖家(10)持 5 个道具 7001;买家(20)持 1000 金币。
	if _, err := uc.GrantItems(context.Background(), 10, []data.ItemGrant{{ItemConfigID: 7001, Count: 5}}, 0, "seed-seller"); err != nil {
		t.Fatalf("seed seller err: %v", err)
	}
	if _, err := uc.GrantItems(context.Background(), 20, nil, 1000, "seed-buyer"); err != nil {
		t.Fatalf("seed buyer err: %v", err)
	}
	// 成交:卖家交付 3 个 @ 单价 100 = 300 金币。
	if err := uc.SettleAuctionMatch(context.Background(), 999, 10, 20, 7001, 3, 100); err != nil {
		t.Fatalf("settle err: %v", err)
	}
	if repo.items[10][7001] != 2 {
		t.Fatalf("seller item want 2, got %d", repo.items[10][7001])
	}
	if repo.items[20][7001] != 3 {
		t.Fatalf("buyer item want 3, got %d", repo.items[20][7001])
	}
	if repo.gold[10] != 300 {
		t.Fatalf("seller gold want 300, got %d", repo.gold[10])
	}
	if repo.gold[20] != 700 {
		t.Fatalf("buyer gold want 700, got %d", repo.gold[20])
	}
}

func TestSettleAuctionMatch_Idempotent(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	if _, err := uc.GrantItems(context.Background(), 10, []data.ItemGrant{{ItemConfigID: 7001, Count: 5}}, 0, "seed-seller"); err != nil {
		t.Fatalf("seed seller err: %v", err)
	}
	if _, err := uc.GrantItems(context.Background(), 20, nil, 1000, "seed-buyer"); err != nil {
		t.Fatalf("seed buyer err: %v", err)
	}
	if err := uc.SettleAuctionMatch(context.Background(), 999, 10, 20, 7001, 3, 100); err != nil {
		t.Fatalf("first settle err: %v", err)
	}
	// 重复结算同一 match_id:资产不可二次转移。
	if err := uc.SettleAuctionMatch(context.Background(), 999, 10, 20, 7001, 3, 100); err != nil {
		t.Fatalf("idempotent settle err: %v", err)
	}
	if repo.items[10][7001] != 2 || repo.items[20][7001] != 3 || repo.gold[10] != 300 || repo.gold[20] != 700 {
		t.Fatalf("idempotent settle must not double-transfer: sellerItem=%d buyerItem=%d sellerGold=%d buyerGold=%d",
			repo.items[10][7001], repo.items[20][7001], repo.gold[10], repo.gold[20])
	}
}

func TestSettleAuctionMatch_SellerItemInsufficient(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	if _, err := uc.GrantItems(context.Background(), 10, []data.ItemGrant{{ItemConfigID: 7001, Count: 1}}, 0, "seed-seller"); err != nil {
		t.Fatalf("seed seller err: %v", err)
	}
	if _, err := uc.GrantItems(context.Background(), 20, nil, 1000, "seed-buyer"); err != nil {
		t.Fatalf("seed buyer err: %v", err)
	}
	err := uc.SettleAuctionMatch(context.Background(), 999, 10, 20, 7001, 3, 100)
	if errcode.As(err) != errcode.ErrInventoryInsufficient {
		t.Fatalf("seller item insufficient should be ErrInventoryInsufficient, got %v", err)
	}
}

func TestSettleAuctionMatch_BuyerGoldInsufficient(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	if _, err := uc.GrantItems(context.Background(), 10, []data.ItemGrant{{ItemConfigID: 7001, Count: 5}}, 0, "seed-seller"); err != nil {
		t.Fatalf("seed seller err: %v", err)
	}
	if _, err := uc.GrantItems(context.Background(), 20, nil, 100, "seed-buyer"); err != nil {
		t.Fatalf("seed buyer err: %v", err)
	}
	err := uc.SettleAuctionMatch(context.Background(), 999, 10, 20, 7001, 3, 100) // 需要 300,只有 100
	if errcode.As(err) != errcode.ErrInventoryInsufficient {
		t.Fatalf("buyer gold insufficient should be ErrInventoryInsufficient, got %v", err)
	}
}

func TestSettleAuctionMatch_Validation(t *testing.T) {
	uc := newUC(newFakeRepo())
	if err := uc.SettleAuctionMatch(context.Background(), 0, 10, 20, 7001, 1, 1); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("zero match_id should be ErrInvalidArg, got %v", err)
	}
	if err := uc.SettleAuctionMatch(context.Background(), 1, 10, 10, 7001, 1, 1); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("self-trade should be ErrInvalidArg, got %v", err)
	}
	if err := uc.SettleAuctionMatch(context.Background(), 1, 10, 20, 7001, 0, 1); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("zero quantity should be ErrInvalidArg, got %v", err)
	}
	if err := uc.SettleAuctionMatch(context.Background(), 1, 10, 20, 7001, 1, 0); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("zero unit_price should be ErrInvalidArg, got %v", err)
	}
}
