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
	"strconv"
	"time"

	"github.com/go-kratos/kratos/v2"
	kconfig "github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"

	"github.com/luyuancpp/pandora/pkg/kafkax"
	"github.com/luyuancpp/pandora/pkg/killswitch"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/redisx"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"

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
	rdb := redisx.NewUniversalClient(rc)
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

	// 4.1 presence fan-out worker(§13.4 / §13.5):弱依赖,默认关闭(纯拉模式)。
	//     开启需 cfg.Kafka.Brokers(往 pandora.presence.update 生产 → push 投递)。
	var presenceHub *biz.PresenceHub
	if cfg.Presence.Enabled {
		if len(cfg.Kafka.Brokers) == 0 {
			helper.Warnw("msg", "presence_enabled_but_no_kafka", "hint", "set kafka.brokers; fan-out disabled, fallback pure-pull")
		} else {
			producer, perr := kafkax.NewKeyOrderedProducer(cfg.Kafka, kafkax.TopicPresenceUpdate)
			if perr != nil {
				helper.Warnw("msg", "presence_producer_init_failed", "err", perr, "hint", "fan-out disabled, fallback pure-pull")
			} else {
				defer func() { _ = producer.Close() }()
				ksKey := cfg.Presence.KillSwitchKey
				ks := func() (bool, string) { return killswitch.Disabled(ksKey) }
				presenceHub = biz.NewPresenceHub(
					&presencePusher{p: producer},
					cfg.Presence.DebounceWindow.Std(),
					cfg.Presence.CoalesceTick.Std(),
					ks,
				)
				presenceHub.Start()
				defer presenceHub.Close()
				helper.Infow("msg", "presence_fanout_enabled",
					"debounce", cfg.Presence.DebounceWindow.String(),
					"coalesce_tick", cfg.Presence.CoalesceTick.String(),
					"kill_switch_key", ksKey)
			}
		}
	}

	// presenceHub 为 nil 时,usecase 走纯拉(SubscribePresence no-op)。
	var presence biz.PresenceNotifier
	if presenceHub != nil {
		presence = presenceHub
	}
	uc := biz.NewLocatorUsecase(repo, cfg.Locator.LocationTTL.Std(), presence)
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

// presencePusher 把 biz.PresencePusher 接口适配到 kafkax.KeyOrderedProducer。
// kafka key = subscriber_id(不变量 §9:同订阅者事件保序;push 服务按 key 路由到该玩家 stream)。
// payload = PresenceBatchEvent proto bytes(push 服务直接透传给客户端解码)。
type presencePusher struct {
	p *kafkax.KeyOrderedProducer
}

func (a *presencePusher) PushPresence(ctx context.Context, subscriberID uint64, changes []biz.PresenceChangeOut) error {
	pbChanges := make([]*locatorv1.PresenceChange, 0, len(changes))
	for _, c := range changes {
		pbChanges = append(pbChanges, &locatorv1.PresenceChange{
			PlayerId: c.PlayerID,
			Status:   locatorv1.PresenceStatus(c.Status),
			TsMs:     c.TsMs,
		})
	}
	evt := &locatorv1.PresenceBatchEvent{Changes: pbChanges}
	return a.p.Send(ctx, strconv.FormatUint(subscriberID, 10), evt)
}
