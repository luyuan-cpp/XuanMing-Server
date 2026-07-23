// friend_repo_mysql_test.go — friend 数据层好友申请容量的真实 MySQL / TiDB 回归。
//
// PANDORA_TEST_MYSQL_DSN / PANDORA_TEST_TIDB_DSN 必须是不带 database 的专用测试实例
// DSN。测试只创建、删除名称严格匹配 pandora_friend_capacity_it_<32 hex> 的随机临时库；
// 对应变量未设置时该后端明确 Skip，变量已设置但不可达则硬失败。
package data

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

const friendCapacityDBTimeout = 30 * time.Second

var friendCapacityTestDBPattern = regexp.MustCompile(`^pandora_friend_capacity_it_[0-9a-f]{32}$`)

const friendRequestsCapacityDDL = `CREATE TABLE friend_requests (
	request_id   BIGINT UNSIGNED NOT NULL,
	requester_id BIGINT UNSIGNED NOT NULL,
	target_id    BIGINT UNSIGNED NOT NULL,
	status       TINYINT         NOT NULL DEFAULT 1,
	created_at   DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at   DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
	PRIMARY KEY (request_id),
	UNIQUE KEY uk_requester_target (requester_id, target_id),
	KEY idx_target_status (target_id, status),
	KEY idx_status_updated (status, updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`

// R5 复审 P1-2/3/4:CreateRequest/AcceptRequest/Block 现依赖好友边、黑名单与两张守卫表
// (与 deploy/mysql-init/06-social-tables.sql、deploy/tidb-init/01-social-tidb.sql 同步维护)。
var friendCapacityExtraDDLs = []string{
	`CREATE TABLE friendships (
	id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
	player_id  BIGINT UNSIGNED NOT NULL,
	friend_id  BIGINT UNSIGNED NOT NULL,
	created_at DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (id),
	UNIQUE KEY uk_player_friend (player_id, friend_id),
	KEY idx_player (player_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE blocks (
	id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
	player_id  BIGINT UNSIGNED NOT NULL,
	blocked_id BIGINT UNSIGNED NOT NULL,
	created_at DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (id),
	UNIQUE KEY uk_player_blocked (player_id, blocked_id),
	KEY idx_player (player_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE friend_player_guards (
	player_id BIGINT UNSIGNED NOT NULL,
	PRIMARY KEY (player_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE friend_pair_guards (
	lo_id BIGINT UNSIGNED NOT NULL,
	hi_id BIGINT UNSIGNED NOT NULL,
	PRIMARY KEY (lo_id, hi_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
}

type friendCapacityFixture struct {
	admin   *sql.DB
	db      *sql.DB
	dbName  string
	created bool
}

type friendCapacityBackend struct {
	name string
	env  string
}

func forEachFriendCapacityBackend(t *testing.T, fn func(t *testing.T, backend string, db *sql.DB)) {
	t.Helper()
	backends := [...]friendCapacityBackend{
		{name: "mysql", env: "PANDORA_TEST_MYSQL_DSN"},
		{name: "tidb", env: "PANDORA_TEST_TIDB_DSN"},
	}
	for _, backend := range backends {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(backend.env))
			if dsn == "" {
				t.Skipf("跳过 %s 好友申请容量集成测试：未设置 %s", backend.name, backend.env)
			}
			fixture := openFriendCapacityFixture(t, backend.name, dsn)
			fn(t, backend.name, fixture.db)
		})
	}
}

func openFriendCapacityFixture(t *testing.T, backend, dsn string) *friendCapacityFixture {
	t.Helper()
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("解析 %s 测试 DSN：%v", backend, err)
	}
	if cfg.DBName != "" {
		t.Fatalf("%s 测试 DSN 禁止携带 database，got=%q", backend, cfg.DBName)
	}
	cfg.ParseTime = true
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = friendCapacityDBTimeout
	cfg.WriteTimeout = friendCapacityDBTimeout

	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatalf("打开 %s 管理连接：%v", backend, err)
	}
	fixture := &friendCapacityFixture{admin: admin}
	t.Cleanup(func() { fixture.cleanup(t, backend) })

	ctx, cancel := context.WithTimeout(context.Background(), friendCapacityDBTimeout)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("已设置 %s 测试 DSN 但后端不可达：%v", backend, err)
	}

	var randomBytes [16]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		t.Fatalf("生成 %s 随机测试库名：%v", backend, err)
	}
	fixture.dbName = "pandora_friend_capacity_it_" + hex.EncodeToString(randomBytes[:])
	if !friendCapacityTestDBPattern.MatchString(fixture.dbName) {
		t.Fatalf("%s 随机测试库名未通过删除白名单：%q", backend, fixture.dbName)
	}
	if _, err := admin.ExecContext(ctx,
		"CREATE DATABASE `"+fixture.dbName+"` DEFAULT CHARACTER SET utf8mb4"); err != nil {
		t.Fatalf("创建 %s 随机测试库 %s：%v", backend, fixture.dbName, err)
	}
	fixture.created = true

	testCfg := cfg.Clone()
	testCfg.DBName = fixture.dbName
	db, err := sql.Open("mysql", testCfg.FormatDSN())
	if err != nil {
		t.Fatalf("打开 %s 随机测试库 %s：%v", backend, fixture.dbName, err)
	}
	fixture.db = db
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(32)
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("连接 %s 随机测试库 %s：%v", backend, fixture.dbName, err)
	}
	if _, err := db.ExecContext(ctx, friendRequestsCapacityDDL); err != nil {
		t.Fatalf("初始化 %s friend_requests：%v", backend, err)
	}
	for _, ddl := range friendCapacityExtraDDLs {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			t.Fatalf("初始化 %s 附属表：%v", backend, err)
		}
	}
	return fixture
}

func (f *friendCapacityFixture) cleanup(t *testing.T, backend string) {
	t.Helper()
	if f.db != nil {
		_ = f.db.Close()
	}
	if f.admin == nil {
		return
	}
	defer func() { _ = f.admin.Close() }()
	if !f.created {
		return
	}
	if !friendCapacityTestDBPattern.MatchString(f.dbName) {
		t.Errorf("拒绝删除未通过白名单的 %s 测试库：%q", backend, f.dbName)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), friendCapacityDBTimeout)
	defer cancel()
	if _, err := f.admin.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+f.dbName+"`"); err != nil {
		t.Errorf("删除 %s 随机测试库 %s：%v", backend, f.dbName, err)
	}
}

