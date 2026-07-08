// mail 服务启动入口(2026-06-29)。
//
// 装配链:logger → MySQL(强依赖)→ Snowflake → repo/usecase/service → Kratos.Run。
// mail 不依赖 kafka(系统/公会邮件拉取式,个人邮件落库即达;红点推送复用 system.notify 由运营侧发)。
package main

import (
	"flag"
	"os"
	"path/filepath"

	"github.com/go-kratos/kratos/v2"
	kconfig "github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"

	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/mysqlx"
	"github.com/luyuancpp/pandora/pkg/snowflake/etcdnode"

	"github.com/luyuancpp/pandora/services/social/mail/internal/biz"
	"github.com/luyuancpp/pandora/services/social/mail/internal/conf"
	"github.com/luyuancpp/pandora/services/social/mail/internal/data"
	"github.com/luyuancpp/pandora/services/social/mail/internal/server"
	"github.com/luyuancpp/pandora/services/social/mail/internal/service"
)

const serviceName = "mail"

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "etc/mail-dev.yaml", "config file path")
}

func main() {
	flag.Parse()

	logger := plog.Setup(serviceName)
	helper := plog.NewHelper(logger)
	helper.Infow("msg", "service_starting", "conf", flagConf)

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

	// MySQL 强依赖(pandora_social:邮件 + 公会成员表)
	if cfg.Node.MySQLClient.DSN == "" {
		helper.Errorw("msg", "mysql_dsn_required", "hint", "node.mysql_client.dsn required (pandora_social)")
		os.Exit(1)
	}
	db := mysqlx.MustNewClient(cfg.Node.MySQLClient)
	defer func() { _ = db.Close() }()
	helper.Infow("msg", "mysql_connected", "dsn", maskDSN(cfg.Node.MySQLClient.DSN))

	// 系统/公会邮件单节点生成,channel 内 mail_id 严格递增(游标比较零漏拉)
	// node_id_source=static 静态；=etcd 走 etcd 自动抢占独占 nodeID，失租自动退出
	sf, sfCloser := etcdnode.MustProvideSnowflake(serviceName, cfg.Node.NodeId, cfg.Snowflake)
	defer func() { _ = sfCloser.Close() }()

	repo := data.NewMySQLMailRepo(db)

	// inventory 客户端:领附件入库用。地址缺省且非测试空领 → 拒启,防裸奔丢奖
	var granter biz.ItemGranter
	var instGranter biz.InstanceGranter
	if cfg.Mail.InventoryAddr != "" {
		g := data.NewGrpcItemGranter(cfg.Mail.InventoryAddr)
		defer func() { _ = g.Close() }()
		granter = g
		instGranter = g // 同一连接:装备实例型附件领取走 GrantInstances
		helper.Infow("msg", "inventory_client_ready", "addr", cfg.Mail.InventoryAddr)
	} else if !cfg.Mail.AllowNoopGrant {
		helper.Errorw("msg", "inventory_addr_required", "hint", "mail.inventory_addr required, or set mail.allow_noop_grant for test")
		os.Exit(1)
	} else {
		helper.Warnw("msg", "inventory_noop_grant", "hint", "claim will only mark, no items granted")
	}

	uc := biz.NewMailUsecase(repo, cfg.Mail, granter)
	if instGranter != nil {
		uc.SetInstanceGranter(instGranter)
	}
	mailSvc := service.NewMailService(uc, sf)

	grpcSrv := server.NewGRPCServer(&cfg, mailSvc)
	httpSrv := server.NewHTTPServer(&cfg)

	helper.Infow("msg", "service_ready", "grpc", cfg.Server.Grpc.Addr, "http", cfg.Server.Http.Addr)

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

func maskDSN(dsn string) string {
	at, colon := -1, -1
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
