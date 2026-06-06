// Pandora matchmaker 服务入口(W4 ①,2026-06-06)。
//
// 启动顺序:
//  1. 解析 -conf 路径,加载 yaml
//  2. conf.Defaults 填默认值
//  3. log.Setup → 全局 zap logger
//  4. Redis client 连通性 Ping(强依赖)
//  5. Snowflake Node(zone_id 来自 yaml)
//  6. team gRPC reader(team_addr 留空则跳过 team 校验)
//  7. kafkax.KeyOrderedProducer(topic=pandora.match.progress) → matchPusher
//  8. 装配链:RedisMatchRepo → MatchUsecase → MatchService → gRPC/HTTP server
//  9. 后台 RunMatchLoop(撮合 + 确认期超时扫描)
//  10. kratos.New(...).Run() 阻塞
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

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/kafkax"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/snowflake"

	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/biz"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/conf"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/data"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/server"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/service"
)

const serviceName = "matchmaker"

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "etc/matchmaker-dev.yaml", "config file path")
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

	// 4. Snowflake
	sf := snowflake.NewNode(uint64(cfg.Node.ZoneId))

	// 5. team gRPC reader(弱依赖:team_addr 留空 → 跳过队伍校验)
	var reader biz.TeamReader
	if cfg.Match.TeamAddr != "" {
		tr := data.NewGrpcTeamReader(cfg.Match.TeamAddr)
		defer func() { _ = tr.Close() }()
		reader = tr
		helper.Infow("msg", "team_reader_ready", "team_addr", cfg.Match.TeamAddr)
	} else {
		helper.Warnw("msg", "team_addr_empty", "hint", "StartMatch will skip team validation")
	}

	// 6. Kafka producer → matchPusher(弱依赖:broker 不通则 warn,push 静默 fail)
	var pusher biz.MatchEventPusher
	if len(cfg.Kafka.Brokers) > 0 {
		producer, err := kafkax.NewKeyOrderedProducer(cfg.Kafka, kafkax.TopicMatchProgress)
		if err != nil {
			helper.Warnw("msg", "kafka_producer_init_failed", "err", err,
				"hint", "match progress push will be silently dropped until kafka is available")
		} else {
			defer func() { _ = producer.Close() }()
			pusher = &kafkaPusher{p: producer}
			helper.Infow("msg", "kafka_producer_ready", "topic", kafkax.TopicMatchProgress)
		}
	} else {
		helper.Warnw("msg", "kafka_brokers_empty", "hint", "match progress push disabled")
	}

	// 7. 装配链
	repo := data.NewRedisMatchRepo(rdb)

	// DSAllocator:ds_allocator_addr 非空 → 真 gRPC 拉 DS + 签 battle 票据;否则 W4 ① 打桩
	var allocator biz.DSAllocator
	if cfg.Match.DSAllocatorAddr != "" {
		authCfg := auth.Config{
			Issuer:      cfg.JWT.Issuer,
			Audience:    cfg.JWT.Audience,
			Secret:      []byte(cfg.JWT.Secret),
			SessionTTL:  cfg.JWT.SessionTTL.Std(),
			DSTicketTTL: cfg.JWT.DSTicketTTL.Std(),
		}
		signer, serr := auth.NewSigner(authCfg)
		if serr != nil {
			helper.Errorw("msg", "ds_ticket_signer_init_failed", "err", serr,
				"hint", "jwt.secret must be >=32 bytes and match login/envoy")
			os.Exit(1)
		}
		ga := data.NewGrpcDSAllocator(cfg.Match.DSAllocatorAddr, signer, cfg.Match.MapId, cfg.Match.GameMode)
		defer func() { _ = ga.Close() }()
		allocator = ga
		helper.Infow("msg", "ds_allocator_grpc_ready", "ds_allocator_addr", cfg.Match.DSAllocatorAddr,
			"map_id", cfg.Match.MapId, "game_mode", cfg.Match.GameMode)
	} else {
		allocator = biz.NewStubDSAllocator("") // W4 ① 打桩;无 ds_allocator_addr 时兜底
		helper.Warnw("msg", "ds_allocator_addr_empty", "hint", "using StubDSAllocator (mock ds_addr + mock tickets)")
	}

	uc := biz.NewMatchUsecase(repo, reader, pusher, allocator, sf, cfg.Match)
	svc := service.NewMatchService(uc, sf)

	// 8. gRPC + HTTP
	grpcSrv := server.NewGRPCServer(&cfg, svc)
	httpSrv := server.NewHTTPServer(&cfg)

	// 9. 后台撮合循环(随进程生命周期启停)
	loopCtx, loopCancel := context.WithCancel(context.Background())
	defer loopCancel()
	go uc.RunMatchLoop(loopCtx)

	helper.Infow(
		"msg", "service_ready",
		"grpc", cfg.Server.Grpc.Addr,
		"http", cfg.Server.Http.Addr,
		"redis_addr", rc.Host,
		"team_addr", cfg.Match.TeamAddr,
		"confirm_timeout", cfg.Match.ConfirmTimeout.String(),
		"match_interval", cfg.Match.MatchInterval.String(),
		"team_size", cfg.Match.TeamSize,
	)

	// 10. Kratos App
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

// kafkaPusher 把 biz.MatchEventPusher 接口适配到 kafkax.KeyOrderedProducer。
type kafkaPusher struct {
	p *kafkax.KeyOrderedProducer
}

func (k *kafkaPusher) PushMatchProgress(ctx context.Context, callerPlayerID uint64, toPlayerIDs []uint64, payload []byte) (int, error) {
	return k.p.PushToPlayers(ctx, callerPlayerID, toPlayerIDs, payload)
}
