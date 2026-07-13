// Package data 是 auction 服务的数据层(MySQL 撮合权威 + Redis 兼容缓存,2026-07-13)。
//
// MySQL 同时保存订单、成交意图和待补偿副作用。任何会消耗或释放 escrow 的外部调用，
// 都必须先把意图持久化，再以 match_id / order_id 调用幂等账本；进程崩溃后由后台重试。
package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/mysqlx"
)

// Side 是挂单方向(对齐 proto OrderSide:SELL=1 BUY=2)。
type Side int8

const (
	SideSell Side = 1
	SideBuy  Side = 2
)

// 挂单状态。StatusPending 是仅服务端可见的冻结前状态，不映射为客户端业务状态。
const (
	StatusPending  int32 = 0
	StatusOpen     int32 = 1
	StatusPartial  int32 = 2
	StatusFilled   int32 = 3
	StatusCanceled int32 = 4
	StatusExpired  int32 = 5
)

// 成交结算状态。DDL 默认必须是 COMPLETED，使未指定该列的旧二进制保持原语义。
const (
	SettlementPending   int32 = 0
	SettlementCompleted int32 = 1
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
	// ReleasePending 表示终态订单仍需用 order_id 幂等释放 escrow。
	ReleasePending bool
	// MatchPending 表示该订单曾作为 incoming 部分成交，仍需主动把已交叉对手盘扫尽。
	MatchPending bool
	// EscrowVerified 只有在 inventory 确认剩余托管充足后才允许成为撮合双方。
	EscrowVerified bool
	// ReconcileNextAttemptAtMs 是 PENDING/legacy escrow/撮合续跑的公平退避时间。
	ReconcileNextAttemptAtMs int64
	// ReleaseNextAttemptAtMs 把永久失败的释放记录暂时移出就绪批次，避免饿死后续订单。
	ReleaseNextAttemptAtMs int64
	IdempotencyKey         string
	CreatedAtMs            int64
	UpdatedAtMs            int64
}

// Remaining 返回未成交剩余量。
func (r *OrderRecord) Remaining() int64 { return r.Quantity - r.FilledQuantity }

// MatchRecord 是 auction_matches 一行的存储视图(成交事实 + 结算意图)。
type MatchRecord struct {
	MatchID          uint64
	MarketID         uint32
	SellOrderID      uint64
	BuyOrderID       uint64
	SellerID         uint64
	BuyerID          uint64
	ItemConfigID     uint32
	Quantity         int64
	Price            int64
	MatchedAtMs      int64
	SettlementStatus int32
	// SettlementNextAttemptAtMs 把永久失败的结算记录暂时移出就绪批次，避免饿死后续成交。
	SettlementNextAttemptAtMs int64
	// EventPending 表示 COMPLETED 成交仍需按 match_id 至少一次投递 Kafka。
	EventPending         bool
	EventNextAttemptAtMs int64
}

// AuctionRepo 是 auction 撮合权威库抽象。biz 只依赖此接口，不依赖 *sql.DB。
type AuctionRepo interface {
	// ClaimOrder 以 PENDING 幂等插入订单。命中 uk(owner+idem)时返回已有快照。
	ClaimOrder(ctx context.Context, rec *OrderRecord) (existing *OrderRecord, already bool, err error)
	GetOrder(ctx context.Context, marketID uint32, orderID uint64) (*OrderRecord, bool, error)
	// ActivateOrder 只允许 PENDING -> OPEN；Freeze 成功后调用。
	ActivateOrder(ctx context.Context, marketID uint32, orderID uint64, updatedAtMs int64) (bool, error)
	// ConfirmOrderEscrow 幂等确认 inventory 验证结果；已确认且仍可继续的订单也返回 true。
	// 撮合前必须成功。
	ConfirmOrderEscrow(ctx context.Context, marketID uint32, orderID uint64, updatedAtMs int64) (bool, error)
	// RejectPendingOrder 只允许 PENDING -> CANCELED，并登记 release_pending 以覆盖 Freeze 结果不确定窗口。
	RejectPendingOrder(ctx context.Context, marketID uint32, orderID uint64, updatedAtMs int64) (bool, error)
	// ListPendingOrders 扫描进程可能在 Claim/Freeze/Activate 窗口遗留的内部 PENDING 订单。
	ListPendingOrders(ctx context.Context, limit int) ([]*OrderRecord, error)
	// ListMatchPendingOrders 扫描 Reserve 提交后进程退出留下的主动撮合续跑标记。
	ListMatchPendingOrders(ctx context.Context, limit int) ([]*OrderRecord, error)
	// ListUnverifiedActiveOrders 扫描旧二进制/历史 OPEN、PARTIAL，验证后才准入新 matcher。
	ListUnverifiedActiveOrders(ctx context.Context, limit int) ([]*OrderRecord, error)
	DeferOrderReconcile(ctx context.Context, marketID uint32, orderID uint64, nextAttemptAtMs int64) (bool, error)
	// ClearMatchPending 仅在一次主动撮合完整扫到无交叉候选后清除标记。
	ClearMatchPending(ctx context.Context, marketID uint32, orderID uint64) (bool, error)

	// FindBestActiveOrder 从 MySQL 权威库选择精确物品、非自己的价格-时间优先对手单。
	FindBestActiveOrder(ctx context.Context, marketID, itemConfigID uint32, side Side, excludeOwnerID uint64) (*OrderRecord, bool, error)
	// ReserveMatch 在单个事务中锁定两张订单、复验全部成交不变量、写入 PENDING 成交意图，
	// 并原子推进双方 filled/status。候选已失效时 reserved=false，不产生任何写入。
	ReserveMatch(ctx context.Context, marketID uint32, incomingOrderID, restingOrderID, matchID uint64, matchedAtMs int64) (
		match *MatchRecord, incoming, resting *OrderRecord, reserved bool, err error)
	// CompleteMatch 在账本 Settle(match_id) 成功后把成交意图改为 COMPLETED。
	CompleteMatch(ctx context.Context, marketID uint32, matchID uint64) (bool, error)
	DeferMatchSettlement(ctx context.Context, marketID uint32, matchID uint64, nextAttemptAtMs int64) (bool, error)
	ListPendingMatches(ctx context.Context, limit int) ([]*MatchRecord, error)
	// ListPendingMatchEvents 只返回结算已完成，且关联终态订单没有 release_pending 的事件。
	// 终态 escrow 必须先完成释放；兼容旧 default=1，活跃/部分订单即使 marker=1 也不阻塞事件。
	ListPendingMatchEvents(ctx context.Context, limit int) ([]*MatchRecord, error)
	DeferMatchEvent(ctx context.Context, marketID uint32, matchID uint64, nextAttemptAtMs int64) (bool, error)
	ClearMatchEventPending(ctx context.Context, marketID uint32, matchID uint64) (bool, error)

	// MarkOrderTerminal 原子把活跃订单改为 CANCELED/EXPIRED，并同时置 release_pending=1。
	MarkOrderTerminal(ctx context.Context, marketID uint32, orderID uint64, status int32, updatedAtMs int64) (bool, error)
	// 只有不存在引用该订单的 PENDING 成交时，订单才可释放 escrow。
	GetReleasableOrder(ctx context.Context, marketID uint32, orderID uint64) (*OrderRecord, bool, error)
	ListReleasableOrders(ctx context.Context, limit int) ([]*OrderRecord, error)
	DeferOrderRelease(ctx context.Context, marketID uint32, orderID uint64, nextAttemptAtMs int64) (bool, error)
	ClearReleasePending(ctx context.Context, marketID uint32, orderID uint64) (bool, error)
	ListTerminalOrdersForRepair(ctx context.Context, limit int) ([]*OrderRecord, error)
	RepairTerminalMarkers(ctx context.Context, marketID uint32, orderID uint64) (bool, error)

	ListMarketOrders(ctx context.Context, marketID uint32, side Side, limit int) ([]*OrderRecord, error)
	// ListOwnerOrders 按全局 order_id DESC 做游标分页；cursorOrderID=0 表示首页。
	// limit 是最终全局上限，各分片最多读取 limit 条后再归并截断。
	ListOwnerOrders(ctx context.Context, ownerID uint64, activeOnly bool, cursorOrderID uint64, limit int) ([]*OrderRecord, error)
	// ListOwnerActiveAndPending 按最老 order_id 优先返回内部占用配额的订单，limit 必须有界。
	ListOwnerActiveAndPending(ctx context.Context, ownerID uint64, limit int) ([]*OrderRecord, error)
	ListExpirableOrders(ctx context.Context, createdBeforeMs int64, limit int) ([]*OrderRecord, error)
}

