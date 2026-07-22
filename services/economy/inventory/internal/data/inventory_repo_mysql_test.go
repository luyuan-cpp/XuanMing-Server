// inventory_repo_mysql_test.go — EnsureAuctionEscrow 的真实 MySQL 8.4 集成测试。
//
// PANDORA_TEST_MYSQL_DSN 必须是不带 database 的专用测试实例 DSN。测试只会创建并删除名称严格匹配
// pandora_inventory_it_* 的随机临时库；未设置时明确 Skip，设置后连接/DDL/事务失败一律硬失败。
package data

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

const inventoryMySQLTestTimeout = 15 * time.Second

var inventoryMySQLTestDBPattern = regexp.MustCompile(`^pandora_inventory_it_[0-9]+_[0-9a-f]{12}$`)

type inventoryMySQLFixture struct {
	admin   *sql.DB
	db      *sql.DB
	dbName  string
	created bool
}

func TestInventoryEnsureAuctionEscrow_MySQL(t *testing.T) {
	f := openInventoryMySQLFixture(t)
	repo := NewMySQLInventoryRepo(f.db)

	t.Run("ExistingActiveValidatedWithoutRefreeze", func(t *testing.T) {
		seedItem(t, f.db, 101, 7001, 10)
		if _, err := repo.FreezeForOrder(context.Background(), 101, 1001,
			EscrowKindItem, 7001, 5, 0); err != nil {
			t.Fatalf("预冻 existing escrow: %v", err)
		}
		already, err := repo.EnsureAuctionEscrow(context.Background(), 101, 1001,
			EscrowKindItem, 7001, 3, 100)
		if err != nil || !already {
			t.Fatalf("校验 existing escrow: already=%v err=%v", already, err)
		}
		if got := queryItemCount(t, f.db, 101, 7001); got != 5 {
			t.Fatalf("existing ensure 不得二次扣道具: got=%d want=5", got)
		}
		e := queryEscrow(t, f.db, 101, 1001)
		if e.kind != int8(EscrowKindItem) || e.itemConfigID != 7001 || e.frozenQty != 5 || e.status != escrowStatusActive {
			t.Fatalf("existing escrow 被意外改变: %+v", e)
		}
	})

	t.Run("MissingEscrowAtomicallyFreezes", func(t *testing.T) {
		seedItem(t, f.db, 102, 7002, 5)
		already, err := repo.EnsureAuctionEscrow(context.Background(), 102, 1002,
			EscrowKindItem, 7002, 3, 80)
		if err != nil || already {
			t.Fatalf("补建 sell escrow: already=%v err=%v", already, err)
		}
		if got := queryItemCount(t, f.db, 102, 7002); got != 2 {
			t.Fatalf("补冻后 active item=%d want=2", got)
		}
		if got := queryEscrow(t, f.db, 102, 1002).frozenQty; got != 3 {
			t.Fatalf("补冻 item escrow=%d want=3", got)
		}

		seedGold(t, f.db, 103, 1000)
		already, err = repo.EnsureAuctionEscrow(context.Background(), 103, 1003,
			EscrowKindGold, 7003, 4, 125)
		if err != nil || already {
			t.Fatalf("补建 buy escrow: already=%v err=%v", already, err)
		}
		if got := queryGold(t, f.db, 103); got != 500 {
			t.Fatalf("补冻后 active gold=%d want=500", got)
		}
		if got := queryEscrow(t, f.db, 103, 1003).frozenGold; got != 500 {
			t.Fatalf("补冻 gold escrow=%d want=500", got)
		}
	})

	t.Run("InsufficientRollsBackEscrowAndAssets", func(t *testing.T) {
		seedItem(t, f.db, 104, 7004, 1)
		already, err := repo.EnsureAuctionEscrow(context.Background(), 104, 1004,
			EscrowKindItem, 7004, 2, 100)
		if already || errcode.As(err) != errcode.ErrInventoryInsufficient {
			t.Fatalf("不足应明确失败: already=%v err=%v", already, err)
		}
		if got := queryItemCount(t, f.db, 104, 7004); got != 1 {
			t.Fatalf("失败后 active item=%d want=1", got)
		}
		var rows int
		if err := f.db.QueryRow(`SELECT COUNT(*) FROM auction_escrow WHERE player_id=104 AND order_id=1004`).Scan(&rows); err != nil {
			t.Fatalf("查询失败后的 escrow: %v", err)
		}
		if rows != 0 {
			t.Fatalf("失败后不得留下 escrow 行: rows=%d", rows)
		}
	})

	t.Run("MismatchClosedAndShortReturnDeterministicErrors", func(t *testing.T) {
		seedItem(t, f.db, 105, 7005, 5)
		if _, err := repo.FreezeForOrder(context.Background(), 105, 1005,
			EscrowKindItem, 7005, 3, 0); err != nil {
			t.Fatalf("预冻 mismatch escrow: %v", err)
		}
		if _, err := repo.EnsureAuctionEscrow(context.Background(), 105, 1005,
			EscrowKindItem, 7999, 2, 100); errcode.As(err) != errcode.ErrInventoryIdempotencyConflict {
			t.Fatalf("item mismatch want conflict, got %v", err)
		}
		if _, err := repo.EnsureAuctionEscrow(context.Background(), 105, 1005,
			EscrowKindGold, 7005, 2, 100); errcode.As(err) != errcode.ErrInventoryIdempotencyConflict {
			t.Fatalf("kind mismatch want conflict, got %v", err)
		}
		if _, err := f.db.Exec(`UPDATE auction_escrow SET frozen_qty=1 WHERE player_id=105 AND order_id=1005`); err != nil {
			t.Fatalf("制造 short escrow: %v", err)
		}
		if _, err := repo.EnsureAuctionEscrow(context.Background(), 105, 1005,
			EscrowKindItem, 7005, 2, 100); errcode.As(err) != errcode.ErrInventoryInsufficient {
			t.Fatalf("short escrow want insufficient, got %v", err)
		}
		if _, err := repo.ReleaseEscrow(context.Background(), 105, 1005); err != nil {
			t.Fatalf("关闭 escrow: %v", err)
		}
		if _, err := repo.EnsureAuctionEscrow(context.Background(), 105, 1005,
			EscrowKindItem, 7005, 1, 100); errcode.As(err) != errcode.ErrInventoryIdempotencyConflict {
			t.Fatalf("closed escrow want conflict, got %v", err)
		}
	})

	t.Run("ConcurrentSameRequestFreezesExactlyOnce", func(t *testing.T) {
		seedItem(t, f.db, 106, 7006, 5)
		const workers = 16
		start := make(chan struct{})
		errs := make(chan error, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), inventoryMySQLTestTimeout)
				defer cancel()
				_, err := repo.EnsureAuctionEscrow(ctx, 106, 1006,
					EscrowKindItem, 7006, 5, 100)
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Errorf("并发同请求 ensure: %v", err)
			}
		}
		if got := queryItemCount(t, f.db, 106, 7006); got != 0 {
			t.Fatalf("并发同请求只能扣一次: active=%d want=0", got)
		}
		e := queryEscrow(t, f.db, 106, 1006)
		if e.frozenQty != 5 || e.status != escrowStatusActive {
			t.Fatalf("并发同请求 escrow=%+v want qty=5 active", e)
		}
	})

	t.Run("ConcurrentConflictingRequestNeverTreats1062AsSuccess", func(t *testing.T) {
		seedItem(t, f.db, 107, 7101, 5)
		seedItem(t, f.db, 107, 7102, 5)
		start := make(chan struct{})
		type result struct {
			item uint32
			err  error
		}
		results := make(chan result, 2)
		var wg sync.WaitGroup
		for _, itemID := range []uint32{7101, 7102} {
			itemID := itemID
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), inventoryMySQLTestTimeout)
				defer cancel()
				_, err := repo.EnsureAuctionEscrow(ctx, 107, 1007,
					EscrowKindItem, itemID, 5, 100)
				results <- result{item: itemID, err: err}
			}()
		}
		close(start)
		wg.Wait()
		close(results)

		var succeeded uint32
		for got := range results {
			if got.err == nil {
				if succeeded != 0 {
					t.Fatalf("冲突请求不应都成功: first=%d second=%d", succeeded, got.item)
				}
				succeeded = got.item
				continue
			}
			if errcode.As(got.err) != errcode.ErrInventoryIdempotencyConflict {
				t.Fatalf("冲突 loser item=%d 应返回 conflict, got %v", got.item, got.err)
			}
		}
		if succeeded == 0 {
			t.Fatal("冲突请求应恰有一个创建成功")
		}
		e := queryEscrow(t, f.db, 107, 1007)
		if e.itemConfigID != succeeded || e.frozenQty != 5 {
			t.Fatalf("escrow=%+v, want winner item=%d qty=5", e, succeeded)
		}
		for _, itemID := range []uint32{7101, 7102} {
			want := int64(5)
			if itemID == succeeded {
				want = 0
			}
			if got := queryItemCount(t, f.db, 107, itemID); got != want {
				t.Fatalf("item=%d active=%d want=%d", itemID, got, want)
			}
		}
	})
}

