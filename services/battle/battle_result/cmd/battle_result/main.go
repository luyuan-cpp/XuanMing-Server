// Pandora battle_result 服务入口(W4 ③,2026-06-06)。
//
// 职责:消费 pandora.battle.result 幂等落库 + 算 MMR(不变量 §2/§6),
// 消费 pandora.ds.lifecycle 的 ABANDONED 做 DS 崩溃补偿(不变量 §4),
// 落库后发 pandora.player.update(player 上线后消费),并提供战绩查询 RPC。
//
// 启动顺序(对齐 ds_allocator / push):
//  1. 解析 -conf 路径,加载 yaml
//  2. conf.Defaults 填默认值
//  3. log.Setup → 全局 zap logger
//  4. MySQL client + Ping(强依赖:结算落库不可降级)
//  5. MMR reader(W4 ③ player 未上线 → StaticMMRReader)
//  6. player.update kafka producer(弱依赖:broker 不通则 warn,push 静默丢)
//  7. 装配 BattleResultUsecase → BattleResultService → gRPC/HTTP server
//  8. 按 ConsumeTopics 每 topic 一个 KafkaConsumer,handler 按 topic 路由
//  9. kratos.New(...).Run() 阻塞
package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strconv"

	"github.com/go-kratos/kratos/v2"
	kconfig "github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"
	klog "github.com/go-kratos/kratos/v2/log"

	"github.com/luyuancpp/pandora/pkg/kafkax"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/mysqlx"

	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/biz"
	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/data"
	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/server"
	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/service"
)

const serviceName = "battle_result"

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "etc/battle_result-dev.yaml", "config file path")
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

	// 3. MySQL(强依赖:结算落库不可降级)
	if cfg.Node.MySQLClient.DSN == "" {
		helper.Errorw("msg", "mysql_dsn_required", "hint", "node.mysql_client.dsn required (pandora_battle)")
		os.Exit(1)
	}
	db := mysqlx.MustNewClient(cfg.Node.MySQLClient)
	defer func() { _ = db.Close() }()
	helper.Infow("msg", "mysql_connected", "dsn", maskDSN(cfg.Node.MySQLClient.DSN))

	// 4. MMR reader(W4 ③ player 未上线 → 静态 BaseMMR;PlayerAddr 留作 player 上线钩子)
	var mmr biz.MMRReader = biz.NewStaticMMRReader(cfg.Battle.BaseMMR)
	if cfg.Battle.PlayerAddr != "" {
		helper.Warnw("msg", "player_addr_set_but_static_reader",
			"hint", "W4 ③ 暂用 StaticMMRReader;接 player gRPC reader 留后续",
			"player_addr", cfg.Battle.PlayerAddr)
	}

	// 5. player.update producer(弱依赖)
	var pusher biz.PlayerUpdatePusher
	if len(cfg.Kafka.Brokers) > 0 {
		producer, perr := kafkax.NewKeyOrderedProducer(cfg.Kafka, kafkax.TopicPlayerUpdate)
		if perr != nil {
			helper.Warnw("msg", "player_update_producer_init_failed", "err", perr,
				"hint", "player.update push will be silently dropped until kafka is available")
		} else {
			defer func() { _ = producer.Close() }()
			pusher = &playerUpdatePusher{p: producer}
			helper.Infow("msg", "player_update_producer_ready", "topic", kafkax.TopicPlayerUpdate)
		}
	} else {
		helper.Warnw("msg", "kafka_brokers_empty", "hint", "player.update push disabled")
	}

	// 6. 装配链
	repo := data.NewMySQLBattleRepo(db)
	uc := biz.NewBattleResultUsecase(repo, mmr, pusher, cfg.Battle)
	svc := service.NewBattleResultService(uc)

	grpcSrv := server.NewGRPCServer(&cfg, svc)
	httpSrv := server.NewHTTPServer(&cfg)

	// 7. KafkaConsumer:按 ConsumeTopics 每 topic 一个,handler 按 topic 路由
	consumers := mustBuildConsumers(&cfg, uc, helper)
	for _, kc := range consumers {
		kc.Start()
	}
	defer func() {
		for _, kc := range consumers {
			if cerr := kc.Close(); cerr != nil {
				helper.Warnw("msg", "kafka_consumer_close_failed", "err", cerr)
			}
		}
	}()

	helper.Infow(
		"msg", "service_ready",
		"grpc", cfg.Server.Grpc.Addr,
		"http", cfg.Server.Http.Addr,
		"kafka_brokers", cfg.Kafka.Brokers,
		"kafka_group", cfg.Kafka.GroupID,
		"consume_topics", cfg.Battle.ConsumeTopics,
		"elo_k", cfg.Battle.EloKFactor,
		"base_mmr", cfg.Battle.BaseMMR,
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

// mustBuildConsumers 按 cfg.Battle.ConsumeTopics 起 KafkaConsumer,handler 按 topic 路由。
// brokers / topics 空时致命(battle_result 不可降级:不消费就不结算)。
func mustBuildConsumers(cfg *conf.Config, uc *biz.BattleResultUsecase, h *klog.Helper) []*kafkax.KeyOrderedConsumer {
	if len(cfg.Kafka.Brokers) == 0 {
		h.Errorw("msg", "kafka_brokers_empty", "hint", "kafka.brokers required")
		os.Exit(1)
	}
	if len(cfg.Battle.ConsumeTopics) == 0 {
		h.Errorw("msg", "consume_topics_empty", "hint", "battle.consume_topics required")
		os.Exit(1)
	}

	out := make([]*kafkax.KeyOrderedConsumer, 0, len(cfg.Battle.ConsumeTopics))
	for _, topic := range cfg.Battle.ConsumeTopics {
		var handler kafkax.Handler
		switch topic {
		case kafkax.TopicBattleResult:
			handler = uc.BattleResultHandler()
		case kafkax.TopicDSLifecycle:
			handler = uc.DSLifecycleHandler()
		default:
			h.Warnw("msg", "unknown_consume_topic_skipped", "topic", topic)
			continue
		}
		kc, err := kafkax.NewKeyOrderedConsumer(kafkax.ConsumerConfig{
			Brokers:        cfg.Kafka.Brokers,
			Topic:          topic,
			GroupID:        cfg.Kafka.GroupID,
			PartitionCount: cfg.Kafka.PartitionCnt,
		}, handler)
		if err != nil {
			h.Errorw("msg", "kafka_consumer_new_failed", "topic", topic, "err", err)
			os.Exit(1)
		}
		out = append(out, kc)
		h.Infow("msg", "kafka_consumer_ready", "topic", topic, "group", cfg.Kafka.GroupID)
	}
	if len(out) == 0 {
		h.Errorw("msg", "no_valid_consumer", "hint", "consume_topics 全部无效")
		os.Exit(1)
	}
	return out
}

// playerUpdatePusher 把 biz.PlayerUpdatePusher 适配到 kafkax.KeyOrderedProducer。
// key=player_id(不变量 §9 同玩家事件保序)。
type playerUpdatePusher struct {
	p *kafkax.KeyOrderedProducer
}

func (k *playerUpdatePusher) PushPlayerUpdate(ctx context.Context, playerID uint64, payload []byte) error {
	return k.p.SendRaw(ctx, strconv.FormatUint(playerID, 10), payload)
}

// maskDSN 脱敏 DSN 里的密码(对齐 login main.go)。
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
	if colon != -1 && at != -1 && at > colon {
		return dsn[:colon+1] + "***" + dsn[at:]
	}
	return dsn
}