// DBRouter 按 market_id 选库。
type DBRouter interface {
	ForMarket(marketID uint32) *sql.DB
	ForOwner(ownerID uint64) *sql.DB
	All() []*sql.DB
}

type SingleDB struct{ DB *sql.DB }

func (s SingleDB) ForMarket(uint32) *sql.DB { return s.DB }
func (s SingleDB) ForOwner(uint64) *sql.DB  { return s.DB }
func (s SingleDB) All() []*sql.DB           { return []*sql.DB{s.DB} }

type ShardedDB struct{ Set *mysqlx.ShardSet }

func (s ShardedDB) ForMarket(marketID uint32) *sql.DB { return s.Set.For(uint64(marketID)) }
func (s ShardedDB) ForOwner(ownerID uint64) *sql.DB   { return s.Set.For(ownerID) }
func (s ShardedDB) All() []*sql.DB                    { return s.Set.All() }

type MySQLAuctionRepo struct{ r DBRouter }

func NewMySQLAuctionRepo(r DBRouter) *MySQLAuctionRepo { return &MySQLAuctionRepo{r: r} }

const orderCols = `order_id, market_id, owner_id, side, item_config_id, quantity, filled_quantity, price, status, release_pending, match_pending, escrow_verified, reconcile_next_attempt_at_ms, release_next_attempt_at_ms, idempotency_key, created_at_ms, updated_at_ms`
const orderColsO = `o.order_id, o.market_id, o.owner_id, o.side, o.item_config_id, o.quantity, o.filled_quantity, o.price, o.status, o.release_pending, o.match_pending, o.escrow_verified, o.reconcile_next_attempt_at_ms, o.release_next_attempt_at_ms, o.idempotency_key, o.created_at_ms, o.updated_at_ms`
const matchCols = `match_id, market_id, sell_order_id, buy_order_id, seller_id, buyer_id, item_config_id, quantity, price, matched_at_ms, settlement_status, settlement_next_attempt_at_ms, event_pending, event_next_attempt_at_ms`
const matchColsM = `m.match_id, m.market_id, m.sell_order_id, m.buy_order_id, m.seller_id, m.buyer_id, m.item_config_id, m.quantity, m.price, m.matched_at_ms, m.settlement_status, m.settlement_next_attempt_at_ms, m.event_pending, m.event_next_attempt_at_ms`

func scanOrder(row interface{ Scan(...any) error }) (*OrderRecord, error) {
	var o OrderRecord
	var side, releasePending, matchPending, escrowVerified int8
	if err := row.Scan(&o.OrderID, &o.MarketID, &o.OwnerID, &side, &o.ItemConfigID,
		&o.Quantity, &o.FilledQuantity, &o.Price, &o.Status, &releasePending, &matchPending,
		&escrowVerified, &o.ReconcileNextAttemptAtMs, &o.ReleaseNextAttemptAtMs,
		&o.IdempotencyKey, &o.CreatedAtMs, &o.UpdatedAtMs); err != nil {
		return nil, err
	}
	o.Side = Side(side)
	o.ReleasePending = releasePending != 0
	o.MatchPending = matchPending != 0
	o.EscrowVerified = escrowVerified != 0
	return &o, nil
}

func scanMatch(row interface{ Scan(...any) error }) (*MatchRecord, error) {
	var m MatchRecord
	var eventPending int8
	if err := row.Scan(&m.MatchID, &m.MarketID, &m.SellOrderID, &m.BuyOrderID,
		&m.SellerID, &m.BuyerID, &m.ItemConfigID, &m.Quantity, &m.Price,
		&m.MatchedAtMs, &m.SettlementStatus, &m.SettlementNextAttemptAtMs,
		&eventPending, &m.EventNextAttemptAtMs); err != nil {
		return nil, err
	}
	m.EventPending = eventPending != 0
	return &m, nil
}

