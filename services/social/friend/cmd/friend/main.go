// Pandora friend 服务入口(2026-06-15)。
//
// 职责:好友请求 / 接受 / 列表 / 拉黑;好友图落 pandora_social(MySQL 强依赖);
// 好友请求 / 接受经 kafka pandora.friend.event → push 推送给接收方(弱依赖);
// ListFriends 经 player_locator 填在线状态(弱依赖)。
//
// 启动顺序(对齐 team / player):
//  1. 解析 -conf 路径,加载 yaml
//  2. conf.Defaults 填默认值
//  3. log.Setup → 全局 zap logger
//  4. MySQL client + Ping(强依赖:好友图落库不可降级)
//  5. Snowflake Node(request_id 生成,zone_id 来自 yaml)
//  6. kafka producer(topic=pandora.friend.event)→ friendEventPusher(弱依赖)
//  7. player_locator gRPC client → OnlineStatusReader(弱依赖,addr 空则离线)
//  8. 装配 FriendUsecase → FriendService → gRPC/HTTP server
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

	"github.com/luyuancpp/pandora/pkg/grpcclient"
	"github.com/luyuancpp/pandora/pkg/kafkax"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/mysqlx"
	"github.com/luyuancpp/pandora/pkg/snowflake"
	friendv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/friend/v1"

	"github.com/luyuancpp/pandora/services/social/friend/internal/biz"
	"github.com/luyuancpp/pandora/services/social/friend/internal/conf"
	"github.com/luyuancpp/pandora/services/social/friend/internal/data"
	"github.com/luyuancpp/pandora/services/social/friend/internal/server"
	"github.com/luyuancpp/pandora/services/social/friend/internal/service"
)

const serviceName = "friend"

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "etc/friend-dev.yaml", "config file path")
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

	// 3. MySQL(强依赖:好友图落库不可降级)
	if cfg.Node.MySQLClient.DSN == "" {
		helper.Errorw("msg", "mysql_dsn_required", "hint", "node.mysql_client.dsn required (pandora_social)")
		os.Exit(1)
	}
	db := mysqlx.MustNewClient(cfg.Node.MySQLClient)
	defer func() { _ = db.Close() }()
	helper.Infow("msg", "mysql_connected", "dsn", maskDSN(cfg.Node.MySQLClient.DSN))

	// 4. Snowflake(request_id 生成)
	sf := snowflake.NewNode(uint64(cfg.Node.ZoneId))

	// 5. kafka producer → friendEventPusher(弱依赖:broker 不通则 warn 并继续,推送静默 fail)
	var pusher biz.FriendEventPusher
	if len(cfg.Kafka.Brokers) > 0 {
		producer, perr := kafkax.NewKeyOrderedProducer(cfg.Kafka, kafkax.TopicFriendEvent)
		if perr != nil {
			helper.Warnw("msg", "kafka_producer_init_failed", "err", perr,
				"hint", "friend push silently dropped until kafka is available")
		} else {
			defer func() { _ = producer.Close() }()
			pusher = &friendEventPusher{p: producer}
			helper.Infow("msg", "kafka_producer_ready", "topic", kafkax.TopicFriendEvent)
		}
	} else {
		helper.Warnw("msg", "kafka_brokers_empty", "hint", "friend push disabled")
	}

	// 6. player_locator gRPC client → OnlineStatusReader(弱依赖:addr 空则在线状态全离线)
	var online data.OnlineStatusReader
	if cfg.Friend.LocatorAddr != "" {
		conn := grpcclient.MustDialInsecure(cfg.Friend.LocatorAddr)
		defer func() { _ = conn.Close() }()
		online = data.NewGrpcOnlineStatusReader(conn)
		helper.Infow("msg", "locator_client_ready", "locator_addr", cfg.Friend.LocatorAddr)
	} else {
		helper.Warnw("msg", "locator_addr_empty", "hint", "friend online status disabled (all offline)")
	}

	// 7. 装配链
	repo := data.NewMySQLFriendRepo(db)
	uc := biz.NewFriendUsecase(repo, pusher, online, cfg.Friend)
	svc := service.NewFriendService(uc, sf)

	grpcSrv := server.NewGRPCServer(&cfg, svc)
	httpSrv := server.NewHTTPServer(&cfg)

	helper.Infow(
		"msg", "service_ready",
		"grpc", cfg.Server.Grpc.Addr,
		"http", cfg.Server.Http.Addr,
		"kafka_brokers", cfg.Kafka.Brokers,
		"locator_addr", cfg.Friend.LocatorAddr,
		"max_friends", cfg.Friend.MaxFriends,
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

// friendEventPusher 把 biz.FriendEventPusher 接口适配到 kafkax.KeyOrderedProducer。
// kafka key = to_player_id(不变量 §9:同接收方事件保序;push 服务按 key 路由到该玩家 stream)。
type friendEventPusher struct {
	p *kafkax.KeyOrderedProducer
}

func (k *friendEventPusher) PushFriendEvent(ctx context.Context, toPlayerID uint64, evt *friendv1.FriendEvent) error {
	return k.p.Send(ctx, strconv.FormatUint(toPlayerID, 10), evt)
}

// maskDSN 脱敏 DSN 里的密码(对齐 player / battle_result main.go)。
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
