// Pandora push 服务入口。
//
// 启动顺序(对齐 login,见 services/account/login/cmd/login/main.go):
//  1. 解析 -conf 路径,加载 yaml(Kratos config + file source)
//  2. 填默认值(conf.Defaults)
//  3. log.Setup → 全局 zap logger
//  4. ConnectionManager + PushUsecase + PushService 三层构造
//  5. gRPC + HTTP server 注册(HTTP 仅 /metrics)
//  6. kratos.New(...).Run() 阻塞
//
// 信号处理:Kratos App 默认监听 SIGINT/SIGTERM,优雅 stop server。
// 优雅 stop 时,所有在线 Subscribe stream 的 ctx 会被 cancel,RunMockStream 自然退出。
package main

import (
	"flag"
	"os"
	"path/filepath"

	"github.com/go-kratos/kratos/v2"
	kconfig "github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"

	plog "github.com/luyuancpp/pandora/pkg/log"

	"github.com/luyuancpp/pandora/services/runtime/push/internal/biz"
	"github.com/luyuancpp/pandora/services/runtime/push/internal/conf"
	"github.com/luyuancpp/pandora/services/runtime/push/internal/server"
	"github.com/luyuancpp/pandora/services/runtime/push/internal/service"
)

const serviceName = "push"

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "etc/push-dev.yaml", "config file path")
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

	// 3. 三层装配
	conns := biz.NewConnectionManager()
	uc := biz.NewPushUsecase(conns, cfg.Push.MockTickInterval.Std(), cfg.Push.MockTopic, cfg.Push.MockPayload)
	svc := service.NewPushService(uc)

	// 4. gRPC + HTTP server
	grpcSrv := server.NewGRPCServer(&cfg, svc)
	httpSrv := server.NewHTTPServer(&cfg)

	helper.Infow(
		"msg", "service_ready",
		"grpc", cfg.Server.Grpc.Addr,
		"http", cfg.Server.Http.Addr,
		"mock_tick", cfg.Push.MockTickInterval.String(),
		"mock_topic", cfg.Push.MockTopic,
	)

	// 5. Kratos App
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