func (r *MySQLAuctionRepo) ClaimOrder(ctx context.Context, rec *OrderRecord) (*OrderRecord, bool, error) {
	// owner coordinator 与 market 分片可能不同。绝不能持 coordinator 事务连接再查询/写入
	// 其他 shard：双向请求可各自占满连接池并形成 database/sql 看不见的连接池环。
	// 因此协议分三段：事务外扫描 legacy → coordinator 单库事务登记不可变 canonical →
	// 事务外幂等补回 market PENDING。只有三段全部成功 biz 才 Freeze；任一点退出都可重试收敛。
	coordinator := r.r.ForOwner(rec.OwnerID)
	if _, err := coordinator.ExecContext(ctx,
		`INSERT IGNORE INTO auction_owner_guards (owner_id) VALUES (?)`, rec.OwnerID); err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "create owner guard %d: %v", rec.OwnerID, err)
	}

	// 已登记的 registry 是 immutable 快路径，不需要持 guard 锁做跨分片 I/O。
	canonical, found, err := readIdempotencyClaim(ctx, coordinator, rec.OwnerID, rec.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	if found {
		if !sameOrderFingerprint(canonical, rec) {
			return nil, false, idempotencyConflict(rec.OwnerID, rec.IdempotencyKey)
		}
		existing, err := r.ensureCanonicalOrder(ctx, canonical)
		return existing, true, err
	}

	// registry 首次 miss 才广播兼容历史订单。扫描时不持任何 DB 事务/连接；发布门禁要求
	// green 接写前旧实例已停止写入，因此扫描结果在随后 guard 事务内重查 registry 后可安全采用。
	legacy, err := r.findOrdersByOwnerIdempotency(ctx, rec.OwnerID, rec.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	if len(legacy) > 1 {
		return nil, false, errcode.New(errcode.ErrAuctionIdempotencyConflict,
			"multiple legacy orders for owner=%d key=%s require reconciliation", rec.OwnerID, rec.IdempotencyKey)
	}

	tx, err := coordinator.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "begin owner idempotency claim: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var lockedOwner uint64
	if err := tx.QueryRowContext(ctx,
		`SELECT owner_id FROM auction_owner_guards WHERE owner_id = ? FOR UPDATE`, rec.OwnerID).Scan(&lockedOwner); err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "lock owner guard %d: %v", rec.OwnerID, err)
	}

	// 扫描期间可能已有另一个新实例登记 canonical；锁内必须重读，以库内映射为准。
	canonical, found, err = readIdempotencyClaim(ctx, tx, rec.OwnerID, rec.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	already := found
	if found {
		if !sameOrderFingerprint(canonical, rec) {
			return nil, false, idempotencyConflict(rec.OwnerID, rec.IdempotencyKey)
		}
	} else if len(legacy) == 1 {
		canonical = legacy[0]
		if !sameOrderFingerprint(canonical, rec) {
			return nil, false, idempotencyConflict(rec.OwnerID, rec.IdempotencyKey)
		}
		if err := insertIdempotencyClaim(ctx, tx, canonical); err != nil {
			return nil, false, err
		}
		already = true
	} else {
		canonical = rec
		if err := insertIdempotencyClaim(ctx, tx, canonical); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "commit owner claim: %v", err)
	}

	existing, err := r.ensureCanonicalOrder(ctx, canonical)
	return existing, already, err
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readIdempotencyClaim(
	ctx context.Context, q queryRower, ownerID uint64, key string,
) (*OrderRecord, bool, error) {
	// 必须读回库内 canonical key 字面值。默认 MySQL collation 会把大小写/尾空格视为
	// 等价；若沿用本次请求的字面值修复缺失 market 行，registry 与订单会永久漂移。
	const query = `SELECT idempotency_key,order_id,market_id,side,item_config_id,quantity,price,created_at_ms
		FROM auction_idempotency_keys WHERE owner_id = ? AND idempotency_key = ? LIMIT 1`
	o := &OrderRecord{OwnerID: ownerID, IdempotencyKey: key, Status: StatusPending}
	var side int8
	err := q.QueryRowContext(ctx, query, ownerID, key).Scan(
		&o.IdempotencyKey, &o.OrderID, &o.MarketID, &side, &o.ItemConfigID, &o.Quantity, &o.Price, &o.CreatedAtMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "read owner idempotency claim: %v", err)
	}
	o.Side = Side(side)
	o.UpdatedAtMs = o.CreatedAtMs
	return o, true, nil
}

func insertIdempotencyClaim(ctx context.Context, tx *sql.Tx, o *OrderRecord) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO auction_idempotency_keys
		(owner_id,idempotency_key,order_id,market_id,side,item_config_id,quantity,price,created_at_ms)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		o.OwnerID, o.IdempotencyKey, o.OrderID, o.MarketID, int8(o.Side), o.ItemConfigID,
		o.Quantity, o.Price, o.CreatedAtMs)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "insert owner idempotency claim: %v", err)
	}
	return nil
}

