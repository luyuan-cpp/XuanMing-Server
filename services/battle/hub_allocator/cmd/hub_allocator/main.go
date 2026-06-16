// Pandora hub_allocator 服务入口(W4 ⑤,2026-06-06)。
//
// 职责:大厅 DS 分片调度。login 登录成功后调 AssignHub 给玩家分一个 hub DS 分片并签 hub 票据;
// Hub DS 每 5s 调 Heartbeat 续命,心跳超时由后台扫描标记 draining 停止分配。
//
// 启动顺序:
//  1. 解析 -conf 路径,加载 yaml
//  2. conf.Defaults 填默认值
//  3. log.Setup → 全局 zap logger
//  4. Redis client 连通性 Ping(强依赖:分片镜像 + 玩家归属)
//  5. pkg/auth.Signer 构造(强依赖:AssignHub 必须签 hub DSTicket)
//  6. 装配链:RedisHubRepo → MockHubFleetProvider → HubUsecase → HubService → gRPC/HTTP server
//  7. 后台 RunHeartbeatSweep(心跳超时扫描)
//  8. kratos.New(...).Run() 阻塞
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
	"github.com/google/uuid"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/kafkax"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/redisx"

	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/biz"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/data"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/server"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/service"
)

const serviceName = "hub_allocator"

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "etc/hub_allocator-dev.yaml", "config file path")
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
	helper.Infow("msg", "redis_connected", "addr", rc.Host)

	// 4. JWT Signer(强依赖:AssignHub / TransferHub 必须签 hub DSTicket;secret 须与 login/envoy 一致)
	signer, serr := auth.NewSigner(auth.Config{
		Issuer:      cfg.JWT.Issuer,
		Audience:    cfg.JWT.Audience,
		Secret:      []byte(cfg.JWT.Secret),
		SessionTTL:  cfg.JWT.SessionTTL.Std(),
		DSTicketTTL: cfg.JWT.DSTicketTTL.Std(),
	})
	if serr != nil {
		helper.Errorw("msg", "hub_ticket_signer_init_failed", "err", serr,
			"hint", "jwt.secret must be >=32 bytes and match login/envoy")
		os.Exit(1)
	}
	helper.Infow("msg", "hub_ticket_signer_ready", "ds_ticket_ttl", cfg.JWT.DSTicketTTL.String())

	// 5. 装配链
	repo := data.NewRedisHubRepo(rdb)
	// W4 ⑬:agones.enabled=true → 真 GameServer 列表发现分片拓扑;否则回退 Mock(本地/无集群联调)。
	var fleet biz.HubFleetProvider
	if cfg.Agones.Enabled {
		af, ferr := biz.NewAgonesHubFleetProvider(cfg)
		if ferr != nil {
			helper.Errorw("msg", "agones_fleet_provider_init_failed", "err", ferr,
				"hint", "检查 agones.fleet_name / ca_path 配置")
			os.Exit(1)
		}
		fleet = af
		helper.Infow("msg", "agones_fleet_provider_ready",
			"api_server", cfg.Agones.APIServer, "namespace", cfg.Agones.Namespace, "fleet", cfg.Agones.FleetName)
	} else {
		fleet = biz.NewMockHubFleetProvider(cfg.Hub)
		helper.Warnw("msg", "mock_fleet_provider_active",
			"hint", "agones.enabled=false,用确定性假分片(无真实 Hub DS)")
		// Mock 是拓扑-only 不实现 HubFleetScaler:autoscale/consolidation 在此模式下不会运行。
		// 明确告警避免“yaml 开了但实际没生效”的误导。
		if cfg.Hub.AutoScaleEnabled || cfg.Hub.ConsolidationEnabled {
			helper.Warnw("msg", "autoscale_inert_under_mock",
				"autoscale_enabled", cfg.Hub.AutoScaleEnabled,
				"consolidation_enabled", cfg.Hub.ConsolidationEnabled,
				"hint", "Mock 无真实 Fleet scaler:自动扩缩容/强制整合不会运行,需 agones.enabled=true")
		}
	}
	uc := biz.NewHubUsecase(repo, fleet, &hubTicketSigner{signer: signer}, cfg.Hub)

	// 5.1 Kafka producer → migratePusher(弱依赖:broker 不通则 warn 并继续,迁移推送静默丢弃,
	// Hub DS drain 心跳指令仍兜底让客户端重连到新分片)。强制整合 consolidation 才需要。
	if len(cfg.Kafka.Brokers) > 0 {
		producer, perr := kafkax.NewKeyOrderedProducer(cfg.Kafka, kafkax.TopicHubMigrate)
		if perr != nil {
			helper.Warnw("msg", "kafka_producer_init_failed", "err", perr,
				"hint", "hub migrate push will be silently dropped until kafka is available")
		} else {
			defer func() { _ = producer.Close() }()
			uc.SetMigratePusher(&kafkaMigratePusher{p: producer})
			helper.Infow("msg", "kafka_producer_ready", "topic", kafkax.TopicHubMigrate)
		}
	} else if cfg.Hub.ConsolidationEnabled {
		helper.Warnw("msg", "kafka_brokers_empty",
			"hint", "consolidation_enabled 但无 kafka:迁移仅靠 Hub DS drain 心跳兜底,无无缝倒计时推送")
	}

	svc := service.NewHubService(uc)

	// 6. gRPC + HTTP
	grpcSrv := server.NewGRPCServer(&cfg, svc)
	httpSrv := server.NewHTTPServer(&cfg)

	// 7. 后台心跳超时扫描(随进程生命周期启停)
	sweepCtx, sweepCancel := context.WithCancel(context.Background())
	defer sweepCancel()
	go uc.RunHeartbeatSweep(sweepCtx)

	helper.Infow(
		"msg", "service_ready",
		"grpc", cfg.Server.Grpc.Addr,
		"http", cfg.Server.Http.Addr,
		"redis_addr", rc.Host,
		"heartbeat_timeout", cfg.Hub.HeartbeatTimeout.String(),
		"sweep_interval", cfg.Hub.SweepInterval.String(),
		"default_region", cfg.Hub.DefaultRegion,
		"mock_shard_count", cfg.Hub.MockShardCount,
		"fleet_mode", fleetMode(cfg.Agones.Enabled),
		"autoscale_enabled", cfg.Hub.AutoScaleEnabled,
		"consolidation_enabled", cfg.Hub.ConsolidationEnabled,
	)

	// 8. Kratos App
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

// hubTicketSigner 把 biz.TicketSigner 适配到 pkg/auth.Signer。
// hub DSTicket:ds_type=hub,match_id=0(不变量 §3 短时效 5min;jti=uuid v4 防重放)。
type hubTicketSigner struct {
	signer *auth.Signer
}

func (h *hubTicketSigner) SignHubTicket(playerID uint64) (string, int64, error) {
	return h.signer.SignDSTicket(playerID, auth.DSTypeHub, 0, uuid.NewString())
}

// kafkaMigratePusher 把 biz.HubMigratePusher 适配到 kafkax.KeyOrderedProducer。
// 强制整合时把 HubMigrateEvent payload 按 player_id(kafka key)推给被迁移玩家本人。
type kafkaMigratePusher struct {
	p *kafkax.KeyOrderedProducer
}

func (k *kafkaMigratePusher) PushMigrate(ctx context.Context, playerID uint64, payload []byte) error {
	_, err := k.p.PushToPlayers(ctx, 0, []uint64{playerID}, payload)
	return err
}

// fleetMode 返回 service_ready 日志里的分片发现模式字符串。
func fleetMode(agonesEnabled bool) string {
	if agonesEnabled {
		return "agones"
	}
	return "mock"
}
