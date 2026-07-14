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
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/dsauthfence"
	"github.com/luyuancpp/pandora/pkg/grpcclient"
	"github.com/luyuancpp/pandora/pkg/kafkax"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/middleware"
	"github.com/luyuancpp/pandora/pkg/redisx"
	"github.com/luyuancpp/pandora/pkg/releasetrack"
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
	if err := cfg.DSAuth.ValidateRedisFence(); err != nil {
		helper.Errorw("msg", "ds_auth_fence_config_invalid", "err", err)
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

	// 4. 装配链
	repo := data.NewRedisBattleRepo(rdb)

	// 4.0 DS 回调服务令牌(审核 P1 #1):签发器(分配时下发给战斗 DS)+ 守卫(校验 DS 回调)。
	// secret 未配 → dsSigner=nil 不签发;mode=off → dsGuard=nil 不校验(默认,现行为不变)。
	dsSigner, err := middleware.NewDSCallbackSignerFromConf(cfg.DSAuth)
	if err != nil {
		helper.Errorw("msg", "ds_auth_signer_init_failed", "err", err)
		os.Exit(1)
	}
	dsGuard, err := middleware.NewDSCallbackGuardFromConf(cfg.DSAuth)
	if err != nil {
		helper.Errorw("msg", "ds_auth_guard_init_failed", "err", err)
		os.Exit(1)
	}
	// 启动期 TTL 正值/最小值校验(审核 P1):本服务签发(dsSigner!=nil)或校验(guard!=nil)DS 回调
	// 令牌时,BattleTokenTTL/HubTokenTTL 必须 >= 最小值,否则令牌签发即过期属误配,启动即拒。
	if err := cfg.DSAuth.Validate(dsSigner != nil || dsGuard != nil); err != nil {
		helper.Errorw("msg", "ds_auth_ttl_invalid", "err", err)
		os.Exit(1)
	}
	// 战斗令牌不续期(一局一签、DS 一局一销毁),TTL 必须覆盖「战斗镜像 TTL(battle_ttl,最长对局 +
	// 补偿重试上界)+ 重连/ready 余量」。否则活跃对局跑到一半令牌过期,battle_result / 心跳等 DS 回调被
	// enforce 守卫全拒、赛果无法结算(审核 P1:battle 令牌下限须关联 battle_ttl,不能只看固定 30m/1h)。
	if dsSigner != nil {
		const battleReconnectMargin = 15 * time.Minute
		needTTL := cfg.Allocator.BattleTTL.Std() + battleReconnectMargin
		if cfg.DSAuth.BattleTokenTTL.Std() < needTTL {
			helper.Errorw("msg", "ds_auth_battle_token_ttl_too_small_vs_battle_ttl",
				"battle_token_ttl", cfg.DSAuth.BattleTokenTTL.Std().String(),
				"battle_ttl", cfg.Allocator.BattleTTL.Std().String(),
				"need_at_least", needTTL.String(),
				"hint", "战斗令牌不续期,须 >= battle_ttl + 15m 重连余量;调大 ds_auth.battle_token_ttl 或调小 allocator.battle_ttl")
			os.Exit(1)
		}
	}
	// 签发回调:battle 令牌绑 match_id(pod 分配时未知,pod↔match 绑定由心跳 pod_mismatch 逻辑兜底)。
	issueBattleToken := func(matchID uint64) (string, error) {
		tok, _, serr := dsSigner.SignDSCallback(auth.DSTypeBattle, "", matchID, cfg.DSAuth.BattleTokenTTL.Std())
		return tok, serr
	}
	// local-off-v1 不接 Redis pending/ACK，但仍必须给 UE 完整 Model-B tuple，不能回退 legacy JWT。
	// 每个本机进程有随机实例 UID 与 jti；一局一实例，epoch/gen 从 1 起且不会在实例内回退。
	issueLocalBattleCredential := func(matchID uint64, podName, instanceUID string, instanceEpoch uint32) (string, error) {
		res, serr := dsSigner.SignBattleCredential(
			matchID, podName, instanceUID, instanceEpoch, 1, uuid.NewString(), cfg.DSAuth.BattleTokenTTL.Std())
		if serr != nil {
			return "", serr
		}
		return res.Token, nil
	}
	// enforce 下签发失败必须 fail-closed(不分配无令牌的 DS,否则回调被守卫全拒)。
	dsEnforce := dsGuard.Mode() == middleware.DSAuthEnforce
	modelB := cfg.DSAuth.AuthorityModeRedis()
	if modelB && (cfg.Mode != conf.ModeAgones || !dsEnforce || dsSigner == nil) {
		helper.Errorw("msg", "battle_model_b_invalid_activation",
			"allocator_mode", cfg.Mode, "guard_mode", dsGuard.Mode().String(), "signer_ready", dsSigner != nil,
			"hint", "authority_mode=redis requires mode=agones + ds_auth.mode=enforce + signing key; no legacy fallback")
		os.Exit(1)
	}
	if dsSigner != nil {
		helper.Infow("msg", "ds_callback_token_issuer_ready",
			"battle_token_ttl", cfg.DSAuth.BattleTokenTTL.Std().String(), "guard_mode", dsGuard.Mode().String())
	}

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
		if dsSigner != nil && !modelB {
			ag.SetDSTokenIssuer(issueBattleToken, dsEnforce) // 令牌经 GameServerAllocation annotation 下发
		}
		helper.Infow("msg", "agones_allocator_ready",
			"api_server", cfg.Agones.APIServer, "namespace", cfg.Agones.Namespace, "fleet", cfg.Agones.FleetName)
	case conf.ModeLocal:
		if perr := auth.ValidateDSLocalProfileOffV1(dsGuard.Mode().String(), cfg.DSAuth.AuthorityMode, dsSigner != nil); perr != nil {
			helper.Errorw("msg", "local_battle_auth_profile_invalid",
				"err", perr,
				"hint", "mode=local requires ds_auth.mode=off + authority_mode=legacy + signing key (local-off-v1); Redis Model-B local authority is not implemented")
			os.Exit(1)
		}
		ld, lerr := data.NewLocalGameServerAllocator(cfg.LocalDS)
		if lerr != nil {
			helper.Errorw("msg", "local_ds_allocator_init_failed", "err", lerr,
				"hint", "mode=local 需 local_ds.executable_path 指向打包好的 UE Windows DS 可执行文件")
			os.Exit(1)
		}
		// 进程退出时杀掉全部在管 DS,避免遗留孤儿。
		defer func() { _ = ld.Close() }()
		allocator = ld
		ld.SetDSTokenIssuer(issueLocalBattleCredential, true) // 完整 tuple 经 env 下发；失败必须 fail-closed
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
	releasePolicy, policyErr := releasetrack.New(cfg.Agones.CanaryPercent, cfg.Agones.CanarySeed)
	if policyErr != nil {
		helper.Errorw("msg", "battle_release_track_policy_invalid", "err", policyErr)
		os.Exit(1)
	}
	uc.SetReleaseTrackPolicy(releasePolicy)
	helper.Infow("msg", "battle_release_track_policy_ready",
		"canary_percent", cfg.Agones.CanaryPercent, "canary_seed_configured", cfg.Agones.CanarySeed != "")
	battleAuthRepo := data.NewRedisBattleAuthRepo(rdb)
	if modelB {
		if err := uc.EnableRedisAuthority(battleAuthRepo, dsSigner, cfg.DSAuth.BattleTokenTTL.Std()); err != nil {
			helper.Errorw("msg", "battle_model_b_init_failed", "err", err)
			os.Exit(1)
		}
		helper.Infow("msg", "battle_model_b_enabled", "required_writer_epoch", data.BattleDSWriterEpochV2,
			"authority", "redis", "k8s_role", "delivery-only")
	}
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
	svc.SetDSCallbackGuard(dsGuard) // DS 回调令牌校验(Heartbeat);nil=off

	// GmService(GM / 运维指令下发):与 ds_allocator 同进程复用 gRPC 端口。
	// 运维 GM 工具 SendCommand 入 Redis 队列 → 战斗 DS 轮询 PollCommands 拉取执行(如给玩家发道具)。
	// 内部接口,不经 Envoy 暴露给玩家客户端。
	gmSvc := gm.NewService(rdb, logger)
	gmSvc.SetDSCallbackGuard(dsGuard) // DS 回调令牌校验(PollCommands/AckCommand);nil=off
	if modelB {
		if err := gmSvc.EnableRedisAuthority(battleAuthRepo); err != nil {
			helper.Errorw("msg", "gm_battle_model_b_init_failed", "err", err)
			os.Exit(1)
		}
	}
	// SendCommand 前置校验目标对局是否有活跃战斗镜像:typo / 已结束的 match_id 立即拒,
	// 避免静默入僵尸队列(repo 天然满足 BattleLivenessChecker,复用同一 Redis)。
	gmSvc.SetBattleChecker(repo)

	// 5. gRPC + HTTP
	grpcSrv := server.NewGRPCServer(&cfg, svc, gmSvc)
	httpSrv := server.NewHTTPServer(&cfg)

	// sweep/capacity watcher 也是 writer；capability 未取得前禁止启动任何后台循环或 RPC。
	if modelB {
		fence, err := dsauthfence.AcquireRuntime(context.Background(), dsauthfence.RuntimeConfig{
			Endpoints: cfg.DSAuth.Fence.EtcdEndpoints, Prefix: cfg.DSAuth.Fence.EtcdPrefix,
			Service: serviceName, KeysetRevision: cfg.DSAuth.Fence.KeysetRevision,
			WriterEpoch: dsauthfence.ProtocolEpochV2,
			Features:    []string{"battle-release-expected-tuple-v1"},
			LeaseTTLSec: cfg.DSAuth.Fence.EtcdLeaseTTLSec, DialTimeout: cfg.DSAuth.Fence.EtcdDialTimeout.Std(),
		})
		if err != nil {
			helper.Errorw("msg", "ds_auth_fence_acquire_failed", "err", err)
			os.Exit(1)
		}
		defer func() { _ = fence.Close() }()
		go func() {
			<-fence.Lost()
			helper.Errorw("msg", "ds_auth_fence_lost", "hint", "立即退出，禁止失租/旧 epoch allocator 继续分配或接收 DS 写回")
			os.Exit(1)
		}()
		helper.Infow("msg", "ds_auth_fence_ready", "required_writer_epoch", fence.RequiredEpoch())
	}

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
