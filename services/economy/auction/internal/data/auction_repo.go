// Package data 是 auction 服务的数据层(MySQL 撮合权威 + Redis 订单簿,2026-06-19)。
//
// 库表(deploy/mysql-init/09-auction-tables.sql,pandora_auction 库,按 market_id 分片):
//
//	auction_orders   挂单 / 出价(uk owner_id+idempotency_key 防重复挂单,不变量 §9.7)
//	auction_matches  成交流水(PK match_id 防重复结算,不变量 §9.2 / §9.7)
//
// 撮合是「每个 market 单写者」的交易所模型,不跨分片事务;所以 MySQL 分库即可,不需要 TiDB。
// OrderRecord / MatchRecord 是存储层内部结构(含 idempotency_key 等存储独有字段,CLAUDE.md §5.10),
// 与客户端可见的 proto AuctionOrder 分离(不变量 §14)。
package data

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/mysqlx"
)

// Side 是挂单方向(对齐 proto OrderSide:SELL=1 BUY=2)。
type Side int8

const (
	SideSell Side = 1
	SideBuy  Side = 2
)

// 挂单状态(对齐 proto AuctionOrderStatus 数值)。
const (
	StatusOpen     int32 = 1
	StatusPartial  int32 = 2
	StatusFilled   int32 = 3
	StatusCanceled int32 = 4
	StatusExpired  int32 = 5
)

// OrderRecord 是 auction_orders 一行的存储视图。
type OrderRecord struct {
	OrderID        uint64
	MarketID       uint32
	OwnerID        uint64
	Side           Side
	ItemConfigID   uint32
	Quantity       int64
	FilledQuantity int64
	Price          int64
	Status         int32
	IdempotencyKey string
	CreatedAtMs    int64
	UpdatedAtMs    int64
}

// Remaining 返回未成交剩余量。
func (r *OrderRecord) Remaining() int64 { return r.Quantity - r.FilledQuantity }

// MatchRecord 是 auction_matches 一行的存储视图(成交事实)。
type MatchRecord struct {
	MatchID      uint64
	MarketID     uint32
	SellOrderID  uint64
	BuyOrderID   uint64
	SellerID     uint64
	BuyerID      uint64
	ItemConfigID uint32
	Quantity     int64
	Price        int64
	MatchedAtMs  int64
}

// AuctionRepo 是 auction 撮合权威库抽象。biz 只依赖此接口,不依赖 *sql.DB。
type AuctionRepo interface {
	// ClaimOrder 幂等插入挂单。命中 uk(owner+idem):
	//   - 同一请求(market/side/item/quantity/price 一致)→ already=true,返回已存订单(回放);
	//   - 不同请求 → ErrAuctionIdempotencyConflict。
	ClaimOrder(ctx context.Context, rec *OrderRecord) (existing *OrderRecord, already bool, err error)
	// GetOrder 按 market_id(分片路由)+ order_id 读单行。
	GetOrder(ctx context.Context, marketID uint32, orderID uint64) (*OrderRecord, bool, error)
	// UpdateOrder 更新成交量 / 状态(撮合 / 撤单后持久化)。
	UpdateOrder(ctx context.Context, rec *OrderRecord) error
	// RecordMatch 幂等插入成交。命中 PK match_id → already=true(崩溃重放幂等)。
	RecordMatch(ctx context.Context, m *MatchRecord) (already bool, err error)
	// ListMarketOrders 列某市场某方向挂在簿上(OPEN / PARTIAL)的订单。
	ListMarketOrders(ctx context.Context, marketID uint32, side Side, limit int) ([]*OrderRecord, error)
	// ListOwnerOrders 列某玩家的挂单 / 出价(跨 market;分库模式广播扫各分片)。
	ListOwnerOrders(ctx context.Context, ownerID uint64, activeOnly bool) ([]*OrderRecord, error)
}

// DBRouter 按 market_id 选库:单库模式恒返回同一库,分库模式按 market_id % N 路由。
type DBRouter interface {
	ForMarket(marketID uint32) *sql.DB
	All() []*sql.DB
}

// SingleDB 是单库路由(W1 默认)。
type SingleDB struct{ DB *sql.DB }

