// backend_check.go — owner 权威库后端强校验(§9.22)。
//
// owner CAS 依赖「线性一致 + 确认写不回滚」:MySQL 异步复制主从切换会回滚已确认写,
// 一次回滚就可能让两台 DS 同时拿到同一玩家的 owner(双 owner 脑裂),生产禁用。
// dev 单机 MySQL(无复制)天然满足,故校验由配置 require_tidb 驱动:
// -Prod 产物由 gen_cluster_config.ps1 机械注入 require_tidb: true,dev 模板保持 false。
package data

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// AssertTiDBBackend 校验连接的权威库确为 TiDB,不是则返回错误(调用方 fail-fast 退出)。
// TiDB 的 VERSION() 形如 "8.0.11-TiDB-v8.1.0",以 "-TiDB-" 特征串判定。
func AssertTiDBBackend(ctx context.Context, db *sql.DB) error {
	var version string
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return fmt.Errorf("owner 权威库 VERSION() 查询失败: %w", err)
	}
	if !isTiDBVersion(version) {
		return fmt.Errorf("owner 权威库要求 TiDB(require_tidb=true),实际 VERSION()=%q:"+
			"MySQL 异步复制切换会回滚已确认写,违反 §9.22,拒绝启动", version)
	}
	return nil
}

// isTiDBVersion 判定 VERSION() 返回串是否 TiDB。
func isTiDBVersion(version string) bool {
	return strings.Contains(version, "-TiDB-")
}
