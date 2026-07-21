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

	"github.com/luyuancpp/pandora/pkg/cellroute/etcdtable"
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
	if err := cfg.Player.ValidateExpCurve(); err != nil {
		helper.Errorw("msg", "exp_curve_invalid", "err", err,
			"hint", "player.exp_curve 每项须 >0,与客户端 j_玩家等级经验.xlsx 同源")
		os.Exit(1)
	}

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
	// 启动 schema gate:经验相关表列缺失时 fail-fast,不能让副本 Ready 后在首个
	// GetProfile / AddExperience 才大面积报错(迁移顺序错误要在发布时拦住)。
	schemaCtx, schemaCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := repo.ValidateExperienceSchema(schemaCtx); err != nil {
		schemaCancel()
		helper.Errorw("msg", "player_experience_schema_invalid", "err", err,
			"hint", "先执行 pandora_player migration 000002_experience(players.exp / exp_history / player_push_outbox)")
		os.Exit(1)
	}
	schemaCancel()
	uc := biz.NewPlayerUsecase(repo, cfg.Player)
	if closeCell, e := etcdtable.WireRouter(context.Background(), cfg.CellRoute, uc.SetCellRouter); e != nil {
		helper.Errorw("msg", "cellroute_init_failed", "err", e)
		os.Exit(1)
	} else if closeCell != nil {
		defer func() { _ = closeCell() }()
	}
	svc := service.NewPlayerService(uc)

	grpcSrv := server.NewGRPCServer(&cfg, svc)
	httpSrv := server.NewHTTPServer(&cfg)

	// 5. 经验推送出箱发布器(实时成长):producer 可用才注入,失败只警告(出箱积压不丢,
	// 与 battle_result player.update producer 同语义)。event_type 走 kafka header,push 透传。
	// ⚠️ 经验事件走独立 topic pandora.player.experience,绝不能发 pandora.player.update:
	// 旧 player 副本消费 player.update 时不看 event_type header,会把经验事件误解码成
	// MMR 事件污染段位(金丝雀混跑,不变量 §21;详见 kafkax.TopicPlayerUpdate 注释)。
	pubCtx, pubCancel := context.WithCancel(context.Background())
	defer pubCancel()
	if len(cfg.Kafka.Brokers) > 0 {
		producer, perr := kafkax.NewKeyOrderedProducer(cfg.Kafka, kafkax.TopicPlayerExperience)
		if perr != nil {
			helper.Warnw("msg", "player_push_producer_init_failed", "err", perr,
				"hint", "经验推送出箱积压不丢,producer 可用后重启补发")
		} else {
			defer func() { _ = producer.Close() }()
			uc.SetExperiencePusher(&playerEventPusher{p: producer})
			helper.Infow("msg", "player_push_producer_ready", "topic", kafkax.TopicPlayerExperience)
		}
	}
	go uc.RunPushOutboxPublisher(pubCtx)
	go uc.RunExpHistoryJanitor(pubCtx)

	// 6. KafkaConsumer:按 ConsumeTopics 每 topic 一个,handler 按 topic 路由
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

// playerEventPusher 把 biz.ExperiencePusher 适配到 kafkax.KeyOrderedProducer。
// key=player_id(不变量 §9 同玩家事件保序);event_type 走 kafka header(push.proto 域内路由)。
type playerEventPusher struct {
	p *kafkax.KeyOrderedProducer
}

func (k *playerEventPusher) PushPlayerEvent(ctx context.Context, playerID uint64, eventType uint32, payload []byte) error {
	return k.p.SendRawWithEventType(ctx, strconv.FormatUint(playerID, 10), payload, eventType)
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
