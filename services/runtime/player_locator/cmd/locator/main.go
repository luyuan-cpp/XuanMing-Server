// Pandora player_locator 服务入口(W3 ⑤,2026-06-05)。
//
// 启动顺序(对齐 push,见 services/runtime/push/cmd/push/main.go):
//  1. 解析 -conf 路径,加载 yaml(Kratos config + file source)
//  2. 填默认值(conf.Defaults)
//  3. log.Setup → 全局 zap logger
//  4. Redis client + LocationRepo + LocatorUsecase + LocatorService 装配
//  5. gRPC + HTTP server 注册
//  6. kratos.New(...).Run() 阻塞
//
// Redis 强依赖:启动期 Ping 失败直接 exit(本服务是 "玩家在哪" 唯一真源)。
package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"time"

	"github.com/go-kratos/kratos/v2"
	kconfig "github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"

	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/redisx"

	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/biz"
	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/conf"
	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/data"
	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/server"
	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/service"
)

const serviceName = "player_locator"

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "etc/locator-dev.yaml", "config file path")
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

	// 3. Redis(强依赖)
	rc := cfg.Node.RedisClient
	if rc.Host == "" {
		helper.Errorw("msg", "redis_host_required")
		os.Exit(1)
	}
	rdb := redisx.NewClient(rc)
	defer func() { _ = rdb.Close() }()

	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		cancel()
		helper.Errorw("msg", "redis_ping_failed", "err", err, "addr", rc.Host)
		os.Exit(1)
	}
	cancel()

	// 4. 三层装配
	repo := data.NewRedisLocationRepo(rdb)
	uc := biz.NewLocatorUsecase(repo, cfg.Locator.LocationTTL.Std())
	svc := service.NewLocatorService(uc)

	// 5. gRPC + HTTP
	grpcSrv := server.NewGRPCServer(&cfg, svc)
	httpSrv := server.NewHTTPServer(&cfg)

	helper.Infow(
		"msg", "service_ready",
		"grpc", cfg.Server.Grpc.Addr,
		"http", cfg.Server.Http.Addr,
		"redis_addr", rc.Host,
		"location_ttl", cfg.Locator.LocationTTL.String(),
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