func openInventoryMySQLFixture(t *testing.T) *inventoryMySQLFixture {
	t.Helper()
	serverDSN := strings.TrimSpace(os.Getenv("PANDORA_TEST_MYSQL_DSN"))
	if serverDSN == "" {
		t.Skip("未设置 PANDORA_TEST_MYSQL_DSN，跳过 EnsureAuctionEscrow 真 MySQL 集成测试")
	}
	cfg, err := mysql.ParseDSN(serverDSN)
	if err != nil {
		t.Fatalf("解析 PANDORA_TEST_MYSQL_DSN: %v", err)
	}
	if cfg.DBName != "" {
		t.Fatalf("PANDORA_TEST_MYSQL_DSN 禁止带 database，got=%q", cfg.DBName)
	}
	cfg.MultiStatements = true
	cfg.ParseTime = true
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = inventoryMySQLTestTimeout
	cfg.WriteTimeout = inventoryMySQLTestTimeout

	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatalf("打开 MySQL 管理连接: %v", err)
	}
	f := &inventoryMySQLFixture{admin: admin}
	t.Cleanup(func() { f.cleanup(t) })
	ctx, cancel := context.WithTimeout(context.Background(), inventoryMySQLTestTimeout)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("已设置测试 DSN 但 MySQL 不可达: %v", err)
	}

	f.dbName = randomInventoryMySQLTestDBName(t)
	if !inventoryMySQLTestDBPattern.MatchString(f.dbName) {
		t.Fatalf("随机测试库名未通过安全校验: %q", f.dbName)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+f.dbName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("创建随机测试库 %s: %v", f.dbName, err)
	}
	f.created = true

	schema := readInventoryMySQLSchema(t, f.dbName)
	if _, err := admin.ExecContext(ctx, schema); err != nil {
		t.Fatalf("初始化 inventory schema: %v", err)
	}
	testCfg := cfg.Clone()
	testCfg.DBName = f.dbName
	db, err := sql.Open("mysql", testCfg.FormatDSN())
	if err != nil {
		t.Fatalf("打开随机测试库: %v", err)
	}
	f.db = db
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(32)
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("连接随机测试库: %v", err)
	}
	var selected string
	if err := db.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&selected); err != nil || selected != f.dbName {
		t.Fatalf("当前数据库校验失败: selected=%q want=%q err=%v", selected, f.dbName, err)
	}
	return f
}

