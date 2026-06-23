// Pandora player 服务入口(W4 ④,2026-06-06)。
//
// 职责:玩家档案 / 段位 MMR / 英雄池;消费 pandora.player.update 幂等 UpdateMMR
// (不变量 §2,idempotency_key=match_id);GetMMR 供 battle_result 当真实 MMRReader。
//
// 启动顺序(对齐 battle_result):
//  1. 解析 -conf 路径,加载 yaml
//  2. conf.Defaults 填默认值
//  3. log.Setup → 全局 zap logger
//  4. MySQL client + Ping(强依赖:玩家档案落库不可降级)
//  5. 装配 PlayerUsecase → PlayerService → gRPC/HTTP server
//  6. 按 ConsumeTopics 每 topic 一个 KafkaConsumer(player.update)
//  7. kratos.New(...).Run() 阻塞
package main

import (
	"flag"
	"os"
	"path/filepath"
	"time"

	"github.com/go-kratos/kratos/v2"
	kconfig "github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"
	klog "github.com/go-kratos/kratos/v2/log"

	"github.com/luyuancpp/pandora/pkg/kafkax"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/mysqlx"

	"github.com/luyuancpp/pandora/services/account/player/internal/biz"
	"github.com/luyuancpp/pandora/services/account/player/internal/conf"
	"github.com/luyuancpp/pandora/services/account/player/internal/data"
	"github.com/luyuancpp/pandora/services/account/player/internal/server"
	"github.com/luyuancpp/pandora/services/account/player/internal/service"
)

const serviceName = "player"

// Kafka 消费失败处理:业务瞬时错误进程内重试 dlqMaxRetries 次(间隔 dlqRetryBackoff)后进 DLQ
// (infra.md §4.4「失败 3 次进 DLQ」)。
const (
	dlqMaxRetries   = 3
	dlqRetryBackoff = 500 * time.Millisecond
)

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "etc/player-dev.yaml", "config file path")
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

	// 3. MySQL(强依赖:玩家档案落库不可降级)
	if cfg.Node.MySQLClient.DSN == "" {
		helper.Errorw("msg", "mysql_dsn_required", "hint", "node.mysql_client.dsn required (pandora_player)")
		os.Exit(1)
	}
	db := mysqlx.MustNewClient(cfg.Node.MySQLClient)
	defer func() { _ = db.Close() }()
	helper.Infow("msg", "mysql_connected", "dsn", maskDSN(cfg.Node.MySQLClient.DSN))

	// 4. 装配链
	repo := data.NewMySQLPlayerRepo(db)
	uc := biz.NewPlayerUsecase(repo, cfg.Player)
	svc := service.NewPlayerService(uc)

	grpcSrv := server.NewGRPCServer(&cfg, svc)
	httpSrv := server.NewHTTPServer(&cfg)

	// 5. KafkaConsumer:按 ConsumeTopics 每 topic 一个,handler 按 topic 路由
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
		"consume_topics", cfg.Player.ConsumeTopics,
		"base_mmr", cfg.Player.BaseMMR,
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

// mustBuildConsumers 按 cfg.Player.ConsumeTopics 起 KafkaConsumer,handler 按 topic 路由。
// brokers / topics 空时致命(player 不消费 player.update 就无法做幂等 UpdateMMR)。
//
// 每个消费者配一个 DLQ producer(topic=pandora.dlq.<topic>):解码毒丸直接进 DLQ,
// 业务瞬时错误重试 dlqMaxRetries 次后进 DLQ。DLQ producer 构造失败致命:不可静默丢 MMR 更新。
func mustBuildConsumers(cfg *conf.Config, uc *biz.PlayerUsecase, h *klog.Helper) ([]*kafkax.KeyOrderedConsumer, []*kafkax.KeyOrderedProducer) {
	if len(cfg.Kafka.Brokers) == 0 {
		h.Errorw("msg", "kafka_brokers_empty", "hint", "kafka.brokers required")
		os.Exit(1)
	}
	if len(cfg.Player.ConsumeTopics) == 0 {
		h.Errorw("msg", "consume_topics_empty", "hint", "player.consume_topics required")
		os.Exit(1)
	}

	out := make([]*kafkax.KeyOrderedConsumer, 0, len(cfg.Player.ConsumeTopics))
	dlqProducers := make([]*kafkax.KeyOrderedProducer, 0, len(cfg.Player.ConsumeTopics))
	for _, topic := range cfg.Player.ConsumeTopics {
		var handler kafkax.Handler
		switch topic {
		case kafkax.TopicPlayerUpdate:
			handler = uc.PlayerUpdateHandler()
		default:
			h.Warnw("msg", "unknown_consume_topic_skipped", "topic", topic)
			continue
		}
		dlqTopic := kafkax.BuildDLQTopic(topic)
		dlq, derr := kafkax.NewKeyOrderedProducer(cfg.Kafka, dlqTopic)
		if derr != nil {
			h.Errorw("msg", "dlq_producer_init_failed", "topic", topic, "dlq_topic", dlqTopic, "err", derr,
				"hint", "player.update 不可静默降级,DLQ 必须可用")
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

// maskDSN 脱敏 DSN 里的密码(对齐 battle_result / login main.go)。
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
