// Package data 是 inventory 服务的数据层(MySQL 货币 / 道具 / 幂等流水)。
//
// 库表(deploy/mysql-init/08-inventory-tables.sql,pandora_trade 库):
//
//	player_currency   玩家货币余额(PK player_id)
//	player_items      背包道具堆叠(uk player_id+item_config_id)
//	inventory_ledger  发放 / 使用 / 出售幂等流水(uk player_id+idempotency_key)
//
// 反作弊 / 一致性(不变量 §9.7):GrantItems / UseItem / SellItem 全部在一个事务里
// 先 INSERT inventory_ledger(命中 uk → 幂等已处理),再原子改 player_items / player_currency;
// 扣减用 SELECT ... FOR UPDATE 锁行 + 数量校验,避免并发超扣。
//
// player_items / player_currency 是结构化列(CLAUDE.md §5.9 不强制 proto 化),直接映射字段。
package data

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// ItemStack 是背包里某配置道具的持有堆叠。
type ItemStack struct {
	ItemConfigID uint32
	Count        int64
}

// ItemGrant 是一次发放里对某配置道具增加的数量(Count>0)。
type ItemGrant struct {
	ItemConfigID uint32
	Count        int64
}

// InventoryRepo 是 inventory 数据层抽象。biz 只依赖此接口,不依赖 *sql.DB。
type InventoryRepo interface {
	// GetInventory 读玩家货币 + 道具堆叠(按 item_config_id 排序;未建档 → gold=0 空道具)。
	GetInventory(ctx context.Context, playerID uint64) (gold int64, items []ItemStack, err error)

	// GrantItems 幂等发放道具 + 货币(事务:INSERT ledger 命中 uk → 已处理读回 gold;
	// 否则 upsert player_items 累加、player_currency 累加 gold)。返回发放后 gold。
	GrantItems(ctx context.Context, playerID uint64, items []ItemGrant, gold int64, idempotencyKey, detail string) (newGold int64, already bool, err error)

	// UseItem 幂等扣减道具(事务:INSERT ledger;SELECT count FOR UPDATE 校验 >= n;扣减)。
	// 数量不足 → ErrInventoryInsufficient;道具不存在 → ErrInventoryItemNotFound。返回剩余数量。
	UseItem(ctx context.Context, playerID uint64, itemConfigID uint32, count int64, idempotencyKey, detail string) (remaining int64, already bool, err error)

	// SellItem 幂等出售(事务:INSERT ledger;扣道具 + 加 gold)。返回剩余数量 + 出售后 gold。
	SellItem(ctx context.Context, playerID uint64, itemConfigID uint32, count, gold int64, idempotencyKey, detail string) (remaining, newGold int64, already bool, err error)

	// SettleAuctionMatch 原子结算一笔拍卖成交(一个本地事务内卖↔买双方对转):
	//   卖家扣 quantity 个 itemConfigID + 加 totalGold 金币;
	//   买家扣 totalGold 金币 + 加 quantity 个 itemConfigID。
	// idempotencyKey(= 业务层基于 match_id 派生)在事务内给买卖双方各记一条流水,
	// 重复结算命中 uk → already=true(资产只转一次,不变量 §9.2 / §9.7);
	// 卖家道具不足 / 买家金币不足 → ErrInventoryInsufficient,整笔回滚。
	SettleAuctionMatch(ctx context.Context, matchID, sellerID, buyerID uint64, itemConfigID uint32, quantity, totalGold int64, idempotencyKey, detail string) (already bool, err error)
}

// MySQLInventoryRepo 是基于 database/sql 的 InventoryRepo 实现。
type MySQLInventoryRepo struct {
	db *sql.DB
}

// NewMySQLInventoryRepo 构造。db 由 pkg/mysqlx.MustNewClient 提供(连 pandora_trade 库)。
func NewMySQLInventoryRepo(db *sql.DB) *MySQLInventoryRepo {
	return &MySQLInventoryRepo{db: db}
}

