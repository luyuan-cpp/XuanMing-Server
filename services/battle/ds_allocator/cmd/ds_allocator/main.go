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

	"github.com/luyuancpp/pandora/pkg/grpcclient"
	"github.com/luyuancpp/pandora/pkg/kafkax"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/redisx"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"

	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/biz"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/data"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/gm"
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
	// 单实例填 host,Redis Cluster / Sentinel 只填 addrs,两者皆空才算未配置。
	rc := cfg.Node.RedisClient
	if rc.Host == "" && len(rc.Addrs) == 0 {
		helper.Errorw("msg", "redis_endpoint_required",
			"hint", "set node.redis_client.host (single) or node.redis_client.addrs (cluster)")
		os.Exit(1)
	}
	rdb := redisx.NewUniversalClient(rc)
	defer func() { _ = rdb.Close() }()

	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		cancel()
		helper.Errorw("msg", "redis_ping_failed", "err", err, "addr", rc.Host, "addrs", rc.Addrs)
		os.Exit(1)
	}
	cancel()
	helper.Infow("msg", "redis_connected", "addr", rc.Host, "addrs", rc.Addrs)

	// 4. 装配链
	repo := data.NewRedisBattleRepo(rdb)
	// DS 启动方式由 cfg.Mode 单一开关决定(标准两模式 + 离线兜底),biz 逻辑零改:
	//   - mode=agones → 真 GameServerAllocation(Linux 生产)
	//   - mode=local  → 本机拉起 Windows DS 进程(Windows 单机自测)
	//   - mode=mock   → Mock 确定性假地址(无真实 DS,离线联调)
	var allocator biz.GameServerAllocator
	var agonesAlloc *data.AgonesGameServerAllocator // 仅 mode=agones 非空,供 Fleet 容量巡检
	switch cfg.Mode {
	case conf.ModeAgones:
		ag, aerr := data.NewAgonesGameServerAllocator(cfg.Agones)
		if aerr != nil {
			helper.Errorw("msg", "agones_allocator_init_failed", "err", aerr,
				"hint", "检查 agones.fleet_name / ca_path 配置")
			os.Exit(1)
		}
		allocator = ag
		agonesAlloc = ag
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
	if cfg.Mode == conf.ModeLocal {
		// local 模式 UE DS 无 Agones,收到 stop 指令不会自杀 → 让后端在 orphan/pod_mismatch/终态
		// 心跳时主动 kill 该 DS,防幽灵进程占端口污染下一局(配合端口 bind 探测双保险)。
		uc.SetKillOrphanOnStop(true)
	}

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

	// 4.2 player_locator 客户端(弱依赖:心跳时续期玩家 BATTLE 位置 TTL,支持断线重连直连回原
	// battle DS,docs/design/battle-reconnect.md §2.2)。locator_addr 留空 → 不续期,不影响对局。
	if cfg.LocatorAddr != "" {
		conn := grpcclient.MustDialInsecure(cfg.LocatorAddr)
		defer func() { _ = conn.Close() }()
		uc.SetLocationRefresher(data.NewGrpcLocationRefresher(conn))
		helper.Infow("msg", "locator_client_ready", "locator_addr", cfg.LocatorAddr)
	} else {
		helper.Warnw("msg", "locator_addr_empty",
			"hint", "BATTLE 位置不续期,长对局中途掉线重登可能退化为回大厅(仍可回大厅,不阻断)")
	}

	svc := service.NewAllocatorService(uc)

	// GmService(GM / 运维指令下发):与 ds_allocator 同进程复用 gRPC 端口。
	// 运维 GM 工具 SendCommand 入 Redis 队列 → 战斗 DS 轮询 PollCommands 拉取执行(如给玩家发道具)。
	// 内部接口,不经 Envoy 暴露给玩家客户端。
	gmSvc := gm.NewService(rdb, logger)
	// SendCommand 前置校验目标对局是否有活跃战斗镜像:typo / 已结束的 match_id 立即拒,
	// 避免静默入僵尸队列(repo 天然满足 BattleLivenessChecker,复用同一 Redis)。
	gmSvc.SetBattleChecker(repo)

	// 5. gRPC + HTTP
	grpcSrv := server.NewGRPCServer(&cfg, svc, gmSvc)
	httpSrv := server.NewHTTPServer(&cfg)

	// 6. 后台心跳超时扫描(随进程生命周期启停)
	sweepCtx, sweepCancel := context.WithCancel(context.Background())
	defer sweepCancel()
	go uc.RunHeartbeatSweep(sweepCtx)

	// 6.1 Fleet 容量巡检(仅 agones 模式):定期 GET Fleet status → 暴露
	// pandora_ds_allocator_fleet_* 指标,容量快到上限时打预警日志
	// (ds_fleet_capacity_near_limit / ds_fleet_capacity_exhausted),让运维在打满前扩 Fleet。
	// capacity_watch_interval 设负值可禁用(NewCapacityWatcher 返 nil)。
	if agonesAlloc != nil {
		if watcher := biz.NewCapacityWatcher(agonesAlloc, cfg.Agones); watcher != nil {
			go watcher.Run(sweepCtx)
			helper.Infow("msg", "fleet_capacity_watch_enabled",
				"interval", cfg.Agones.CapacityWatchInterval.String(),
				"warn_ratio", cfg.Agones.CapacityWarnRatio,
				"fleets", agonesAlloc.WatchedFleets())
		}
	}

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
