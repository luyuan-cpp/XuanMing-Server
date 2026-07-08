// inventory_test.go — InventoryUsecase 业务逻辑单测(W5 ③,2026-06-18)。
//
// 用内存版 fakeRepo 复刻 MySQL 幂等 / 扣减语义,无需真 DB;
// 验证 usable / sellable 规则裁决、幂等键去重、数量不足拦截。
package biz

import (
	"context"
	"fmt"
	"sort"
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

	// 装备实例(W5 ④):instances[playerID][instanceID]=inst;instGrant 复刻 grant_inst 幂等。
	instances map[uint64]map[uint64]*data.ItemInstance
	instGrant map[string]instGrantEntry // key=playerID|idempotencyKey
}

// instGrantEntry 复刻 grant_inst 幂等流水:指纹 + 首次发放的 instance_id 列表(回放用)。
type instGrantEntry struct {
	fingerprint string
	ids         []uint64
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		gold:      map[uint64]int64{},
		items:     map[uint64]map[uint32]int64{},
		ledger:    map[string]ledgerEntry{},
		escrow:    map[string]*escrowEntry{},
		instances: map[uint64]map[uint64]*data.ItemInstance{},
		instGrant: map[string]instGrantEntry{},
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

func (f *fakeRepo) SettlePlayerTrade(_ context.Context, _, sellerID, buyerID uint64, sellerItems, buyerItems []data.ItemGrant, price int64, idempotencyKey, _ string) (bool, error) {
	fp := data.PlayerTradeSettleFingerprint(sellerID, buyerID, sellerItems, buyerItems, price)
	sk := keyOf(sellerID, idempotencyKey)
	bk := keyOf(buyerID, idempotencyKey)
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
	// 校验双方活跃余额足够(无 escrow,直接从活跃背包 / 金币扣转)。
	for _, it := range sellerItems {
		if f.items[sellerID][it.ItemConfigID] < it.Count {
			return false, errcode.New(errcode.ErrInventoryInsufficient, "seller item insufficient")
		}
	}
	for _, it := range buyerItems {
		if f.items[buyerID][it.ItemConfigID] < it.Count {
			return false, errcode.New(errcode.ErrInventoryInsufficient, "buyer item insufficient")
		}
	}
	if price > 0 && f.gold[buyerID] < price {
		return false, errcode.New(errcode.ErrInventoryInsufficient, "buyer gold insufficient")
	}
	if f.items[sellerID] == nil {
		f.items[sellerID] = map[uint32]int64{}
	}
	if f.items[buyerID] == nil {
		f.items[buyerID] = map[uint32]int64{}
	}
	// 卖家交付 sellerItems → 买家;买家交付 buyerItems → 卖家;买家付 price 金币 → 卖家。
	for _, it := range sellerItems {
		f.items[sellerID][it.ItemConfigID] -= it.Count
		f.items[buyerID][it.ItemConfigID] += it.Count
	}
	for _, it := range buyerItems {
		f.items[buyerID][it.ItemConfigID] -= it.Count
		f.items[sellerID][it.ItemConfigID] += it.Count
	}
	if price > 0 {
		f.gold[buyerID] -= price
		f.gold[sellerID] += price
	}
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

// ── 装备实例(W5 ④)内存实现,复刻 player_item_instance 语义 ──

func (f *fakeRepo) instMap(playerID uint64) map[uint64]*data.ItemInstance {
	if f.instances[playerID] == nil {
		f.instances[playerID] = map[uint64]*data.ItemInstance{}
	}
	return f.instances[playerID]
}

func (f *fakeRepo) ListInstances(_ context.Context, playerID uint64) ([]data.ItemInstance, error) {
	m := f.instances[playerID]
	ids := make([]uint64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]data.ItemInstance, 0, len(ids))
	for _, id := range ids {
		out = append(out, *m[id])
	}
	return out, nil
}

func (f *fakeRepo) instancesByIDs(playerID uint64, ids []uint64) []data.ItemInstance {
	m := f.instances[playerID]
	sorted := append([]uint64(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	out := make([]data.ItemInstance, 0, len(sorted))
	for _, id := range sorted {
		if inst, ok := m[id]; ok {
			out = append(out, *inst)
		}
	}
	return out
}

func (f *fakeRepo) lowestFreeSlot(playerID uint64, capacity int32) (int32, bool) {
	occ := map[int32]struct{}{}
	for _, inst := range f.instances[playerID] {
		if inst.SlotIndex >= 0 {
			occ[inst.SlotIndex] = struct{}{}
		}
	}
	for s := int32(0); s < capacity; s++ {
		if _, taken := occ[s]; !taken {
			return s, true
		}
	}
	return -1, false
}

func (f *fakeRepo) GrantInstances(_ context.Context, playerID uint64, instanceIDs []uint64, itemConfigIDs []uint32, capacity int32, idempotencyKey, _ string) ([]data.ItemInstance, bool, error) {
	gk := keyOf(playerID, idempotencyKey)
	fp := data.GrantInstancesFingerprint(itemConfigIDs)
	if e, ok := f.instGrant[gk]; ok {
		if e.fingerprint != fp {
			return nil, false, errcode.New(errcode.ErrInventoryIdempotencyConflict, "idempotency conflict")
		}
		return f.instancesByIDs(playerID, e.ids), true, nil
	}
	if capacity <= 0 {
		return nil, false, errcode.New(errcode.ErrInventoryCapacityFull, "instance inventory disabled")
	}
	m := f.instMap(playerID)
	if len(m)+len(instanceIDs) > int(capacity) {
		return nil, false, errcode.New(errcode.ErrInventoryCapacityFull, "capacity full")
	}
	out := make([]data.ItemInstance, 0, len(instanceIDs))
	for i, id := range instanceIDs {
		slot, ok := f.lowestFreeSlot(playerID, capacity)
		if !ok {
			return nil, false, errcode.New(errcode.ErrInventoryCapacityFull, "no free slot")
		}
		inst := &data.ItemInstance{InstanceID: id, ItemConfigID: itemConfigIDs[i], SlotIndex: slot}
		m[id] = inst
		out = append(out, *inst)
	}
	f.instGrant[gk] = instGrantEntry{fingerprint: fp, ids: append([]uint64(nil), instanceIDs...)}
	return out, false, nil
}

func (f *fakeRepo) IdentifyInstance(_ context.Context, playerID, instanceID uint64, attrs []data.ItemAttribute) (data.ItemInstance, bool, error) {
	inst, ok := f.instances[playerID][instanceID]
	if !ok {
		return data.ItemInstance{}, false, errcode.New(errcode.ErrInventoryItemNotFound, "instance not found")
	}
	if inst.Identified {
		return *inst, true, nil
	}
	inst.Identified = true
	inst.Attributes = attrs
	return *inst, false, nil
}

func (f *fakeRepo) MoveInstance(_ context.Context, playerID, instanceID uint64, toSlot, capacity int32) (data.ItemInstance, error) {
	if toSlot < 0 || toSlot >= capacity {
		return data.ItemInstance{}, errcode.New(errcode.ErrInventorySlotOccupied, "slot out of range")
	}
	inst, ok := f.instances[playerID][instanceID]
	if !ok {
		return data.ItemInstance{}, errcode.New(errcode.ErrInventoryItemNotFound, "instance not found")
	}
	if inst.SlotIndex == toSlot {
		return *inst, nil
	}
	for id, other := range f.instances[playerID] {
		if id != instanceID && other.SlotIndex == toSlot {
			return data.ItemInstance{}, errcode.New(errcode.ErrInventorySlotOccupied, "slot occupied")
		}
	}
	inst.SlotIndex = toSlot
	return *inst, nil
}

func (f *fakeRepo) DiscardInstance(_ context.Context, playerID, instanceID uint64) error {
	delete(f.instances[playerID], instanceID)
	return nil
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

func TestSettlePlayerTrade_OK(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	ctx := context.Background()
	// 卖家 10 持有 5 个 7001;买家 20 持有 1000 金币 + 2 个 8002(回付道具)。
	if _, err := uc.GrantItems(ctx, 10, []data.ItemGrant{{ItemConfigID: 7001, Count: 5}}, 0, "seed-seller"); err != nil {
		t.Fatalf("seed seller err: %v", err)
	}
	if _, err := uc.GrantItems(ctx, 20, []data.ItemGrant{{ItemConfigID: 8002, Count: 2}}, 1000, "seed-buyer"); err != nil {
		t.Fatalf("seed buyer err: %v", err)
	}
	// 交易:卖家给 3 个 7001;买家给 2 个 8002 + 300 金币。
	err := uc.SettlePlayerTrade(ctx, 12345, 10, 20,
		[]data.ItemGrant{{ItemConfigID: 7001, Count: 3}},
		[]data.ItemGrant{{ItemConfigID: 8002, Count: 2}}, 300)
	if err != nil {
		t.Fatalf("settle err: %v", err)
	}
	if repo.items[10][7001] != 2 || repo.items[20][7001] != 3 {
		t.Fatalf("item 7001 transfer wrong: seller=%d buyer=%d", repo.items[10][7001], repo.items[20][7001])
	}
	if repo.items[20][8002] != 0 || repo.items[10][8002] != 2 {
		t.Fatalf("item 8002 transfer wrong: buyer=%d seller=%d", repo.items[20][8002], repo.items[10][8002])
	}
	if repo.gold[10] != 300 || repo.gold[20] != 700 {
		t.Fatalf("gold transfer wrong: seller=%d buyer=%d", repo.gold[10], repo.gold[20])
	}
}

func TestSettlePlayerTrade_Insufficient(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	ctx := context.Background()
	// 卖家只有 1 个,交易要给 3 个 → 不足。
	if _, err := uc.GrantItems(ctx, 10, []data.ItemGrant{{ItemConfigID: 7001, Count: 1}}, 0, "seed-seller"); err != nil {
		t.Fatalf("seed seller err: %v", err)
	}
	if _, err := uc.GrantItems(ctx, 20, nil, 1000, "seed-buyer"); err != nil {
		t.Fatalf("seed buyer err: %v", err)
	}
	err := uc.SettlePlayerTrade(ctx, 12345, 10, 20,
		[]data.ItemGrant{{ItemConfigID: 7001, Count: 3}}, nil, 300)
	if errcode.As(err) != errcode.ErrInventoryInsufficient {
		t.Fatalf("want ErrInventoryInsufficient, got %v", err)
	}
}

func TestSettlePlayerTrade_Idempotent(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	ctx := context.Background()
	if _, err := uc.GrantItems(ctx, 10, []data.ItemGrant{{ItemConfigID: 7001, Count: 5}}, 0, "seed-seller"); err != nil {
		t.Fatalf("seed seller err: %v", err)
	}
	if _, err := uc.GrantItems(ctx, 20, nil, 1000, "seed-buyer"); err != nil {
		t.Fatalf("seed buyer err: %v", err)
	}
	settle := func() error {
		return uc.SettlePlayerTrade(ctx, 12345, 10, 20,
			[]data.ItemGrant{{ItemConfigID: 7001, Count: 3}}, nil, 300)
	}
	if err := settle(); err != nil {
		t.Fatalf("first settle err: %v", err)
	}
	if err := settle(); err != nil {
		t.Fatalf("idempotent settle err: %v", err)
	}
	// 重复结算同一 order_id:资产不可二次转移。
	if repo.items[10][7001] != 2 || repo.items[20][7001] != 3 || repo.gold[10] != 300 || repo.gold[20] != 700 {
		t.Fatalf("idempotent settle must not double-transfer: sellerItem=%d buyerItem=%d sellerGold=%d buyerGold=%d",
			repo.items[10][7001], repo.items[20][7001], repo.gold[10], repo.gold[20])
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

// ── 装备实例 / 鉴定单测(W5 ④)──

// seqGen 是确定性 instance_id 生成器(测试用,从 base 递增)。
type seqGen struct{ next uint64 }

func (g *seqGen) Generate() uint64 { g.next++; return g.next }

// newInstanceUC 构造带实例背包(容量 4)+ 鉴定规则(道具 5001 从 3 属性池抽 2 条)的 usecase,
// 注入确定性 snowflake + 确定性随机源(randIntn 恒返 0 → 洗牌不变、每条取区间下界),便于断言。
func newInstanceUC() *InventoryUsecase {
	uc := NewInventoryUsecase(newFakeRepo(), conf.InventoryConf{
		Capacity: 4,
		IdentifyRules: []conf.IdentifyRule{{
			ItemConfigID: 5001,
			AttrCount:    2,
			Pool: []conf.IdentifyAttrRoll{
				{AttrID: 101, Min: 10, Max: 20},
				{AttrID: 102, Min: 5, Max: 5},
				{AttrID: 103, Min: 1, Max: 100},
			},
		}},
	})
	uc.SetSnowflake(&seqGen{})
	uc.SetRandSource(func(int64) int64 { return 0 })
	return uc
}

func TestGrantInstances_AssignsSlotsAndCapacity(t *testing.T) {
	uc := newInstanceUC()
	ctx := context.Background()
	insts, err := uc.GrantInstances(ctx, 100, []uint32{5001, 5002}, "drop-m1")
	if err != nil {
		t.Fatalf("grant instances err: %v", err)
	}
	if len(insts) != 2 {
		t.Fatalf("want 2 instances, got %d", len(insts))
	}
	if insts[0].SlotIndex != 0 || insts[1].SlotIndex != 1 {
		t.Fatalf("want slots 0,1, got %d,%d", insts[0].SlotIndex, insts[1].SlotIndex)
	}
	if insts[0].Identified {
		t.Fatalf("newly granted instance must be unidentified")
	}
	// 容量 4:再发 3 件 → 超容量拒。
	if _, err := uc.GrantInstances(ctx, 100, []uint32{5001, 5001, 5001}, "drop-m2"); errcode.As(err) != errcode.ErrInventoryCapacityFull {
		t.Fatalf("over-capacity grant should be ErrInventoryCapacityFull, got %v", err)
	}
}

func TestGrantInstances_Idempotent(t *testing.T) {
	uc := newInstanceUC()
	ctx := context.Background()
	first, err := uc.GrantInstances(ctx, 100, []uint32{5001}, "drop-m1")
	if err != nil {
		t.Fatalf("first grant err: %v", err)
	}
	second, err := uc.GrantInstances(ctx, 100, []uint32{5001}, "drop-m1")
	if err != nil {
		t.Fatalf("replay grant err: %v", err)
	}
	if len(second) != 1 || second[0].InstanceID != first[0].InstanceID {
		t.Fatalf("idempotent grant must replay same instance, first=%d second=%v", first[0].InstanceID, second)
	}
	// 只发一件,不重复创建。
	_, _, capacity, instances, _ := uc.GetInventoryFull(ctx, 100)
	if capacity != 4 || len(instances) != 1 {
		t.Fatalf("want capacity 4 and 1 instance, got cap=%d n=%d", capacity, len(instances))
	}
}

func TestIdentifyItem_RollsAttributesAndIdempotent(t *testing.T) {
	uc := newInstanceUC()
	ctx := context.Background()
	insts, err := uc.GrantInstances(ctx, 100, []uint32{5001}, "drop-m1")
	if err != nil {
		t.Fatalf("grant err: %v", err)
	}
	id := insts[0].InstanceID
	got, err := uc.IdentifyItem(ctx, 100, id)
	if err != nil {
		t.Fatalf("identify err: %v", err)
	}
	if !got.Identified {
		t.Fatalf("identified flag must be set")
	}
	// randIntn 恒 0:Fisher-Yates 对 [0,1,2] 洗成 [1,2,0],取前 2 条 → pool[1]{102},pool[2]{103};
	// 每条取区间下界(102→5,103→1)。
	if len(got.Attributes) != 2 {
		t.Fatalf("want 2 rolled attrs, got %d", len(got.Attributes))
	}
	if got.Attributes[0].AttrID != 102 || got.Attributes[0].Value != 5 {
		t.Fatalf("attr0 want {102,5}, got %+v", got.Attributes[0])
	}
	if got.Attributes[1].AttrID != 103 || got.Attributes[1].Value != 1 {
		t.Fatalf("attr1 want {103,1}, got %+v", got.Attributes[1])
	}
	// 幂等:再次鉴定回放同属性,不 re-roll。
	again, err := uc.IdentifyItem(ctx, 100, id)
	if err != nil {
		t.Fatalf("re-identify err: %v", err)
	}
	if len(again.Attributes) != 2 || again.Attributes[0].Value != 5 {
		t.Fatalf("re-identify must replay, got %+v", again.Attributes)
	}
}

func TestIdentifyItem_NotFound(t *testing.T) {
	uc := newInstanceUC()
	if _, err := uc.IdentifyItem(context.Background(), 100, 99999); errcode.As(err) != errcode.ErrInventoryItemNotFound {
		t.Fatalf("identify missing instance should be ErrInventoryItemNotFound, got %v", err)
	}
}

func TestMoveInstance_SlotOccupied(t *testing.T) {
	uc := newInstanceUC()
	ctx := context.Background()
	insts, err := uc.GrantInstances(ctx, 100, []uint32{5001, 5002}, "drop-m1")
	if err != nil {
		t.Fatalf("grant err: %v", err)
	}
	// insts[0] 在格 0,insts[1] 在格 1。把 insts[1] 移到格 0(被占)→ 拒。
	if _, err := uc.MoveInstance(ctx, 100, insts[1].InstanceID, 0); errcode.As(err) != errcode.ErrInventorySlotOccupied {
		t.Fatalf("move onto occupied slot should be ErrInventorySlotOccupied, got %v", err)
	}
	// 移到空格 2 → OK。
	moved, err := uc.MoveInstance(ctx, 100, insts[1].InstanceID, 2)
	if err != nil {
		t.Fatalf("move to free slot err: %v", err)
	}
	if moved.SlotIndex != 2 {
		t.Fatalf("want slot 2, got %d", moved.SlotIndex)
	}
}

func TestDiscardInstance_Idempotent(t *testing.T) {
	uc := newInstanceUC()
	ctx := context.Background()
	insts, err := uc.GrantInstances(ctx, 100, []uint32{5001}, "drop-m1")
	if err != nil {
		t.Fatalf("grant err: %v", err)
	}
	id := insts[0].InstanceID
	if err := uc.DiscardInstance(ctx, 100, id); err != nil {
		t.Fatalf("discard err: %v", err)
	}
	// 再次丢弃 no-op。
	if err := uc.DiscardInstance(ctx, 100, id); err != nil {
		t.Fatalf("idempotent discard err: %v", err)
	}
	_, _, _, instances, _ := uc.GetInventoryFull(ctx, 100)
	if len(instances) != 0 {
		t.Fatalf("want 0 instances after discard, got %d", len(instances))
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