func (r *MySQLInventoryRepo) GetInventory(ctx context.Context, playerID uint64) (int64, []ItemStack, error) {
	var gold int64
	gerr := r.db.QueryRowContext(ctx, `SELECT gold FROM player_currency WHERE player_id = ? LIMIT 1`, playerID).Scan(&gold)
	if gerr != nil && !errors.Is(gerr, sql.ErrNoRows) {
		return 0, nil, errcode.New(errcode.ErrInternal, "read gold player=%d: %v", playerID, gerr)
	}

	const q = `SELECT item_config_id, count FROM player_items WHERE player_id = ? AND count > 0 ORDER BY item_config_id`
	rows, err := r.db.QueryContext(ctx, q, playerID)
	if err != nil {
		return 0, nil, errcode.New(errcode.ErrInternal, "query items player=%d: %v", playerID, err)
	}
	defer func() { _ = rows.Close() }()

	var items []ItemStack
	for rows.Next() {
		var it ItemStack
		if serr := rows.Scan(&it.ItemConfigID, &it.Count); serr != nil {
			return 0, nil, errcode.New(errcode.ErrInternal, "scan item player=%d: %v", playerID, serr)
		}
		items = append(items, it)
	}
	if rerr := rows.Err(); rerr != nil {
		return 0, nil, errcode.New(errcode.ErrInternal, "iterate items player=%d: %v", playerID, rerr)
	}
	return gold, items, nil
}

// ── 幂等指纹 ────────────────────────────────────────────────────────────────
//
// 同一 idempotency_key 复用到**不同**请求(op/item/count/gold 不同)会被静默当 no-op
// 是反作弊隐患;指纹把 key 绑定到请求内容:首次执行记录指纹 + 结果快照,
// 重复请求指纹不一致 → ErrInventoryIdempotencyConflict;一致 → 回放首次结果快照。

// GrantFingerprint 计算发放请求指纹(items 按 item_config_id 排序后规范化 + gold)。
func GrantFingerprint(items []ItemGrant, gold int64) string {
	sorted := append([]ItemGrant(nil), items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ItemConfigID < sorted[j].ItemConfigID })
	var b strings.Builder
	b.WriteString("grant")
	for _, it := range sorted {
		b.WriteByte('|')
		b.WriteString(strconv.FormatUint(uint64(it.ItemConfigID), 10))
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(it.Count, 10))
	}
	b.WriteString("|gold=")
	b.WriteString(strconv.FormatInt(gold, 10))
	return hashHex(b.String())
}

// UseFingerprint 计算使用请求指纹。
func UseFingerprint(itemConfigID uint32, count int64) string {
	return hashHex(fmt.Sprintf("use|%d:%d", itemConfigID, count))
}

// SellFingerprint 计算出售请求指纹(含算得的 gold)。
func SellFingerprint(itemConfigID uint32, count, gold int64) string {
	return hashHex(fmt.Sprintf("sell|%d:%d|gold=%d", itemConfigID, count, gold))
}

// AuctionSettleFingerprint 计算拍卖结算请求指纹(双方 + 道具 + 数量 + 总价)。
// 同一 idempotency_key 复用到不同成交内容 → 指纹不一致判冲突,防 key 复用串改账。
func AuctionSettleFingerprint(sellerID, buyerID uint64, itemConfigID uint32, quantity, totalGold int64) string {
	return hashHex(fmt.Sprintf("auction_settle|seller=%d|buyer=%d|item=%d|qty=%d|gold=%d",
		sellerID, buyerID, itemConfigID, quantity, totalGold))
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// claimLedger 在事务里声明幂等键 + 记录请求指纹。
//   - 首次:插入成功 → already=false
//   - 重复(uk 1062):读回已存指纹 + 结果快照;
//     指纹不一致 → ErrInventoryIdempotencyConflict;一致 → already=true + 首次结果快照(回放)
func claimLedger(ctx context.Context, tx *sql.Tx, playerID uint64, idempotencyKey, op, fingerprint, detail string) (already bool, snapRemaining, snapGold int64, err error) {
	const ins = `INSERT INTO inventory_ledger (player_id, idempotency_key, op, request_fingerprint, detail) VALUES (?, ?, ?, ?, ?)`
	if _, lerr := tx.ExecContext(ctx, ins, playerID, idempotencyKey, op, fingerprint, detail); lerr != nil {
		if !isDupErr(lerr) {
			return false, 0, 0, errcode.New(errcode.ErrInternal, "insert ledger player=%d key=%s: %v", playerID, idempotencyKey, lerr)
		}
		// 幂等命中:读回首次请求指纹 + 结果快照比对。
		var storedFP string
		qerr := tx.QueryRowContext(ctx,
			`SELECT request_fingerprint, result_remaining, result_gold FROM inventory_ledger WHERE player_id = ? AND idempotency_key = ? LIMIT 1`,
			playerID, idempotencyKey).Scan(&storedFP, &snapRemaining, &snapGold)
		if qerr != nil {
			return false, 0, 0, errcode.New(errcode.ErrInternal, "read ledger player=%d key=%s: %v", playerID, idempotencyKey, qerr)
		}
		if storedFP != fingerprint {
			return false, 0, 0, errcode.New(errcode.ErrInventoryIdempotencyConflict,
				"idempotency_key reused for different request player=%d key=%s", playerID, idempotencyKey)
		}
		return true, snapRemaining, snapGold, nil
	}
	return false, 0, 0, nil
}

// updateLedgerResult 在事务里把首次执行的结果快照写回流水(供后续幂等回放返回稳定值)。
func updateLedgerResult(ctx context.Context, tx *sql.Tx, playerID uint64, idempotencyKey string, remaining, gold int64) error {
	const upd = `UPDATE inventory_ledger SET result_remaining = ?, result_gold = ? WHERE player_id = ? AND idempotency_key = ?`
	if _, uerr := tx.ExecContext(ctx, upd, remaining, gold, playerID, idempotencyKey); uerr != nil {
		return errcode.New(errcode.ErrInternal, "update ledger result player=%d key=%s: %v", playerID, idempotencyKey, uerr)
	}
	return nil
}

// readGoldTx 在事务里读 gold(无行 → 0)。
func readGoldTx(ctx context.Context, tx *sql.Tx, playerID uint64) (int64, error) {
	var gold int64
	err := tx.QueryRowContext(ctx, `SELECT gold FROM player_currency WHERE player_id = ? LIMIT 1`, playerID).Scan(&gold)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "read gold player=%d: %v", playerID, err)
	}
	return gold, nil
}

