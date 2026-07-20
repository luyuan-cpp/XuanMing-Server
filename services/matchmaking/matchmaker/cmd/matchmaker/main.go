// Pandora matchmaker 服务入口(W4 ①,2026-06-06)。
//
// 启动顺序:
//  1. 解析 -conf 路径,加载 yaml
//  2. conf.Defaults 填默认值
//  3. log.Setup → 全局 zap logger
//  4. Redis client 连通性 Ping(强依赖)
//  5. Snowflake Node(node_id 来自 yaml)
//  6. team gRPC reader(team_addr 留空则跳过 team 校验)
//  7. kafkax.KeyOrderedProducer(topic=pandora.match.progress) → matchPusher(brokers 配置时为启动强依赖)
//  8. 装配链:RedisMatchRepo → MatchUsecase → MatchService → gRPC/HTTP server
//  9. 后台 RunMatchLoop(撮合 + 确认期超时扫描)
//  10. kratos.New(...).Run() 阻塞
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-kratos/kratos/v2"
	kconfig "github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/cellroute/etcdtable"
	pconfig "github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/grpcclient"
	"github.com/luyuancpp/pandora/pkg/internalrpcauth"
	"github.com/luyuancpp/pandora/pkg/kafkax"
	"github.com/luyuancpp/pandora/pkg/leader/etcdleader"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/redisx"
	"github.com/luyuancpp/pandora/pkg/snowflake/etcdnode"

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
	if err := cfg.Validate(); err != nil {
		helper.Errorw("msg", "config_validation_failed", "err", err)
		os.Exit(1)
	}

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

	// 4. Snowflake(node_id_source=static 静态，=etcd 走 etcd 自动抢占，失租自动退出)
	sf, sfCloser := etcdnode.MustProvideSnowflake(serviceName, cfg.Node.NodeId, cfg.Snowflake)
	defer func() { _ = sfCloser.Close() }()

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

	// 6. Kafka producer → matchPusher。
	// kafka.brokers 非空表示启用 match 进度推送；此时 producer 是启动强依赖。组队匹配只有
	// 队长持有 StartMatch 返回的 match_id 可轮询 GetMatchProgress 兜底，其余成员得知成局 /
	// READY / Battle 落点的唯一通道就是 pandora.match.progress 推送；初始化失败必须在对外
	// Ready 前退出，让 Kubernetes 保留旧 Pod 并在 Kafka 恢复后重试新 Pod，不能再以
	// pusher=nil 受理匹配后把整场进度推送静默丢弃（非队长成员会一直停在 Hub）。
	var pusher biz.MatchEventPusher
	publication, perr := initializeMatchPublication(cfg.Kafka, func(kcfg pconfig.KafkaConfig, topic string) (rawMatchProducer, error) {
		return kafkax.NewKeyOrderedProducer(kcfg, topic)
	})
	if perr != nil {
		helper.Errorw("msg", "kafka_producer_required_but_unavailable", "err", perr,
			"hint", "matchmaker exits before Ready so the orchestrator can retry after Kafka recovers")
		os.Exit(1)
	}
	if publication.producer != nil {
		defer func() { _ = publication.producer.Close() }()
		pusher = publication.pusher
		helper.Infow("msg", "kafka_producer_ready", "topic", kafkax.TopicMatchProgress, "required", true)
	} else {
		helper.Warnw("msg", "kafka_producer_disabled_dev_only", "reason", publication.disabledReason,
			"hint", "match progress push disabled; only the captain can see READY via GetMatchProgress polling in this explicit no-Kafka development mode")
	}

	// 7. 装配链
	repo := data.NewRedisMatchRepo(rdb, cfg.Match.GameMode)

	// DSAllocator:ds_allocator_addr 非空 → 真 gRPC 拉 DS + 签 battle 票据;否则 W4 ① 打桩
	var allocator biz.DSAllocator
	if cfg.Match.DSAllocatorAddr != "" {
		// 真实 DS 分配链固定使用 Model-B RS256 实例绑定票。配置漂移时禁止回退到
		// legacy HS256，否则线上 Fleet（只有 public JWKS）会全量拒票，且重新引入玩家 HMAC。
		if !cfg.DSTicket.SignerEnabled() {
			helper.Errorw("msg", "ds_allocator_requires_ds_ticket_v2",
				"hint", "configure revisioned ds_ticket.private_key_file + active_kid; legacy fallback is forbidden")
			os.Exit(1)
		}
		v2Signer, verr := auth.NewDSTicketSignerFromConf(cfg.DSTicket)
		if verr != nil {
			helper.Errorw("msg", "ds_ticket_v2_signer_init_failed", "err", verr,
				"hint", "check ds_ticket.private_key_file / active_kid / ttl")
			os.Exit(1)
		}
		helper.Infow("msg", "ds_ticket_v2_signer_ready", "kid", v2Signer.Kid(), "ttl", v2Signer.TTL().String())
		abortSigner, abortErr := internalrpcauth.NewSigner(cfg.Match.AllocationAbortAuthSecret,
			serviceName, cfg.Match.AllocationAbortAuthAudience)
		if abortErr != nil {
			helper.Errorw("msg", "allocation_abort_service_auth_init_failed", "err", abortErr)
			os.Exit(1)
		}
		ga := data.NewGrpcDSAllocator(cfg.Match.DSAllocatorAddr, nil, v2Signer, abortSigner,
			cfg.Match.MapId, cfg.Match.GameMode, cfg.Match.DSAllocateTimeout.Std())
		defer func() { _ = ga.Close() }()
		allocator = ga
		helper.Infow("msg", "ds_allocator_grpc_ready", "ds_allocator_addr", cfg.Match.DSAllocatorAddr,
			"map_id", cfg.Match.MapId, "game_mode", cfg.Match.GameMode)
	} else {
		allocator = biz.NewStubDSAllocator("") // W4 ① 打桩;无 ds_allocator_addr 时兜底
		helper.Warnw("msg", "ds_allocator_addr_empty", "hint", "using StubDSAllocator (mock ds_addr + mock tickets)")
	}
	// player_locator gRPC notifier（弱依赖：locator_addr 留空 → 不上报位置）
	// 撮合成局→MATCHING、全员确认就绪→BATTLE（不变量 §1）
	var locator biz.LocationNotifier
	if cfg.Match.LocatorAddr != "" {
		locatorConn := grpcclient.MustDialInsecure(cfg.Match.LocatorAddr)
		ln := data.NewGrpcLocationNotifier(locatorConn)
		defer func() { _ = ln.Close() }()
		locator = ln
		helper.Infow("msg", "locator_notifier_ready", "locator_addr", cfg.Match.LocatorAddr)
	} else {
		helper.Warnw("msg", "locator_addr_empty", "hint", "match state (MATCHING/BATTLE) will not be reported to player_locator")
	}
	uc := biz.NewMatchUsecase(repo, reader, pusher, allocator, sf, locator, cfg.Match)

	// 蜂窝扩容:按 cfg.CellRoute 装配确定性 region/cell 路由(off/static/etcd 统一口）。
	// 单 Cell(mode 空）→ router=nil,行为不变;多 Cell → 两级撮合 + battle 放置感知 region。
	if router, cellClose, cerr := etcdtable.BuildRouter(context.Background(), cfg.CellRoute); cerr != nil {
		helper.Errorw("msg", "cellroute_init_failed", "err", cerr)
		os.Exit(1)
	} else if router != nil {
		if cellClose != nil {
			defer func() { _ = cellClose() }()
		}
		uc.SetCellRouter(router)
		uc.SetRegionPolicy(biz.DefaultRegionMatchPolicy())
		helper.Infow("msg", "cellroute_enabled", "mode", cfg.CellRoute.Mode,
			"self_region", cfg.CellRoute.SelfRegion, "self_cell", cfg.CellRoute.SelfCell)
	}
	resumeReplayStore, replayErr := internalrpcauth.NewRedisReplayStore(rdb,
		"pandora:matchmaker:resolve-context:nonce:")
	if replayErr != nil {
		helper.Errorw("msg", "match_resume_replay_store_init_failed", "err", replayErr)
		os.Exit(1)
	}
	resumeAuth, authErr := internalrpcauth.NewVerifier(cfg.Match.MatchResumeAuthSecret,
		"login", cfg.Match.MatchResumeAuthAudience, 30*time.Second, resumeReplayStore)
	if authErr != nil {
		helper.Errorw("msg", "match_resume_service_auth_init_failed", "err", authErr)
		os.Exit(1)
	}
	svc := service.NewMatchService(uc, sf, resumeAuth)

	// 8. gRPC + HTTP
	grpcSrv := server.NewGRPCServer(&cfg, svc)
	httpSrv := server.NewHTTPServer(&cfg)

	// 9. 后台撮合循环(随进程生命周期启停)
	//
	// 单写者(见 docs/design/decision-revisit-matchmaker-single-writer.md):撮合循环在共享队列上
	// 做全局优化,天然是单写者问题。多副本部署时若每个副本都无条件跑,会重复成局(同一玩家进两场
	// match,违反不变量 §1)。
	//   - Leader.Enabled=false(默认):本副本直接跑(单副本 / dev 行为不变)。
	//   - Leader.Enabled=true:经 etcd 选举,仅当选副本跑;失主取消 loop 的 ctx 但进程不退出,继续
	//     服务 RPC,新 leader 在 lease TTL 内接管(不停机滚动更新,不变量 §16)。
	loopCtx, loopCancel := context.WithCancel(context.Background())
	defer loopCancel()
	if cfg.Match.Leader.Enabled {
		// 分片键 = game_mode × region:同一 (mode, region) 的副本竞争同一个 leader。
		electionName := fmt.Sprintf("matchmaker/%s/r%d", cfg.Match.GameMode, cfg.CellRoute.SelfRegion)
		go func() {
			err := etcdleader.Run(loopCtx, etcdleader.Config{
				Endpoints:   cfg.Match.Leader.EtcdEndpoints,
				Election:    electionName,
				Prefix:      cfg.Match.Leader.Prefix,
				LeaseTTLSec: cfg.Match.Leader.LeaseTTLSec,
			}, uc.RunMatchLoop)
			if err != nil && loopCtx.Err() == nil {
				helper.Errorw("msg", "match_leader_run_failed", "election", electionName, "err", err)
			}
		}()
		helper.Infow("msg", "match_loop_leader_gated", "election", electionName,
			"etcd_endpoints", cfg.Match.Leader.EtcdEndpoints)
	} else {
		go uc.RunMatchLoop(loopCtx)
		helper.Infow("msg", "match_loop_direct", "hint", "single-replica / leader election disabled")
	}

	helper.Infow(
		"msg", "service_ready",
		"grpc", cfg.Server.Grpc.Addr,
		"http", cfg.Server.Http.Addr,
		"redis_addr", rc.Host,
		"team_addr", cfg.Match.TeamAddr,
		"confirm_timeout", cfg.Match.ConfirmTimeout.String(),
		"match_interval", cfg.Match.MatchInterval.String(),
		"team_size", cfg.Match.TeamSize,
		"enable_solo_match", cfg.Match.EnableSoloMatch,
		"auto_confirm_match", cfg.Match.AutoConfirmMatch,
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

// rawMatchProducer 是启动门禁测试可替换的 Kafka 最小能力面。
type rawMatchProducer interface {
	PushToPlayers(context.Context, uint64, []uint64, []byte) (int, error)
	Close() error
}

type matchPublicationInit struct {
	pusher         biz.MatchEventPusher
	producer       rawMatchProducer
	disabledReason string
}

// initializeMatchPublication 集中约束 match 进度推送的启动语义（与 team 同口径）：
//   - kafka.brokers 显式为空时保留纯 RPC 本地调试模式（仅队长可轮询）；
//   - 只要配置了 broker，producer 就是强依赖，初始化失败或返回 nil 都拒绝启动。
//
// 该门禁必须发生在 gRPC server 构造与 app.Run 之前，确保 readiness 永远不会把
// “StartMatch 受理成功但 READY 推送永久关闭”的进程加入 Service Endpoints。
func initializeMatchPublication(
	cfg pconfig.KafkaConfig,
	factory func(pconfig.KafkaConfig, string) (rawMatchProducer, error),
) (matchPublicationInit, error) {
	configured := false
	for _, broker := range cfg.Brokers {
		if strings.TrimSpace(broker) != "" {
			configured = true
			break
		}
	}
	if !configured {
		return matchPublicationInit{disabledReason: "kafka.brokers is empty"}, nil
	}

	producer, err := factory(cfg, kafkax.TopicMatchProgress)
	if err != nil {
		return matchPublicationInit{}, fmt.Errorf("initialize required %s producer: %w", kafkax.TopicMatchProgress, err)
	}
	if producer == nil {
		return matchPublicationInit{}, fmt.Errorf("initialize required %s producer: factory returned nil", kafkax.TopicMatchProgress)
	}
	return matchPublicationInit{
		pusher:   &kafkaPusher{p: producer},
		producer: producer,
	}, nil
}

// kafkaPusher 把 biz.MatchEventPusher 接口适配到 Kafka producer。
type kafkaPusher struct {
	p rawMatchProducer
}

func (k *kafkaPusher) PushMatchProgress(ctx context.Context, callerPlayerID uint64, toPlayerIDs []uint64, payload []byte) (int, error) {
	return k.p.PushToPlayers(ctx, callerPlayerID, toPlayerIDs, payload)
}
