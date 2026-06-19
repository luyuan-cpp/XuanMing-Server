// auction 撮合引擎单测(2026-06-19):验证不超卖 / 价格-时间优先 / 幂等 / 部分成交。
//
// 用真 Redis ZSET 订单簿(miniredis)验证 score+member 编码的撮合顺序;
// 用内存 fakeRepo 当撮合权威库;用 trackLedger 计数结算次数(验证不超卖 / 不重复结算)。
package biz

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	auctionv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/auction/v1"

	"github.com/luyuancpp/pandora/services/economy/auction/internal/conf"
	"github.com/luyuancpp/pandora/services/economy/auction/internal/data"
)

// ── 测试替身 ────────────────────────────────────────────────────────────────

// seqGen 是递增雪花替身;order_id 递增 → 订单簿同价按 member 字典序 = 时间优先可被验证。
type seqGen struct {
	mu sync.Mutex
	n  uint64
}

func (g *seqGen) Generate() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return g.n
}

// trackLedger 记录每笔结算(计数 = 成交笔数 + 量,验证不超卖 / 不重复结算)。
type trackLedger struct {
	mu      sync.Mutex
	matches []*data.MatchRecord
}

func (l *trackLedger) Settle(_ context.Context, m *data.MatchRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.matches = append(l.matches, m)
	return nil
}

func (l *trackLedger) totalQty() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var sum int64
	for _, m := range l.matches {
		sum += m.Quantity
	}
	return sum
}

// fakeRepo 是内存版 AuctionRepo。
type fakeRepo struct {
	mu      sync.Mutex
	orders  map[uint64]*data.OrderRecord
	byIdem  map[string]uint64
	matches map[uint64]*data.MatchRecord
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		orders:  make(map[uint64]*data.OrderRecord),
		byIdem:  make(map[string]uint64),
		matches: make(map[uint64]*data.MatchRecord),
	}
}

func copyOrder(o *data.OrderRecord) *data.OrderRecord {
	c := *o
	return &c
}

func idemKey(owner uint64, key string) string { return fmt.Sprintf("%d:%s", owner, key) }

func (r *fakeRepo) ClaimOrder(_ context.Context, rec *data.OrderRecord) (*data.OrderRecord, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := idemKey(rec.OwnerID, rec.IdempotencyKey)
	if id, ok := r.byIdem[k]; ok {
		return copyOrder(r.orders[id]), true, nil
	}
	r.byIdem[k] = rec.OrderID
	r.orders[rec.OrderID] = copyOrder(rec)
	return rec, false, nil
}

func (r *fakeRepo) GetOrder(_ context.Context, _ uint32, orderID uint64) (*data.OrderRecord, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.orders[orderID]
	if !ok {
		return nil, false, nil
	}
	return copyOrder(o), true, nil
}

func (r *fakeRepo) UpdateOrder(_ context.Context, rec *data.OrderRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.orders[rec.OrderID] = copyOrder(rec)
	return nil
}

func (r *fakeRepo) RecordMatch(_ context.Context, m *data.MatchRecord) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.matches[m.MatchID]; ok {
		return true, nil
	}
	r.matches[m.MatchID] = m
	return false, nil
}

