// Package biz 是 auction 服务的业务逻辑层(全服拍卖行 / 撮合引擎,2026-06-19)。
//
// 职责(docs/design/decision-revisit-auction-engine.md):
//   - 挂单(SELL)/ 出价(BUY)进按 market_id 分片的订单簿;
//   - 「每个 market 单写者」串行撮合,价格-时间优先(被动挂单价成交);
//   - 两层幂等:① 挂单 idempotency_key(uk owner+key,重试不重复挂单);
//     ② 结算 match_id(uk,资产只转一次,不变量 §9.2 / §9.7);
//   - 成交发 kafka pandora.auction.match,订单流转发 pandora.auction.audit(弱依赖)。
//
// 单写者实现:进程内 per-market 互斥锁(striped lock)。同一 market 的挂单 / 出价 / �leg单
// 全程持锁串行,订单簿与权威库不会并发改 → 不会超卖。跨实例的「每 market 单写者」需配一致性
// 哈希路由(每个 market 固定落一个实例),属扩容步骤,后续接入;W1 单实例进程内串行即可。
//
// owner_id / buyer_id 一律以 JWT ctx 为准(R5),service 层注入。
package biz

import (
	"context"
	"sync"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	auctionv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/auction/v1"

	"github.com/luyuancpp/pandora/services/economy/auction/internal/conf"
	"github.com/luyuancpp/pandora/services/economy/auction/internal/data"
)

// SettlementLedger 抽象「成交资产原子转移 + 幂等」(不变量 §9.7)。
//
// Settle 在每笔撮合成交时调用:把卖家道具转给买家、买家货币(quantity*price)转给卖家,
// 幂等键 = match_id 保证同一成交重复结算只生效一次。
// 返回 ErrAuctionInsufficient 表示买方货币 / 卖方道具不足。
// W1 暂用 NoopSettlementLedger 占位;真实账本接 inventory / 货币原子事务后替换。
type SettlementLedger interface {
	Settle(ctx context.Context, m *data.MatchRecord) error
}

// NoopSettlementLedger 是占位实现:总是结算成功。
type NoopSettlementLedger struct{}

// Settle 永远成功(占位)。
func (NoopSettlementLedger) Settle(_ context.Context, _ *data.MatchRecord) error { return nil }

// AuctionEventPusher 把成交 / 订单流转发 kafka(main.go 注入;弱依赖,nil 静默)。
type AuctionEventPusher interface {
	PushMatch(ctx context.Context, e *auctionv1.AuctionMatchEvent) error
	PushAudit(ctx context.Context, o *auctionv1.AuctionOrder) error
}

// snowflakeGen 是 snowflake.Node 的最小接口。
type snowflakeGen interface {
	Generate() uint64
}

// AuctionUsecase 是 auction 服务业务逻辑核心。
type AuctionUsecase struct {
	repo   data.AuctionRepo
	book   data.BookStore
	ledger SettlementLedger
	events AuctionEventPusher // 弱依赖,可为 nil
	sf     snowflakeGen
	cfg    conf.AuctionConf

	mu    sync.Mutex             // 保护 locks map 本身
	locks map[uint32]*sync.Mutex // per-market 单写者锁(惰性建)
}

// NewAuctionUsecase 构造。ledger 为 nil 时退化为 Noop;events 允许 nil。
func NewAuctionUsecase(repo data.AuctionRepo, book data.BookStore, ledger SettlementLedger, events AuctionEventPusher, sf snowflakeGen, cfg conf.AuctionConf) *AuctionUsecase {
	if ledger == nil {
		ledger = NoopSettlementLedger{}
	}
	if cfg.MaxQuantityPerOrder <= 0 {
		cfg.MaxQuantityPerOrder = 1_000_000
	}
	if cfg.MaxPrice <= 0 {
		cfg.MaxPrice = 1_000_000_000
	}
	if cfg.DefaultListLimit <= 0 {
		cfg.DefaultListLimit = 50
	}
	if cfg.MaxListLimit <= 0 {
		cfg.MaxListLimit = 200
	}
	return &AuctionUsecase{
		repo:   repo,
		book:   book,
		ledger: ledger,
		events: events,
		sf:     sf,
		cfg:    cfg,
		locks:  make(map[uint32]*sync.Mutex),
	}
}

