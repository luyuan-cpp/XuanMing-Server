// Pandora push 服务入口(W3 ④,2026-06-05 真实化)。
//
// 启动顺序(对齐 login,见 services/account/login/cmd/login/main.go):
//  1. 解析 -conf 路径,加载 yaml(Kratos config + file source)
//  2. 填默认值(conf.Defaults)
//  3. log.Setup → 全局 zap logger
//  4. Redis client + Ping(失败致命:离线缓存不可降级)
//  5. ConnectionManager + RedisOfflineCacheRepo + PushUsecase + PushService 装配
//  6. 每个 push topic 一个 KafkaConsumer,共享 cfg.Kafka.GroupID
//  7. gRPC + HTTP server 注册(HTTP 仅 /metrics)
//  8. kratos.New(...).Run() 阻塞
//
// 信号处理:Kratos App 默认监听 SIGINT/SIGTERM。
// 优雅 stop 时,先 stop 所有 KafkaConsumer(取消上下文 + 等 worker),再 stop server。
// 所有在线 Subscribe stream 的 ctx 会被 cancel,RunSubscribeStream 自然退出。
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

	plog "github.com/luyuancpp/pandora/pkg/log"

	"github.com/luyuancpp/pandora/services/runtime/push/internal/biz"
	"github.com/luyuancpp/pandora/services/runtime/push/internal/conf"
	"github.com/luyuancpp/pandora/services/runtime/push/internal/data"
	"github.com/luyuancpp/pandora/services/runtime/push/internal/server"
	"github.com/luyuancpp/pandora/services/runtime/push/internal/service"
)

const serviceName = "push"

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "etc/push-dev.yaml", "config file path")
}

func main() {
	flag.Parse()

	// 1. Logger 先起
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

	// 3. Redis 客户端 + ping(失败致命)
	rdb := mustBuildRedis(&cfg, helper)
	defer func() { _ = rdb.Close() }()

	// 4. 三层装配
	conns := biz.NewConnectionManager()
	offline := data.NewRedisOfflineCacheRepo(rdb, cfg.Push.OfflineCacheTTL.Std())
	uc := biz.NewPushUsecase(conns, offline)
	svc := service.NewPushService(uc)

	// 5. KafkaConsumer:每 topic 一个,共享 GroupID
	consumers := mustBuildConsumers(&cfg, conns, offline, helper)
	for _, kc := range consumers {
		kc.Start()
	}
	defer func() {
		for _, kc := range consumers {
			if err := kc.Close(); err != nil {
				helper.Warnw("msg", "kafka_consumer_close_failed", "err", err)
			}
		}
	}()

	// 6. gRPC + HTTP server
	grpcSrv := server.NewGRPCServer(&cfg, svc)
	httpSrv := server.NewHTTPServer(&cfg)

	helper.Infow(
		"msg", "service_ready",
		"grpc", cfg.Server.Grpc.Addr,
		"http", cfg.Server.Http.Addr,
		"redis_addr", cfg.Node.RedisClient.Host,
		"kafka_brokers", cfg.Kafka.Brokers,
		"kafka_group", cfg.Kafka.GroupID,
		"topics", cfg.Push.Topics,
		"offline_ttl", cfg.Push.OfflineCacheTTL.String(),
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

// mustBuildRedis 构造 redis 客户端并 ping;失败 exit(W3 ④ push 不可降级,
// 没有 redis 就没有离线缓存,选 fail-fast 而不是假装运行)。
func mustBuildRedis(cfg *conf.Config, h kratosHelper) *redis.Client {
	rc := cfg.Node.RedisClient
	if rc.Host == "" {
		h.Errorw("msg", "redis_host_empty", "hint", "node.redis_client.host required for push offline cache")
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		h.Errorw("msg", "redis_ping_failed", "err", err, "addr", rc.Host)
		os.Exit(1)
	}
	h.Infow("msg", "redis_connected", "addr", rc.Host, "db", rc.DB)
	return rdb
}

// mustBuildConsumers 按 cfg.Push.Topics 列表,每 topic 起一个 KafkaConsumer。
// brokers 空 / topics 空 时 panic(W3 ④ push 不可降级)。
func mustBuildConsumers(
	cfg *conf.Config,
	cm biz.FrameSender,
	offline data.OfflineCacheRepo,
	h kratosHelper,
) []*biz.KafkaConsumer {
	if len(cfg.Kafka.Brokers) == 0 {
		h.Errorw("msg", "kafka_brokers_empty", "hint", "kafka.brokers required")
		os.Exit(1)
	}
	if len(cfg.Push.Topics) == 0 {
		h.Errorw("msg", "push_topics_empty", "hint", "push.topics required (or rely on conf.Defaults)")
		os.Exit(1)
	}

	out := make([]*biz.KafkaConsumer, 0, len(cfg.Push.Topics))
	for _, topic := range cfg.Push.Topics {
		kc, err := biz.NewKafkaConsumer(
			cfg.Kafka.Brokers,
			cfg.Kafka.GroupID,
			topic,
			cfg.Kafka.PartitionCnt,
			cm,
			offline,
		)
		if err != nil {
			h.Errorw("msg", "kafka_consumer_new_failed", "topic", topic, "err", err)
			os.Exit(1)
		}
		out = append(out, kc)
		h.Infow("msg", "kafka_consumer_ready", "topic", topic, "group", cfg.Kafka.GroupID)
	}
	return out
}

// kratosHelper 是 *klog.Helper 的简化接口(对齐 login main.go)。
type kratosHelper interface {
	Infow(keyvals ...any)
	Warnw(keyvals ...any)
	Errorw(keyvals ...any)
}
