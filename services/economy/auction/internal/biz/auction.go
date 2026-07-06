// Package biz 是 auction 服务的业务逻辑层(全服拍卖行 / 撮合引擎,2026-06-19)。
//
// 职责(docs/design/decision-revisit-auction-engine.md):
//   - 挂单(SELL)/ 出价(BUY)进按 market_id 分片的订单簿;
//   - 「每个 market 单写者」串行撮合,价格-时间优先(被动挂单价成交);
//   - 两层幂等:① 挂单 idempotency_key(uk owner+key,重试不重复挂单);
//     ② 结算 match_id(uk,资产只转一次,不变量 §9.2 / §9.7);
//   - 成交发 kafka pandora.auction.match,订单流转发 pandora.auction.audit(弱依赖)。
//
// 单写者实现:进程内 per-market 互斥锁(striped lock)。同一 market 的挂单 / 出价 / 撤单
// 全程持锁串行,订单簿与权威库不会并发改 → 不会超卖。跨实例的「每 market 单写者」需配一致性
// 哈希路由(每个 market 固定落一个实例),属扩容步骤,后续接入;W1 单实例进程内串行即可。
//
// owner_id / buyer_id 一律以 JWT ctx 为准(R5),service 层注入。
package biz

import (
	"context"
	"math"
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
// 三段式 escrow(挂单冻结 / 成交从 escrow 消费 / 撤单过期退还),消除「成交瞬间余额不足而失败」:
//   - Freeze 在挂单 / 出价时把资产冻进 escrow(卖单冻道具 / 买单冻金币),幂等键 = order_id;
//   - Settle 在每笔撮合成交时从双方 escrow 消费完成对转,幂等键 = match_id(资产只转一次);
//   - Release 在撤单 / 过期 / 完全成交后退还 escrow 残余(含买单成交价优于出价的价差),幂等键 = order_id。
//
// Freeze 返回 ErrAuctionInsufficient 表示挂单方资产不足(挂单即失败,不进簿)。
// W1 可用 NoopSettlementLedger 占位;真实账本接 inventory FreezeForOrder / SettleAuctionMatch / ReleaseEscrow。
type SettlementLedger interface {
	// Freeze 挂单冻结:side=SELL 冻 quantity 道具,side=BUY 冻 quantity*price 金币。资产不足 → ErrAuctionInsufficient。
	Freeze(ctx context.Context, playerID, orderID uint64, side data.Side, itemConfigID uint32, quantity, price int64) error
	// Settle 成交从双方 escrow 消费对转。幂等键 = m.MatchID。
	Settle(ctx context.Context, m *data.MatchRecord) error
	// Release 退还某挂单 escrow 残余。幂等键 = orderID。
	Release(ctx context.Context, playerID, orderID uint64) error
}

// NoopSettlementLedger 是占位实现:冻结 / 结算 / 退还都成功(不真实扣转资产)。
type NoopSettlementLedger struct{}

// Freeze 永远成功(占位)。
func (NoopSettlementLedger) Freeze(_ context.Context, _, _ uint64, _ data.Side, _ uint32, _, _ int64) error {
	return nil
}

// Settle 永远成功(占位)。
func (NoopSettlementLedger) Settle(_ context.Context, _ *data.MatchRecord) error { return nil }

// Release 永远成功(占位)。
func (NoopSettlementLedger) Release(_ context.Context, _, _ uint64) error { return nil }

// AuctionEventPusher 把成交 / 订单流转发 kafka(main.go 注入;弱依赖,nil 静默)。
type AuctionEventPusher interface {
	PushMatch(ctx context.Context, e *auctionv1.AuctionMatchEvent) error
	PushAudit(ctx context.Context, o *auctionv1.AuctionOrder) error
}

// snowflakeGen 是 snowflake.Node 的最小接口。
type snowflakeGen interface {
	Generate() uint64
}

// MarketLocker 抽象「跨实例的 per-market 单写者锁」(限制#2:多实例一致性)。
//
// 进程内 striped lock 只在单实例内串行;多实例部署时,同一 market 的挂单 / 撮合可能落到不同实例,
// 订单簿(Redis)与权威库(MySQL)会被并发改 → 可能超卖。MarketLocker 用 Redis 单写者 token
// (pkg/redislock,TTL ≤ 30s,不变量 §10)保证任一时刻同一 market 全局只有一个实例在撮合。
//
// 推荐再叠一致性哈希路由(同一 market 固定落同一实例,见 docs/design/infra.md)把锁竞争降到最低;
// 即便路由抖动 / rebalance,本锁仍兜底跨实例互斥。nil = 单实例,仅靠进程内 striped lock。
type MarketLocker interface {
	// Lock 阻塞式获取 market 的跨实例写锁,返回释放函数。竞争超时返回 ErrAuctionMarketBusy。
	Lock(ctx context.Context, marketID uint32) (release func(), err error)
}

// AuctionUsecase 是 auction 服务业务逻辑核心。
type AuctionUsecase struct {
	repo   data.AuctionRepo
	book   data.BookStore
	ledger SettlementLedger
	events AuctionEventPusher // 弱依赖,可为 nil
	sf     snowflakeGen
	cfg    conf.AuctionConf

	marketLocker MarketLocker // 跨实例单写者锁(nil = 仅进程内串行)

	// marketRouter 是「市场 → 实例归属」一致性哈希路由(nil = 单实例,本实例拥有全部 market)。
	// 多实例部署时由 main 经 SetMarketRouter 注入:同一 market 固定落 owner 实例,把跨实例锁竞争
	// 降到最低。非 owner 实例处理某 market 仅作观测告警(路由抖动 / rebalance 信号),
	// 正确性仍由 marketLocker 兜底,不阻断业务(转发属基础设施,见 market_router.go 头注释)。
	marketRouter *MarketRouter

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

// SetMarketLocker 注入跨实例单写者锁(main.go 配 Redis 后调用)。nil 保持单实例进程内串行。
func (u *AuctionUsecase) SetMarketLocker(ml MarketLocker) { u.marketLocker = ml }

// SetMarketRouter 注入「市场 → 实例归属」一致性哈希路由(main.go 多实例部署时调用)。
// nil 保持单实例(本实例拥有全部 market)。
func (u *AuctionUsecase) SetMarketRouter(r *MarketRouter) { u.marketRouter = r }

// guardMarket 获取 market 的单写者保护:先进程内 striped lock(总是),
// 再(若配置)叠加跨实例 Redis 单写者锁。返回的释放函数按相反顺序解锁。
// 跨实例锁竞争超时返回 ErrAuctionMarketBusy(进程内锁已回退)。
func (u *AuctionUsecase) guardMarket(ctx context.Context, marketID uint32) (func(), error) {
	// 路由观测:多实例部署时,非 owner 实例处理某 market 说明路由抖动 / rebalance,
	// 仅告警(marketLocker 仍兜底正确性),不阻断 —— 转发由边缘 / 服务发现处理(基础设施)。
	if u.marketRouter != nil && !u.marketRouter.OwnsMarket(marketID) {
		plog.With(ctx).Warnw("msg", "auction_market_not_owned",
			"market_id", marketID, "self", u.marketRouter.Self(), "owner", u.marketRouter.Owner(marketID))
	}

	m := u.lockMarket(marketID)
	m.Lock()
	if u.marketLocker == nil {
		return func() { m.Unlock() }, nil
	}
	release, err := u.marketLocker.Lock(ctx, marketID)
	if err != nil {
		m.Unlock()
		return nil, err
	}
	return func() {
		release()
		m.Unlock()
	}, nil
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
	// 防止成交总额(quantity * price)溢出 int64:下游 inventory 结算会算 total = quantity * unitPrice,
	// 即便单值都在上界内,极端组合仍可能溢出 → 在入口拒绝。
	if quantity > math.MaxInt64/price {
		return nil, errcode.New(errcode.ErrInvalidArg, "total value overflow: quantity %d * price %d", quantity, price)
	}
	if idemKey == "" {
		return nil, errcode.New(errcode.ErrInvalidArg, "idempotency_key required")
	}

	release, err := u.guardMarket(ctx, marketID)
	if err != nil {
		return nil, err
	}
	defer release()

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

	// 冻结挂单资产(escrow):卖单冻道具 / 买单冻金币。失败(余额不足)→ 挂单作废,不进簿、不撮合。
	// 这样保证后续成交从 escrow 消费必然充足,消除「成交瞬间余额不足而失败」。
	if ferr := u.ledger.Freeze(ctx, ownerID, rec.OrderID, side, itemConfigID, quantity, price); ferr != nil {
		rec.Status = data.StatusCanceled
		rec.UpdatedAtMs = nowMs()
		if uerr := u.repo.UpdateOrder(ctx, rec); uerr != nil {
			plog.With(ctx).Warnw("msg", "auction_cancel_after_freeze_fail_persist_failed", "order_id", rec.OrderID, "err", uerr)
		}
		return nil, ferr
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
	} else {
		// 完全成交:先持久化终态,再退 escrow 残余(买单价差返还;卖单残余 0,no-op)。
		// 顺序不可倒:若先释放后 UpdateOrder 失败,库里订单停在非终态而 escrow 已没,
		// 后续撤单/过期清扫会再对它操作;反之 UpdateOrder 成功后释放失败只是残余
		// 暂冻(幂等键=orderID,过期清扫/人工补退可恢复),严格更安全。
		rec.Status = data.StatusFilled
	}
	rec.UpdatedAtMs = nowMs()
	if err := u.repo.UpdateOrder(ctx, rec); err != nil {
		return nil, err
	}
	if rec.Status == data.StatusFilled {
		u.releaseEscrow(ctx, rec.OwnerID, rec.OrderID)
	}
	u.pushAudit(ctx, toProtoOrder(rec))
	return toProtoOrder(rec), nil
}

// match 让 incoming 与对手盘逐笔撮合。调用方已持 market 锁(单写者)。
func (u *AuctionUsecase) match(ctx context.Context, incoming *data.OrderRecord) error {
	opp := opposite(incoming.Side)

	// 自撮合跳过:遇到自己挂在对手盘上的单,临时移出簿(避免被 Best 反复选中导致死循环),
	// 本次撮合结束后原样放回。调用方已持 market 单写者锁,移出 / 放回期间无并发访问。
	// inventory 结算侧也拒 seller==buyer,这里在撮合层提前跳过,避免自成交浪费一次结算往返。
	type deferredSelf struct {
		orderID uint64
		price   int64
	}
	var selfOrders []deferredSelf
	defer func() {
		for _, d := range selfOrders {
			if rerr := u.book.Add(ctx, incoming.MarketID, opp, d.orderID, d.price); rerr != nil {
				plog.With(ctx).Warnw("msg", "auction_self_order_restore_failed",
					"market_id", incoming.MarketID, "order_id", d.orderID, "err", rerr)
			}
		}
	}()

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

		// 自撮合跳过:同一 owner 的对手单不与自己成交。临时移出簿,撮合结束后由上面的 defer 放回。
		if resting.OwnerID == incoming.OwnerID {
			if rerr := u.book.Remove(ctx, incoming.MarketID, opp, bestID); rerr != nil {
				return rerr
			}
			selfOrders = append(selfOrders, deferredSelf{orderID: bestID, price: resting.Price})
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
		// 被动单完全成交:终态已持久化,再退 escrow 残余(买单价差返还;卖单 no-op)。
		// 顺序同 Submit:先 UpdateOrder 再释放,避免释放后持久化失败留下
		// 「库里非终态但 escrow 已清」的不一致窗口。
		if resting.Status == data.StatusFilled {
			u.releaseEscrow(ctx, resting.OwnerID, resting.OrderID)
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
	release, err := u.guardMarket(ctx, marketID)
	if err != nil {
		return err
	}
	defer release()

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
	// 退还 escrow 残余(未成交道具 / 金币)到玩家活跃余额。
	u.releaseEscrow(ctx, o.OwnerID, o.OrderID)
	u.pushAudit(ctx, toProtoOrder(o))
	return nil
}

// ExpireDueOrders 清扫一批已过期(创建超过 OrderTTL)仍未成交的挂单:置 EXPIRED、移出订单簿、
// 退还 escrow(限制#1 补偿:挂单冻结的资产不会因长期挂单而永久锁死)。返回本批处理条数。
// OrderTTLSeconds <= 0 时直接返回 0(不过期)。每单都按 market 单写者锁串行处理,与撮合 / 撤单互斥。
func (u *AuctionUsecase) ExpireDueOrders(ctx context.Context) (int, error) {
	if u.cfg.OrderTTLSeconds <= 0 {
		return 0, nil
	}
	cutoff := nowMs() - u.cfg.OrderTTLSeconds*1000
	batch := u.cfg.ExpirySweepBatch
	if batch <= 0 {
		batch = 200
	}
	dueOrders, err := u.repo.ListExpirableOrders(ctx, cutoff, batch)
	if err != nil {
		return 0, err
	}
	var done int
	for _, o := range dueOrders {
		if err := u.expireOne(ctx, o.MarketID, o.OrderID); err != nil {
			// 单条失败不阻断整批:记日志继续(下轮重扫)。
			plog.With(ctx).Warnw("msg", "auction_expire_one_failed",
				"market_id", o.MarketID, "order_id", o.OrderID, "err", err)
			continue
		}
		done++
	}
	return done, nil
}

// expireOne 在持有 market 单写者锁下让单个挂单过期(置 EXPIRED + 移出簿 + 退还 escrow)。
func (u *AuctionUsecase) expireOne(ctx context.Context, marketID uint32, orderID uint64) error {
	release, err := u.guardMarket(ctx, marketID)
	if err != nil {
		return err
	}
	defer release()

	// 持锁后重读:可能已被撮合 / 撤单到终态,避免误改。
	o, found, err := u.repo.GetOrder(ctx, marketID, orderID)
	if err != nil {
		return err
	}
	if !found || isTerminal(o.Status) {
		return nil
	}
	if rerr := u.book.Remove(ctx, marketID, o.Side, orderID); rerr != nil {
		return rerr
	}
	o.Status = data.StatusExpired
	o.UpdatedAtMs = nowMs()
	if uerr := u.repo.UpdateOrder(ctx, o); uerr != nil {
		return uerr
	}
	// 退还 escrow 残余(未成交道具 / 金币)。
	u.releaseEscrow(ctx, o.OwnerID, o.OrderID)
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

// releaseEscrow 退还某挂单 escrow 残余(撤单 / 过期 / 完全成交后)。
// 幂等(键 = orderID),失败不回滚主流程:资产仍冻结在 inventory,可由过期清扫或人工补退。
// 记 Error 级日志以便告警,因为这意味着玩家资产暂时被锁。
func (u *AuctionUsecase) releaseEscrow(ctx context.Context, playerID, orderID uint64) {
	if err := u.ledger.Release(ctx, playerID, orderID); err != nil {
		plog.With(ctx).Errorw("msg", "auction_release_escrow_failed",
			"player_id", playerID, "order_id", orderID, "err", err)
	}
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func nowMs() int64 { return time.Now().UnixMilli() }
