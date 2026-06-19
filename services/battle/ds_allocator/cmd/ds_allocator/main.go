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
//  5. 装配链:RedisBattleRepo → (Agones 或 Mock) GameServerAllocator → AllocatorUsecase → AllocatorService → gRPC/HTTP server
//  6. 后台 RunHeartbeatSweep(心跳超时扫描)
//  7. kratos.New(...).Run() 阻塞
package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-kratos/kratos/v2"
	kconfig "github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/kafkax"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/redisx"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"

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
	rdb := redisx.NewUniversalClient(rc)
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
	// DS 启动方式由 cfg.Mode 单一开关决定(标准两模式 + 离线兜底),biz 逻辑零改:
	//   - mode=agones → 真 GameServerAllocation(Linux 生产)
	//   - mode=local  → 本机拉起 Windows DS 进程(Windows 单机自测)
	//   - mode=mock   → Mock 确定性假地址(无真实 DS,离线联调)
	var allocator biz.GameServerAllocator
	switch cfg.Mode {
	case conf.ModeAgones:
		ag, aerr := data.NewAgonesGameServerAllocator(cfg.Agones)
		if aerr != nil {
			helper.Errorw("msg", "agones_allocator_init_failed", "err", aerr,
				"hint", "检查 agones.fleet_name / ca_path 配置")
			os.Exit(1)
		}
		allocator = ag
		helper.Infow("msg", "agones_allocator_ready",
			"api_server", cfg.Agones.APIServer, "namespace", cfg.Agones.Namespace, "fleet", cfg.Agones.FleetName)
	case conf.ModeLocal:
		ld, lerr := data.NewLocalGameServerAllocator(cfg.LocalDS)
		if lerr != nil {
			helper.Errorw("msg", "local_ds_allocator_init_failed", "err", lerr,
				"hint", "mode=local 需 local_ds.executable_path 指向打包好的 UE Windows DS 可执行文件")
			os.Exit(1)
		}
		// 进程退出时杀掉全部在管 DS,避免遗留孤儿。
		defer func() { _ = ld.Close() }()
		allocator = ld
		helper.Infow("msg", "local_ds_allocator_ready",
			"executable", cfg.LocalDS.ExecutablePath, "map", cfg.LocalDS.MapName,
			"advertise_host", cfg.LocalDS.AdvertiseHost,
			"port_base", cfg.LocalDS.PortBase, "port_range", cfg.LocalDS.PortRange)
	default:
		allocator = biz.NewMockGameServerAllocator(cfg.Allocator)
		helper.Warnw("msg", "mock_allocator_active",
			"mode", cfg.Mode, "hint", "mode=mock,用确定性假地址(无真实 DS)")
	}
	uc := biz.NewAllocatorUsecase(repo, allocator, cfg.Allocator)

	// 4.1 ds.lifecycle producer(弱依赖:心跳超时 abandoned → 通知 battle_result 段位回滚补偿,不变量 §4)
	if len(cfg.Kafka.Brokers) > 0 {
		producer, perr := kafkax.NewKeyOrderedProducer(cfg.Kafka, kafkax.TopicDSLifecycle)
		if perr != nil {
			helper.Warnw("msg", "ds_lifecycle_producer_init_failed", "err", perr,
				"hint", "abandoned 事件将不发送,abandoned 镜像仍落 Redis 供查")
		} else {
			defer func() { _ = producer.Close() }()
			uc.SetLifecyclePusher(&dsLifecyclePusher{p: producer})
			helper.Infow("msg", "ds_lifecycle_producer_ready", "topic", kafkax.TopicDSLifecycle)
		}
	} else {
		helper.Warnw("msg", "kafka_brokers_empty", "hint", "ds.lifecycle abandoned 事件禁用")
	}

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
		"allocator_mode", cfg.Mode,
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

// dsLifecyclePusher 把 biz.DSLifecyclePusher 适配到 kafkax.KeyOrderedProducer。
// key=match_id(不变量 §9 同对局事件保序)。
type dsLifecyclePusher struct {
	p *kafkax.KeyOrderedProducer
}

func (d *dsLifecyclePusher) PublishLifecycle(ctx context.Context, evt *dsv1.DSLifecycleEvent) error {
	payload, err := proto.Marshal(evt)
	if err != nil {
		return err
	}
	return d.p.SendRaw(ctx, strconv.FormatUint(evt.GetMatchId(), 10), payload)
}