func (r *MySQLAuctionRepo) insertOrderForClaim(ctx context.Context, rec *OrderRecord) error {
	const ins = `INSERT INTO auction_orders (` + orderCols + `) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	_, err := r.r.ForMarket(rec.MarketID).ExecContext(ctx, ins,
		rec.OrderID, rec.MarketID, rec.OwnerID, int8(rec.Side), rec.ItemConfigID,
		rec.Quantity, rec.FilledQuantity, rec.Price, rec.Status, rec.ReleasePending, rec.MatchPending,
		rec.EscrowVerified, rec.ReconcileNextAttemptAtMs, rec.ReleaseNextAttemptAtMs,
		rec.IdempotencyKey, rec.CreatedAtMs, rec.UpdatedAtMs)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "insert order owner=%d key=%s: %v", rec.OwnerID, rec.IdempotencyKey, err)
	}
	return nil
}

func (r *MySQLAuctionRepo) getOrderForClaim(ctx context.Context, marketID uint32, orderID uint64) (*OrderRecord, bool, error) {
	o, err := scanOrder(r.r.ForMarket(marketID).QueryRowContext(ctx,
		`SELECT `+orderCols+` FROM auction_orders WHERE order_id = ? LIMIT 1`, orderID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "read canonical order %d: %v", orderID, err)
	}
	return o, true, nil
}

// ensureCanonicalOrder 在 coordinator commit 后幂等补回 market PENDING，并严格校验两侧 immutable 字段。
// INSERT 成功但响应丢失、或其他实例抢先补回，都会通过 1062 后权威回读收敛。
func (r *MySQLAuctionRepo) ensureCanonicalOrder(ctx context.Context, canonical *OrderRecord) (*OrderRecord, error) {
	insertErr := r.insertOrderForClaim(ctx, canonical)
	if insertErr != nil && !isDupErr(insertErr) {
		return nil, insertErr
	}
	existing, found, err := r.getOrderForClaim(ctx, canonical.MarketID, canonical.OrderID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errcode.New(errcode.ErrInternal,
			"canonical market row missing after ensure owner=%d key=%s order=%d",
			canonical.OwnerID, canonical.IdempotencyKey, canonical.OrderID)
	}
	if !sameCanonicalOrder(existing, canonical) {
		return nil, errcode.New(errcode.ErrInternal,
			"canonical order drift owner=%d key=%s order=%d",
			canonical.OwnerID, canonical.IdempotencyKey, canonical.OrderID)
	}
	return existing, nil
}

func (r *MySQLAuctionRepo) findOrdersByOwnerIdempotency(
	ctx context.Context, ownerID uint64, key string,
) ([]*OrderRecord, error) {
	const query = `SELECT ` + orderCols + ` FROM auction_orders
		WHERE owner_id = ? AND idempotency_key = ? ORDER BY order_id ASC LIMIT 2`
	var out []*OrderRecord
	for _, db := range r.r.All() {
		rows, err := db.QueryContext(ctx, query, ownerID, key)
		if err != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan legacy owner idempotency: %v", err)
		}
		part, scanErr := collectOrders(rows)
		_ = rows.Close()
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, part...)
		if len(out) > 1 {
			break
		}
	}
	return out, nil
}

func sameOrderFingerprint(a, b *OrderRecord) bool {
	return a != nil && b != nil && a.OwnerID == b.OwnerID && a.MarketID == b.MarketID &&
		a.Side == b.Side && a.ItemConfigID == b.ItemConfigID && a.Quantity == b.Quantity && a.Price == b.Price
}

func sameCanonicalOrder(a, b *OrderRecord) bool {
	return a != nil && b != nil && a.OrderID == b.OrderID &&
		a.IdempotencyKey == b.IdempotencyKey && a.CreatedAtMs == b.CreatedAtMs &&
		sameOrderFingerprint(a, b)
}

func idempotencyConflict(ownerID uint64, key string) error {
	return errcode.New(errcode.ErrAuctionIdempotencyConflict,
		"idempotency_key reused for different request owner=%d key=%s", ownerID, key)
}

func (r *MySQLAuctionRepo) GetOrder(ctx context.Context, marketID uint32, orderID uint64) (*OrderRecord, bool, error) {
	const q = `SELECT ` + orderCols + ` FROM auction_orders WHERE order_id = ? LIMIT 1`
	o, err := scanOrder(r.r.ForMarket(marketID).QueryRowContext(ctx, q, orderID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "get order %d: %v", orderID, err)
	}
	return o, true, nil
}

func (r *MySQLAuctionRepo) ActivateOrder(ctx context.Context, marketID uint32, orderID uint64, updatedAtMs int64) (bool, error) {
	return execChanged(ctx, r.r.ForMarket(marketID),
		`UPDATE auction_orders SET status = ?, reconcile_next_attempt_at_ms = 0, updated_at_ms = ?
		 WHERE order_id = ? AND status = ? AND escrow_verified = 1`,
		"activate order", StatusOpen, updatedAtMs, orderID, StatusPending)
}

func (r *MySQLAuctionRepo) ConfirmOrderEscrow(ctx context.Context, marketID uint32, orderID uint64, updatedAtMs int64) (bool, error) {
	db := r.r.ForMarket(marketID)
	result, err := db.ExecContext(ctx,
		`UPDATE auction_orders SET escrow_verified = 1,
		 match_pending = IF(status IN (?, ?), 1, match_pending),
		 reconcile_next_attempt_at_ms = 0, updated_at_ms = ?
		 WHERE order_id = ? AND status IN (?, ?, ?) AND filled_quantity < quantity`,
		StatusOpen, StatusPartial, updatedAtMs, orderID, StatusPending, StatusOpen, StatusPartial)
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "confirm order escrow: %v", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "confirm order escrow rows affected: %v", err)
	}
	if changed > 0 {
		return true, nil
	}
	// 同一毫秒重放时所有赋值都可能与现值相同，MySQL RowsAffected=0；回读确认，避免
	// Freeze 已成功且 marker 已落库后进程退出，重试却永久卡在 PENDING。
	o, found, err := r.GetOrder(ctx, marketID, orderID)
	if err != nil || !found {
		return false, err
	}
	return o.EscrowVerified && o.Remaining() > 0 &&
		(o.Status == StatusPending || o.Status == StatusOpen || o.Status == StatusPartial), nil
}

func (r *MySQLAuctionRepo) RejectPendingOrder(ctx context.Context, marketID uint32, orderID uint64, updatedAtMs int64) (bool, error) {
	return execChanged(ctx, r.r.ForMarket(marketID),
		`UPDATE auction_orders SET status = ?, release_pending = 1, match_pending = 0, escrow_verified = 0,
		 reconcile_next_attempt_at_ms = 0, release_next_attempt_at_ms = 0, updated_at_ms = ?
		 WHERE order_id = ? AND status = ?`,
		"reject pending order", StatusCanceled, updatedAtMs, orderID, StatusPending)
}

func (r *MySQLAuctionRepo) ListPendingOrders(ctx context.Context, limit int) ([]*OrderRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []*OrderRecord
	readyAtMs := time.Now().UnixMilli()
	for _, db := range r.r.All() {
		// 每分片各取一批，不能让首个繁忙分片饿死后续分片。
		q := `SELECT ` + orderCols + ` FROM auction_orders
			WHERE status = ? AND reconcile_next_attempt_at_ms <= ?
			ORDER BY reconcile_next_attempt_at_ms ASC, order_id ASC LIMIT ?`
		rows, err := db.QueryContext(ctx, q, StatusPending, readyAtMs, limit)
		if err != nil {
			return nil, errcode.New(errcode.ErrInternal, "list pending orders: %v", err)
		}
		part, cerr := collectOrders(rows)
		_ = rows.Close()
		if cerr != nil {
			return nil, cerr
		}
		out = append(out, part...)
	}
	return out, nil
}

func (r *MySQLAuctionRepo) ListMatchPendingOrders(ctx context.Context, limit int) ([]*OrderRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []*OrderRecord
	readyAtMs := time.Now().UnixMilli()
	for _, db := range r.r.All() {
		q := `SELECT ` + orderCols + ` FROM auction_orders
			WHERE match_pending = 1 AND escrow_verified = 1 AND status IN (?, ?)
			  AND reconcile_next_attempt_at_ms <= ?
			ORDER BY reconcile_next_attempt_at_ms ASC, order_id ASC LIMIT ?`
		rows, err := db.QueryContext(ctx, q, StatusOpen, StatusPartial, readyAtMs, limit)
		if err != nil {
			return nil, errcode.New(errcode.ErrInternal, "list match pending orders: %v", err)
		}
		part, cerr := collectOrders(rows)
		_ = rows.Close()
		if cerr != nil {
			return nil, cerr
		}
		out = append(out, part...)
	}
	return out, nil
}

func (r *MySQLAuctionRepo) ListUnverifiedActiveOrders(ctx context.Context, limit int) ([]*OrderRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	readyAtMs := time.Now().UnixMilli()
	var out []*OrderRecord
	for _, db := range r.r.All() {
		q := `SELECT ` + orderCols + ` FROM auction_orders
			WHERE escrow_verified = 0 AND status IN (?, ?) AND filled_quantity < quantity
			  AND reconcile_next_attempt_at_ms <= ?
			ORDER BY reconcile_next_attempt_at_ms ASC, order_id ASC LIMIT ?`
		rows, err := db.QueryContext(ctx, q, StatusOpen, StatusPartial, readyAtMs, limit)
		if err != nil {
			return nil, errcode.New(errcode.ErrInternal, "list unverified active orders: %v", err)
		}
		part, scanErr := collectOrders(rows)
		_ = rows.Close()
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, part...)
	}
	return out, nil
}

func (r *MySQLAuctionRepo) DeferOrderReconcile(
	ctx context.Context, marketID uint32, orderID uint64, nextAttemptAtMs int64,
) (bool, error) {
	if nextAttemptAtMs <= 0 {
		return false, errcode.New(errcode.ErrInternal, "invalid order reconcile retry time %d", nextAttemptAtMs)
	}
	return execChanged(ctx, r.r.ForMarket(marketID),
		`UPDATE auction_orders SET reconcile_next_attempt_at_ms = ? WHERE order_id = ? AND
		 (status = ? OR (status IN (?, ?) AND (match_pending = 1 OR escrow_verified = 0)))`,
		"defer order reconcile", nextAttemptAtMs, orderID, StatusPending, StatusOpen, StatusPartial)
}

func (r *MySQLAuctionRepo) ClearMatchPending(ctx context.Context, marketID uint32, orderID uint64) (bool, error) {
	return execChanged(ctx, r.r.ForMarket(marketID),
		`UPDATE auction_orders SET match_pending = 0, reconcile_next_attempt_at_ms = 0
		 WHERE order_id = ? AND match_pending = 1 AND status IN (?, ?)`,
		"clear match pending", orderID, StatusOpen, StatusPartial)
}

func (r *MySQLAuctionRepo) FindBestActiveOrder(
	ctx context.Context, marketID, itemConfigID uint32, side Side, excludeOwnerID uint64,
) (*OrderRecord, bool, error) {
	order := "price ASC, order_id ASC"
	if side == SideBuy {
		order = "price DESC, order_id ASC"
	}
	q := `SELECT ` + orderCols + ` FROM auction_orders
		WHERE market_id = ? AND item_config_id = ? AND side = ?
		  AND escrow_verified = 1 AND status IN (?, ?) AND owner_id <> ? AND filled_quantity < quantity
		ORDER BY ` + order + ` LIMIT 1`
	o, err := scanOrder(r.r.ForMarket(marketID).QueryRowContext(ctx, q,
		marketID, itemConfigID, int8(side), StatusOpen, StatusPartial, excludeOwnerID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errcode.New(errcode.ErrInternal,
			"find best active order market=%d item=%d side=%d: %v", marketID, itemConfigID, side, err)
	}
	return o, true, nil
}

func (r *MySQLAuctionRepo) ReserveMatch(
	ctx context.Context, marketID uint32, incomingOrderID, restingOrderID, matchID uint64, matchedAtMs int64,
) (*MatchRecord, *OrderRecord, *OrderRecord, bool, error) {
	if incomingOrderID == 0 || restingOrderID == 0 || incomingOrderID == restingOrderID {
		return nil, nil, nil, false, errcode.New(errcode.ErrInternal, "invalid reserve order ids")
	}
	db := r.r.ForMarket(marketID)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, nil, false, errcode.New(errcode.ErrInternal, "begin reserve match: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	first, second := incomingOrderID, restingOrderID
	if first > second {
		first, second = second, first
	}
	q := `SELECT ` + orderCols + ` FROM auction_orders WHERE order_id IN (?, ?) ORDER BY order_id ASC FOR UPDATE`
	rows, err := tx.QueryContext(ctx, q, first, second)
	if err != nil {
		return nil, nil, nil, false, errcode.New(errcode.ErrInternal, "lock reserve orders: %v", err)
	}
	locked, err := collectOrders(rows)
	_ = rows.Close()
	if err != nil {
		return nil, nil, nil, false, err
	}
	if len(locked) != 2 {
		return nil, nil, nil, false, nil
	}
	var incoming, resting *OrderRecord
	for _, o := range locked {
		switch o.OrderID {
		case incomingOrderID:
			incoming = o
		case restingOrderID:
			resting = o
		}
	}
	if !validReservation(marketID, incoming, resting) {
		return nil, incoming, resting, false, nil
	}

	qty := incoming.Remaining()
	if resting.Remaining() < qty {
		qty = resting.Remaining()
	}
	m := buildReservedMatch(matchID, incoming, resting, qty, resting.Price, matchedAtMs)
	const ins = `INSERT INTO auction_matches (` + matchCols + `) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	if _, err := tx.ExecContext(ctx, ins,
		m.MatchID, m.MarketID, m.SellOrderID, m.BuyOrderID, m.SellerID, m.BuyerID,
		m.ItemConfigID, m.Quantity, m.Price, m.MatchedAtMs, SettlementPending, m.SettlementNextAttemptAtMs,
		m.EventPending, m.EventNextAttemptAtMs); err != nil {
		return nil, nil, nil, false, errcode.New(errcode.ErrInternal, "insert pending match %d: %v", matchID, err)
	}

	incoming.FilledQuantity += qty
	resting.FilledQuantity += qty
	advanceReservedOrder(incoming, matchedAtMs)
	advanceReservedOrder(resting, matchedAtMs)
	// incoming 部分成交后必须持久续跑；否则进程在本事务提交后退出会留下已交叉 PARTIAL 单。
	incoming.MatchPending = incoming.Remaining() > 0
	if resting.Remaining() == 0 {
		resting.MatchPending = false
	}
	if err := updateReservedOrder(ctx, tx, incoming); err != nil {
		return nil, nil, nil, false, err
	}
	if err := updateReservedOrder(ctx, tx, resting); err != nil {
		return nil, nil, nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, nil, false, errcode.New(errcode.ErrInternal, "commit reserve match %d: %v", matchID, err)
	}
	return m, incoming, resting, true, nil
}

