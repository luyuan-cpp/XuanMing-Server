// Pandora auction 服务入口(全服拍卖行 / 撮合,2026-06-19)。
//
// 职责(docs/design/decision-revisit-auction-engine.md):
//
//	挂单 / 出价进按 market_id 分片的订单簿,单写者价格-时间优先撮合;
//	撮合权威落 MySQL(pandora_auction,强依赖);订单簿用 Redis ZSET(强依赖);
//	成交发 kafka pandora.auction.match,流转发 pandora.auction.audit(弱依赖)。
//
// 启动顺序(对齐 trade / inventory):
//  1. Logger
//  2. 加载 yaml → conf.Defaults
//  3. MySQL(强依赖:撮合权威;单库 dsn 或分库 shards 按 market_id 路由)
//  4. Redis + Ping(强依赖:订单簿不可降级)
//  5. Snowflake(order_id / match_id 生成)
//  6. kafka producer(pandora.auction.match + pandora.auction.audit)→ pusher(弱依赖)
//  7. SettlementLedger(W1 用 NoopSettlementLedger 占位)
//  8. 装配 AuctionUsecase → AuctionService → gRPC/HTTP server
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
	"github.com/luyuancpp/pandora/pkg/snowflake"
	auctionv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/auction/v1"

	"github.com/luyuancpp/pandora/services/economy/auction/internal/biz"
	"github.com/luyuancpp/pandora/services/economy/auction/internal/conf"
	"github.com/luyuancpp/pandora/services/economy/auction/internal/data"
	"github.com/luyuancpp/pandora/services/economy/auction/internal/server"
	"github.com/luyuancpp/pandora/services/economy/auction/internal/service"
)

const serviceName = "auction"

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "etc/auction-dev.yaml", "config file path")
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

	// 3. MySQL(强依赖:撮合权威库 pandora_auction)。分库优先,否则单库。
	var router data.DBRouter
	switch {
	case len(cfg.Node.MySQLClient.Shards) > 0:
		set, serr := mysqlx.NewShardSet(cfg.Node.MySQLClient)
		if serr != nil {
			helper.Errorw("msg", "mysql_shardset_failed", "err", serr)
			os.Exit(1)
		}
		defer func() {
			for _, db := range set.All() {
				_ = db.Close()
			}
		}()
		router = data.ShardedDB{Set: set}
		helper.Infow("msg", "mysql_connected", "mode", "sharded", "shards", set.Count())
	case cfg.Node.MySQLClient.DSN != "":
		db := mysqlx.MustNewClient(cfg.Node.MySQLClient)
		defer func() { _ = db.Close() }()
		router = data.SingleDB{DB: db}
		helper.Infow("msg", "mysql_connected", "mode", "single", "dsn", maskDSN(cfg.Node.MySQLClient.DSN))
	default:
		helper.Errorw("msg", "mysql_required", "hint", "node.mysql_client.dsn or .shards required (pandora_auction)")
		os.Exit(1)
	}

	// 4. Redis(强依赖:订单簿 ZSET 不可降级)
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

	// 5. Snowflake(order_id / match_id 生成)
	sf := snowflake.NewNode(uint64(cfg.Node.ZoneId))

	// 6. kafka producer → auctionEventPusher(弱依赖:broker 不通则 warn 并继续)
	var events biz.AuctionEventPusher
	if len(cfg.Kafka.Brokers) > 0 {
		matchTopic := config.BuildTopic("auction", "match") // pandora.auction.match
		auditTopic := config.BuildTopic("auction", "audit") // pandora.auction.audit
		pusher := &auctionEventPusher{}
		if p, perr := kafkax.NewKeyOrderedProducer(cfg.Kafka, matchTopic); perr != nil {
			helper.Warnw("msg", "kafka_match_producer_init_failed", "err", perr)
		} else {
			defer func() { _ = p.Close() }()
			pusher.match = p
			helper.Infow("msg", "kafka_producer_ready", "topic", matchTopic)
		}
		if p, perr := kafkax.NewKeyOrderedProducer(cfg.Kafka, auditTopic); perr != nil {
			helper.Warnw("msg", "kafka_audit_producer_init_failed", "err", perr)
		} else {
			defer func() { _ = p.Close() }()
			pusher.audit = p
			helper.Infow("msg", "kafka_producer_ready", "topic", auditTopic)
		}
		if pusher.match != nil || pusher.audit != nil {
			events = pusher
		}
	} else {
		helper.Warnw("msg", "kafka_brokers_empty", "hint", "auction events disabled")
	}

	// 7. SettlementLedger:W1 占位(总是成功)。真实背包 / 货币原子事务接入后替换。
	ledger := biz.NoopSettlementLedger{}

	// 8. 装配链
	repo := data.NewMySQLAuctionRepo(router)
	book := data.NewRedisBookStore(rdb)
	uc := biz.NewAuctionUsecase(repo, book, ledger, events, sf, cfg.Auction)
	svc := service.NewAuctionService(uc)

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

// auctionEventPusher 把 biz.AuctionEventPusher 适配到 kafkax.KeyOrderedProducer。
//   - 成交 → pandora.auction.match,kafka key = match_id(同一成交保序,不变量 §9)
//   - 流转 → pandora.auction.audit,kafka key = order_id(同一挂单保序)
type auctionEventPusher struct {
	match *kafkax.KeyOrderedProducer
	audit *kafkax.KeyOrderedProducer
}

func (k *auctionEventPusher) PushMatch(ctx context.Context, e *auctionv1.AuctionMatchEvent) error {
	if k.match == nil {
		return nil
	}
	return k.match.Send(ctx, strconv.FormatUint(e.GetMatchId(), 10), e)
}

func (k *auctionEventPusher) PushAudit(ctx context.Context, o *auctionv1.AuctionOrder) error {
	if k.audit == nil {
		return nil
	}
	return k.audit.Send(ctx, strconv.FormatUint(o.GetOrderId(), 10), o)
}

// maskDSN 脱敏 DSN 里的密码(对齐 trade / inventory main.go)。
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
	if colon >= 0 && at > colon {
		return dsn[:colon+1] + "***" + dsn[at:]
	}
	return dsn
}