func (r *MySQLInventoryRepo) GrantItems(ctx context.Context, playerID uint64, items []ItemGrant, gold int64, idempotencyKey, detail string) (int64, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	already, _, snapGold, lerr := claimLedger(ctx, tx, playerID, idempotencyKey, "grant", GrantFingerprint(items, gold), detail)
	if lerr != nil {
		return 0, false, lerr
	}
	if already {
		return snapGold, true, nil
	}

	const upItem = `INSERT INTO player_items (player_id, item_config_id, count) VALUES (?, ?, ?)
ON DUPLICATE KEY UPDATE count = count + VALUES(count)`
	for _, it := range items {
		if _, ierr := tx.ExecContext(ctx, upItem, playerID, it.ItemConfigID, it.Count); ierr != nil {
			return 0, false, errcode.New(errcode.ErrInternal, "grant item player=%d item=%d: %v", playerID, it.ItemConfigID, ierr)
		}
	}

	const upGold = `INSERT INTO player_currency (player_id, gold) VALUES (?, ?)
ON DUPLICATE KEY UPDATE gold = gold + VALUES(gold)`
	if _, gerr := tx.ExecContext(ctx, upGold, playerID, gold); gerr != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "grant gold player=%d: %v", playerID, gerr)
	}

	newGold, rerr := readGoldTx(ctx, tx, playerID)
	if rerr != nil {
		return 0, false, rerr
	}
	if uerr := updateLedgerResult(ctx, tx, playerID, idempotencyKey, 0, newGold); uerr != nil {
		return 0, false, uerr
	}
	if cerr := tx.Commit(); cerr != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "commit grant player=%d: %v", playerID, cerr)
	}
	return newGold, false, nil
}

// deductItemTx 在事务里锁道具行并扣减 count。
//   - 行不存在 → ErrInventoryItemNotFound
//   - count < n → ErrInventoryInsufficient
//   - 成功 → 返回扣减后剩余数量
func deductItemTx(ctx context.Context, tx *sql.Tx, playerID uint64, itemConfigID uint32, n int64) (int64, error) {
	var have int64
	err := tx.QueryRowContext(ctx,
		`SELECT count FROM player_items WHERE player_id = ? AND item_config_id = ? FOR UPDATE`,
		playerID, itemConfigID).Scan(&have)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errcode.New(errcode.ErrInventoryItemNotFound, "item not found player=%d item=%d", playerID, itemConfigID)
	}
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "lock item player=%d item=%d: %v", playerID, itemConfigID, err)
	}
	if have < n {
		return 0, errcode.New(errcode.ErrInventoryInsufficient, "insufficient item player=%d item=%d need=%d have=%d", playerID, itemConfigID, n, have)
	}
	remaining := have - n
	if _, uerr := tx.ExecContext(ctx,
		`UPDATE player_items SET count = ? WHERE player_id = ? AND item_config_id = ?`,
		remaining, playerID, itemConfigID); uerr != nil {
		return 0, errcode.New(errcode.ErrInternal, "deduct item player=%d item=%d: %v", playerID, itemConfigID, uerr)
	}
	return remaining, nil
}