func (f *inventoryMySQLFixture) cleanup(t *testing.T) {
	t.Helper()
	if f.db != nil {
		if err := f.db.Close(); err != nil {
			t.Errorf("关闭 inventory 测试库连接: %v", err)
		}
	}
	if f.created {
		if !inventoryMySQLTestDBPattern.MatchString(f.dbName) {
			t.Errorf("拒绝 DROP 非预期测试库名 %q", f.dbName)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), inventoryMySQLTestTimeout)
			_, err := f.admin.ExecContext(ctx, "DROP DATABASE `"+f.dbName+"`")
			cancel()
			if err != nil {
				t.Errorf("删除随机测试库 %s: %v", f.dbName, err)
			}
		}
	}
	if f.admin != nil {
		if err := f.admin.Close(); err != nil {
			t.Errorf("关闭 MySQL 管理连接: %v", err)
		}
	}
}

func randomInventoryMySQLTestDBName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("生成随机测试库后缀: %v", err)
	}
	return fmt.Sprintf("pandora_inventory_it_%d_%s", time.Now().UnixMilli(), hex.EncodeToString(b))
}

func readInventoryMySQLSchema(t *testing.T, dbName string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位 inventory_repo_mysql_test.go")
	}
	path := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "..",
		"deploy", "mysql-init", "08-inventory-tables.sql"))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 inventory schema %s: %v", path, err)
	}
	schema := string(b)
	const needle = "USE `pandora_trade`;"
	if strings.Count(schema, needle) != 1 {
		t.Fatalf("inventory schema USE 锚点数量异常: %d", strings.Count(schema, needle))
	}
	return strings.Replace(schema, needle, "USE `"+dbName+"`;", 1)
}