// ForMarket 单库恒返回同一库。
func (s SingleDB) ForMarket(uint32) *sql.DB { return s.DB }

// All 单库只有一个库。
func (s SingleDB) All() []*sql.DB { return []*sql.DB{s.DB} }

// ShardedDB 是分库路由(mysqlx.ShardSet 按 market_id 路由)。
type ShardedDB struct{ Set *mysqlx.ShardSet }

// ForMarket 按 market_id % N 选分库。
func (s ShardedDB) ForMarket(marketID uint32) *sql.DB { return s.Set.For(uint64(marketID)) }

// All 返回全部分库(ListOwnerOrders 广播用)。
func (s ShardedDB) All() []*sql.DB { return s.Set.All() }

// MySQLAuctionRepo 是基于 database/sql 的 AuctionRepo 实现。
type MySQLAuctionRepo struct {
	r DBRouter
}

// NewMySQLAuctionRepo 构造。
func NewMySQLAuctionRepo(r DBRouter) *MySQLAuctionRepo { return &MySQLAuctionRepo{r: r} }

const orderCols = `order_id, market_id, owner_id, side, item_config_id, quantity, filled_quantity, price, status, idempotency_key, created_at_ms, updated_at_ms`

func scanOrder(row interface{ Scan(...any) error }) (*OrderRecord, error) {
	var o OrderRecord
	var side int8
	if err := row.Scan(&o.OrderID, &o.MarketID, &o.OwnerID, &side, &o.ItemConfigID,
		&o.Quantity, &o.FilledQuantity, &o.Price, &o.Status, &o.IdempotencyKey,
		&o.CreatedAtMs, &o.UpdatedAtMs); err != nil {
		return nil, err
	}
	o.Side = Side(side)
	return &o, nil
}

func (r *MySQLAuctionRepo) ClaimOrder(ctx context.Context, rec *OrderRecord) (*OrderRecord, bool, error) {
	db := r.r.ForMarket(rec.MarketID)
	const ins = `INSERT INTO auction_orders (` + orderCols + `) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`
	_, err := db.ExecContext(ctx, ins,
		rec.OrderID, rec.MarketID, rec.OwnerID, int8(rec.Side), rec.ItemConfigID,
		rec.Quantity, rec.FilledQuantity, rec.Price, rec.Status, rec.IdempotencyKey,
		rec.CreatedAtMs, rec.UpdatedAtMs)
	if err == nil {
		return rec, false, nil
	}
	if !isDupErr(err) {
		return nil, false, errcode.New(errcode.ErrInternal, "insert order owner=%d key=%s: %v", rec.OwnerID, rec.IdempotencyKey, err)
	}
	// 幂等命中:按 owner+idem 读回已存挂单。
	const q = `SELECT ` + orderCols + ` FROM auction_orders WHERE owner_id = ? AND idempotency_key = ? LIMIT 1`
	existing, serr := scanOrder(db.QueryRowContext(ctx, q, rec.OwnerID, rec.IdempotencyKey))
	if serr != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "read claimed order owner=%d key=%s: %v", rec.OwnerID, rec.IdempotencyKey, serr)
	}
	// 指纹比对:同 key 不同请求 → 冲突(防 idempotency_key 复用)。
	if existing.MarketID != rec.MarketID || existing.Side != rec.Side ||
		existing.ItemConfigID != rec.ItemConfigID || existing.Quantity != rec.Quantity ||
		existing.Price != rec.Price {
		return nil, false, errcode.New(errcode.ErrAuctionIdempotencyConflict,
			"idempotency_key reused for different request owner=%d key=%s", rec.OwnerID, rec.IdempotencyKey)
	}
	return existing, true, nil
}

func (r *MySQLAuctionRepo) GetOrder(ctx context.Context, marketID uint32, orderID uint64) (*OrderRecord, bool, error) {
	db := r.r.ForMarket(marketID)
	const q = `SELECT ` + orderCols + ` FROM auction_orders WHERE order_id = ? LIMIT 1`
	o, err := scanOrder(db.QueryRowContext(ctx, q, orderID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "get order %d: %v", orderID, err)
	}
	return o, true, nil
}