func (r *fakeRepo) ListMarketOrders(_ context.Context, marketID uint32, side data.Side, limit int) ([]*data.OrderRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*data.OrderRecord
	for _, o := range r.orders {
		if o.MarketID == marketID && o.Side == side && (o.Status == data.StatusOpen || o.Status == data.StatusPartial) {
			out = append(out, copyOrder(o))
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (r *fakeRepo) ListOwnerOrders(_ context.Context, ownerID uint64, activeOnly bool) ([]*data.OrderRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*data.OrderRecord
	for _, o := range r.orders {
		if o.OwnerID != ownerID {
			continue
		}
		if activeOnly && o.Status != data.StatusOpen && o.Status != data.StatusPartial {
			continue
		}
		out = append(out, copyOrder(o))
	}
	return out, nil
}

// newTestUsecase 装配:内存 repo + miniredis 订单簿 + 计数 ledger。
func newTestUsecase(t *testing.T) (*AuctionUsecase, *fakeRepo, *trackLedger) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	repo := newFakeRepo()
	book := data.NewRedisBookStore(rdb)
	ledger := &trackLedger{}
	uc := NewAuctionUsecase(repo, book, ledger, nil, &seqGen{n: 1000}, conf.AuctionConf{})
	return uc, repo, ledger
}

// ── 用例 ──────────────────────────────────────────────────────────────────────

func TestPlaceOrder_RestsWhenNoCounterparty(t *testing.T) {
	uc, _, ledger := newTestUsecase(t)
	ctx := context.Background()

	o, err := uc.PlaceOrder(ctx, 1, 100, 200, 10, 100, "s1")
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if o.GetStatus() != auctionv1.AuctionOrderStatus_AUCTION_ORDER_STATUS_OPEN {
		t.Fatalf("status = %v, want OPEN", o.GetStatus())
	}
	if o.GetFilledQuantity() != 0 {
		t.Fatalf("filled = %d, want 0", o.GetFilledQuantity())
	}
	if got := ledger.totalQty(); got != 0 {
		t.Fatalf("settled qty = %d, want 0", got)
	}
}

func TestMatch_FullFill(t *testing.T) {
	uc, _, ledger := newTestUsecase(t)
	ctx := context.Background()

	sell, _ := uc.PlaceOrder(ctx, 1, 100, 200, 10, 100, "s1")
	bid, err := uc.Bid(ctx, 2, 100, 200, 10, 100, "b1")
	if err != nil {
		t.Fatalf("bid: %v", err)
	}
	if bid.GetStatus() != auctionv1.AuctionOrderStatus_AUCTION_ORDER_STATUS_FILLED {
		t.Fatalf("bid status = %v, want FILLED", bid.GetStatus())
	}
	if bid.GetFilledQuantity() != 10 {
		t.Fatalf("bid filled = %d, want 10", bid.GetFilledQuantity())
	}
	if got := ledger.totalQty(); got != 10 {
		t.Fatalf("settled qty = %d, want 10", got)
	}
	_ = sell
}

func TestMatch_NoOversell_Concurrent(t *testing.T) {
	uc, repo, ledger := newTestUsecase(t)
	ctx := context.Background()

	sell, _ := uc.PlaceOrder(ctx, 1, 100, 200, 10, 100, "s1")

	// 两买家并发各吃 10:per-market 单写者串行 → 总成交恰好 10,绝不超卖。
	var wg sync.WaitGroup
	for i, buyer := range []uint64{2, 3} {
		wg.Add(1)
		go func(buyer uint64, idx int) {
			defer wg.Done()
			_, _ = uc.Bid(ctx, buyer, 100, 200, 10, 100, fmt.Sprintf("b%d", idx))
		}(buyer, i)
	}
	wg.Wait()

	if got := ledger.totalQty(); got != 10 {
		t.Fatalf("oversell: settled qty = %d, want 10", got)
	}
	got, _, _ := repo.GetOrder(ctx, 100, sell.GetOrderId())
	if got.FilledQuantity != 10 {
		t.Fatalf("seller filled = %d, want 10", got.FilledQuantity)
	}
}

func TestPlaceOrder_Idempotent(t *testing.T) {
	uc, repo, ledger := newTestUsecase(t)
	ctx := context.Background()

	first, _ := uc.Bid(ctx, 2, 100, 200, 5, 100, "dup")
	second, err := uc.Bid(ctx, 2, 100, 200, 5, 100, "dup")
	if err != nil {
		t.Fatalf("second bid: %v", err)
	}
	if first.GetOrderId() != second.GetOrderId() {
		t.Fatalf("idempotent replay returned different order: %d vs %d", first.GetOrderId(), second.GetOrderId())
	}
	owned, _ := repo.ListOwnerOrders(ctx, 2, false)
	if len(owned) != 1 {
		t.Fatalf("orders = %d, want 1 (no duplicate insert)", len(owned))
	}
	if got := ledger.totalQty(); got != 0 {
		t.Fatalf("settled qty = %d, want 0", got)
	}
}

func TestMatch_PriceTimePriority(t *testing.T) {
	uc, _, _ := newTestUsecase(t)
	ctx := context.Background()

	earlier, _ := uc.PlaceOrder(ctx, 1, 100, 200, 5, 100, "s_early")
	_, _ = uc.PlaceOrder(ctx, 4, 100, 200, 5, 100, "s_late")

	// 买家只吃 5:应优先成交更早挂的 earlier(同价时间优先)。
	bid, err := uc.Bid(ctx, 2, 100, 200, 5, 100, "b1")
	if err != nil {
		t.Fatalf("bid: %v", err)
	}
	if bid.GetFilledQuantity() != 5 {
		t.Fatalf("bid filled = %d, want 5", bid.GetFilledQuantity())
	}
	// earlier 应被吃满。
	uc2 := uc
	got := mustGetOrder(t, uc2, earlier.GetOrderId())
	if got.GetStatus() != auctionv1.AuctionOrderStatus_AUCTION_ORDER_STATUS_FILLED {
		t.Fatalf("earlier status = %v, want FILLED (time priority)", got.GetStatus())
	}
}

func TestMatch_PartialFill(t *testing.T) {
	uc, repo, ledger := newTestUsecase(t)
	ctx := context.Background()

	sell, _ := uc.PlaceOrder(ctx, 1, 100, 200, 10, 100, "s1")
	bid, err := uc.Bid(ctx, 2, 100, 200, 4, 100, "b1")
	if err != nil {
		t.Fatalf("bid: %v", err)
	}
	if bid.GetStatus() != auctionv1.AuctionOrderStatus_AUCTION_ORDER_STATUS_FILLED {
		t.Fatalf("bid status = %v, want FILLED", bid.GetStatus())
	}
	got, _, _ := repo.GetOrder(ctx, 100, sell.GetOrderId())
	if got.Remaining() != 6 {
		t.Fatalf("seller remaining = %d, want 6", got.Remaining())
	}
	if got.Status != data.StatusPartial {
		t.Fatalf("seller status = %d, want PARTIAL", got.Status)
	}
	if q := ledger.totalQty(); q != 4 {
		t.Fatalf("settled qty = %d, want 4", q)
	}
}

func TestCancelOrder(t *testing.T) {
	uc, _, _ := newTestUsecase(t)
	ctx := context.Background()

	sell, _ := uc.PlaceOrder(ctx, 1, 100, 200, 10, 100, "s1")
	if err := uc.CancelOrder(ctx, 1, 100, sell.GetOrderId()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// 撤单后买家来吃应吃不到(簿已移除)。
	bid, _ := uc.Bid(ctx, 2, 100, 200, 10, 100, "b1")
	if bid.GetFilledQuantity() != 0 {
		t.Fatalf("bid filled = %d after cancel, want 0", bid.GetFilledQuantity())
	}
}

func mustGetOrder(t *testing.T, uc *AuctionUsecase, orderID uint64) *auctionv1.AuctionOrder {
	t.Helper()
	o, ok, err := uc.repo.GetOrder(context.Background(), 100, orderID)
	if err != nil || !ok {
		t.Fatalf("get order %d: ok=%v err=%v", orderID, ok, err)
	}
	return toProtoOrder(o)
}