func (r *MySQLInventoryRepo) UseItem(ctx context.Context, playerID uint64, itemConfigID uint32, count int64, idempotencyKey, detail string) (int64, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	already, snapRemaining, _, lerr := claimLedger(ctx, tx, playerID, idempotencyKey, "use", UseFingerprint(itemConfigID, count), detail)
	if lerr != nil {
		return 0, false, lerr
	}
	if already {
		// 幂等命中:回放首次执行的剩余数量快照(不重新读当前状态,避免随后续操作漂移)。
		return snapRemaining, true, nil
	}

	remaining, derr := deductItemTx(ctx, tx, playerID, itemConfigID, count)
	if derr != nil {
		return 0, false, derr
	}
	if uerr := updateLedgerResult(ctx, tx, playerID, idempotencyKey, remaining, 0); uerr != nil {
		return 0, false, uerr
	}
	if cerr := tx.Commit(); cerr != nil {
		return 0, false, errcode.New(errcode.ErrInternal, "commit use player=%d: %v", playerID, cerr)
	}
	return remaining, false, nil
}

func (r *MySQLInventoryRepo) SellItem(ctx context.Context, playerID uint64, itemConfigID uint32, count, gold int64, idempotencyKey, detail string) (int64, int64, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, false, errcode.New(errcode.ErrInternal, "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	already, snapRemaining, snapGold, lerr := claimLedger(ctx, tx, playerID, idempotencyKey, "sell", SellFingerprint(itemConfigID, count, gold), detail)
	if lerr != nil {
		return 0, 0, false, lerr
	}
	if already {
		// 幂等命中:回放首次执行的剩余数量 + 金币快照。
		return snapRemaining, snapGold, true, nil
	}

	remaining, derr := deductItemTx(ctx, tx, playerID, itemConfigID, count)
	if derr != nil {
		return 0, 0, false, derr
	}

	const upGold = `INSERT INTO player_currency (player_id, gold) VALUES (?, ?)
ON DUPLICATE KEY UPDATE gold = gold + VALUES(gold)`
	if _, gerr := tx.ExecContext(ctx, upGold, playerID, gold); gerr != nil {
		return 0, 0, false, errcode.New(errcode.ErrInternal, "add gold player=%d: %v", playerID, gerr)
	}
	newGold, rerr := readGoldTx(ctx, tx, playerID)
	if rerr != nil {
		return 0, 0, false, rerr
	}
	if uerr := updateLedgerResult(ctx, tx, playerID, idempotencyKey, remaining, newGold); uerr != nil {
		return 0, 0, false, uerr
	}
	if cerr := tx.Commit(); cerr != nil {
		return 0, 0, false, errcode.New(errcode.ErrInternal, "commit sell player=%d: %v", playerID, cerr)
	}
	return remaining, newGold, false, nil
}

// deductGoldTx 在事务里锁货币行并扣减 n。
//   - 行不存在(无货币记录,余额 0)或余额 < n → ErrInventoryInsufficient
//   - 成功 → 返回扣减后余额
func deductGoldTx(ctx context.Context, tx *sql.Tx, playerID uint64, n int64) (int64, error) {
	var have int64
	err := tx.QueryRowContext(ctx,
		`SELECT gold FROM player_currency WHERE player_id = ? FOR UPDATE`, playerID).Scan(&have)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errcode.New(errcode.ErrInventoryInsufficient, "insufficient gold player=%d need=%d have=0", playerID, n)
	}
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "lock gold player=%d: %v", playerID, err)
	}
	if have < n {
		return 0, errcode.New(errcode.ErrInventoryInsufficient, "insufficient gold player=%d need=%d have=%d", playerID, n, have)
	}
	remaining := have - n
	if _, uerr := tx.ExecContext(ctx,
		`UPDATE player_currency SET gold = ? WHERE player_id = ?`, remaining, playerID); uerr != nil {
		return 0, errcode.New(errcode.ErrInternal, "deduct gold player=%d: %v", playerID, uerr)
	}
	return remaining, nil
}