// TestFriendRepoFullInboxPendingRetryMySQLAndTiDB 验证收件箱已满时，已有 pending 请求的
// 重试先命中 (requester,target) 幂等行并返回 canonical request_id，不被容量检查误拒。
func TestFriendRepoFullInboxPendingRetryMySQLAndTiDB(t *testing.T) {
	forEachFriendCapacityBackend(t, func(t *testing.T, backend string, db *sql.DB) {
		ctx, cancel := context.WithTimeout(context.Background(), friendCapacityDBTimeout)
		defer cancel()
		repo := NewMySQLFriendRepo(db)

		const (
			maxIncoming       = 1
			targetID          = uint64(9_001)
			requesterID       = uint64(1_001)
			otherRequesterID  = uint64(1_002)
			canonicalRequest  = uint64(10_001)
			retryRequest      = uint64(10_002)
			rejectedRequestID = uint64(10_003)
		)

		gotID, reused, err := repo.CreateRequest(
			ctx, canonicalRequest, requesterID, targetID, maxIncoming)
		if err != nil || gotID != canonicalRequest || reused {
			t.Fatalf("%s 首次建 pending：id=%d reused=%v err=%v", backend, gotID, reused, err)
		}

		_, _, err = repo.CreateRequest(
			ctx, rejectedRequestID, otherRequesterID, targetID, maxIncoming)
		if code := errcode.As(err); code != errcode.ErrFriendRequestLimit {
			t.Fatalf("%s 满收件箱新请求 code=%d want=%d err=%v",
				backend, code, errcode.ErrFriendRequestLimit, err)
		}

		gotID, reused, err = repo.CreateRequest(
			ctx, retryRequest, requesterID, targetID, maxIncoming)
		if err != nil {
			t.Fatalf("%s 满收件箱重试已有 pending：%v", backend, err)
		}
		if gotID != canonicalRequest || !reused {
			t.Fatalf("%s 重试未返回 canonical：id=%d reused=%v want_id=%d want_reused=true",
				backend, gotID, reused, canonicalRequest)
		}

		var rowCount int
		var storedRequestID uint64
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(request_id)
FROM friend_requests WHERE target_id = ? AND status = ?`, targetID, requestStatusPending).
			Scan(&rowCount, &storedRequestID); err != nil {
			t.Fatalf("%s 查询 pending 收件箱：%v", backend, err)
		}
		if rowCount != maxIncoming || storedRequestID != canonicalRequest {
			t.Fatalf("%s 满收件箱落库异常：count=%d id=%d want_count=%d want_id=%d",
				backend, rowCount, storedRequestID, maxIncoming, canonicalRequest)
		}
	})
}

// TestFriendRepoIncomingLimitConcurrencyMySQLAndTiDB(R5 复审 P1-2):8 个不同 requester
// 并发向同一 target 发申请、上限 3——守卫行串行化后成功数必须恰为 3。修复前 TiDB
// (无 gap 锁)下 COUNT..FOR UPDATE 挡不住并发插入,可穿透到 >3。
func TestFriendRepoIncomingLimitConcurrencyMySQLAndTiDB(t *testing.T) {
	forEachFriendCapacityBackend(t, func(t *testing.T, backend string, db *sql.DB) {
		ctx, cancel := context.WithTimeout(context.Background(), friendCapacityDBTimeout)
		defer cancel()
		repo := NewMySQLFriendRepo(db)

		const (
			maxIncoming = 3
			concurrent  = 8
			targetID    = uint64(9_101)
		)
		var wg sync.WaitGroup
		errs := make([]error, concurrent)
		for i := 0; i < concurrent; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, _, errs[i] = repo.CreateRequest(ctx,
					uint64(20_000+i), uint64(2_001+i), targetID, maxIncoming)
			}(i)
		}
		wg.Wait()

		succeeded := 0
		for i, err := range errs {
			switch errcode.As(err) {
			case 0:
				succeeded++
			case errcode.ErrFriendRequestLimit:
			default:
				t.Fatalf("%s 并发申请 %d 意外错误:%v", backend, i, errs[i])
			}
		}
		var pending int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM friend_requests
WHERE target_id = ? AND status = ?`, targetID, requestStatusPending).Scan(&pending); err != nil {
			t.Fatalf("%s 查询 pending:%v", backend, err)
		}
		if succeeded != maxIncoming || pending != maxIncoming {
			t.Fatalf("%s P1-2 上限穿透:succeeded=%d pending=%d want=%d",
				backend, succeeded, pending, maxIncoming)
		}
	})
}

