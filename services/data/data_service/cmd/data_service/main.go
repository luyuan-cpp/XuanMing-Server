// Pandora data_service 服务入口(2026-06-16)。
//
// 职责:玩家数据统一读写网关。
//   - ReadPlayer:cache-aside(Redis 命中直返,miss 读 MySQL 回填)
//   - WritePlayer:MySQL 乐观锁版本写(UPDATE ... WHERE version=?),写后删缓存
//   - InvalidateCache:主动删缓存
//
// 依赖策略:
//   - MySQL 强依赖(事实源,不可降级,连不上直接退出)
//   - Redis 弱依赖(旁路缓存,Ping 失败则降级为直连 MySQL,cache=nil)
//   - 不接 kafka(避免与 player.update 语义重复)
//
// 启动顺序(对齐 chat / trade):
//  1. 解析 -conf 路径,加载 yaml
//  2. conf.Defaults 填默认值
//  3. log.Setup → 全局 zap logger
//  4. MySQL client(强依赖)
//  5. Redis client + Ping(弱依赖,失败降级)
//  6. 装配 DataUsecase → DataService → gRPC/HTTP server
//  7. kratos.New(...).Run() 阻塞
package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-kratos/kratos/v2"
	kconfig "github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"

	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/mysqlx"
	"github.com/luyuancpp/pandora/pkg/redisx"

	"github.com/luyuancpp/pandora/services/data/data_service/internal/biz"
	"github.com/luyuancpp/pandora/services/data/data_service/internal/conf"
	"github.com/luyuancpp/pandora/services/data/data_service/internal/data"
	"github.com/luyuancpp/pandora/services/data/data_service/internal/server"
	"github.com/luyuancpp/pandora/services/data/data_service/internal/service"
)

const serviceName = "data_service"

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "etc/data_service-dev.yaml", "config file path")
}

func main() {
	flag.Parse()

	// 1. Logger
	logger := plog.Setup(serviceName)
	helper := plog.NewHelper(logger)
	helper.Infow("msg", "service_starting", "conf", flagConf)

	// 2. 加载 yaml
	cfgPath, err := filepath.Abs(flagConf)
	if err != nil {
		helper.Errorw("msg", "abs_conf_path_failed", "err", err)
		os.Exit(1)
	}
	c := kconfig.New(kconfig.WithSource(file.NewSource(cfgPath)))
	defer func() { _ = c.Close() }()

	if err := c.Load(); err != nil {
		helper.Errorw("msg", "config_load_failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	var cfg conf.Config
	if err := c.Scan(&cfg); err != nil {
		helper.Errorw("msg", "config_scan_failed", "err", err)
		os.Exit(1)
	}
	cfg.Defaults()

	// 3. MySQL(强依赖:玩家数据事实源,不可降级)
	if cfg.Node.MySQLClient.DSN == "" {
		helper.Errorw("msg", "mysql_dsn_required", "hint", "node.mysql_client.dsn required (pandora_player)")
		os.Exit(1)
	}
	db := mysqlx.MustNewClient(cfg.Node.MySQLClient)
	defer func() { _ = db.Close() }()
	helper.Infow("msg", "mysql_connected", "dsn", maskDSN(cfg.Node.MySQLClient.DSN))

	// 4. Redis(弱依赖:旁路缓存,Ping 失败则降级为直连 MySQL)
	// 单实例填 host,Redis Cluster / Sentinel 只填 addrs,两者皆空才算未配置。
	var cache data.PlayerCache
	if rc := cfg.Node.RedisClient; rc.Host != "" || len(rc.Addrs) > 0 {
		rdb := redisx.NewUniversalClient(rc)
		pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if perr := rdb.Ping(pingCtx).Err(); perr != nil {
			cancel()
			_ = rdb.Close()
			helper.Warnw("msg", "redis_ping_failed", "err", perr, "addr", rc.Host, "addrs", rc.Addrs,
				"hint", "degrade to direct MySQL (no cache)")
		} else {
			cancel()
			defer func() { _ = rdb.Close() }()
			cache = data.NewRedisPlayerCache(rdb)
			helper.Infow("msg", "redis_connected", "addr", rc.Host, "addrs", rc.Addrs, "cache_ttl", cfg.Data.CacheTTL.String())
		}
	} else {
		helper.Warnw("msg", "redis_endpoint_empty", "hint", "cache disabled (direct MySQL)")
	}

	// 5. 装配链
	store := data.NewMySQLPlayerStore(db)
	uc := biz.NewDataUsecase(store, cache, cfg.Data, logger)
	svc := service.NewDataService(uc)

	grpcSrv := server.NewGRPCServer(&cfg, svc)
	httpSrv := server.NewHTTPServer(&cfg)

	helper.Infow(
		"msg", "service_ready",
		"grpc", cfg.Server.Grpc.Addr,
		"http", cfg.Server.Http.Addr,
		"mysql", maskDSN(cfg.Node.MySQLClient.DSN),
		"cache_enabled", cache != nil,
	)

	// 6. Kratos App
	app := kratos.New(
		kratos.Name(serviceName),
		kratos.Logger(logger),
		kratos.Server(grpcSrv, httpSrv),
	)
	if err := app.Run(); err != nil {
		helper.Errorw("msg", "app_run_failed", "err", err)
		os.Exit(1)
	}
}

// maskDSN 脱敏 DSN 里的密码,只保留 user@host/db 形态用于日志。
// 形如 user:pass@tcp(host:port)/db → user:***@tcp(host:port)/db。
func maskDSN(dsn string) string {
	at := strings.Index(dsn, "@")
	if at < 0 {
		return dsn
	}
	colon := strings.Index(dsn, ":")
	if colon < 0 || colon > at {
		return dsn
	}
	return dsn[:colon+1] + "***" + dsn[at:]
}
