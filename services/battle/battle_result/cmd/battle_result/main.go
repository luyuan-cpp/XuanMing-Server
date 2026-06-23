// Pandora battle_result 服务入口(W4 ③,2026-06-06)。
//
// 职责:消费 pandora.battle.result 幂等落库 + 算 MMR(不变量 §2/§6),
// 消费 pandora.ds.lifecycle 的 ABANDONED 做 DS 崩溃补偿(不变量 §4),
// 落库同事务写 player.update 出箱 + 后台发布器可靠投递(W4 ⑨ 不变量 §4),
// 并提供战绩查询 RPC。
//
// 启动顺序(对齐 ds_allocator / push):
//  1. 解析 -conf 路径,加载 yaml
//  2. conf.Defaults 填默认值
//  3. log.Setup → 全局 zap logger
//  4. MySQL client + Ping(强依赖:结算落库不可降级)
//  5. MMR reader(W4 ③ player 未上线 → StaticMMRReader)
//  6. player.update kafka producer(弱依赖:broker 不通则 warn;player.update 已写事务出箱,
//     producer/broker 不可用时出箱积压不丢,等 producer 可用后由发布器补发,当前需重启/重配)
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
	"time"

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

// Kafka 消费失败处理:业务瞬时错误进程内重试 dlqMaxRetries 次(间隔 dlqRetryBackoff)后进 DLQ
// (infra.md §4.4「失败 3 次进 DLQ」)。
const (
	dlqMaxRetries   = 3
	dlqRetryBackoff = 500 * time.Millisecond
)

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

	// 4. MMR reader(W4 ④ player 上线 → 接真实 player gRPC reader;PlayerAddr 空则静态 BaseMMR 兜底)
	var mmr biz.MMRReader
	if cfg.Battle.PlayerAddr != "" {
		reader := data.NewGrpcMMRReader(cfg.Battle.PlayerAddr)
		defer func() { _ = reader.Close() }()
		mmr = reader
		helper.Infow("msg", "mmr_reader_grpc", "player_addr", cfg.Battle.PlayerAddr)
	} else {
		mmr = biz.NewStaticMMRReader(cfg.Battle.BaseMMR)
		helper.Infow("msg", "mmr_reader_static", "base_mmr", cfg.Battle.BaseMMR,
			"hint", "player_addr 未配置 → StaticMMRReader 兜底")
	}

	// 5. player.update producer(出箱发布器使用;init 失败则出箱积压等 producer 可用,不丢)
	var pusher biz.PlayerUpdatePusher
	if len(cfg.Kafka.Brokers) > 0 {
		producer, perr := kafkax.NewKeyOrderedProducer(cfg.Kafka, kafkax.TopicPlayerUpdate)
		if perr != nil {
			helper.Warnw("msg", "player_update_producer_init_failed", "err", perr,
				"hint", "outbox rows accumulate (not dropped); publisher resumes when producer is available")
		} else {
			defer func() { _ = producer.Close() }()
			pusher = &playerUpdatePusher{p: producer}
			helper.Infow("msg", "player_update_producer_ready", "topic", kafkax.TopicPlayerUpdate)
		}
	} else {
		helper.Warnw("msg", "kafka_brokers_empty", "hint", "outbox publisher idle until brokers configured")
	}

	// 6. 装配链
	repo := data.NewMySQLBattleRepo(db)

	// 6.0 matchmaker releaser(弱依赖:MatchmakerAddr 空 → 不通知释放,仅 Warn 不阻断落库)
	// 用于结算/废弃落库后调 matchmaker.ReleaseMatch 释放残留撮合状态,
	// 修复"结算返回 Hub 后玩家无法再次匹配(StartMatch 4002)"。
	var releaser biz.MatchReleaser
	if cfg.Battle.MatchmakerAddr != "" {
		mr := data.NewGrpcMatchReleaser(cfg.Battle.MatchmakerAddr)
		defer func() { _ = mr.Close() }()
		releaser = mr
		helper.Infow("msg", "match_releaser_grpc", "matchmaker_addr", cfg.Battle.MatchmakerAddr)
	} else {
		helper.Warnw("msg", "match_releaser_disabled",
			"hint", "matchmaker_addr 未配置 → 结算后不通知 matchmaker 释放撮合状态(玩家可能需等 TTL 才能再次匹配)")
	}

	uc := biz.NewBattleResultUsecase(repo, mmr, pusher, releaser, cfg.Battle)
	svc := service.NewBattleResultService(uc)

	grpcSrv := server.NewGRPCServer(&cfg, svc)
	httpSrv := server.NewHTTPServer(&cfg)

	// 6.1 后台 player.update 出箱发布器(W4 ⑨ 可靠补偿,随进程生命周期启停)
	pubCtx, pubCancel := context.WithCancel(context.Background())
	defer pubCancel()
	go uc.RunOutboxPublisher(pubCtx)

	// 7. KafkaConsumer:按 ConsumeTopics 每 topic 一个,handler 按 topic 路由
	consumers, dlqProducers := mustBuildConsumers(&cfg, uc, helper)
	for _, kc := range consumers {
		kc.Start()
	}
	defer func() {
		for _, kc := range consumers {
			if cerr := kc.Close(); cerr != nil {
				helper.Warnw("msg", "kafka_consumer_close_failed", "err", cerr)
			}
		}
		for _, dp := range dlqProducers {
			_ = dp.Close()
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
		"outbox_interval", cfg.Battle.OutboxPublishInterval.String(),
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
//
// 每个消费者配一个 DLQ producer(topic=pandora.dlq.<topic>):解码毒丸直接进 DLQ,
// 业务瞬时错误重试 dlqMaxRetries 次后进 DLQ(infra.md §4.4「失败 3 次进 DLQ」)。
// DLQ producer 构造失败致命:battle_result 不可静默降级为丢消息模式。
func mustBuildConsumers(cfg *conf.Config, uc *biz.BattleResultUsecase, h *klog.Helper) ([]*kafkax.KeyOrderedConsumer, []*kafkax.KeyOrderedProducer) {
	if len(cfg.Kafka.Brokers) == 0 {
		h.Errorw("msg", "kafka_brokers_empty", "hint", "kafka.brokers required")
		os.Exit(1)
	}
	if len(cfg.Battle.ConsumeTopics) == 0 {
		h.Errorw("msg", "consume_topics_empty", "hint", "battle.consume_topics required")
		os.Exit(1)
	}

	out := make([]*kafkax.KeyOrderedConsumer, 0, len(cfg.Battle.ConsumeTopics))
	dlqProducers := make([]*kafkax.KeyOrderedProducer, 0, len(cfg.Battle.ConsumeTopics))
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
		dlqTopic := kafkax.BuildDLQTopic(topic)
		dlq, derr := kafkax.NewKeyOrderedProducer(cfg.Kafka, dlqTopic)
		if derr != nil {
			h.Errorw("msg", "dlq_producer_init_failed", "topic", topic, "dlq_topic", dlqTopic, "err", derr,
				"hint", "battle_result 不可静默降级,DLQ 必须可用")
			os.Exit(1)
		}
		dlqProducers = append(dlqProducers, dlq)
		kc, err := kafkax.NewKeyOrderedConsumer(kafkax.ConsumerConfig{
			Brokers:        cfg.Kafka.Brokers,
			Topic:          topic,
			GroupID:        cfg.Kafka.GroupID,
			PartitionCount: cfg.Kafka.PartitionCnt,
			RetryPolicy:    kafkax.RetryPolicy{MaxRetries: dlqMaxRetries, Backoff: dlqRetryBackoff},
			DLQ:            dlq,
		}, handler)
		if err != nil {
			h.Errorw("msg", "kafka_consumer_new_failed", "topic", topic, "err", err)
			os.Exit(1)
		}
		out = append(out, kc)
		h.Infow("msg", "kafka_consumer_ready", "topic", topic, "group", cfg.Kafka.GroupID, "dlq_topic", dlqTopic)
	}
	if len(out) == 0 {
		h.Errorw("msg", "no_valid_consumer", "hint", "consume_topics 全部无效")
		os.Exit(1)
	}
	return out, dlqProducers
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
