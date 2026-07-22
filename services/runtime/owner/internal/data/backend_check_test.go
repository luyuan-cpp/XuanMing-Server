// backend_check_test.go — owner 权威库 TiDB 强校验测试。
//
// 单测覆盖版本串判定;集成用例(PANDORA_TEST_MYSQL_DSN,同 owner_repo_mysql_test.go 门控)
// 反向证明:对真实 MySQL 执行 AssertTiDBBackend 必须报错 —— 这正是生产门要拦的组合。
package data

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
)

func TestIsTiDBVersion(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"8.0.11-TiDB-v8.1.0", true},
		{"5.7.25-TiDB-v6.5.3", true},
		{"8.0.36", false},                       // 原生 MySQL
		{"8.0.36-0ubuntu0.22.04.1", false},      // 发行版 MySQL
		{"10.11.6-MariaDB-1:10.11.6", false},    // MariaDB
		{"8.0.11-TiDB", false},                  // 缺尾部连字符,非标准串不放行
		{"tidb", false},                         // 大小写不符,fail-closed
		{"", false},
	}
	for _, c := range cases {
		if got := isTiDBVersion(c.version); got != c.want {
			t.Errorf("isTiDBVersion(%q) = %v, want %v", c.version, got, c.want)
		}
	}
}

// TestAssertTiDBBackendRejectsMySQL 真实 MySQL 上必须拒绝(负向集成证明)。
func TestAssertTiDBBackendRejectsMySQL(t *testing.T) {
	dsn := os.Getenv("PANDORA_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("PANDORA_TEST_MYSQL_DSN 未设置,跳过真实 MySQL 集成用例")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), ownerMySQLTestTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping mysql: %v", err)
	}

	err = AssertTiDBBackend(ctx, db)
	if err == nil {
		t.Fatal("AssertTiDBBackend 对真实 MySQL 必须返回错误(require_tidb 生产门)")
	}
	if !strings.Contains(err.Error(), "require_tidb") {
		t.Fatalf("错误信息应包含 require_tidb 上下文,实际: %v", err)
	}
}
