// Pandora ds_allocator 服务入口(W4 ②,2026-06-06)。
//
// 职责:战斗 DS 调度。matchmaker 全员确认后调 AllocateBattle 申请 DS,
// 战斗 DS 每 5s 调 Heartbeat 续命,心跳超时由后台扫描标记 abandoned。
//
// 启动顺序:
//  1. 解析 -conf 路径,加载 yaml
//  2. conf.Defaults 填默认值
//  3. log.Setup → 全局 zap logger
//  4. Redis client 连通性 Ping(强依赖:DS 状态镜像)
//  5. 装配链:RedisBattleRepo → MockGameServerAllocator → AllocatorUsecase → AllocatorService → gRPC/HTTP server
//  6. 后台 RunHeartbeatSweep(心跳超时扫描)
//  7. kratos.New(...).Run() 阻塞
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
	"github.com/redis/go-redis/v9"

	plog "github.com/luyuancpp/pandora/pkg/log"

	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/biz"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/data"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/server"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/service"
)

const serviceName = "ds_allocator"

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "etc/ds_allocator-dev.yaml", "config file path")
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
	rdb := redis.NewClient(&redis.Options{
		Addr:         rc.Host,
		Password:     rc.Password,
		DB:           int(rc.DB),
		DialTimeout:  rc.DialTimeout.Std(),
		ReadTimeout:  rc.ReadTimeout.Std(),
		WriteTimeout: rc.WriteTimeout.Std(),
	})
	defer func() { _ = rdb.Close() }()

	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		cancel()
		helper.Errorw("msg", "redis_ping_failed", "err", err, "addr", rc.Host)
		os.Exit(1)
	}
	cancel()
	helper.Infow("msg", "redis_connected", "addr", rc.Host)

	// 4. 装配链
	repo := data.NewRedisBattleRepo(rdb)
	allocator := biz.NewMockGameServerAllocator(cfg.Allocator) // W4 ② 打桩;W4 ③ 接 Agones
	uc := biz.NewAllocatorUsecase(repo, allocator, cfg.Allocator)
	svc := service.NewAllocatorService(uc)

	// 5. gRPC + HTTP
	grpcSrv := server.NewGRPCServer(&cfg, svc)
	httpSrv := server.NewHTTPServer(&cfg)

	// 6. 后台心跳超时扫描(随进程生命周期启停)
	sweepCtx, sweepCancel := context.WithCancel(context.Background())
	defer sweepCancel()
	go uc.RunHeartbeatSweep(sweepCtx)

	helper.Infow(
		"msg", "service_ready",
		"grpc", cfg.Server.Grpc.Addr,
		"http", cfg.Server.Http.Addr,
		"redis_addr", rc.Host,
		"heartbeat_timeout", cfg.Allocator.HeartbeatTimeout.String(),
		"sweep_interval", cfg.Allocator.SweepInterval.String(),
		"mock_ds_host", cfg.Allocator.MockDSAddrHost,
	)

	// 7. Kratos App
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