func validReservation(marketID uint32, incoming, resting *OrderRecord) bool {
	if incoming == nil || resting == nil || incoming.MarketID != marketID || resting.MarketID != marketID ||
		incoming.ItemConfigID != resting.ItemConfigID || incoming.Side == resting.Side ||
		incoming.OwnerID == resting.OwnerID || !incoming.EscrowVerified || !resting.EscrowVerified ||
		!isIncomingStatus(incoming.Status) || !isActiveStatus(resting.Status) ||
		incoming.Remaining() <= 0 || resting.Remaining() <= 0 {
		return false
	}
	if incoming.Side == SideSell {
		return resting.Price >= incoming.Price
	}
	return resting.Price <= incoming.Price
}

func buildReservedMatch(matchID uint64, incoming, resting *OrderRecord, qty, price, matchedAtMs int64) *MatchRecord {
	m := &MatchRecord{
		MatchID: matchID, MarketID: incoming.MarketID, ItemConfigID: incoming.ItemConfigID,
		Quantity: qty, Price: price, MatchedAtMs: matchedAtMs, SettlementStatus: SettlementPending,
	}
	if incoming.Side == SideSell {
		m.SellOrderID, m.SellerID = incoming.OrderID, incoming.OwnerID
		m.BuyOrderID, m.BuyerID = resting.OrderID, resting.OwnerID
	} else {
		m.BuyOrderID, m.BuyerID = incoming.OrderID, incoming.OwnerID
		m.SellOrderID, m.SellerID = resting.OrderID, resting.OwnerID
	}
	return m
}