type mysqlEscrowRow struct {
	kind         int8
	itemConfigID uint32
	frozenQty    int64
	frozenGold   int64
	status       int8
}

func seedItem(t *testing.T, db *sql.DB, playerID uint64, itemID uint32, count int64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO player_items(player_id,item_config_id,count) VALUES(?,?,?)`,
		playerID, itemID, count); err != nil {
		t.Fatalf("seed item player=%d item=%d: %v", playerID, itemID, err)
	}
}

func seedGold(t *testing.T, db *sql.DB, playerID uint64, gold int64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO player_currency(player_id,gold) VALUES(?,?)`, playerID, gold); err != nil {
		t.Fatalf("seed gold player=%d: %v", playerID, err)
	}
}

func queryItemCount(t *testing.T, db *sql.DB, playerID uint64, itemID uint32) int64 {
	t.Helper()
	var got int64
	err := db.QueryRow(`SELECT count FROM player_items WHERE player_id=? AND item_config_id=?`,
		playerID, itemID).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return 0 // 堆叠扣空即删行(2026-07-22):无行 = 持有 0,语义一致。
	}
	if err != nil {
		t.Fatalf("query item player=%d item=%d: %v", playerID, itemID, err)
	}
	return got
}

// queryItemRowExists 断言行物理存在性(扣空即删行回归专用)。
func queryItemRowExists(t *testing.T, db *sql.DB, playerID uint64, itemID uint32) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM player_items WHERE player_id=? AND item_config_id=?`,
		playerID, itemID).Scan(&n); err != nil {
		t.Fatalf("count item rows player=%d item=%d: %v", playerID, itemID, err)
	}
	return n > 0
}

// TestUseItemEmptiedRowDeleted 堆叠道具用尽即删行(2026-07-22 用户要求):
// 扣到 0 时 player_items 行物理删除(不留 count=0 死行);再发放同 config 走 upsert 重建。
func TestUseItemEmptiedRowDeleted(t *testing.T) {
	f := openInventoryMySQLFixture(t)
	repo := NewMySQLInventoryRepo(f.db)
	ctx := context.Background()

	const player, item = 901, 7901
	if _, _, err := repo.GrantItems(ctx, player, []ItemGrant{{ItemConfigID: item, Count: 3}}, 0, "g1", ""); err != nil {
		t.Fatalf("发放: %v", err)
	}
	remaining, _, err := repo.UseItem(ctx, player, item, 3, "u1", "")
	if err != nil || remaining != 0 {
		t.Fatalf("用尽: remaining=%d err=%v", remaining, err)
	}
	if queryItemRowExists(t, f.db, player, item) {
		t.Fatal("扣空后行必须物理删除,不得留 count=0 死行")
	}
	// 幂等重放:同 key 重试仍返回快照 0,不复活行。
	if remaining, already, err := repo.UseItem(ctx, player, item, 3, "u1", ""); err != nil || !already || remaining != 0 {
		t.Fatalf("幂等重放: remaining=%d already=%v err=%v", remaining, already, err)
	}
	// 再发放同 config:upsert 重建行,行为不变。
	if _, _, err := repo.GrantItems(ctx, player, []ItemGrant{{ItemConfigID: item, Count: 2}}, 0, "g2", ""); err != nil {
		t.Fatalf("重建发放: %v", err)
	}
	if got := queryItemCount(t, f.db, player, item); got != 2 {
		t.Fatalf("重建后应为 2,实际 %d", got)
	}
}

func queryGold(t *testing.T, db *sql.DB, playerID uint64) int64 {
	t.Helper()
	var got int64
	if err := db.QueryRow(`SELECT gold FROM player_currency WHERE player_id=?`, playerID).Scan(&got); err != nil {
		t.Fatalf("query gold player=%d: %v", playerID, err)
	}
	return got
}

func queryEscrow(t *testing.T, db *sql.DB, playerID, orderID uint64) mysqlEscrowRow {
	t.Helper()
	var got mysqlEscrowRow
	if err := db.QueryRow(`SELECT kind,item_config_id,frozen_qty,frozen_gold,status
        FROM auction_escrow WHERE player_id=? AND order_id=?`, playerID, orderID).
		Scan(&got.kind, &got.itemConfigID, &got.frozenQty, &got.frozenGold, &got.status); err != nil {
		t.Fatalf("query escrow player=%d order=%d: %v", playerID, orderID, err)
	}
	return got
}
