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

	"github.com/luyuancpp/pandora/pkg/cellroute/etcdtable"
	"github.com/luyuancpp/pandora/pkg/kafkax"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/redisx"
	"github.com/luyuancpp/pandora/pkg/sessiongate"

	"github.com/luyuancpp/pandora/services/runtime/push/internal/biz"
	"github.com/luyuancpp/pandora/services/runtime/push/internal/conf"
	"github.com/luyuancpp/pandora/services/runtime/push/internal/data"
	"github.com/luyuancpp/pandora/services/runtime/push/internal/server"
	"github.com/luyuancpp/pandora/services/runtime/push/internal/service"
)

const serviceName = "push"

// Kafka 消费失败处理:业务瞬时错误(offline.Append 撞 redis 抖动)进程内重试
// dlqMaxRetries 次(间隔 dlqRetryBackoff)后投 DLQ(infra.md §4.4,对齐 battle_result)。
const (
	dlqMaxRetries   = 3
	dlqRetryBackoff = 500 * time.Millisecond
)

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
	offline := data.NewRedisOfflineCacheRepo(rdb, cfg.Push.OfflineCacheTTL.Std(), cfg.Push.OfflineCacheMaxFrames)
	uc := biz.NewPushUsecase(conns, offline)
	// 会话现行性门(P0,INC-20260722-004):login 的 pandora:sess 权威在同一 Redis;
	// require 档由配置控制(prod 生成器机械置 true,dev 直连联调保持宽松)。
	// R5 复审 P0-1:实现收敛到共享 pkg/sessiongate(Subscribe 建流门 + unary 中间件共用)。
	sessGate := sessiongate.NewRedisGate(rdb)
	uc.SetSessionGate(sessGate, cfg.Push.RequireSessionGate)
	svc := service.NewPushService(uc)

	// 5. KafkaConsumer:每 topic 一个,共享 GroupID
	consumers := mustBuildConsumers(&cfg, conns, offline, helper)
	// 跨 Pod 唤醒信号(R5 复审 P2-10):写缓冲的 Pod 本地无连接时 PUBLISH player_id,
	// 持有连接的 Pod 订阅后立即拉取投递;30s 兜底轮询保留为信号丢失时的正确性兜底。
	wakeCtx, wakeCancel := context.WithCancel(context.Background())
	defer wakeCancel()
	wakeSignal := data.NewRedisWakeSignal(rdb)
	for _, kc := range consumers {
		kc.SetWakePublisher(wakeSignal)
	}
	go data.RunWakeSubscriber(wakeCtx, rdb, func(playerID uint64) { conns.SendTo(playerID) })
	// 蜂窝扩容:多 Cell 时注入归属守卫(本 cell 消费者只应交付 owner==本 cell 的玩家,
	// 否则告警暴露漂移;单 Cell mode 空 → router=nil,行为不变)。
	if router, closeCell, e := etcdtable.BuildRouter(context.Background(), cfg.CellRoute); e != nil {
		helper.Errorw("msg", "cellroute_init_failed", "err", e)
		os.Exit(1)
	} else if router != nil {
		if closeCell != nil {
			defer func() { _ = closeCell() }()
		}
		for _, kc := range consumers {
			kc.SetCellOwnership(router, cfg.CellRoute.SelfRegion, cfg.CellRoute.SelfCell)
		}
		helper.Infow("msg", "cellroute_enabled", "self_region", cfg.CellRoute.SelfRegion, "self_cell", cfg.CellRoute.SelfCell)
	}
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

	// 6. gRPC + HTTP server(unary 链挂 SessionCurrent,与 Subscribe 建流门同一 gate)
	grpcSrv := server.NewGRPCServer(&cfg, svc, sessGate)
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
func mustBuildRedis(cfg *conf.Config, h kratosHelper) redis.UniversalClient {
	rc := cfg.Node.RedisClient
	// 单实例填 host,Redis Cluster / Sentinel 只填 addrs,两者皆空才算未配置。
	if rc.Host == "" && len(rc.Addrs) == 0 {
		h.Errorw("msg", "redis_endpoint_empty", "hint", "node.redis_client.host (single) or addrs (cluster) required for push offline cache")
		os.Exit(1)
	}
	rdb := redisx.NewUniversalClient(rc)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		h.Errorw("msg", "redis_ping_failed", "err", err, "addr", rc.Host, "addrs", rc.Addrs)
		os.Exit(1)
	}
	// 持久性/驱逐门(审计 R4 P1 + R4 复审 P1-5):投递缓冲与会话门都以这台 Redis 为
	// 权威。maxmemory-policy 非 noeviction 时,内存压力下 allkeys-lru 等策略会静默驱逐
	// offline/sess key = 无告警丢帧 + 会话门失效,必须 fail-fast 拒绝启动。
	// R4 复审修正两点:
	//   - CONFIG GET 失败缺省 fail-closed 拒启动(「查不了」≠「配置正确」);托管 Redis
	//     禁用 CONFIG 的环境须人工确认策略后显式置 push.allow_unverified_eviction_policy
	//     并列入部署核对清单。
	//   - Cluster 模式逐 master 核验(单次 CONFIG GET 只落在被路由到的一个节点,
	//     证明不了整个拓扑);Sentinel/单实例核验当前连接的主节点。
	verifyEvictionPolicy(ctx, rdb, cfg.Push.AllowUnverifiedEvictionPolicy, h)
	h.Infow("msg", "redis_connected", "addr", rc.Host, "addrs", rc.Addrs, "db", rc.DB)
	return rdb
}