// TestFriendRepoAcceptBlockNeverBothMySQLAndTiDB(R5 复审 P1-4):Accept 与 Block 并发,
// 终态不变量 = 绝不允许「既好友又拉黑」。pair 守卫使两者全序:Block 先行 → Accept 见
// block 拒绝;Accept 先行 → Block 删净好友边。多轮重复以覆盖交错。
func TestFriendRepoAcceptBlockNeverBothMySQLAndTiDB(t *testing.T) {
	forEachFriendCapacityBackend(t, func(t *testing.T, backend string, db *sql.DB) {
		ctx, cancel := context.WithTimeout(context.Background(), friendCapacityDBTimeout)
		defer cancel()
		repo := NewMySQLFriendRepo(db)

		for round := 0; round < 20; round++ {
			requesterID := uint64(30_000 + round*2)
			targetID := uint64(30_001 + round*2)
			reqID := uint64(40_000 + round)
			if _, _, err := repo.CreateRequest(ctx, reqID, requesterID, targetID, 0); err != nil {
				t.Fatalf("%s round=%d 建 pending:%v", backend, round, err)
			}

			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				// accepted=false / blocked 错误都合法,只校验终态不变量。
				_, _ = repo.AcceptRequest(ctx, reqID, targetID, 0)
			}()
			go func() {
				defer wg.Done()
				if err := repo.Block(ctx, targetID, requesterID, 0); err != nil {
					t.Errorf("%s round=%d Block:%v", backend, round, err)
				}
			}()
			wg.Wait()

			var friendRows, blockRows int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM friendships
WHERE (player_id = ? AND friend_id = ?) OR (player_id = ? AND friend_id = ?)`,
				requesterID, targetID, targetID, requesterID).Scan(&friendRows); err != nil {
				t.Fatalf("%s round=%d 查好友边:%v", backend, round, err)
			}
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blocks
WHERE (player_id = ? AND blocked_id = ?) OR (player_id = ? AND blocked_id = ?)`,
				requesterID, targetID, targetID, requesterID).Scan(&blockRows); err != nil {
				t.Fatalf("%s round=%d 查黑名单:%v", backend, round, err)
			}
			if friendRows > 0 && blockRows > 0 {
				t.Fatalf("%s round=%d P1-4 不变量破坏:既好友(%d 行)又拉黑(%d 行)",
					backend, round, friendRows, blockRows)
			}
			// Block 无条件执行成功 → 终态必须有黑名单且无好友边。
			if blockRows == 0 || friendRows != 0 {
				t.Fatalf("%s round=%d 终态异常:friend=%d block=%d(Block 已成功,边必须删净)",
					backend, round, friendRows, blockRows)
			}
		}
	})
}

