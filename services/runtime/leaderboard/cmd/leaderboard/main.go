// Pandora leaderboard 服务入口(通用排行榜,2026-06-27)。
//
// 职责(docs/design/decision-revisit-leaderboard.md):
//
//	通用 / 可扩展排行榜(全服 / 类型 / 工会 / 副本局内临时);Redis ZSET 做实时排名(强依赖);
//	结算 SettleBoard 取 Top-N 落 MySQL 快照 + 按 RewardTable 幂等发奖(调 inventory.GrantItems)
//	+ 发 kafka pandora.leaderboard.settle(弱依赖)。
//
// 启动顺序(对齐 auction / inventory):
//  1. Logger
//  2. 加载 yaml → conf.Defaults
//  3. MySQL(强依赖:结算归档库 pandora_leaderboard)
//  4. Redis + Ping(强依赖:排行榜 ZSET 不可降级)
//  5. Snowflake(settlement_id 生成)
//  6. kafka producer(pandora.leaderboard.settle)→ pusher(弱依赖)
//  7. RewardGranter(配 inventory_addr 走真实发奖;留空且 allow_noop_reward=true 才退 Noop,否则 fail-fast)
//  8. 装配 LeaderboardUsecase → LeaderboardService → gRPC/HTTP server
//  9. kratos.New(...).Run() 阻塞
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

	"github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/kafkax"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/mysqlx"
	"github.com/luyuancpp/pandora/pkg/redisx"
	"github.com/luyuancpp/pandora/pkg/snowflake/etcdnode"
	leaderboardv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/leaderboard/v1"

	"github.com/luyuancpp/pandora/services/runtime/leaderboard/internal/biz"
	"github.com/luyuancpp/pandora/services/runtime/leaderboard/internal/conf"
	"github.com/luyuancpp/pandora/services/runtime/leaderboard/internal/data"
	"github.com/luyuancpp/pandora/services/runtime/leaderboard/internal/server"
	"github.com/luyuancpp/pandora/services/runtime/leaderboard/internal/service"
)

const serviceName = "leaderboard"

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "etc/leaderboard-dev.yaml", "config file path")
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

	// 3. MySQL(强依赖:结算归档库 pandora_leaderboard)
	if cfg.Node.MySQLClient.DSN == "" {
		helper.Errorw("msg", "mysql_required", "hint", "node.mysql_client.dsn required (pandora_leaderboard)")
		os.Exit(1)
	}
	db := mysqlx.MustNewClient(cfg.Node.MySQLClient)
	defer func() { _ = db.Close() }()
	helper.Infow("msg", "mysql_connected", "dsn", maskDSN(cfg.Node.MySQLClient.DSN))

	// 4. Redis(强依赖:排行榜 ZSET 不可降级)
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

	// 5. Snowflake(settlement_id 生成；node_id_source=static 静态，=etcd 走 etcd 自动抢占，失租自动退出)
	sf, sfCloser := etcdnode.MustProvideSnowflake(serviceName, cfg.Node.NodeId, cfg.Snowflake)
	defer func() { _ = sfCloser.Close() }()

	// 6. kafka producer → settleEventPusher(弱依赖:broker 不通则 warn 并继续)
	var events biz.SettleEventPusher
	if len(cfg.Kafka.Brokers) > 0 {
		settleTopic := config.BuildTopic("leaderboard", "settle") // pandora.leaderboard.settle
		if p, perr := kafkax.NewKeyOrderedProducer(cfg.Kafka, settleTopic); perr != nil {
			helper.Warnw("msg", "kafka_settle_producer_init_failed", "err", perr)
		} else {
			defer func() { _ = p.Close() }()
			events = &settleEventPusher{settle: p}
			helper.Infow("msg", "kafka_producer_ready", "topic", settleTopic)
		}
	} else {
		helper.Warnw("msg", "kafka_brokers_empty", "hint", "leaderboard settle events disabled")
	}

	// 7. RewardGranter:配了 inventory_addr → 真实发奖(GrantItems 幂等);留空 → 仅当
	//    allow_noop_reward=true 才退回 NoopRewardGranter,否则 fail-fast 防生产漏配后「结算不发奖」。
	var granter biz.RewardGranter
	if addr := cfg.Leaderboard.InventoryAddr; addr != "" {
		g := data.NewGrpcInventoryRewardGranter(addr)
		defer func() { _ = g.Close() }()
		granter = g
		helper.Infow("msg", "reward_granter_ready", "mode", "inventory_grpc", "inventory_addr", addr)
	} else if cfg.Leaderboard.AllowNoopReward {
		granter = biz.NoopRewardGranter{}
		helper.Warnw("msg", "reward_granter_noop", "hint", "leaderboard.inventory_addr empty; settle grants nothing (allow_noop_reward=true)")
	} else {
		helper.Errorw("msg", "reward_granter_missing",
			"hint", "leaderboard.inventory_addr 必填(真实发奖);仅联调/单测可显式设 leaderboard.allow_noop_reward=true")
		os.Exit(1)
	}

	// 8. 装配链
	repo := data.NewMySQLLeaderboardRepo(db)
	board := data.NewRedisBoardStore(rdb)
	uc := biz.NewLeaderboardUsecase(repo, board, granter, events, sf, cfg.Leaderboard)
	svc := service.NewLeaderboardService(uc)

	grpcSrv := server.NewGRPCServer(&cfg, svc)
	httpSrv := server.NewHTTPServer(&cfg)

	helper.Infow(
		"msg", "service_ready",
		"grpc", cfg.Server.Grpc.Addr,
		"http", cfg.Server.Http.Addr,
		"redis_addr", rc.Host,
		"kafka_brokers", cfg.Kafka.Brokers,
	)

	// 9. Kratos App
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

// settleEventPusher 把 biz.SettleEventPusher 适配到 kafkax.KeyOrderedProducer。
//   - 结算 → pandora.leaderboard.settle,kafka key = settlement_id(同一结算保序,不变量 §9)
type settleEventPusher struct {
	settle *kafkax.KeyOrderedProducer
}

func (k *settleEventPusher) PushSettle(ctx context.Context, settlementID uint64, b data.BoardKey, winners []*leaderboardv1.LeaderboardEntry) error {
	if k.settle == nil {
		return nil
	}
	evt := &leaderboardv1.LeaderboardSettleEvent{
		SettlementId: settlementID,
		Board: &leaderboardv1.BoardKey{
			BoardType: b.BoardType,
			Scope:     leaderboardv1.LeaderboardScope(b.Scope),
			ScopeId:   b.ScopeID,
			Period:    b.Period,
		},
		Winners:     winners,
		SettledAtMs: time.Now().UnixMilli(),
	}
	return k.settle.Send(ctx, strconv.FormatUint(settlementID, 10), evt)
}

// maskDSN 脱敏 DSN 里的密码(对齐 auction / trade main.go)。
func maskDSN(dsn string) string {
	at := -1
	colon := -1
	for i := 0; i < len(dsn); i++ {
		if dsn[i] == ':' && colon == -1 {
			colon = i
		}
		if dsn[i] == '@' {
			at = i
			break
		}
	}
	if colon > 0 && at > colon {
		return dsn[:colon+1] + "****" + dsn[at:]
	}
	return dsn
}
