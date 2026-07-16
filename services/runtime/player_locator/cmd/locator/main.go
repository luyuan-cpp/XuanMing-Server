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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-kratos/kratos/v2"
	kconfig "github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"

	"github.com/luyuancpp/pandora/pkg/cellroute/etcdtable"
	"github.com/luyuancpp/pandora/pkg/dsauthfence"
	"github.com/luyuancpp/pandora/pkg/kafkax"
	"github.com/luyuancpp/pandora/pkg/killswitch"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/middleware"
	"github.com/luyuancpp/pandora/pkg/placement"
	"github.com/luyuancpp/pandora/pkg/redisx"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"

	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/biz"
	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/conf"
	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/data"
	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/placementpreflight"
	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/server"
	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/service"
)

const serviceName = "player_locator"

var (
	flagConf               string
	flagPlacementPreflight bool
	flagPreflightTimeout   time.Duration
	flagPreflightScanCount int64
)

func init() {
	flag.StringVar(&flagConf, "conf", "etc/locator-dev.yaml", "config file path")
	flag.BoolVar(&flagPlacementPreflight, "placement-preflight", false,
		"run the read-only all-master placement rollout gate and exit")
	flag.DurationVar(&flagPreflightTimeout, "placement-preflight-timeout", 10*time.Minute,
		"hard deadline for the embedded placement rollout gate")
	flag.Int64Var(&flagPreflightScanCount, "placement-preflight-scan-count", 1000,
		"Redis SCAN count hint per master for the embedded placement rollout gate")
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
	if err := cfg.ValidateDSAuthAuthorityMode(); err != nil {
		helper.Errorw("msg", "ds_auth_authority_mode_invalid", "err", err)
		os.Exit(1)
	}
	if cfg.DSAuth.AuthorityModeRedis() {
		if err := cfg.DSAuth.ValidateRedisFence(); err != nil {
			helper.Errorw("msg", "ds_auth_fence_config_invalid", "err", err)
			os.Exit(1)
		}
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

	// Recreate prevents old/new locator writers from overlapping, while this
	// init-container mode mechanically blocks the new writer until every Redis
	// master is proven free of legacy/malformed placement records. It is the
	// exact same read-only implementation used by cmd/placement_preflight.
	if flagPlacementPreflight {
		if flagPreflightTimeout <= 0 || flagPreflightScanCount <= 0 {
			helper.Errorw("msg", "placement_preflight_args_invalid",
				"timeout", flagPreflightTimeout, "scan_count", flagPreflightScanCount)
			os.Exit(1)
		}
		preflightCtx, preflightCancel := context.WithTimeout(context.Background(), flagPreflightTimeout)
		defer preflightCancel()
		summary := new(placementpreflight.AuditSummary)
		if err := placementpreflight.AuditRedis(preflightCtx, rdb, flagPreflightScanCount, summary); err != nil {
			helper.Errorw("msg", "placement_preflight_audit_failed", "err", err)
			os.Exit(1)
		}
		if summary.Nodes == 0 {
			helper.Errorw("msg", "placement_preflight_zero_nodes")
			os.Exit(1)
		}
		sort.Slice(summary.Findings, func(i, j int) bool {
			a, b := summary.Findings[i], summary.Findings[j]
			if a.Key != b.Key {
				return a.Key < b.Key
			}
			if a.Reason != b.Reason {
				return a.Reason < b.Reason
			}
			return a.Source < b.Source
		})
		for _, finding := range summary.Findings {
			helper.Errorw("msg", "placement_preflight_unsafe_record",
				"source", finding.Source, "key", finding.Key,
				"player_id", finding.PlayerID, "reason", finding.Reason)
		}
		if len(summary.Findings) != 0 {
			helper.Errorw("msg", "placement_preflight_blocked",
				"nodes", summary.Nodes, "records", summary.Records,
				"skipped_non_record_keys", summary.Skipped, "findings", len(summary.Findings))
			os.Exit(1)
		}
		helper.Infow("msg", "placement_preflight_passed", "nodes", summary.Nodes,
			"records", summary.Records, "skipped_non_record_keys", summary.Skipped)
		return
	}

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
	if closeCell, e := etcdtable.WireRouter(context.Background(), cfg.CellRoute, uc.SetCellRouter); e != nil {
		helper.Errorw("msg", "cellroute_init_failed", "err", e)
		os.Exit(1)
	} else if closeCell != nil {
		defer func() { _ = closeCell() }()
	}
	svc := service.NewLocatorService(uc)
	placementRepo := data.NewRedisPlacementRepo(rdb)
	proofSecret := func(env, fallback string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		return fallback
	}
	hubTransferSecret := proofSecret("PANDORA_PLACEMENT_HUB_TRANSFER_SECRET", cfg.Locator.PlacementHubTransferProofSecret)
	battleDepartureSecret := proofSecret("PANDORA_PLACEMENT_BATTLE_DEPARTURE_SECRET", cfg.Locator.PlacementBattleDepartureProofSecret)
	authoritySecrets := map[string]string{
		"account-bootstrap": proofSecret("PANDORA_PLACEMENT_ACCOUNT_BOOTSTRAP_SECRET", cfg.Locator.PlacementAccountBootstrapProofSecret),
		"match-start":       proofSecret("PANDORA_PLACEMENT_MATCH_START_SECRET", cfg.Locator.PlacementMatchStartProofSecret),
		"battle-exit":       proofSecret("PANDORA_PLACEMENT_BATTLE_EXIT_SECRET", cfg.Locator.PlacementBattleExitProofSecret),
		"hub-transfer":      hubTransferSecret,
		"battle-departure":  battleDepartureSecret,
	}
	if err := validatePlacementAuthoritySecrets(authoritySecrets, cfg.DSAuth.Secret); err != nil {
		helper.Errorw("msg", "placement_proof_authority_invalid", "err", err)
		os.Exit(1)
	}
	var proofVerifier biz.PlacementProofVerifier
	proofKeys := map[int32]string{
		placement.ProofAccountBootstrap: authoritySecrets["account-bootstrap"],
		placement.ProofMatchStart:       authoritySecrets["match-start"],
		placement.ProofMatchTerminal:    authoritySecrets["battle-exit"],
		placement.ProofPlayerLeave:      authoritySecrets["battle-exit"],
		placement.ProofHubTransfer:      hubTransferSecret,
		placement.ProofHubDeparture:     hubTransferSecret,
		placement.ProofBattleDeparture:  battleDepartureSecret,
	}
	keyring, err := placement.NewProofKeyring(proofKeys)
	if err != nil {
		helper.Errorw("msg", "placement_proof_keyring_invalid", "err", err)
		os.Exit(1)
	}
	if len(proofKeys) > 0 {
		proofVerifier = keyring
		if proofKeys[placement.ProofAccountBootstrap] != "" || proofKeys[placement.ProofMatchStart] != "" ||
			proofKeys[placement.ProofMatchTerminal] != "" || proofKeys[placement.ProofHubTransfer] != "" ||
			proofKeys[placement.ProofBattleDeparture] != "" {
			helper.Infow("msg", "placement_proof_verifier_ready")
		}
	}
	svc.SetPlacementUsecase(biz.NewPlacementUsecase(placementRepo, proofVerifier))

	// 4.2 DS 回调令牌守卫(审核 P1 #1):校验 Hub DS 经 :8444 的 SetLocation/ReportDisconnect。
	// mode=off(默认)→ dsGuard 为 nil,不校验。
	dsGuard, derr := middleware.NewDSCallbackGuardFromConf(cfg.DSAuth)
	if derr != nil {
		helper.Errorw("msg", "ds_auth_guard_init_failed", "err", derr)
		os.Exit(1)
	}
	svc.SetDSCallbackGuard(dsGuard)
	if dsGuard != nil {
		helper.Infow("msg", "ds_callback_guard_ready", "mode", dsGuard.Mode().String())
	}
	// Model B 跨服务终态门：JWT 验签之后再读 Redis 唯一授权权威，只有当前 active
	// 凭据可执行 SetLocation(HUB)/ReportDisconnect。legacy/off/permissive 保持原行为。
	if dsGuard.Mode() == middleware.DSAuthEnforce && cfg.DSAuth.AuthorityModeRedis() {
		svc.SetHubCredentialStateChecker(service.NewHubCredentialStateChecker(
			data.NewRedisHubAuthReader(rdb), cfg.DSAuth.ActiveHeartbeatMaxAge.Std()))
		helper.Infow("msg", "hub_active_credential_checker_ready", "authority_mode", "redis")
	}

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

	// critical dependencies 与 active checker 全部装配成功后才注册 capability。
	if cfg.DSAuth.AuthorityModeRedis() {
		fence, err := dsauthfence.AcquireRuntime(context.Background(), dsauthfence.RuntimeConfig{
			Endpoints: cfg.DSAuth.Fence.EtcdEndpoints, Prefix: cfg.DSAuth.Fence.EtcdPrefix,
			Service: serviceName, KeysetRevision: cfg.DSAuth.Fence.KeysetRevision,
			WriterEpoch: dsauthfence.ProtocolEpochV2,
			LeaseTTLSec: cfg.DSAuth.Fence.EtcdLeaseTTLSec, DialTimeout: cfg.DSAuth.Fence.EtcdDialTimeout.Std(),
		})
		if err != nil {
			helper.Errorw("msg", "ds_auth_fence_acquire_failed", "err", err)
			os.Exit(1)
		}
		defer func() { _ = fence.Close() }()
		go func() {
			<-fence.Lost()
			helper.Errorw("msg", "ds_auth_fence_lost", "hint", "立即退出，禁止失租/旧 epoch 副本继续接受 Hub 写回")
			os.Exit(1)
		}()
		helper.Infow("msg", "ds_auth_fence_ready", "required_writer_epoch", fence.RequiredEpoch())
	}
	// Presence worker 可写 Kafka；Redis authority 模式下必须在 capability 成功后才启动。
	if presenceHub != nil {
		presenceHub.Start()
		defer presenceHub.Close()
	}

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

func validatePlacementAuthoritySecrets(authorities map[string]string, dsCallbackSecret string) error {
	required := []string{"account-bootstrap", "match-start", "battle-exit", "hub-transfer", "battle-departure"}
	names := make([]string, 0, len(required))
	for _, name := range required {
		secret, ok := authorities[name]
		if !ok {
			return fmt.Errorf("placement authority %s is missing", name)
		}
		if strings.TrimSpace(secret) == "" || len([]byte(secret)) < 32 {
			return fmt.Errorf("placement authority %s requires an independent key of at least 32 bytes", name)
		}
		if dsCallbackSecret != "" && secret == dsCallbackSecret {
			return fmt.Errorf("placement authority %s reuses the DS callback key", name)
		}
		names = append(names, name)
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if authorities[names[i]] == authorities[names[j]] {
				return fmt.Errorf("placement authorities %s and %s reuse one key", names[i], names[j])
			}
		}
	}
	return nil
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