// TestFriendRepoFriendLimitConcurrentAcceptsMySQLAndTiDB(R5 复审 P1-2):目标玩家上限 3,
// 6 条 pending 并发接受——player 守卫串行化后成功数与好友边数必须恰为 3。
func TestFriendRepoFriendLimitConcurrentAcceptsMySQLAndTiDB(t *testing.T) {
	forEachFriendCapacityBackend(t, func(t *testing.T, backend string, db *sql.DB) {
		ctx, cancel := context.WithTimeout(context.Background(), friendCapacityDBTimeout)
		defer cancel()
		repo := NewMySQLFriendRepo(db)

		const (
			maxFriends = 3
			concurrent = 6
			accepterID = uint64(9_201)
		)
		reqIDs := make([]uint64, concurrent)
		for i := 0; i < concurrent; i++ {
			reqIDs[i] = uint64(50_000 + i)
			if _, _, err := repo.CreateRequest(ctx, reqIDs[i], uint64(3_001+i), accepterID, 0); err != nil {
				t.Fatalf("%s 建 pending %d:%v", backend, i, err)
			}
		}

		var wg sync.WaitGroup
		accepted := make([]bool, concurrent)
		errs := make([]error, concurrent)
		for i := 0; i < concurrent; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				accepted[i], errs[i] = repo.AcceptRequest(ctx, reqIDs[i], accepterID, maxFriends)
			}(i)
		}
		wg.Wait()

		okCount := 0
		for i := range errs {
			switch {
			case errs[i] == nil && accepted[i]:
				okCount++
			case errcode.As(errs[i]) == errcode.ErrFriendLimit:
			case errs[i] == nil && !accepted[i]:
			default:
				t.Fatalf("%s 并发接受 %d 意外错误:%v", backend, i, errs[i])
			}
		}
		var edges int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM friendships WHERE player_id = ?`, accepterID).Scan(&edges); err != nil {
			t.Fatalf("%s 查好友边:%v", backend, err)
		}
		if okCount != maxFriends || edges != maxFriends {
			t.Fatalf("%s P1-2 好友上限穿透:accepted=%d edges=%d want=%d",
				backend, okCount, edges, maxFriends)
		}
	})
}

func TestFriendCapacityRandomDBNameDeletionWhitelist(t *testing.T) {
	valid := "pandora_friend_capacity_it_0123456789abcdef0123456789abcdef"
	if !friendCapacityTestDBPattern.MatchString(valid) {
		t.Fatalf("合法随机库名未通过白名单：%s", valid)
	}
	for _, invalid := range []string{
		"pandora_social",
		"pandora_friend_capacity_it_0123456789abcdef",
		"pandora_friend_capacity_it_0123456789abcdef0123456789abcdeg",
		"pandora_friend_capacity_it_0123456789abcdef0123456789abcdef` DROP DATABASE pandora_social",
	} {
		if friendCapacityTestDBPattern.MatchString(invalid) {
			t.Errorf("危险库名误过删除白名单：%q", invalid)
		}
	}
}
