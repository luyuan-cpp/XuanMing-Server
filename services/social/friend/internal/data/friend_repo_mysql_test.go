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