// verifyEvictionPolicy 核验 Redis(含 Cluster 全部 master)maxmemory-policy=noeviction;
// 违规 fail-fast,核验失败按 allowUnverified 决定放行(告警)或拒启动(缺省)。
func verifyEvictionPolicy(ctx context.Context, rdb redis.UniversalClient, allowUnverified bool, h kratosHelper) {
	failUnverifiable := func(err error) {
		if allowUnverified {
			h.Warnw("msg", "redis_eviction_policy_unverifiable_allowed", "err", err,
				"hint", "allow_unverified_eviction_policy=true 放行:必须已人工确认全拓扑 maxmemory-policy=noeviction")
			return
		}
		h.Errorw("msg", "redis_eviction_policy_unverifiable", "err", err,
			"hint", "CONFIG GET 失败,无法证明 maxmemory-policy=noeviction,fail-closed 拒启动;"+
				"托管 Redis 禁用 CONFIG 时人工确认策略后置 push.allow_unverified_eviction_policy=true")
		os.Exit(1)
	}
	checkOne := func(c redis.Cmdable, node string) error {
		vals, cerr := c.ConfigGet(ctx, "maxmemory-policy").Result()
		if cerr != nil {
			return cerr
		}
		if policy, ok := vals["maxmemory-policy"]; !ok || policy != "noeviction" {
			h.Errorw("msg", "redis_eviction_policy_unsafe", "policy", vals["maxmemory-policy"], "node", node,
				"hint", "push 投递缓冲/会话门要求 maxmemory-policy=noeviction,驱逐策略会静默丢帧/放行旧会话")
			os.Exit(1)
		}
		return nil
	}

	if cc, ok := rdb.(*redis.ClusterClient); ok {
		err := cc.ForEachMaster(ctx, func(fctx context.Context, node *redis.Client) error {
			vals, cerr := node.ConfigGet(fctx, "maxmemory-policy").Result()
			if cerr != nil {
				return cerr
			}
			if policy, pok := vals["maxmemory-policy"]; !pok || policy != "noeviction" {
				h.Errorw("msg", "redis_eviction_policy_unsafe", "policy", vals["maxmemory-policy"],
					"node", node.String(),
					"hint", "cluster master 驱逐策略违规:push 要求全部 master maxmemory-policy=noeviction")
				os.Exit(1)
			}
			return nil
		})
		if err != nil {
			failUnverifiable(err)
		}
		return
	}
	if err := checkOne(rdb, "primary"); err != nil {
		failUnverifiable(err)
	}
}

// mustBuildConsumers 按 cfg.Push.Topics 列表,每 topic 起一个 KafkaConsumer。
// brokers 空 / topics 空 时 panic(W3 ④ push 不可降级)。
//
// 每个消费者配 RetryPolicy + DLQ(topic=pandora.dlq.<topic>,对齐 battle_result 模式):
// offline.Append 瞬时失败进程内重试,耗尽后投 DLQ 可回放,不再“首败即 ack 丢帧”。
// DLQ producer 构造失败致命:不可静默降级为丢消息模式。
func mustBuildConsumers(
	cfg *conf.Config,
	cm biz.FrameRouter,
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
		dlqTopic := kafkax.BuildDLQTopic(topic)
		dlq, derr := kafkax.NewKeyOrderedProducer(cfg.Kafka, dlqTopic)
		if derr != nil {
			h.Errorw("msg", "dlq_producer_init_failed", "topic", topic, "dlq_topic", dlqTopic, "err", derr,
				"hint", "push 离线信箱不可静默降级,DLQ 必须可用")
			os.Exit(1)
		}
		kc, err := biz.NewKafkaConsumer(
			cfg.Kafka.Brokers,
			cfg.Kafka.GroupID,
			topic,
			cfg.Kafka.PartitionCnt,
			cm,
			offline,
			kafkax.RetryPolicy{MaxRetries: dlqMaxRetries, Backoff: dlqRetryBackoff},
			dlq,
		)
		if err != nil {
			h.Errorw("msg", "kafka_consumer_new_failed", "topic", topic, "err", err)
			os.Exit(1)
		}
		out = append(out, kc)
		h.Infow("msg", "kafka_consumer_ready", "topic", topic, "group", cfg.Kafka.GroupID, "dlq_topic", dlqTopic)
	}
	return out
}

// kratosHelper 是 *klog.Helper 的简化接口(对齐 login main.go)。
type kratosHelper interface {
	Infow(keyvals ...any)
	Warnw(keyvals ...any)
	Errorw(keyvals ...any)
}
