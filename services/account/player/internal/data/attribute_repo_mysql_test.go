// 属性加点 repo 真实 MySQL 集成测试(2026-07-12)。
//
// 默认随 go test 编译，仅由 PANDORA_TEST_MYSQL_DSN 门控执行：
//   - 未设置：明确 Skip，适配无 MySQL 的本地 / CI。
//   - 已设置：DSN 必须不带库名，测试创建随机临时库，连接 / 建库 / 建表 /
//     锁视图权限任一失败都硬失败，不得 false-green。
//
// 本地运行(专用测试账号需有 CREATE/DROP DATABASE、临时库 DDL/DML、TRIGGER，以及读取
// information_schema.INNODB_TRX/PROCESSLIST 的 PROCESS 类权限；不得复用生产业务账号)：
//
//	$env:PANDORA_TEST_MYSQL_DSN = 'root:pandora@tcp(127.0.0.1:3307)/?parseTime=true&loc=UTC&charset=utf8mb4'
//	go test -v -count=1 -run TestAttributeRepo_MySQL ./internal/data/...
//
// 安全边界：拒绝 /pandora_player 等带库名 DSN；只在严格校验过的
// pandora_player_it_<timestamp>_<random> 临时库中建表，不删除任何业务库玩家行。
package data

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

const (
	attributeTestDBPrefix    = "pandora_player_it_"
	attributeFaultTrigger    = "attr_it_fail_deduct"
	attributeFaultSentinel   = "attr_it_forced_deduct_failure"
	attributeSetupTimeout    = 10 * time.Second
	attributeLockWaitTimeout = 8 * time.Second
)

var attributeTestDBPattern = regexp.MustCompile(`^pandora_player_it_[0-9]+_[0-9a-f]{16}$`)

// attributeTestDDL 只建 AllocateAttributePoints 及属性点授予依赖的最小表集。
// 列约束取自 deploy/mysql-init/04-player-tables.sql。
var attributeTestDDL = []string{
	`CREATE TABLE players (
		player_id BIGINT UNSIGNED NOT NULL,
		nickname VARCHAR(64) NOT NULL,
		unspent_attr_points INT NOT NULL DEFAULT 0,
		PRIMARY KEY (player_id),
		UNIQUE KEY uk_nickname (nickname)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE player_attributes (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		player_id BIGINT UNSIGNED NOT NULL,
		attr_key VARCHAR(32) NOT NULL,
		points INT NOT NULL DEFAULT 0,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		PRIMARY KEY (id),
		UNIQUE KEY uk_player_attr (player_id, attr_key),
		KEY idx_player (player_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE attr_point_grants (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		player_id BIGINT UNSIGNED NOT NULL,
		idempotency_key VARCHAR(64) NOT NULL,
		points INT NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (id),
		UNIQUE KEY uk_player_grant (player_id, idempotency_key),
		KEY idx_player (player_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
}

type attributeMySQLFixture struct {
	db      *sql.DB
	admin   *sql.DB
	dbName  string
	created bool
}

// openAttributeTestDB 创建完全隔离的随机临时库。
// CREATE 成功后立即注册 Cleanup，因此后续 Ping / 建表失败也不会泄漏测试库。
func openAttributeTestDB(t *testing.T) *attributeMySQLFixture {
	t.Helper()
	dsn := os.Getenv("PANDORA_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("PANDORA_TEST_MYSQL_DSN 未设置，跳过真实 MySQL 集成测试")
	}

	cfg, err := drivermysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("解析 PANDORA_TEST_MYSQL_DSN 失败: %v", err)
	}
	if cfg.DBName != "" {
		t.Fatalf("PANDORA_TEST_MYSQL_DSN 禁止携带库名 %q；必须使用以 / 结尾的服务器级 DSN，测试将自建随机临时库", cfg.DBName)
	}

	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatalf("打开 MySQL 管理连接失败: %v", err)
	}
	f := &attributeMySQLFixture{admin: admin}
	// admin 在建库前已打开；即使 Ping / CREATE 失败也必须报告 Close 错误。
	t.Cleanup(func() { f.cleanup(t) })

	ctx, cancel := context.WithTimeout(context.Background(), attributeSetupTimeout)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("已设 PANDORA_TEST_MYSQL_DSN 但 MySQL 不可达(不允许静默 PASS): %v", err)
	}

	f.dbName = newAttributeTestDBName(t)
	if !attributeTestDBPattern.MatchString(f.dbName) {
		t.Fatalf("内部错误：临时库名未通过安全校验: %q", f.dbName)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+f.dbName+"` DEFAULT CHARSET utf8mb4"); err != nil {
		t.Fatalf("创建随机临时库 %s 失败: %v", f.dbName, err)
	}
	f.created = true

	testCfg := *cfg
	testCfg.DBName = f.dbName
	db, err := sql.Open("mysql", testCfg.FormatDSN())
	if err != nil {
		t.Fatalf("打开临时库 %s 失败: %v", f.dbName, err)
	}
	f.db = db
	db.SetMaxOpenConns(16) // holdTx + 两个并发 writer + 锁视图查询必须使用不同连接。
	db.SetMaxIdleConns(16)
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("连接临时库 %s 失败: %v", f.dbName, err)
	}
	var selectedDB string
	if err := db.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&selectedDB); err != nil {
		t.Fatalf("校验当前数据库失败: %v", err)
	}
	if selectedDB != f.dbName {
		t.Fatalf("连接落到非预期库 %q，期望随机临时库 %q，拒绝执行 DDL", selectedDB, f.dbName)
	}
	for _, stmt := range attributeTestDDL {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("临时库 %s 建表失败: %v\nSQL: %s", f.dbName, err, stmt)
		}
	}
	return f
}

