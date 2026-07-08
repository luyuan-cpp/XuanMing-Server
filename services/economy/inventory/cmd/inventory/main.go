// Pandora inventory 服务入口(背包 / 经济,W5 ③ 2026-06-18)。
//
// 职责:玩家货币 + 背包道具持久化(pandora_trade 库,强依赖);大厅态道具使用 / 出售
// 走事务 + 幂等键(不变量 §9.7);战斗内即时道具走 UE GAS,不经本服务(ds-arch §0.1)。
//
// 启动顺序(对齐 player):
//  1. Logger
//  2. 加载 yaml → conf.Defaults
//  3. MySQL client + 隐含 Ping(强依赖:背包落库不可降级)
//  4. 装配 InventoryUsecase → InventoryService → gRPC/HTTP server
//  5. kratos.New(...).Run() 阻塞
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

	"github.com/luyuancpp/pandora/pkg/cellroute/etcdtable"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/mysqlx"
	"github.com/luyuancpp/pandora/pkg/snowflake/etcdnode"

	"github.com/luyuancpp/pandora/services/economy/inventory/internal/biz"
	"github.com/luyuancpp/pandora/services/economy/inventory/internal/conf"
	"github.com/luyuancpp/pandora/services/economy/inventory/internal/data"
	"github.com/luyuancpp/pandora/services/economy/inventory/internal/server"
	"github.com/luyuancpp/pandora/services/economy/inventory/internal/service"
)

const serviceName = "inventory"

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "etc/inventory-dev.yaml", "config file path")
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

	// 校验道具规则表(可出售必须单价 > 0;非法配置 fail-fast,避免上线后负价扣币)。
	if verr := cfg.Inventory.Validate(); verr != nil {
		helper.Errorw("msg", "inventory_item_rules_invalid", "err", verr)
		os.Exit(1)
	}

	// 3. MySQL(强依赖:背包 / 货币落库不可降级)
	if cfg.Node.MySQLClient.DSN == "" {
		helper.Errorw("msg", "mysql_dsn_required", "hint", "node.mysql_client.dsn required (pandora_trade)")
		os.Exit(1)
	}
	db := mysqlx.MustNewClient(cfg.Node.MySQLClient)
	defer func() { _ = db.Close() }()
	helper.Infow("msg", "mysql_connected", "dsn", maskDSN(cfg.Node.MySQLClient.DSN))

	// 4. 装配链
	repo := data.NewMySQLInventoryRepo(db)
	uc := biz.NewInventoryUsecase(repo, cfg.Inventory)

	// Snowflake(instance_id 生成,W5 ④):仅在启用实例背包(capacity>0)时装配。
	// node_id_source=static 静态,=etcd 走 etcd 自动抢占,失租自动退出。
	if cfg.Inventory.Capacity > 0 {
		// 启动期 schema 检查(2026-07-08):player_item_instance 是后补的表,既有 MySQL
		// volume / PVC 不会自动重放 init SQL;缺表时实例背包全链路必炸。fail-fast 并指向迁移 SQL。
		// 未启用(capacity<=0)不检也不读该表(biz.GetInventoryFull 同步跳过),旧库升级不受影响。
		schemaCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if serr := mysqlx.CheckTables(schemaCtx, db, "deploy/mysql-init/08-inventory-tables.sql", "player_item_instance"); serr != nil {
			cancel()
			helper.Errorw("msg", "mysql_schema_check_failed", "err", serr)
			os.Exit(1)
		}
		cancel()

		sf, sfCloser := etcdnode.MustProvideSnowflake(serviceName, cfg.Node.NodeId, cfg.Snowflake)
		defer func() { _ = sfCloser.Close() }()
		uc.SetSnowflake(sf)
		helper.Infow("msg", "instance_bag_enabled", "capacity", cfg.Inventory.Capacity, "identify_rules", len(cfg.Inventory.IdentifyRules))
	}
	if closeCell, e := etcdtable.WireRouter(context.Background(), cfg.CellRoute, uc.SetCellRouter); e != nil {
		helper.Errorw("msg", "cellroute_init_failed", "err", e)
		os.Exit(1)
	} else if closeCell != nil {
		defer func() { _ = closeCell() }()
	}
	svc := service.NewInventoryService(uc)

	grpcSrv := server.NewGRPCServer(&cfg, svc)
	httpSrv := server.NewHTTPServer(&cfg)

	helper.Infow(
		"msg", "service_ready",
		"grpc", cfg.Server.Grpc.Addr,
		"http", cfg.Server.Http.Addr,
		"item_rules", len(cfg.Inventory.ItemRules),
	)

	// 5. Kratos App
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

// maskDSN 脱敏 DSN 里的密码(对齐 player / trade main.go)。
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