func advanceReservedOrder(o *OrderRecord, updatedAtMs int64) {
	o.UpdatedAtMs = updatedAtMs
	if o.Remaining() == 0 {
		o.Status = StatusFilled
		o.ReleasePending = true
		o.EscrowVerified = false
		o.ReconcileNextAttemptAtMs = 0
	} else {
		o.Status = StatusPartial
	}
}

func updateReservedOrder(ctx context.Context, tx *sql.Tx, o *OrderRecord) error {
	const q = `UPDATE auction_orders SET filled_quantity = ?, status = ?, release_pending = ?, match_pending = ?,
		escrow_verified = ?, reconcile_next_attempt_at_ms = ?, release_next_attempt_at_ms = ?, updated_at_ms = ?
		WHERE order_id = ?`
	if _, err := tx.ExecContext(ctx, q, o.FilledQuantity, o.Status, o.ReleasePending, o.MatchPending,
		o.EscrowVerified, o.ReconcileNextAttemptAtMs, o.ReleaseNextAttemptAtMs, o.UpdatedAtMs, o.OrderID); err != nil {
		return errcode.New(errcode.ErrInternal, "update reserved order %d: %v", o.OrderID, err)
	}
	return nil
}

func (r *MySQLAuctionRepo) CompleteMatch(ctx context.Context, marketID uint32, matchID uint64) (bool, error) {
	return execChanged(ctx, r.r.ForMarket(marketID),
		`UPDATE auction_matches SET settlement_status = ?, settlement_next_attempt_at_ms = 0,
		 event_pending = 1, event_next_attempt_at_ms = 0
		 WHERE match_id = ? AND settlement_status = ?`,
		"complete match", SettlementCompleted, matchID, SettlementPending)
}

// DeferMatchSettlement 把一次失败的外部结算移出当前就绪批次。条件更新保证并发完成后
// 不会把已 COMPLETED 的成交重新排队。
func (r *MySQLAuctionRepo) DeferMatchSettlement(
	ctx context.Context, marketID uint32, matchID uint64, nextAttemptAtMs int64,
) (bool, error) {
	if nextAttemptAtMs <= 0 {
		return false, errcode.New(errcode.ErrInternal, "invalid match retry time %d", nextAttemptAtMs)
	}
	return execChanged(ctx, r.r.ForMarket(marketID),
		`UPDATE auction_matches SET settlement_next_attempt_at_ms = ? WHERE match_id = ? AND settlement_status = ?`,
		"defer match settlement", nextAttemptAtMs, matchID, SettlementPending)
}

func (r *MySQLAuctionRepo) ListPendingMatches(ctx context.Context, limit int) ([]*MatchRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []*MatchRecord
	readyAtMs := time.Now().UnixMilli()
	for _, db := range r.r.All() {
		// limit 是每分片上限；若用全局 remaining，首个繁忙分片会永久饿死后续分片。
		// next_attempt 让永久失败记录退避，避免固定的最老批次饿死同分片后续成交。
		q := `SELECT ` + matchCols + ` FROM auction_matches
			WHERE settlement_status = ? AND settlement_next_attempt_at_ms <= ?
			ORDER BY settlement_next_attempt_at_ms ASC, matched_at_ms ASC, match_id ASC LIMIT ?`
		rows, err := db.QueryContext(ctx, q, SettlementPending, readyAtMs, limit)
		if err != nil {
			return nil, errcode.New(errcode.ErrInternal, "list pending matches: %v", err)
		}
		part, cerr := collectMatches(rows)
		_ = rows.Close()
		if cerr != nil {
			return nil, cerr
		}
		out = append(out, part...)
	}
	return out, nil
}