func newAttributeTestDBName(t *testing.T) string {
	t.Helper()
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		t.Fatalf("生成临时库随机后缀失败: %v", err)
	}
	return fmt.Sprintf("%s%d_%s", attributeTestDBPrefix, time.Now().UnixNano(), hex.EncodeToString(random))
}

func (f *attributeMySQLFixture) cleanup(t *testing.T) {
	t.Helper()
	if f.db != nil {
		if err := f.db.Close(); err != nil {
			t.Errorf("关闭临时库连接 %s 失败: %v", f.dbName, err)
		}
	}
	if f.created {
		// 删库前再做一次独立严格校验；名字异常时宁可泄漏临时库也绝不 DROP。
		if !attributeTestDBPattern.MatchString(f.dbName) {
			t.Errorf("拒绝删除未通过安全校验的数据库 %q", f.dbName)
		} else if f.admin == nil {
			t.Errorf("无管理连接，无法删除临时库 %s", f.dbName)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), attributeSetupTimeout)
			_, err := f.admin.ExecContext(ctx, "DROP DATABASE `"+f.dbName+"`")
			cancel()
			if err != nil {
				t.Errorf("删除临时库 %s 失败: %v", f.dbName, err)
			}
		}
	}
	if f.admin != nil {
		if err := f.admin.Close(); err != nil {
			t.Errorf("关闭 MySQL 管理连接失败: %v", err)
		}
	}
}