// lockMarket 取 market 的单写者锁(惰性建)。
func (u *AuctionUsecase) lockMarket(marketID uint32) *sync.Mutex {
	u.mu.Lock()
	m, ok := u.locks[marketID]
	if !ok {
		m = &sync.Mutex{}
		u.locks[marketID] = m
	}
	u.mu.Unlock()
	return m
}

// PlaceOrder 卖家挂单(SELL)。ownerID 由 service 从 JWT ctx 得到(R5)。
func (u *AuctionUsecase) PlaceOrder(ctx context.Context, ownerID uint64, marketID, itemConfigID uint32, quantity, price int64, idemKey string) (*auctionv1.AuctionOrder, error) {
	return u.submit(ctx, ownerID, data.SideSell, marketID, itemConfigID, quantity, price, idemKey)
}

// Bid 买家出价(BUY)。ownerID 由 service 从 JWT ctx 得到(R5)。
func (u *AuctionUsecase) Bid(ctx context.Context, ownerID uint64, marketID, itemConfigID uint32, quantity, price int64, idemKey string) (*auctionv1.AuctionOrder, error) {
	return u.submit(ctx, ownerID, data.SideBuy, marketID, itemConfigID, quantity, price, idemKey)
}

// submit 是挂单 / 出价的统一入口:幂等登记 → 撮合 → 挂剩余 → 持久化。
func (u *AuctionUsecase) submit(ctx context.Context, ownerID uint64, side data.Side, marketID, itemConfigID uint32, quantity, price int64, idemKey string) (*auctionv1.AuctionOrder, error) {
	if ownerID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "owner required")
	}
	if marketID == 0 || itemConfigID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "market_id / item_config_id required")
	}
	if quantity <= 0 || quantity > u.cfg.MaxQuantityPerOrder {
		return nil, errcode.New(errcode.ErrInvalidArg, "quantity out of range: %d (max %d)", quantity, u.cfg.MaxQuantityPerOrder)
	}
	if price <= 0 || price > u.cfg.MaxPrice {
		return nil, errcode.New(errcode.ErrInvalidArg, "price out of range: %d (max %d)", price, u.cfg.MaxPrice)
	}
	if idemKey == "" {
		return nil, errcode.New(errcode.ErrInvalidArg, "idempotency_key required")
	}

	lock := u.lockMarket(marketID)
	lock.Lock()
	defer lock.Unlock()

	now := nowMs()
	rec := &data.OrderRecord{
		OrderID:        u.sf.Generate(),
		MarketID:       marketID,
		OwnerID:        ownerID,
		Side:           side,
		ItemConfigID:   itemConfigID,
		Quantity:       quantity,
		FilledQuantity: 0,
		Price:          price,
		Status:         data.StatusOpen,
		IdempotencyKey: idemKey,
		CreatedAtMs:    now,
		UpdatedAtMs:    now,
	}

	existing, already, err := u.repo.ClaimOrder(ctx, rec)
	if err != nil {
		return nil, err
	}
	if already {
		// 幂等命中:返回已存挂单快照,不重复撮合。
		return toProtoOrder(existing), nil
	}

	// 撮合:对手盘逐笔成交(价格-时间优先,被动挂单价)。
	if err := u.match(ctx, rec); err != nil {
		return nil, err
	}

	// 剩余量挂到自己这一侧的簿等待对手。
	if rec.Remaining() > 0 {
		if err := u.book.Add(ctx, marketID, side, rec.OrderID, price); err != nil {
			return nil, err
		}
		if rec.FilledQuantity == 0 {
			rec.Status = data.StatusOpen
		} else {
			rec.Status = data.StatusPartial
		}
	}
	rec.UpdatedAtMs = nowMs()
	if err := u.repo.UpdateOrder(ctx, rec); err != nil {
		return nil, err
	}
	u.pushAudit(ctx, toProtoOrder(rec))
	return toProtoOrder(rec), nil
}

