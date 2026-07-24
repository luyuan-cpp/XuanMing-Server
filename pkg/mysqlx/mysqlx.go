// Package mysqlx 提供 Pandora 服务的 MySQL 客户端工厂。
//
// 设计:
//   - 用标准 database/sql + github.com/go-sql-driver/mysql,**不引 ORM**(W2/W3 轻量)
//   - 业务 data 层自己写 SQL,通过 *sql.DB 调 QueryContext / ExecContext
//   - DSN 在业务 yaml 配置;连接池参数有 Pandora 默认值
//
// 用法:
//
//	db := mysqlx.MustNewClient(cfg.Node.MySQLClient)
//	defer db.Close()
//	row := db.QueryRowContext(ctx, "SELECT player_id FROM accounts WHERE account = ?", account)
//
// 故意不在本包导出 Repo 抽象,各业务自己定义 Repo 接口,只把 *sql.DB 当依赖。
package mysqlx

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql" // mysql 驱动注册

	"github.com/luyuancpp/pandora/pkg/config"
)

// 默认连接池参数(开发环境用,prod 应在 yaml 调整)。
const (
	defaultMaxOpenConns    = 32
	defaultMaxIdleConns    = 8
	defaultConnMaxLifetime = 30 * time.Minute
	defaultPingTimeout     = 3 * time.Second
)

// MustNewClient 用 config.MySQLConf 构造 *sql.DB,失败 panic。
//
// 启动期会做一次 PingContext 验证连通性(超时 3s)。
// DSN 示例:`pandora:pandora_dev_pwd@tcp(127.0.0.1:3307)/pandora_account?parseTime=true&loc=UTC&charset=utf8mb4&collation=utf8mb4_0900_ai_ci`
func MustNewClient(c config.MySQLConf) *sql.DB {
	db, err := NewClient(c)
	if err != nil {
		panic(fmt.Sprintf("mysqlx.MustNewClient: %v", err))
	}
	return db
}

// NewClient 构造 *sql.DB 并 Ping 验证。
func NewClient(c config.MySQLConf) (*sql.DB, error) {
	if c.DSN == "" {
		return nil, fmt.Errorf("mysql DSN is empty")
	}

	db, err := sql.Open("mysql", c.DSN)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}

	maxOpen := c.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = defaultMaxOpenConns
	}
	maxIdle := c.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = defaultMaxIdleConns
	}
	maxLife := c.ConnMaxLifetime.Std()
	if maxLife <= 0 {
		maxLife = defaultConnMaxLifetime
	}

	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(maxLife)
	// 门禁-B:opt-in 空闲连接上限,留空(0)沿用 database/sql 默认(不因空闲回收)。
	if maxIdleTime := c.ConnMaxIdleTime.Std(); maxIdleTime > 0 {
		db.SetConnMaxIdleTime(maxIdleTime)
	}

	pingTimeout := c.PingTimeout.Std()
	if pingTimeout <= 0 {
		pingTimeout = defaultPingTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return db, nil
}
