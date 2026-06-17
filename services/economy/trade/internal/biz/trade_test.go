// trade_test.go — TradeUsecase 业务逻辑单测(2026-06-16)。
//
// 用内存版 fakeRepo / fakeLedger / fakeAudit 复刻 Redis + 账本 + kafka 语义,无需真依赖。
// 覆盖:挂单 → 买方确认 → 卖方确认结算闭环 + 自交易 / 空物品 / 越权确认 / 顺序错误 /
// 取消 / 终态再取消 / 结算不足 → FAILED / 过期 / ListMyOrders(activeOnly)。
package biz

import (
	"context"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/errcode"
	tradev1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/trade/v1"
	"github.com/luyuancpp/pandora/services/economy/trade/internal/conf"
)

// ── fakes ─────────────────────────────────────────────────────────────────────

type fakeRepo struct {
	orders  map[uint64]*tradev1.Order
	players map[uint64]map[uint64]bool // player → set(order_id)
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{orders: map[uint64]*tradev1.Order{}, players: map[uint64]map[uint64]bool{}}
}

func (f *fakeRepo) addIndex(playerID, orderID uint64) {
	if f.players[playerID] == nil {
		f.players[playerID] = map[uint64]bool{}
	}
	f.players[playerID][orderID] = true
}

func (f *fakeRepo) CreateOrder(_ context.Context, order *tradev1.Order, _ time.Duration) error {
	f.orders[order.GetOrderId()] = order
	f.addIndex(order.GetSellerId(), order.GetOrderId())
	if order.GetBuyerId() != 0 {
		f.addIndex(order.GetBuyerId(), order.GetOrderId())
	}
	return nil
}

func (f *fakeRepo) GetOrder(_ context.Context, orderID uint64) (*tradev1.Order, bool, error) {
	o, ok := f.orders[orderID]
	return o, ok, nil
}

func (f *fakeRepo) UpdateWithLock(_ context.Context, orderID uint64, _ int, fn func(*tradev1.Order) error, _ time.Duration) error {
	o, ok := f.orders[orderID]
	if !ok {
		return errcode.New(errcode.ErrTradeOrderNotFound, "order %d not found", orderID)
	}
	if err := fn(o); err != nil {
		return err
	}
	return nil
}

func (f *fakeRepo) ListPlayerOrderIDs(_ context.Context, playerID uint64) ([]uint64, error) {
	var ids []uint64
	for id := range f.players[playerID] {
		ids = append(ids, id)
	}
	return ids, nil
}

type fakeLedger struct {
	fail bool
}

func (f *fakeLedger) Settle(_ context.Context, _ *tradev1.Order, _ uint64) error {
	if f.fail {
		return errcode.New(errcode.ErrTradeInsufficient, "insufficient")
	}
	return nil
}

type fakeAudit struct{ count int }

func (f *fakeAudit) PushAudit(_ context.Context, _ *tradev1.Order) error {
	f.count++
	return nil
}

type seqSF struct{ n uint64 }

func (s *seqSF) Generate() uint64 { s.n++; return s.n }

// ── helpers ───────────────────────────────────────────────────────────────────

func newUC(repo *fakeRepo, ledger ResourceLedger) (*TradeUsecase, *fakeAudit) {
	audit := &fakeAudit{}
	cfg := conf.TradeConf{
		OrderTTL:         config.Duration(10 * time.Minute),
		OrderExpire:      config.Duration(5 * time.Minute),
		OptimisticRetry:  3,
		MaxItemsPerOrder: 20,
	}
	return NewTradeUsecase(repo, ledger, audit, &seqSF{}, cfg), audit
}

func items() []*tradev1.TradeItem {
	return []*tradev1.TradeItem{{ItemUid: "sword-1", Count: 1}}
}

func wantCode(t *testing.T, err error, code errcode.Code) {
	t.Helper()
	if errcode.As(err) != code {
		t.Fatalf("want code %d, got err=%v (code=%d)", code, err, errcode.As(err))
	}
}

// ── 测试 ───────────────────────────────────────────────────────────────────────

func TestCreateOrder_OK(t *testing.T) {
	repo := newFakeRepo()
	uc, _ := newUC(repo, &fakeLedger{})
	id, err := uc.CreateOrder(context.Background(), 1, 2, items(), 100)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	o := repo.orders[id]
	if o.GetState() != tradev1.OrderState_ORDER_STATE_PENDING {
		t.Fatalf("want PENDING, got %s", o.GetState())
	}
	if o.GetSellerId() != 1 || o.GetBuyerId() != 2 {
		t.Fatalf("wrong parties: %+v", o)
	}
}

func TestCreateOrder_Self(t *testing.T) {
	uc, _ := newUC(newFakeRepo(), &fakeLedger{})
	_, err := uc.CreateOrder(context.Background(), 1, 1, items(), 100)
	wantCode(t, err, errcode.ErrInvalidArg)
}

func TestCreateOrder_NoItems(t *testing.T) {
	uc, _ := newUC(newFakeRepo(), &fakeLedger{})
	_, err := uc.CreateOrder(context.Background(), 1, 2, nil, 100)
	wantCode(t, err, errcode.ErrInvalidArg)
}