// addGoldTx 在事务里给玩家加金币(upsert,无行则建)。
func addGoldTx(ctx context.Context, tx *sql.Tx, playerID uint64, n int64) error {
	const upGold = `INSERT INTO player_currency (player_id, gold) VALUES (?, ?)
ON DUPLICATE KEY UPDATE gold = gold + VALUES(gold)`
	if _, err := tx.ExecContext(ctx, upGold, playerID, n); err != nil {
		return errcode.New(errcode.ErrInternal, "add gold player=%d: %v", playerID, err)
	}
	return nil
}

// addItemTx 在事务里给玩家加道具(upsert 堆叠,无行则建)。
func addItemTx(ctx context.Context, tx *sql.Tx, playerID uint64, itemConfigID uint32, n int64) error {
	const upItem = `INSERT INTO player_items (player_id, item_config_id, count) VALUES (?, ?, ?)
ON DUPLICATE KEY UPDATE count = count + VALUES(count)`
	if _, err := tx.ExecContext(ctx, upItem, playerID, itemConfigID, n); err != nil {
		return errcode.New(errcode.ErrInternal, "add item player=%d item=%d: %v", playerID, itemConfigID, err)
	}
	return nil
}

// SettleAuctionMatch 在一个本地事务里原子完成拍卖成交的卖↔买双方资产对转。
//
// 防死锁:对共享资源(inventory_ledger / player_items / player_currency)的行锁全部按
//
//	player_id 升序、同一玩家内「先 items 表后 currency 表」的总顺序获取,
//
// 杜绝并发结算(尤其角色对调的两笔)交叉持锁成环。
// 幂等:买卖双方各记一条同 idempotency_key 的流水,任一命中 uk → already=true 回放(不重复扣)。
func (r *MySQLInventoryRepo) SettleAuctionMatch(ctx context.Context, matchID, sellerID, buyerID uint64, itemConfigID uint32, quantity, totalGold int64, idempotencyKey, detail string) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	fp := AuctionSettleFingerprint(sellerID, buyerID, itemConfigID, quantity, totalGold)

	// 1) 幂等流水:按 player_id 升序声明两条(同 key),避免并发交叉插入死锁。
	loID, hiID := sellerID, buyerID
	loOp, hiOp := "auction_sell", "auction_buy"
	if buyerID < sellerID {
		loID, hiID = buyerID, sellerID
		loOp, hiOp = "auction_buy", "auction_sell"
	}
	loAlready, _, _, lerr := claimLedger(ctx, tx, loID, idempotencyKey, loOp, fp, detail)
	if lerr != nil {
		return false, lerr
	}
	hiAlready, _, _, herr := claimLedger(ctx, tx, hiID, idempotencyKey, hiOp, fp, detail)
	if herr != nil {
		return false, herr
	}
	if loAlready || hiAlready {
		// 已结算过(双方流水原子写入,正常下同真同假;异常单边脏数据也按已处理回滚防双扣)。
		return true, nil
	}

	// 2) 资产对转。两条腿都「先动 items 表、后动 currency 表」,配合 player 升序保证全局锁序一致。
	sellerLeg := func() error {
		if _, derr := deductItemTx(ctx, tx, sellerID, itemConfigID, quantity); derr != nil {
			return derr // 卖家道具不足 → ErrInventoryInsufficient
		}
		return addGoldTx(ctx, tx, sellerID, totalGold)
	}
	buyerLeg := func() error {
		if aerr := addItemTx(ctx, tx, buyerID, itemConfigID, quantity); aerr != nil {
			return aerr
		}
		if _, derr := deductGoldTx(ctx, tx, buyerID, totalGold); derr != nil {
			return derr // 买家金币不足 → ErrInventoryInsufficient
		}
		return nil
	}
	first, second := sellerLeg, buyerLeg
	if buyerID < sellerID {
		first, second = buyerLeg, sellerLeg
	}
	if ferr := first(); ferr != nil {
		return false, ferr
	}
	if serr := second(); serr != nil {
		return false, serr
	}

	if cerr := tx.Commit(); cerr != nil {
		return false, errcode.New(errcode.ErrInternal, "commit auction settle match=%d: %v", matchID, cerr)
	}
	return false, nil
}

// isDupErr 判断是否 MySQL 1062 唯一键冲突(go-sql-driver 错误串含 "Error 1062")。
func isDupErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1062")
}
