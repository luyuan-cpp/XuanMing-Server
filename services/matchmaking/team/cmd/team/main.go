// Pandora team 服务入口(W3 ⑦ Phase 4,2026-06-05)。
//
// 启动顺序:
//  1. 解析 -conf 路径,加载 yaml
//  2. conf.Defaults 填默认值
//  3. log.Setup → 全局 zap logger
//  4. Redis client 连通性 Ping(强依赖)
//  5. Snowflake Node(zone_id 来自 yaml)
//  6. kafkax.KeyOrderedProducer(topic=pandora.team.update) → kafkaPusher
//  7. 装配链:RedisTeamRepo → TeamUsecase → TeamService → gRPC/HTTP server
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

	plog "github.com/luyuancpp/pandora/pkg/log"

	"github.com/luyuancpp/pandora/pkg/kafkax"
	"github.com/luyuancpp/pandora/pkg/redisx"
	"github.com/luyuancpp/pandora/pkg/snowflake"
	"github.com/luyuancpp/pandora/services/matchmaking/team/internal/biz"
	"github.com/luyuancpp/pandora/services/matchmaking/team/internal/conf"
	"github.com/luyuancpp/pandora/services/matchmaking/team/internal/data"
	"github.com/luyuancpp/pandora/services/matchmaking/team/internal/server"
	"github.com/luyuancpp/pandora/services/matchmaking/team/internal/service"
)

const serviceName = "team"

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "etc/team-dev.yaml", "config file path")
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

	// 4. Snowflake
	sf := snowflake.NewNode(uint64(cfg.Node.ZoneId))

	// 5. Kafka producer → kafkaPusher(弱依赖:broker 不通则 warn 并继续,push 会静默 fail)
	var pusher biz.TeamEventPusher
	if len(cfg.Kafka.Brokers) > 0 {
		producer, err := kafkax.NewKeyOrderedProducer(cfg.Kafka, kafkax.TopicTeamUpdate)
		if err != nil {
			helper.Warnw("msg", "kafka_producer_init_failed", "err", err,
				"hint", "team push will be silently dropped until kafka is available")
		} else {
			defer func() { _ = producer.Close() }()
			pusher = &kafkaPusher{p: producer}
			helper.Infow("msg", "kafka_producer_ready", "topic", kafkax.TopicTeamUpdate)
		}
	} else {
		helper.Warnw("msg", "kafka_brokers_empty", "hint", "team push disabled")
	}

	// 6. 装配链
	repo := data.NewRedisTeamRepo(rdb)
	uc := biz.NewTeamUsecase(repo, pusher, cfg.Team)
	svc := service.NewTeamService(uc, sf)

	// 7. gRPC + HTTP
	grpcSrv := server.NewGRPCServer(&cfg, svc)
	httpSrv := server.NewHTTPServer(&cfg)

	helper.Infow(
		"msg", "service_ready",
		"grpc", cfg.Server.Grpc.Addr,
		"http", cfg.Server.Http.Addr,
		"redis_addr", rc.Host,
		"invite_ttl", cfg.Team.InviteTTL.String(),
		"max_members", cfg.Team.MaxMembers,
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

// kafkaPusher 把 biz.TeamEventPusher 接口适配到 kafkax.KeyOrderedProducer。
type kafkaPusher struct {
	p *kafkax.KeyOrderedProducer
}

func (k *kafkaPusher) PushTeamUpdate(ctx context.Context, callerPlayerID uint64, toPlayerIDs []uint64, payload []byte) (int, error) {
	return k.p.PushToPlayers(ctx, callerPlayerID, toPlayerIDs, payload)
}

// kratosHelper 是 *klog.Helper 的简化接口。
type kratosHelper interface {
	Infow(keyvals ...any)
	Warnw(keyvals ...any)
	Errorw(keyvals ...any)
}