// match 让 incoming 与对手盘逐笔撮合。调用方已持 market 锁(单写者)。
func (u *AuctionUsecase) match(ctx context.Context, incoming *data.OrderRecord) error {
	opp := opposite(incoming.Side)
	for incoming.Remaining() > 0 {
		bestID, bestPrice, ok, err := u.book.Best(ctx, incoming.MarketID, opp)
		if err != nil {
			return err
		}
		if !ok || !crosses(incoming.Side, incoming.Price, bestPrice) {
			break // 无对手盘或价格不交叉 → 停止撮合
		}

		resting, found, gerr := u.repo.GetOrder(ctx, incoming.MarketID, bestID)
		if gerr != nil {
			return gerr
		}
		if !found || resting.Remaining() <= 0 || isTerminal(resting.Status) {
			// 簿上残留陈旧条目(权威库已终态)→ 清掉继续。
			if rerr := u.book.Remove(ctx, incoming.MarketID, opp, bestID); rerr != nil {
				return rerr
			}
			continue
		}

		qty := minInt64(incoming.Remaining(), resting.Remaining())
		matchPrice := resting.Price // 成交价 = 被动(挂在簿上)挂单价
		m := buildMatch(u.sf.Generate(), incoming, resting, qty, matchPrice, nowMs())

		// 结算(原子转移 + 幂等键 = match_id)。失败 → 中止本次提交,剩余不挂簿。
		if serr := u.ledger.Settle(ctx, m); serr != nil {
			return serr
		}
		if _, rerr := u.repo.RecordMatch(ctx, m); rerr != nil {
			return rerr
		}

		// 更新双方成交量 / 状态。
		incoming.FilledQuantity += qty
		resting.FilledQuantity += qty
		resting.UpdatedAtMs = m.MatchedAtMs
		if resting.Remaining() == 0 {
			resting.Status = data.StatusFilled
			if rerr := u.book.Remove(ctx, incoming.MarketID, opp, resting.OrderID); rerr != nil {
				return rerr
			}
		} else {
			resting.Status = data.StatusPartial
		}
		if uerr := u.repo.UpdateOrder(ctx, resting); uerr != nil {
			return uerr
		}

		if incoming.Remaining() == 0 {
			incoming.Status = data.StatusFilled
		} else {
			incoming.Status = data.StatusPartial
		}
		incoming.UpdatedAtMs = m.MatchedAtMs

		u.pushMatch(ctx, m)
		u.pushAudit(ctx, toProtoOrder(resting))
	}
	return nil
}

// CancelOrder 撤单(仅挂单本人,未终态前)。ownerID 由 service 从 JWT ctx 得到(R5)。
func (u *AuctionUsecase) CancelOrder(ctx context.Context, ownerID uint64, marketID uint32, orderID uint64) error {
	if ownerID == 0 || marketID == 0 || orderID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "owner / market / order required")
	}
	lock := u.lockMarket(marketID)
	lock.Lock()
	defer lock.Unlock()

	o, found, err := u.repo.GetOrder(ctx, marketID, orderID)
	if err != nil {
		return err
	}
	if !found {
		return errcode.New(errcode.ErrAuctionOrderNotFound, "order %d not found", orderID)
	}
	if o.OwnerID != ownerID {
		return errcode.New(errcode.ErrAuctionNotOwner, "player %d not owner of order %d", ownerID, orderID)
	}
	if isTerminal(o.Status) {
		return errcode.New(errcode.ErrAuctionWrongState, "order %d already terminal", orderID)
	}

	if rerr := u.book.Remove(ctx, marketID, o.Side, orderID); rerr != nil {
		return rerr
	}
	o.Status = data.StatusCanceled
	o.UpdatedAtMs = nowMs()
	if uerr := u.repo.UpdateOrder(ctx, o); uerr != nil {
		return uerr
	}
	u.pushAudit(ctx, toProtoOrder(o))
	return nil
}

// ListMarket 看某市场订单簿。side=UNSPECIFIED → 返回买 + 卖两侧。
func (u *AuctionUsecase) ListMarket(ctx context.Context, marketID uint32, side data.Side, limit int) ([]*auctionv1.AuctionOrder, error) {
	if marketID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "market_id required")
	}
	if limit <= 0 {
		limit = u.cfg.DefaultListLimit
	}
	if limit > u.cfg.MaxListLimit {
		limit = u.cfg.MaxListLimit
	}

	var out []*auctionv1.AuctionOrder
	if side == data.SideSell || side == 0 {
		recs, err := u.repo.ListMarketOrders(ctx, marketID, data.SideSell, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, toProtoOrders(recs)...)
	}
	if side == data.SideBuy || side == 0 {
		recs, err := u.repo.ListMarketOrders(ctx, marketID, data.SideBuy, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, toProtoOrders(recs)...)
	}
	return out, nil
}

// ListMyOrders 看玩家自己的挂单 / 出价。ownerID 由 service 从 JWT ctx 得到(R5)。
func (u *AuctionUsecase) ListMyOrders(ctx context.Context, ownerID uint64, activeOnly bool) ([]*auctionv1.AuctionOrder, error) {
	if ownerID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "owner required")
	}
	recs, err := u.repo.ListOwnerOrders(ctx, ownerID, activeOnly)
	if err != nil {
		return nil, err
	}
	return toProtoOrders(recs), nil
}

