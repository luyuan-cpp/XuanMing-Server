// Pandora guild 服务入口(2026-06-27)。
//
// 职责:公会(GuildService)+ 临时群(GroupService)同进程;成员关系落 pandora_social(MySQL 强依赖); 公会成员变更经 kafka pandora.guild.event → push 推送给接收方(弱依赖);
// 临时群 MVP 不单独推送成员变更(客户端拉 ListMyGroups 兜底)。
//
// 启动顺序(对齐 friend / team):
//  1. 解析 -conf 路径,加载 yaml
//  2. conf.Defaults 填默认值
//  3. log.Setup → 全局 zap logger
//  4. MySQL client + Ping(强依赖:公会 / 群关系落库不可降级)
//  5. Snowflake Node(guild_id / group_id / request_id 生成,node_id 来自 yaml)
//  6. kafka producer(topic=pandora.guild.event)→ guildEventPusher(弱依赖)
//  7. 装配 GuildUsecase + GroupUsecase → GuildService + GroupService → gRPC/HTTP server
//  8. kratos.New(...).Run() 阻塞
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

	"github.com/luyuancpp/pandora/pkg/kafkax"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/mysqlx"
	"github.com/luyuancpp/pandora/pkg/snowflake/etcdnode"
	guildv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/guild/v1"

	"github.com/luyuancpp/pandora/services/social/guild/internal/biz"
	"github.com/luyuancpp/pandora/services/social/guild/internal/conf"
	"github.com/luyuancpp/pandora/services/social/guild/internal/data"
	"github.com/luyuancpp/pandora/services/social/guild/internal/server"
	"github.com/luyuancpp/pandora/services/social/guild/internal/service"
)

const serviceName = "guild"

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "etc/guild-dev.yaml", "config file path")
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

	// 3. MySQL(强依赖:公会 / 群关系落库不可降级)
	if cfg.Node.MySQLClient.DSN == "" {
		helper.Errorw("msg", "mysql_dsn_required", "hint", "node.mysql_client.dsn required (pandora_social)")
		os.Exit(1)
	}
	db := mysqlx.MustNewClient(cfg.Node.MySQLClient)
	defer func() { _ = db.Close() }()
	helper.Infow("msg", "mysql_connected", "dsn", maskDSN(cfg.Node.MySQLClient.DSN))

	// 4. Snowflake(guild_id / group_id / request_id 生成；node_id_source=static 静态，=etcd 走 etcd 自动抢占，失租自动退出)
	sf, sfCloser := etcdnode.MustProvideSnowflake(serviceName, cfg.Node.NodeId, cfg.Snowflake)
	defer func() { _ = sfCloser.Close() }()

	// 5. kafka producer → guildEventPusher(弱依赖:broker 不通则 warn 并继续,推送静默 fail)
	var pusher biz.GuildEventPusher
	if len(cfg.Kafka.Brokers) > 0 {
		producer, perr := kafkax.NewKeyOrderedProducer(cfg.Kafka, kafkax.TopicGuildEvent)
		if perr != nil {
			helper.Warnw("msg", "kafka_producer_init_failed", "err", perr,
				"hint", "guild push silently dropped until kafka is available")
		} else {
			defer func() { _ = producer.Close() }()
			pusher = &guildEventPusher{p: producer}
			helper.Infow("msg", "kafka_producer_ready", "topic", kafkax.TopicGuildEvent)
		}
	} else {
		helper.Warnw("msg", "kafka_brokers_empty", "hint", "guild push disabled")
	}

	// 6. 装配链(公会 + 临时群同进程)
	guildRepo := data.NewMySQLGuildRepo(db)
	groupRepo := data.NewMySQLGroupRepo(db)
	guildUC := biz.NewGuildUsecase(guildRepo, pusher, cfg.Guild)
	groupUC := biz.NewGroupUsecase(groupRepo, cfg.Guild)
	guildSvc := service.NewGuildService(guildUC, sf)
	groupSvc := service.NewGroupService(groupUC, sf)

	grpcSrv := server.NewGRPCServer(&cfg, guildSvc, groupSvc)
	httpSrv := server.NewHTTPServer(&cfg)

	helper.Infow(
		"msg", "service_ready",
		"grpc", cfg.Server.Grpc.Addr,
		"http", cfg.Server.Http.Addr,
		"kafka_brokers", cfg.Kafka.Brokers,
		"max_guild_members", cfg.Guild.MaxGuildMembers,
		"max_group_members", cfg.Guild.MaxGroupMembers,
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

// guildEventPusher 把 biz.GuildEventPusher 接口适配到 kafkax.KeyOrderedProducer。
// kafka key = to_player_id(不变量 §9:同接收方事件保序;push 服务按 key 路由到该玩家 stream)。
type guildEventPusher struct {
	p *kafkax.KeyOrderedProducer
}

func (k *guildEventPusher) PushGuildEvent(ctx context.Context, toPlayerID uint64, evt *guildv1.GuildEvent) error {
	return k.p.Send(ctx, strconv.FormatUint(toPlayerID, 10), evt)
}

// maskDSN 脱敏 DSN 里的密码(对齐 friend / player main.go)。
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
