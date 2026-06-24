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

	"github.com/luyuancpp/pandora/pkg/errcode"
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
	mu       sync.Mutex
	matches  []*data.MatchRecord
	freezes  []uint64        // 冻结过的 order_id
	releases []uint64        // 退还过的 order_id
	failFor  map[uint64]bool // 这些 order_id 的 Freeze 返回资产不足
}

func (l *trackLedger) Freeze(_ context.Context, _, orderID uint64, _ data.Side, _ uint32, _, _ int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failFor[orderID] {
		return errcode.New(errcode.ErrAuctionInsufficient, "test freeze insufficient order=%d", orderID)
	}
	l.freezes = append(l.freezes, orderID)
	return nil
}

func (l *trackLedger) Settle(_ context.Context, m *data.MatchRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.matches = append(l.matches, m)
	return nil
}

func (l *trackLedger) Release(_ context.Context, _, orderID uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.releases = append(l.releases, orderID)
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

func (l *trackLedger) releaseCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.releases)
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

func (r *fakeRepo) ListExpirableOrders(_ context.Context, createdBeforeMs int64, limit int) ([]*data.OrderRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*data.OrderRecord
	for _, o := range r.orders {
		if o.CreatedAtMs >= createdBeforeMs {
			continue
		}
		if o.Status != data.StatusOpen && o.Status != data.StatusPartial {
			continue
		}
		out = append(out, copyOrder(o))
		if limit > 0 && len(out) >= limit {
			break
		}
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

// TestMatch_SkipsSelfOrder 验证自撮合跳过:同一玩家的对手单不与自己成交,
// 既不结算、也不把自己的挂单清掉(撮合结束后原样留在簿上)。
func TestMatch_SkipsSelfOrder(t *testing.T) {
	uc, repo, ledger := newTestUsecase(t)
	ctx := context.Background()

	// 玩家 1 先挂一个卖单。
	sell, _ := uc.PlaceOrder(ctx, 1, 100, 200, 10, 100, "s_self")
	// 同一玩家 1 再下一个能交叉的买单 → 不应自成交。
	bid, err := uc.Bid(ctx, 1, 100, 200, 10, 100, "b_self")
	if err != nil {
		t.Fatalf("self bid: %v", err)
	}
	if bid.GetFilledQuantity() != 0 {
		t.Fatalf("self bid filled = %d, want 0 (self-match skipped)", bid.GetFilledQuantity())
	}
	if bid.GetStatus() != auctionv1.AuctionOrderStatus_AUCTION_ORDER_STATUS_OPEN {
		t.Fatalf("self bid status = %v, want OPEN", bid.GetStatus())
	}
	if got := ledger.totalQty(); got != 0 {
		t.Fatalf("settled qty = %d, want 0 (no self settlement)", got)
	}
	// 自己原来的卖单仍未成交,留在簿上。
	got, _, _ := repo.GetOrder(ctx, 100, sell.GetOrderId())
	if got.FilledQuantity != 0 || got.Status != data.StatusOpen {
		t.Fatalf("self sell order changed: filled=%d status=%d, want 0/OPEN", got.FilledQuantity, got.Status)
	}

	// 此时换别的买家(玩家 2)来吃,应能正常成交卖单 —— 证明自单仍在簿上、未被错误移除。
	other, err := uc.Bid(ctx, 2, 100, 200, 10, 100, "b_other")
	if err != nil {
		t.Fatalf("other bid: %v", err)
	}
	if other.GetFilledQuantity() != 10 {
		t.Fatalf("other bid filled = %d, want 10 (self sell still on book)", other.GetFilledQuantity())
	}
	if q := ledger.totalQty(); q != 10 {
		t.Fatalf("settled qty = %d, want 10", q)
	}
}

// TestFreezeFail_OrderRejected 验证挂单冻结失败(资产不足)时:订单作废、不进簿、不撮合。
func TestFreezeFail_OrderRejected(t *testing.T) {
	uc, repo, ledger := newTestUsecase(t)
	ctx := context.Background()

	// 预约:下一个生成的 order_id = 1001(seqGen 从 1000 起,Generate 先自增)。
	ledger.failFor = map[uint64]bool{1001: true}

	o, err := uc.PlaceOrder(ctx, 1, 100, 200, 10, 100, "s_freeze_fail")
	if errcode.As(err) != errcode.ErrAuctionInsufficient {
		t.Fatalf("place with freeze fail should be ErrAuctionInsufficient, got %v (order=%v)", err, o)
	}
	// 订单已落库为 CANCELED,不在簿上。
	got, ok, _ := repo.GetOrder(ctx, 100, 1001)
	if !ok || got.Status != data.StatusCanceled {
		t.Fatalf("order after freeze fail should be CANCELED, ok=%v status=%d", ok, got.Status)
	}
	// 无任何结算。
	if q := ledger.totalQty(); q != 0 {
		t.Fatalf("settled qty = %d, want 0", q)
	}
	// 后续买家来吃应吃不到(没进簿)。
	bid, _ := uc.Bid(ctx, 2, 100, 200, 10, 100, "b_after_fail")
	if bid.GetFilledQuantity() != 0 {
		t.Fatalf("bid filled = %d after rejected order, want 0", bid.GetFilledQuantity())
	}
}

// TestCancelOrder_ReleasesEscrow 验证撤单会退还 escrow。
func TestCancelOrder_ReleasesEscrow(t *testing.T) {
	uc, _, ledger := newTestUsecase(t)
	ctx := context.Background()

	sell, _ := uc.PlaceOrder(ctx, 1, 100, 200, 10, 100, "s_rel")
	if err := uc.CancelOrder(ctx, 1, 100, sell.GetOrderId()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if n := ledger.releaseCount(); n != 1 {
		t.Fatalf("release count after cancel = %d, want 1", n)
	}
}

// TestMatch_FullFillReleasesBothEscrows 验证完全成交时买卖双方都退还 escrow 残余。
func TestMatch_FullFillReleasesBothEscrows(t *testing.T) {
	uc, _, ledger := newTestUsecase(t)
	ctx := context.Background()

	// 卖单先挂(被动),买单后到(主动)完全吃掉 → 双方都 FILLED,各退一次 escrow。
	uc.PlaceOrder(ctx, 1, 100, 200, 10, 100, "s_fill")
	if _, err := uc.Bid(ctx, 2, 100, 200, 10, 100, "b_fill"); err != nil {
		t.Fatalf("bid: %v", err)
	}
	if n := ledger.releaseCount(); n != 2 {
		t.Fatalf("release count after full fill = %d, want 2 (both sides)", n)
	}
}

// TestExpireDueOrders_ExpiresAndReleases 验证过期清扫:超 TTL 的挂单被置 EXPIRED、退还 escrow,
// 之后买家来吃吃不到(已移出簿)。
func TestExpireDueOrders_ExpiresAndReleases(t *testing.T) {
	uc, repo, ledger := newTestUsecase(t)
	uc.cfg.OrderTTLSeconds = 1 // 1 秒过期
	ctx := context.Background()

	sell, _ := uc.PlaceOrder(ctx, 1, 100, 200, 10, 100, "s_exp")
	// 手动把创建时间提前到很久以前,模拟超 TTL。
	repo.mu.Lock()
	repo.orders[sell.GetOrderId()].CreatedAtMs = nowMs() - 10_000
	repo.mu.Unlock()

	n, err := uc.ExpireDueOrders(ctx)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired count = %d, want 1", n)
	}
	got, _, _ := repo.GetOrder(ctx, 100, sell.GetOrderId())
	if got.Status != data.StatusExpired {
		t.Fatalf("order status = %d, want EXPIRED(5)", got.Status)
	}
	if rc := ledger.releaseCount(); rc != 1 {
		t.Fatalf("release count after expire = %d, want 1", rc)
	}
	// 过期后买家来吃吃不到(已移出簿)。
	bid, _ := uc.Bid(ctx, 2, 100, 200, 10, 100, "b_after_exp")
	if bid.GetFilledQuantity() != 0 {
		t.Fatalf("bid filled = %d after expire, want 0", bid.GetFilledQuantity())
	}
}

// TestExpireDueOrders_DisabledByDefault 验证 OrderTTLSeconds<=0 时清扫是 no-op。
func TestExpireDueOrders_DisabledByDefault(t *testing.T) {
	uc, _, _ := newTestUsecase(t)
	ctx := context.Background()
	uc.PlaceOrder(ctx, 1, 100, 200, 10, 100, "s_noexp")
	n, err := uc.ExpireDueOrders(ctx)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 0 {
		t.Fatalf("expired count = %d, want 0 (TTL disabled)", n)
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
