// Pandora login 服务入口。
//
// 启动顺序:
//  1. 解析 -conf 路径,加载 yaml(Kratos config + file source)
//  2. 填默认值(conf.Defaults)
//  3. log.Setup → 全局 zap logger
//  4. data layer + biz usecase + service 三层构造
//  5. gRPC + HTTP server 注册
//  6. kratos.New(...).Run() 阻塞
//
// 信号处理:Kratos App 默认监听 SIGINT/SIGTERM,优雅 stop server。
package main

import (
	"flag"
	"os"
	"path/filepath"

	"github.com/go-kratos/kratos/v2"
	kconfig "github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"

	plog "github.com/luyuancpp/pandora/pkg/log"

	"github.com/luyuancpp/pandora/services/account/login/internal/biz"
	"github.com/luyuancpp/pandora/services/account/login/internal/conf"
	"github.com/luyuancpp/pandora/services/account/login/internal/data"
	"github.com/luyuancpp/pandora/services/account/login/internal/server"
	"github.com/luyuancpp/pandora/services/account/login/internal/service"

	"github.com/luyuancpp/pandora/pkg/snowflake"
)

const serviceName = "login"

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "etc/login-dev.yaml", "config file path")
}

func main() {
	flag.Parse()

	// 1. Logger 先起(后面 panic 走 zap json 到 stdout,便于 docker logs 看)
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

	// 3. 公共依赖(snowflake,W2 不起 Redis/Locker — login mock 不需要)
	sf := snowflake.NewNode(uint64(cfg.Node.ZoneId))
	// MockAccountRepo 固定 player_id(用 snowflake 算一个,落 yaml mock 配置)
	mockPlayerID := int64(sf.Generate())

	// 4. 三层装配
	repo := data.NewMockAccountRepo(cfg.Login.MockAccount, cfg.Login.MockPasswordHash, mockPlayerID)
	uc := biz.NewLoginUsecase(repo, sf, cfg.Login.MockHubDSAddr)
	svc := service.NewLoginService(uc)

	// 5. gRPC + HTTP server
	grpcSrv := server.NewGRPCServer(&cfg, svc)
	httpSrv := server.NewHTTPServer(&cfg, svc)

	helper.Infow(
		"msg", "service_ready",
		"grpc", cfg.Server.Grpc.Addr,
		"http", cfg.Server.Http.Addr,
		"mock_player_id", mockPlayerID,
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