func (r *MySQLAuctionRepo) ListPendingMatchEvents(ctx context.Context, limit int) ([]*MatchRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	readyAtMs := time.Now().UnixMilli()
	var out []*MatchRecord
	for _, db := range r.r.All() {
		q := `SELECT ` + matchColsM + ` FROM auction_matches m
			WHERE m.event_pending = 1 AND m.settlement_status = ? AND m.event_next_attempt_at_ms <= ?
			  AND NOT EXISTS (
				SELECT 1 FROM auction_orders o
				WHERE o.order_id = m.sell_order_id AND o.market_id = m.market_id
				  AND o.release_pending = 1 AND o.status IN (?, ?, ?)
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM auction_orders o
				WHERE o.order_id = m.buy_order_id AND o.market_id = m.market_id
				  AND o.release_pending = 1 AND o.status IN (?, ?, ?)
			  )
			ORDER BY m.event_next_attempt_at_ms ASC, m.matched_at_ms ASC, m.match_id ASC LIMIT ?`
		rows, err := db.QueryContext(ctx, q,
			SettlementCompleted, readyAtMs,
			StatusFilled, StatusCanceled, StatusExpired,
			StatusFilled, StatusCanceled, StatusExpired,
			limit)
		if err != nil {
			return nil, errcode.New(errcode.ErrInternal, "list pending match events: %v", err)
		}
		part, scanErr := collectMatches(rows)
		_ = rows.Close()
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, part...)
	}
	return out, nil
}

func (r *MySQLAuctionRepo) DeferMatchEvent(
	ctx context.Context, marketID uint32, matchID uint64, nextAttemptAtMs int64,
) (bool, error) {
	if nextAttemptAtMs <= 0 {
		return false, errcode.New(errcode.ErrInternal, "invalid match event retry time %d", nextAttemptAtMs)
	}
	return execChanged(ctx, r.r.ForMarket(marketID),
		`UPDATE auction_matches SET event_next_attempt_at_ms = ?
		 WHERE match_id = ? AND event_pending = 1 AND settlement_status = ?`,
		"defer match event", nextAttemptAtMs, matchID, SettlementCompleted)
}

func (r *MySQLAuctionRepo) ClearMatchEventPending(ctx context.Context, marketID uint32, matchID uint64) (bool, error) {
	return execChanged(ctx, r.r.ForMarket(marketID),
		`UPDATE auction_matches SET event_pending = 0, event_next_attempt_at_ms = 0
		 WHERE match_id = ? AND event_pending = 1 AND settlement_status = ?`,
		"clear match event pending", matchID, SettlementCompleted)
}

func (r *MySQLAuctionRepo) MarkOrderTerminal(ctx context.Context, marketID uint32, orderID uint64, status int32, updatedAtMs int64) (bool, error) {
	if status != StatusCanceled && status != StatusExpired {
		return false, errcode.New(errcode.ErrInternal, "invalid terminal status %d", status)
	}
	return execChanged(ctx, r.r.ForMarket(marketID),
		`UPDATE auction_orders SET status = ?, release_pending = 1, match_pending = 0, escrow_verified = 0,
		 reconcile_next_attempt_at_ms = 0, release_next_attempt_at_ms = 0, updated_at_ms = ?
		 WHERE order_id = ? AND status IN (?, ?)`,
		"mark order terminal", status, updatedAtMs, orderID, StatusOpen, StatusPartial)
}

func (r *MySQLAuctionRepo) GetReleasableOrder(ctx context.Context, marketID uint32, orderID uint64) (*OrderRecord, bool, error) {
	q := `SELECT ` + orderColsO + ` FROM auction_orders o
		WHERE o.order_id = ? AND o.release_pending = 1 AND o.status IN (?, ?, ?)
		  AND NOT EXISTS (SELECT 1 FROM auction_matches m WHERE m.settlement_status = ?
		    AND (m.sell_order_id = o.order_id OR m.buy_order_id = o.order_id)) LIMIT 1`
	o, err := scanOrder(r.r.ForMarket(marketID).QueryRowContext(ctx, q, orderID,
		StatusFilled, StatusCanceled, StatusExpired, SettlementPending))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "get releasable order %d: %v", orderID, err)
	}
	return o, true, nil
}

func (r *MySQLAuctionRepo) ListReleasableOrders(ctx context.Context, limit int) ([]*OrderRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []*OrderRecord
	readyAtMs := time.Now().UnixMilli()
	for _, db := range r.r.All() {
		// 每分片各取一批，避免首个分片持续积压时饿死其余分片。
		// next_attempt 让永久失败记录退避，避免固定的最老批次饿死同分片后续订单。
		q := `SELECT ` + orderColsO + ` FROM auction_orders o
			WHERE o.release_pending = 1 AND o.status IN (?, ?, ?)
			  AND o.release_next_attempt_at_ms <= ?
			  AND NOT EXISTS (SELECT 1 FROM auction_matches m WHERE m.settlement_status = ?
			    AND (m.sell_order_id = o.order_id OR m.buy_order_id = o.order_id))
			ORDER BY o.release_next_attempt_at_ms ASC, o.order_id ASC LIMIT ?`
		rows, err := db.QueryContext(ctx, q,
			StatusFilled, StatusCanceled, StatusExpired, readyAtMs, SettlementPending, limit)
		if err != nil {
			return nil, errcode.New(errcode.ErrInternal, "list releasable orders: %v", err)
		}
		part, cerr := collectOrders(rows)
		_ = rows.Close()
		if cerr != nil {
			return nil, cerr
		}
		out = append(out, part...)
	}
	return out, nil
}

// DeferOrderRelease 把一次失败的外部释放移出当前就绪批次。条件更新保证并发完成后
// 不会把已清除 marker 的订单重新排队。
func (r *MySQLAuctionRepo) DeferOrderRelease(
	ctx context.Context, marketID uint32, orderID uint64, nextAttemptAtMs int64,
) (bool, error) {
	if nextAttemptAtMs <= 0 {
		return false, errcode.New(errcode.ErrInternal, "invalid order retry time %d", nextAttemptAtMs)
	}
	return execChanged(ctx, r.r.ForMarket(marketID),
		`UPDATE auction_orders SET release_next_attempt_at_ms = ? WHERE order_id = ? AND release_pending = 1`,
		"defer order release", nextAttemptAtMs, orderID)
}

func (r *MySQLAuctionRepo) ClearReleasePending(ctx context.Context, marketID uint32, orderID uint64) (bool, error) {
	q := `UPDATE auction_orders SET release_pending = 0, release_next_attempt_at_ms = 0
		WHERE auction_orders.order_id = ? AND auction_orders.release_pending = 1 AND auction_orders.status IN (?, ?, ?)
		  AND NOT EXISTS (SELECT 1 FROM auction_matches m WHERE m.settlement_status = ?
		    AND (m.sell_order_id = auction_orders.order_id OR m.buy_order_id = auction_orders.order_id))`
	return execChanged(ctx, r.r.ForMarket(marketID), q, "clear release pending",
		orderID, StatusFilled, StatusCanceled, StatusExpired, SettlementPending)
}