func (r *MySQLAuctionRepo) UpdateOrder(ctx context.Context, rec *OrderRecord) error {
	db := r.r.ForMarket(rec.MarketID)
	const upd = `UPDATE auction_orders SET filled_quantity = ?, status = ?, updated_at_ms = ? WHERE order_id = ?`
	if _, err := db.ExecContext(ctx, upd, rec.FilledQuantity, rec.Status, rec.UpdatedAtMs, rec.OrderID); err != nil {
		return errcode.New(errcode.ErrInternal, "update order %d: %v", rec.OrderID, err)
	}
	return nil
}

func (r *MySQLAuctionRepo) RecordMatch(ctx context.Context, m *MatchRecord) (bool, error) {
	db := r.r.ForMarket(m.MarketID)
	const ins = `INSERT INTO auction_matches (match_id, market_id, sell_order_id, buy_order_id, seller_id, buyer_id, item_config_id, quantity, price, matched_at_ms) VALUES (?,?,?,?,?,?,?,?,?,?)`
	_, err := db.ExecContext(ctx, ins,
		m.MatchID, m.MarketID, m.SellOrderID, m.BuyOrderID, m.SellerID, m.BuyerID,
		m.ItemConfigID, m.Quantity, m.Price, m.MatchedAtMs)
	if err == nil {
		return false, nil
	}
	if isDupErr(err) {
		return true, nil // 同一 match_id 已落库,结算幂等(不变量 §9.2)
	}
	return false, errcode.New(errcode.ErrInternal, "record match %d: %v", m.MatchID, err)
}

func (r *MySQLAuctionRepo) ListMarketOrders(ctx context.Context, marketID uint32, side Side, limit int) ([]*OrderRecord, error) {
	db := r.r.ForMarket(marketID)
	// 卖盘按价升序(最低价在前),买盘按价降序(最高价在前);同价按 order_id 升序(时间优先)。
	order := "price ASC, order_id ASC"
	if side == SideBuy {
		order = "price DESC, order_id ASC"
	}
	q := `SELECT ` + orderCols + ` FROM auction_orders WHERE market_id = ? AND side = ? AND status IN (?, ?) ORDER BY ` + order + ` LIMIT ?`
	rows, err := db.QueryContext(ctx, q, marketID, int8(side), StatusOpen, StatusPartial, limit)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "list market=%d side=%d: %v", marketID, side, err)
	}
	defer func() { _ = rows.Close() }()
	return collectOrders(rows)
}

func (r *MySQLAuctionRepo) ListOwnerOrders(ctx context.Context, ownerID uint64, activeOnly bool) ([]*OrderRecord, error) {
	// 玩家订单跨 market 分布(挂单可在不同品类),分库模式需广播扫各分片(低频「我的订单」可接受)。
	q := `SELECT ` + orderCols + ` FROM auction_orders WHERE owner_id = ?`
	if activeOnly {
		q += ` AND status IN (` + itoa(StatusOpen) + `, ` + itoa(StatusPartial) + `)`
	}
	q += ` ORDER BY order_id DESC`

	var out []*OrderRecord
	for _, db := range r.r.All() {
		rows, err := db.QueryContext(ctx, q, ownerID)
		if err != nil {
			return nil, errcode.New(errcode.ErrInternal, "list owner=%d: %v", ownerID, err)
		}
		shardRows, cerr := collectOrders(rows)
		_ = rows.Close()
		if cerr != nil {
			return nil, cerr
		}
		out = append(out, shardRows...)
	}
	return out, nil
}

func collectOrders(rows *sql.Rows) ([]*OrderRecord, error) {
	var out []*OrderRecord
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan order: %v", err)
		}
		out = append(out, o)
	}
	return out, nil
}

// isDupErr 判断是否 MySQL 1062 唯一键冲突(对齐 inventory_repo)。
func isDupErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1062")
}

// itoa 把状态常量拼进静态 SQL(仅内部小整数常量,无注入风险)。
func itoa(v int32) string {
	switch v {
	case StatusOpen:
		return "1"
	case StatusPartial:
		return "2"
	case StatusFilled:
		return "3"
	case StatusCanceled:
		return "4"
	case StatusExpired:
		return "5"
	default:
		return "0"
	}
}