func TestTwoPhaseConfirm_OK(t *testing.T) {
	repo := newFakeRepo()
	uc, _ := newUC(repo, &fakeLedger{})
	id, _ := uc.CreateOrder(context.Background(), 1, 2, items(), 100)

	// 买方先确认 → BUYER_CONFIRMED
	st, err := uc.ConfirmOrder(context.Background(), 2, id)
	if err != nil {
		t.Fatalf("buyer confirm err: %v", err)
	}
	if st != tradev1.OrderState_ORDER_STATE_BUYER_CONFIRMED {
		t.Fatalf("want BUYER_CONFIRMED, got %s", st)
	}

	// 卖方确认 → 结算 → COMPLETED
	st, err = uc.ConfirmOrder(context.Background(), 1, id)
	if err != nil {
		t.Fatalf("seller confirm err: %v", err)
	}
	if st != tradev1.OrderState_ORDER_STATE_COMPLETED {
		t.Fatalf("want COMPLETED, got %s", st)
	}
}

func TestConfirm_SellerBeforeBuyer(t *testing.T) {
	repo := newFakeRepo()
	uc, _ := newUC(repo, &fakeLedger{})
	id, _ := uc.CreateOrder(context.Background(), 1, 2, items(), 100)

	// 卖方在买方之前确认 → 顺序错误
	_, err := uc.ConfirmOrder(context.Background(), 1, id)
	wantCode(t, err, errcode.ErrTradeWrongState)
}

func TestConfirm_Outsider(t *testing.T) {
	repo := newFakeRepo()
	uc, _ := newUC(repo, &fakeLedger{})
	id, _ := uc.CreateOrder(context.Background(), 1, 2, items(), 100)

	_, err := uc.ConfirmOrder(context.Background(), 99, id)
	wantCode(t, err, errcode.ErrUnauthorized)
}

func TestConfirm_SettleInsufficient(t *testing.T) {
	repo := newFakeRepo()
	uc, _ := newUC(repo, &fakeLedger{fail: true})
	id, _ := uc.CreateOrder(context.Background(), 1, 2, items(), 100)
	_, _ = uc.ConfirmOrder(context.Background(), 2, id) // buyer

	st, err := uc.ConfirmOrder(context.Background(), 1, id) // seller → settle fails
	wantCode(t, err, errcode.ErrTradeInsufficient)
	if st != tradev1.OrderState_ORDER_STATE_FAILED {
		t.Fatalf("want FAILED, got %s", st)
	}
	if repo.orders[id].GetState() != tradev1.OrderState_ORDER_STATE_FAILED {
		t.Fatalf("order not persisted as FAILED: %s", repo.orders[id].GetState())
	}
}

func TestCancel_OK(t *testing.T) {
	repo := newFakeRepo()
	uc, _ := newUC(repo, &fakeLedger{})
	id, _ := uc.CreateOrder(context.Background(), 1, 2, items(), 100)

	if err := uc.CancelOrder(context.Background(), 2, id); err != nil {
		t.Fatalf("cancel err: %v", err)
	}
	if repo.orders[id].GetState() != tradev1.OrderState_ORDER_STATE_CANCELED {
		t.Fatalf("want CANCELED, got %s", repo.orders[id].GetState())
	}
}

func TestCancel_Terminal(t *testing.T) {
	repo := newFakeRepo()
	uc, _ := newUC(repo, &fakeLedger{})
	id, _ := uc.CreateOrder(context.Background(), 1, 2, items(), 100)
	_, _ = uc.ConfirmOrder(context.Background(), 2, id)
	_, _ = uc.ConfirmOrder(context.Background(), 1, id) // COMPLETED

	err := uc.CancelOrder(context.Background(), 1, id)
	wantCode(t, err, errcode.ErrTradeWrongState)
}

func TestConfirm_Expired(t *testing.T) {
	repo := newFakeRepo()
	uc, _ := newUC(repo, &fakeLedger{})
	id, _ := uc.CreateOrder(context.Background(), 1, 2, items(), 100)
	// 手动把订单改成已过期。
	repo.orders[id].ExpiresAtMs = time.Now().Add(-time.Minute).UnixMilli()

	state, err := uc.ConfirmOrder(context.Background(), 2, id)
	wantCode(t, err, errcode.ErrTradeOrderExpired)
	// 返回状态应为 EXPIRED(惰性过期)。
	if state != tradev1.OrderState_ORDER_STATE_EXPIRED {
		t.Fatalf("want returned state EXPIRED, got %s", state)
	}
	// 关键:访问过期订单时必须把 EXPIRED 持久化回 repo(此前只在内存置位不写回,
	// 订单状态停留在 PENDING,过期态永远落不了库)。
	if got := repo.orders[id].GetState(); got != tradev1.OrderState_ORDER_STATE_EXPIRED {
		t.Fatalf("want repo order state EXPIRED after Confirm, got %s", got)
	}
}

func TestListMyOrders_ActiveOnly(t *testing.T) {
	repo := newFakeRepo()
	uc, _ := newUC(repo, &fakeLedger{})
	id1, _ := uc.CreateOrder(context.Background(), 1, 2, items(), 100)
	id2, _ := uc.CreateOrder(context.Background(), 1, 3, items(), 200)
	_ = uc.CancelOrder(context.Background(), 1, id2) // id2 终态

	all, err := uc.ListMyOrders(context.Background(), 1, false)
	if err != nil {
		t.Fatalf("list err: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 all, got %d", len(all))
	}

	active, _ := uc.ListMyOrders(context.Background(), 1, true)
	if len(active) != 1 || active[0].GetOrderId() != id1 {
		t.Fatalf("want 1 active (id %d), got %+v", id1, active)
	}
}

func TestConfirm_NotFound(t *testing.T) {
	uc, _ := newUC(newFakeRepo(), &fakeLedger{})
	_, err := uc.ConfirmOrder(context.Background(), 1, 999)
	wantCode(t, err, errcode.ErrTradeOrderNotFound)
}