func (r *MySQLAuctionRepo) ListTerminalOrdersForRepair(ctx context.Context, limit int) ([]*OrderRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []*OrderRecord
	for _, db := range r.r.All() {
		q := `SELECT ` + orderCols + ` FROM auction_orders
			WHERE status IN (?, ?, ?) AND (escrow_verified = 1 OR match_pending = 1)
			ORDER BY order_id ASC LIMIT ?`
		rows, err := db.QueryContext(ctx, q, StatusFilled, StatusCanceled, StatusExpired, limit)
		if err != nil {
			return nil, errcode.New(errcode.ErrInternal, "list terminal marker repairs: %v", err)
		}
		part, scanErr := collectOrders(rows)
		_ = rows.Close()
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, part...)
	}
	return out, nil
}

func (r *MySQLAuctionRepo) RepairTerminalMarkers(ctx context.Context, marketID uint32, orderID uint64) (bool, error) {
	return execChanged(ctx, r.r.ForMarket(marketID),
		`UPDATE auction_orders SET release_pending = 1, match_pending = 0, escrow_verified = 0,
		 reconcile_next_attempt_at_ms = 0, release_next_attempt_at_ms = 0
		 WHERE order_id = ? AND status IN (?, ?, ?)
		 AND (escrow_verified = 1 OR match_pending = 1)`,
		"repair terminal markers", orderID, StatusFilled, StatusCanceled, StatusExpired)
}

func (r *MySQLAuctionRepo) ListMarketOrders(ctx context.Context, marketID uint32, side Side, limit int) ([]*OrderRecord, error) {
	order := "price ASC, order_id ASC"
	if side == SideBuy {
		order = "price DESC, order_id ASC"
	}
	q := `SELECT ` + orderCols + ` FROM auction_orders
		WHERE market_id = ? AND side = ? AND escrow_verified = 1 AND status IN (?, ?)
		ORDER BY ` + order + ` LIMIT ?`
	rows, err := r.r.ForMarket(marketID).QueryContext(ctx, q, marketID, int8(side), StatusOpen, StatusPartial, limit)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "list market=%d side=%d: %v", marketID, side, err)
	}
	defer func() { _ = rows.Close() }()
	return collectOrders(rows)
}

func (r *MySQLAuctionRepo) ListOwnerOrders(
	ctx context.Context, ownerID uint64, activeOnly bool, cursorOrderID uint64, limit int,
) ([]*OrderRecord, error) {
	if limit <= 0 {
		limit = 51
	}
	// StatusPending 是内部恢复态，不得以 proto UNSPECIFIED 暴露给客户端列表。
	q := `SELECT ` + orderCols + ` FROM auction_orders WHERE owner_id = ? AND status <> ` + itoa(StatusPending)
	args := []any{ownerID}
	if activeOnly {
		q += ` AND status IN (` + itoa(StatusOpen) + `, ` + itoa(StatusPartial) + `)`
	}
	if cursorOrderID > 0 {
		q += ` AND order_id < ?`
		args = append(args, cursorOrderID)
	}
	q += ` ORDER BY order_id DESC LIMIT ?`
	args = append(args, limit)
	var out []*OrderRecord
	for _, db := range r.r.All() {
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, errcode.New(errcode.ErrInternal, "list owner=%d: %v", ownerID, err)
		}
		part, cerr := collectOrders(rows)
		_ = rows.Close()
		if cerr != nil {
			return nil, cerr
		}
		out = append(out, part...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OrderID > out[j].OrderID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *MySQLAuctionRepo) ListOwnerActiveAndPending(
	ctx context.Context, ownerID uint64, limit int,
) ([]*OrderRecord, error) {
	if limit <= 0 {
		limit = 201
	}
	q := `SELECT ` + orderCols + ` FROM auction_orders
		WHERE owner_id = ? AND status IN (?, ?, ?) ORDER BY order_id ASC LIMIT ?`
	var out []*OrderRecord
	for _, db := range r.r.All() {
		rows, err := db.QueryContext(ctx, q, ownerID, StatusPending, StatusOpen, StatusPartial, limit)
		if err != nil {
			return nil, errcode.New(errcode.ErrInternal, "list owner active slots %d: %v", ownerID, err)
		}
		part, scanErr := collectOrders(rows)
		_ = rows.Close()
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, part...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OrderID < out[j].OrderID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *MySQLAuctionRepo) ListExpirableOrders(ctx context.Context, createdBeforeMs int64, limit int) ([]*OrderRecord, error) {
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT ` + orderCols + ` FROM auction_orders WHERE status IN (` +
		itoa(StatusOpen) + `, ` + itoa(StatusPartial) + `) AND created_at_ms < ? ORDER BY created_at_ms ASC LIMIT ?`
	var out []*OrderRecord
	for _, db := range r.r.All() {
		// 每分片各取一批，避免首个分片的老订单让后续分片永远得不到清扫。
		rows, err := db.QueryContext(ctx, q, createdBeforeMs, limit)
		if err != nil {
			return nil, errcode.New(errcode.ErrInternal, "list expirable before=%d: %v", createdBeforeMs, err)
		}
		part, cerr := collectOrders(rows)
		_ = rows.Close()
		if cerr != nil {
			return nil, cerr
		}
		out = append(out, part...)
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
	if err := rows.Err(); err != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate orders: %v", err)
	}
	return out, nil
}

func collectMatches(rows *sql.Rows) ([]*MatchRecord, error) {
	var out []*MatchRecord
	for rows.Next() {
		m, err := scanMatch(rows)
		if err != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan match: %v", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate matches: %v", err)
	}
	return out, nil
}

func execChanged(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, q, action string, args ...any) (bool, error) {
	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "%s: %v", action, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "%s rows affected: %v", action, err)
	}
	return n > 0, nil
}

func isActiveStatus(status int32) bool { return status == StatusOpen || status == StatusPartial }

func isIncomingStatus(status int32) bool { return status == StatusPending || isActiveStatus(status) }

func isDupErr(err error) bool { return err != nil && strings.Contains(err.Error(), "Error 1062") }

func itoa(v int32) string { return fmt.Sprintf("%d", v) }