// ── 辅助 ──────────────────────────────────────────────────────────────────────

// opposite 返回对手盘方向。
func opposite(s data.Side) data.Side {
	if s == data.SideSell {
		return data.SideBuy
	}
	return data.SideSell
}

// crosses 判断 incoming 价格能否吃到对手盘最优价 bestPrice。
//   - incoming SELL @ P:对手是 BUY,bestPrice(最高买价)>= P 才成交。
//   - incoming BUY  @ P:对手是 SELL,bestPrice(最低卖价)<= P 才成交。
func crosses(side data.Side, price, bestPrice int64) bool {
	if side == data.SideSell {
		return bestPrice >= price
	}
	return bestPrice <= price
}

// buildMatch 按 incoming / resting 的买卖角色组装成交记录。
func buildMatch(matchID uint64, incoming, resting *data.OrderRecord, qty, price, ms int64) *data.MatchRecord {
	m := &data.MatchRecord{
		MatchID:      matchID,
		MarketID:     incoming.MarketID,
		ItemConfigID: incoming.ItemConfigID,
		Quantity:     qty,
		Price:        price,
		MatchedAtMs:  ms,
	}
	if incoming.Side == data.SideSell {
		m.SellOrderID, m.SellerID = incoming.OrderID, incoming.OwnerID
		m.BuyOrderID, m.BuyerID = resting.OrderID, resting.OwnerID
	} else {
		m.BuyOrderID, m.BuyerID = incoming.OrderID, incoming.OwnerID
		m.SellOrderID, m.SellerID = resting.OrderID, resting.OwnerID
	}
	return m
}

// isTerminal 判断订单是否已到终态(不可再流转)。
func isTerminal(status int32) bool {
	switch status {
	case data.StatusFilled, data.StatusCanceled, data.StatusExpired:
		return true
	default:
		return false
	}
}

func toProtoOrder(r *data.OrderRecord) *auctionv1.AuctionOrder {
	return &auctionv1.AuctionOrder{
		OrderId:        r.OrderID,
		MarketId:       r.MarketID,
		OwnerId:        r.OwnerID,
		Side:           auctionv1.OrderSide(r.Side),
		ItemConfigId:   r.ItemConfigID,
		Quantity:       r.Quantity,
		FilledQuantity: r.FilledQuantity,
		Price:          r.Price,
		Status:         auctionv1.AuctionOrderStatus(r.Status),
		CreatedAtMs:    r.CreatedAtMs,
		UpdatedAtMs:    r.UpdatedAtMs,
	}
}

func toProtoOrders(recs []*data.OrderRecord) []*auctionv1.AuctionOrder {
	out := make([]*auctionv1.AuctionOrder, 0, len(recs))
	for _, r := range recs {
		out = append(out, toProtoOrder(r))
	}
	return out
}

func toProtoMatch(m *data.MatchRecord) *auctionv1.AuctionMatchEvent {
	return &auctionv1.AuctionMatchEvent{
		MatchId:      m.MatchID,
		MarketId:     m.MarketID,
		SellOrderId:  m.SellOrderID,
		BuyOrderId:   m.BuyOrderID,
		SellerId:     m.SellerID,
		BuyerId:      m.BuyerID,
		ItemConfigId: m.ItemConfigID,
		Quantity:     m.Quantity,
		Price:        m.Price,
		MatchedAtMs:  m.MatchedAtMs,
	}
}

// pushMatch 弱依赖成交事件推送。
func (u *AuctionUsecase) pushMatch(ctx context.Context, m *data.MatchRecord) {
	if u.events == nil {
		return
	}
	if err := u.events.PushMatch(ctx, toProtoMatch(m)); err != nil {
		plog.With(ctx).Warnw("msg", "auction_match_push_failed", "match_id", m.MatchID, "err", err)
	}
}

// pushAudit 弱依赖订单流转审计推送。
func (u *AuctionUsecase) pushAudit(ctx context.Context, o *auctionv1.AuctionOrder) {
	if u.events == nil {
		return
	}
	if err := u.events.PushAudit(ctx, o); err != nil {
		plog.With(ctx).Warnw("msg", "auction_audit_push_failed", "order_id", o.GetOrderId(), "err", err)
	}
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func nowMs() int64 { return time.Now().UnixMilli() }