func seedAttributePlayer(t *testing.T, db *sql.DB, playerID uint64, unspent int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), attributeSetupTimeout)
	defer cancel()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO players (player_id, nickname, unspent_attr_points) VALUES (?, ?, ?)`,
		playerID, fmt.Sprintf("attr_it_%d", playerID), unspent); err != nil {
		t.Fatalf("创建测试玩家 %d 失败: %v", playerID, err)
	}
}

func readAttributeUnspent(t *testing.T, db *sql.DB, playerID uint64) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), attributeSetupTimeout)
	defer cancel()
	var value int
	if err := db.QueryRowContext(ctx,
		`SELECT unspent_attr_points FROM players WHERE player_id = ?`, playerID).Scan(&value); err != nil {
		t.Fatalf("读取玩家 %d 未分配点失败: %v", playerID, err)
	}
	return value
}

func readAttributePoints(t *testing.T, db *sql.DB, playerID uint64, key string) (int, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), attributeSetupTimeout)
	defer cancel()
	var value int
	err := db.QueryRowContext(ctx,
		`SELECT points FROM player_attributes WHERE player_id = ? AND attr_key = ?`, playerID, key).Scan(&value)
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		t.Fatalf("读取玩家 %d 属性 %s 失败: %v", playerID, key, err)
	}
	return value, true
}

func countAttributeRows(t *testing.T, db *sql.DB, playerID uint64) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), attributeSetupTimeout)
	defer cancel()
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM player_attributes WHERE player_id = ?`, playerID).Scan(&count); err != nil {
		t.Fatalf("统计玩家 %d 属性行失败: %v", playerID, err)
	}
	return count
}

// waitForAttributeLockWaiters 严格确认 want 个 writer 同时等待临时库 players 的记录锁。
// 两个 writer 分配同一属性:若删掉首个 SELECT ... FOR UPDATE,一个会先阻塞在
// player_attributes,最多只有一个能走到最终 UPDATE players,因此这里要求两个独立等待
// 线程仍可杀死该 mutant。不要依赖 INNODB_TRX.trx_query:MySQL 8.4 在锁等待期间可能
// 暂时返回 NULL,会让已经存在的 data_lock_waits 被误报为 0。
// 锁视图不可读时必须硬失败；固定 sleep 会使删除 FOR UPDATE 的 mutant false-green。
func waitForAttributeLockWaiters(t *testing.T, db *sql.DB, dbName string, want int) {
	t.Helper()
	const query = `SELECT COUNT(DISTINCT waits.REQUESTING_THREAD_ID)
		FROM performance_schema.data_lock_waits AS waits
		JOIN performance_schema.data_locks AS requested
		  ON requested.ENGINE = waits.ENGINE
		 AND requested.ENGINE_LOCK_ID = waits.REQUESTING_ENGINE_LOCK_ID
		WHERE requested.OBJECT_SCHEMA = ?
		  AND requested.OBJECT_NAME = 'players'
		  AND requested.LOCK_TYPE = 'RECORD'
		  AND requested.LOCK_STATUS = 'WAITING'`

	deadline := time.Now().Add(attributeLockWaitTimeout)
	lastCount := 0
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := db.QueryRowContext(ctx, query, dbName).Scan(&lastCount)
		cancel()
		if err != nil {
			// 共享 MySQL 测试实例高负载时,performance_schema 快照本身可能短暂超过 1s；
			// 继续主动查询直到总 deadline，但最终仍必须真实观测到两个 LOCK WAIT，绝不以 sleep 代替。
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				continue
			}
			t.Fatalf("查询 performance_schema 锁等待视图失败(已设 DSN 时不允许降级为 sleep；请给测试账号读取 data_lock_waits/data_locks 的权限): %v", err)
		}
		if lastCount >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("未在 %s 内建立 %d 个同时等待 players 记录锁的 writer，最后观测为 %d；拒绝把未确定并发当作通过",
		attributeLockWaitTimeout, want, lastCount)
}

