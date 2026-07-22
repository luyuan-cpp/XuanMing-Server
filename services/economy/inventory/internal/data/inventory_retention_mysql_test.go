// inventory_retention_mysql_test.go — 保留期清理的真实 MySQL 集成测试(2026-07-21,§9.24)。
//
// 复用 inventory_repo_mysql_test.go 的随机临时库夹具(PANDORA_TEST_MYSQL_DSN 未设置时 Skip)。
// 覆盖:超期删 / 未超期留、单批 limit 有界、escrow 只删 closed(active 超期也不删)、
// 删后迟到 ReleaseEscrow 幂等 no-op。
package data

import (
	"context"
	"database/sql"
	"testing"
)

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %s: %v", q, err)
	}
}

func countRows(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", q, err)
	}
	return n
}

func TestInventoryRetentionSweep_MySQL(t *testing.T) {
	f := openInventoryMySQLFixture(t)
	repo := NewMySQLInventoryRepo(f.db)
	ctx := context.Background()

	t.Run("LedgerDeletesOnlyExpired", func(t *testing.T) {
		mustExec(t, f.db, `INSERT INTO inventory_ledger(player_id,idempotency_key,op,request_fingerprint,detail,created_at) VALUES
			(1,'old-1','grant','fp','', DATE_SUB(NOW(), INTERVAL 91 DAY)),
			(1,'old-2','use','fp','', DATE_SUB(NOW(), INTERVAL 120 DAY)),
			(1,'fresh','grant','fp','', DATE_SUB(NOW(), INTERVAL 89 DAY))`)

		n, err := repo.DeleteLedgerBefore(ctx, 90, 100)
		if err != nil || n != 2 {
			t.Fatalf("DeleteLedgerBefore: n=%d err=%v, want n=2", n, err)
		}
		if got := countRows(t, f.db, `SELECT COUNT(*) FROM inventory_ledger WHERE player_id=1`); got != 1 {
			t.Fatalf("剩余流水行=%d want=1(只留未超期)", got)
		}
		var key string
		if err := f.db.QueryRow(`SELECT idempotency_key FROM inventory_ledger WHERE player_id=1`).Scan(&key); err != nil || key != "fresh" {
			t.Fatalf("留存行 key=%q err=%v, want fresh", key, err)
		}
	})

	t.Run("LedgerBatchLimitBounded", func(t *testing.T) {
		mustExec(t, f.db, `INSERT INTO inventory_ledger(player_id,idempotency_key,op,request_fingerprint,detail,created_at) VALUES
			(3,'b1','grant','fp','', DATE_SUB(NOW(), INTERVAL 100 DAY)),
			(3,'b2','grant','fp','', DATE_SUB(NOW(), INTERVAL 100 DAY)),
			(3,'b3','grant','fp','', DATE_SUB(NOW(), INTERVAL 100 DAY))`)

		if n, err := repo.DeleteLedgerBefore(ctx, 90, 2); err != nil || n != 2 {
			t.Fatalf("第一批: n=%d err=%v, want n=2(limit 有界)", n, err)
		}
		if n, err := repo.DeleteLedgerBefore(ctx, 90, 2); err != nil || n != 1 {
			t.Fatalf("第二批: n=%d err=%v, want n=1(积压跨轮摊平)", n, err)
		}
		if n, err := repo.DeleteLedgerBefore(ctx, 90, 2); err != nil || n != 0 {
			t.Fatalf("清空后: n=%d err=%v, want n=0(幂等空批)", n, err)
		}
	})

	t.Run("EscrowDeletesOnlyClosedExpired", func(t *testing.T) {
		// order 21: closed 超期 → 删;order 22: closed 未超期 → 留;
		// order 23: active 超期 400 天 → 永不删(遗留 OPEN/PARTIAL 订单核对依赖其存在)。
		mustExec(t, f.db, `INSERT INTO auction_escrow(player_id,order_id,kind,item_config_id,frozen_qty,frozen_gold,status,created_at,updated_at) VALUES
			(2,21,1,7001,0,0,2, DATE_SUB(NOW(), INTERVAL 100 DAY), DATE_SUB(NOW(), INTERVAL 91 DAY)),
			(2,22,1,7001,0,0,2, DATE_SUB(NOW(), INTERVAL 100 DAY), DATE_SUB(NOW(), INTERVAL 10 DAY)),
			(2,23,1,7001,5,0,1, DATE_SUB(NOW(), INTERVAL 400 DAY), DATE_SUB(NOW(), INTERVAL 400 DAY))`)

		n, err := repo.DeleteClosedEscrowBefore(ctx, 90, 100)
		if err != nil || n != 1 {
			t.Fatalf("DeleteClosedEscrowBefore: n=%d err=%v, want n=1", n, err)
		}
		if got := countRows(t, f.db, `SELECT COUNT(*) FROM auction_escrow WHERE player_id=2 AND order_id=21`); got != 0 {
			t.Fatal("closed 超期行未被删除")
		}
		if got := countRows(t, f.db, `SELECT COUNT(*) FROM auction_escrow WHERE player_id=2 AND order_id=22`); got != 1 {
			t.Fatal("closed 未超期行不应被删除")
		}
		if got := countRows(t, f.db, `SELECT COUNT(*) FROM auction_escrow WHERE player_id=2 AND order_id=23`); got != 1 {
			t.Fatal("active 行无论多老都不得清理")
		}

		// 删后迟到 ReleaseEscrow:ErrNoRows → already=true no-op,fail-safe 不报错不退资产。
		already, rerr := repo.ReleaseEscrow(ctx, 2, 21)
		if rerr != nil || !already {
			t.Fatalf("迟到 ReleaseEscrow: already=%v err=%v, want already no-op", already, rerr)
		}
	})
}