func TestAttributeRepo_MySQL(t *testing.T) {
	f := openAttributeTestDB(t)
	repo := NewMySQLPlayerRepo(f.db)
	ctx := context.Background()

	t.Run("Success_CommitsDeductAndUpsert", func(t *testing.T) {
		const playerID uint64 = 1
		seedAttributePlayer(t, f.db, playerID, 30)
		unspent, err := repo.AllocateAttributePoints(ctx, playerID, []AttrAllocation{
			{Key: "str", Points: 10},
			{Key: "agi", Points: 5},
			{Key: "str", Points: 5}, // 重复 key 归并为 str +15。
		})
		if err != nil {
			t.Fatalf("AllocateAttributePoints: %v", err)
		}
		if unspent != 10 || readAttributeUnspent(t, f.db, playerID) != 10 {
			t.Fatalf("提交后 unspent=%d db=%d，期望都为 10", unspent, readAttributeUnspent(t, f.db, playerID))
		}
		if value, ok := readAttributePoints(t, f.db, playerID, "str"); !ok || value != 15 {
			t.Fatalf("str=(%d,%v)，期望 (15,true)", value, ok)
		}
		if value, ok := readAttributePoints(t, f.db, playerID, "agi"); !ok || value != 5 {
			t.Fatalf("agi=(%d,%v)，期望 (5,true)", value, ok)
		}
	})

	t.Run("Insufficient_RejectsWithNoWrite", func(t *testing.T) {
		const playerID uint64 = 2
		seedAttributePlayer(t, f.db, playerID, 10)
		_, err := repo.AllocateAttributePoints(ctx, playerID, []AttrAllocation{{Key: "str", Points: 20}})
		if errcode.As(err) != errcode.ErrPlayerInsufficientPoints {
			t.Fatalf("err=%v，期望 ErrPlayerInsufficientPoints", err)
		}
		if unspent := readAttributeUnspent(t, f.db, playerID); unspent != 10 {
			t.Fatalf("拒绝后 unspent=%d，期望 10", unspent)
		}
		if rows := countAttributeRows(t, f.db, playerID); rows != 0 {
			t.Fatalf("点数不足却写入 %d 条属性", rows)
		}
	})

	t.Run("ColumnOverflow_RejectsWithNoWrite", func(t *testing.T) {
		const playerID uint64 = 3
		seedAttributePlayer(t, f.db, playerID, math.MaxInt32)
		if _, err := f.db.ExecContext(ctx,
			`INSERT INTO player_attributes (player_id, attr_key, points) VALUES (?, 'str', ?)`,
			playerID, int64(math.MaxInt32-3)); err != nil {
			t.Fatalf("预置 str 失败: %v", err)
		}
		_, err := repo.AllocateAttributePoints(ctx, playerID, []AttrAllocation{{Key: "str", Points: 10}})
		if errcode.As(err) != errcode.ErrInvalidArg {
			t.Fatalf("err=%v，期望 ErrInvalidArg", err)
		}
		if value, ok := readAttributePoints(t, f.db, playerID, "str"); !ok || value != math.MaxInt32-3 {
			t.Fatalf("溢出拒绝后 str=(%d,%v)，期望 (%d,true)", value, ok, math.MaxInt32-3)
		}
		if unspent := readAttributeUnspent(t, f.db, playerID); unspent != math.MaxInt32 {
			t.Fatalf("溢出拒绝后 unspent=%d，期望 %d", unspent, math.MaxInt32)
		}
	})

	t.Run("FinalDeductFailure_RollsBackPriorUpserts", func(t *testing.T) {
		const playerID uint64 = 4
		seedAttributePlayer(t, f.db, playerID, 30)
		// AllocateAttributePoints 先 upsert 所有属性，最后 UPDATE players 扣点。
		// 在最后一步注入确定性故障，专门验证“已部分写入后出错”的整事务回滚。
		createTrigger := fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON players
			FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = '%s'`, attributeFaultTrigger, attributeFaultSentinel)
		if _, err := f.db.ExecContext(ctx, createTrigger); err != nil {
			t.Fatalf("创建故障注入触发器失败: %v", err)
		}
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), attributeSetupTimeout)
			_, err := f.db.ExecContext(cleanupCtx, "DROP TRIGGER `"+attributeFaultTrigger+"`")
			cancel()
			if err != nil {
				t.Errorf("清理故障注入触发器失败: %v", err)
			}
		}()

		_, err := repo.AllocateAttributePoints(ctx, playerID, []AttrAllocation{
			{Key: "str", Points: 7},
			{Key: "agi", Points: 5},
		})
		if errcode.As(err) != errcode.ErrInternal || !strings.Contains(err.Error(), attributeFaultSentinel) {
			t.Fatalf("未命中最终扣点故障: code=%d err=%v", errcode.As(err), err)
		}
		if rows := countAttributeRows(t, f.db, playerID); rows != 0 {
			t.Fatalf("最终扣点失败后仍残留 %d 条先前 upsert，事务未完整回滚", rows)
		}
		if unspent := readAttributeUnspent(t, f.db, playerID); unspent != 30 {
			t.Fatalf("最终扣点失败后 unspent=%d，期望 30", unspent)
		}
	})

	t.Run("Concurrent_ForUpdateSerializesDeterministically", func(t *testing.T) {
		const playerID uint64 = 5
		seedAttributePlayer(t, f.db, playerID, 10)

		holdCtx, holdCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer holdCancel()
		holdTx, err := f.db.BeginTx(holdCtx, nil)
		if err != nil {
			t.Fatalf("创建外部持锁事务失败: %v", err)
		}
		holdDone := false
		var lockedID uint64
		if err := holdTx.QueryRowContext(holdCtx,
			`SELECT player_id FROM players WHERE player_id = ? FOR UPDATE`, playerID).Scan(&lockedID); err != nil {
			t.Fatalf("外部事务锁定玩家失败: %v", err)
		}

		type allocateResult struct {
			unspent int
			err     error
		}
		start := make(chan struct{})
		results := make(chan allocateResult, 2)
		workerCtx, workerCancel := context.WithTimeout(context.Background(), 15*time.Second)
		var workers sync.WaitGroup
		workers.Add(2)
		defer func() {
			workerCancel()
			if !holdDone {
				if err := holdTx.Rollback(); err != nil && err != sql.ErrTxDone {
					t.Errorf("回滚外部持锁事务失败: %v", err)
				}
				holdDone = true
			}
			joined := make(chan struct{})
			go func() {
				workers.Wait()
				close(joined)
			}()
			select {
			case <-joined:
			case <-time.After(3 * time.Second):
				t.Errorf("并发 writer 在取消并释放外部行锁后仍未退出")
			}
		}()
		for i := 0; i < 2; i++ {
			go func() {
				defer workers.Done()
				<-start
				unspent, allocErr := repo.AllocateAttributePoints(workerCtx, playerID,
					[]AttrAllocation{{Key: "str", Points: 10}})
				results <- allocateResult{unspent: unspent, err: allocErr}
			}()
		}
		close(start)

		// 两个 writer 必须同时阻塞在余额的首个锁定读；这一观测使
		// “调度器碰巧串行”无法让删除 FOR UPDATE 的实现蒙混过关。
		waitForAttributeLockWaiters(t, f.admin, f.dbName, 2)
		if err := holdTx.Commit(); err != nil {
			t.Fatalf("提交外部持锁事务失败: %v", err)
		}
		holdDone = true

		var success, insufficient int
		for i := 0; i < 2; i++ {
			select {
			case result := <-results:
				switch code := errcode.As(result.err); code {
				case errcode.OK:
					success++
					if result.unspent != 0 {
						t.Errorf("成功 writer 返回 unspent=%d，期望 0", result.unspent)
					}
				case errcode.ErrPlayerInsufficientPoints:
					insufficient++
				default:
					t.Errorf("并发 writer 返回非预期错误 code=%d err=%v", code, result.err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("等待并发 writer 完成超时")
			}
		}
		if success != 1 || insufficient != 1 {
			t.Fatalf("并发结果 success=%d insufficient=%d，期望 1/1", success, insufficient)
		}
		if value, ok := readAttributePoints(t, f.db, playerID, "str"); !ok || value != 10 {
			t.Fatalf("并发后 str=(%d,%v)，期望 (10,true)", value, ok)
		}
		if unspent := readAttributeUnspent(t, f.db, playerID); unspent != 0 {
			t.Fatalf("并发后 unspent=%d，期望 0", unspent)
		}
	})
}
