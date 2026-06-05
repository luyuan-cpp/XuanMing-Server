// Pandora login 服务入口。
//
// 启动顺序:
//  1. 解析 -conf 路径,加载 yaml(Kratos config + file source)
//  2. 填默认值(conf.Defaults)
//  3. log.Setup → 全局 zap logger
//  4. data layer + biz usecase + service 三层构造
//  5. gRPC + HTTP server 注册
//  6. kratos.New(...).Run() 阻塞
//
// 信号处理:Kratos App 默认监听 SIGINT/SIGTERM,优雅 stop server。
//
// W3 ②(2026-06-05):
//   - cfg.Node.MySQLClient.DSN 非空时,接 MySQL(NewMySQLAccountRepo + SeedAccount 种 dev 账号)
//   - cfg.Node.RedisClient.Host 非空时,接 Redis(NewRedisSessionRepo + NewRedisTicketJTIRepo)
//   - 任一为空 → fallback 到 MockAccountRepo / 无 SessionRepo / 无 jtiRepo
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

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/grpcclient"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/mysqlx"
	"github.com/luyuancpp/pandora/pkg/passwd"
	"github.com/luyuancpp/pandora/pkg/snowflake"

	"github.com/luyuancpp/pandora/services/account/login/internal/biz"
	"github.com/luyuancpp/pandora/services/account/login/internal/conf"
	"github.com/luyuancpp/pandora/services/account/login/internal/data"
	"github.com/luyuancpp/pandora/services/account/login/internal/server"
	"github.com/luyuancpp/pandora/services/account/login/internal/service"
)

const serviceName = "login"

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "etc/login-dev.yaml", "config file path")
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

	// 3. snowflake
	sf := snowflake.NewNode(uint64(cfg.Node.ZoneId))

	// 4. JWT signer / verifier
	authCfg := auth.Config{
		Issuer:      cfg.Login.JWT.Issuer,
		Audience:    cfg.Login.JWT.Audience,
		Secret:      []byte(cfg.Login.JWT.Secret),
		SessionTTL:  cfg.Login.JWT.SessionTTL.Std(),
		DSTicketTTL: cfg.Login.JWT.DSTicketTTL.Std(),
	}
	signer, err := auth.NewSigner(authCfg)
	if err != nil {
		helper.Errorw("msg", "auth_signer_init_failed", "err", err)
		os.Exit(1)
	}
	verifier, err := auth.NewVerifier(authCfg)
	if err != nil {
		helper.Errorw("msg", "auth_verifier_init_failed", "err", err)
		os.Exit(1)
	}

	// 5. data 层装配
	accountRepo, mode, db := mustBuildAccountRepo(&cfg, helper, sf)
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()

	sessionRepo, jtiRepo, rdb := mustBuildRedisRepos(&cfg, helper)
	defer func() {
		if rdb != nil {
			_ = rdb.Close()
		}
	}()

	// locator 客户端(W3 ⑤):addr 为空 → 跳过,Login 仅 Warn 日志
	locatorNotifier, locatorConn, locatorMode := mustBuildLocatorNotifier(&cfg, helper)
	defer func() {
		if locatorConn != nil {
			_ = locatorConn.Close()
		}
	}()

	// 6. biz + service 装配
	loginUC := biz.NewLoginUsecase(accountRepo, sessionRepo, locatorNotifier, sf, cfg.Login.MockHubDSAddr, signer, verifier)
	ticketUC := biz.NewTicketUsecase(signer, verifier, jtiRepo)
	svc := service.NewLoginService(loginUC, ticketUC)

	// 7. gRPC + HTTP server
	grpcSrv := server.NewGRPCServer(&cfg, svc)
	httpSrv := server.NewHTTPServer(&cfg, svc)

	helper.Infow(
		"msg", "service_ready",
		"grpc", cfg.Server.Grpc.Addr,
		"http", cfg.Server.Http.Addr,
		"account_repo", mode,
		"session_repo", repoEnabled(sessionRepo != nil),
		"jti_repo", repoEnabled(jtiRepo != nil),
		"locator_notifier", locatorMode,
		"jwt_issuer", cfg.Login.JWT.Issuer,
		"jwt_audience", cfg.Login.JWT.Audience,
		"jwt_session_ttl", cfg.Login.JWT.SessionTTL.String(),
		"jwt_ds_ticket_ttl", cfg.Login.JWT.DSTicketTTL.String(),
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

// mustBuildAccountRepo 按 cfg 决定 MySQL or Mock,失败 panic。
// 返回 (repo, "mysql"|"mock", *sql.DB nullable)。
func mustBuildAccountRepo(cfg *conf.Config, h kratosHelper, sf *snowflake.Node) (data.AccountRepo, string, sqlDBLike) {
	if cfg.Node.MySQLClient.DSN == "" {
		// fallback mock
		mockPlayerID := int64(sf.Generate())
		bcryptHash, err := passwd.Hash(cfg.Login.MockPasswordHash, passwd.DevCost)
		if err != nil {
			h.Errorw("msg", "mock_hash_failed", "err", err)
			os.Exit(1)
		}
		h.Infow("msg", "account_repo_mock", "account", cfg.Login.MockAccount, "player_id", mockPlayerID)
		return data.NewMockAccountRepo(cfg.Login.MockAccount, bcryptHash, mockPlayerID), "mock", nil
	}

	db, err := mysqlx.NewClient(cfg.Node.MySQLClient)
	if err != nil {
		h.Errorw("msg", "mysql_init_failed", "err", err, "dsn_masked", maskDSN(cfg.Node.MySQLClient.DSN))
		os.Exit(1)
	}

	// W3 ②:dev 模式如果配了 mock_account,自动种子(便于不手 INSERT 就能联调)
	if cfg.Login.MockAccount != "" && cfg.Login.MockPasswordHash != "" {
		bcryptHash, herr := passwd.Hash(cfg.Login.MockPasswordHash, passwd.DevCost)
		if herr != nil {
			h.Errorw("msg", "seed_hash_failed", "err", herr)
			os.Exit(1)
		}
		seedID := int64(sf.Generate())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		realID, created, serr := data.SeedAccount(ctx, db, cfg.Login.MockAccount, bcryptHash, seedID)
		cancel()
		if serr != nil {
			h.Errorw("msg", "seed_account_failed", "err", serr)
			os.Exit(1)
		}
		h.Infow("msg", "account_seed_done",
			"account", cfg.Login.MockAccount, "player_id", realID, "created", created)
	}

	h.Infow("msg", "account_repo_mysql", "dsn_masked", maskDSN(cfg.Node.MySQLClient.DSN))
	return data.NewMySQLAccountRepo(db), "mysql", db
}

// mustBuildRedisRepos 按 cfg 决定是否启 Redis Session / JTI repo。
// host 为空时跳过(测试 / mock 模式)。redis 初始化失败 → panic。
func mustBuildRedisRepos(cfg *conf.Config, h kratosHelper) (data.SessionRepo, data.TicketJTIRepo, *redis.Client) {
	rc := cfg.Node.RedisClient
	if rc.Host == "" {
		h.Warnw("msg", "redis_disabled_in_config")
		return nil, nil, nil
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:         rc.Host,
		Password:     rc.Password,
		DB:           int(rc.DB),
		DialTimeout:  rc.DialTimeout.Std(),
		ReadTimeout:  rc.ReadTimeout.Std(),
		WriteTimeout: rc.WriteTimeout.Std(),
	})
	// 启动期 ping 一次,确保 redis 可达;失败致命(login 不可降级)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		h.Errorw("msg", "redis_ping_failed", "err", err, "addr", rc.Host)
		os.Exit(1)
	}
	h.Infow("msg", "redis_connected", "addr", rc.Host, "db", rc.DB)
	return data.NewRedisSessionRepo(rdb), data.NewRedisTicketJTIRepo(rdb), rdb
}

// mustBuildLocatorNotifier 按 cfg.Login.Locator.Addr 决定是否拨号到 player_locator。
// addr 空 → 返回 nil notifier(Login 仅 Warn,不阻断);
// 拨号失败 → panic(注意:grpcclient.MustDialInsecure 内部 panic,这里语义一致)。
func mustBuildLocatorNotifier(cfg *conf.Config, h kratosHelper) (data.LocationNotifier, locatorConnLike, string) {
	addr := cfg.Login.Locator.Addr
	if addr == "" {
		h.Warnw("msg", "locator_disabled_in_config",
			"hint", "set login.locator.addr to 127.0.0.1:50006 to enable LOGIN_PENDING upsert")
		return nil, nil, "disabled"
	}
	conn := grpcclient.MustDialInsecure(addr)
	h.Infow("msg", "locator_dial_ok", "addr", addr)
	return data.NewGrpcLocationNotifier(conn), conn, "grpc"
}

// kratosHelper 是 *klog.Helper 的简化接口,避免 main.go 导出泛型。
type kratosHelper interface {
	Infow(keyvals ...interface{})
	Warnw(keyvals ...interface{})
	Errorw(keyvals ...interface{})
}

// sqlDBLike 给 mustBuildAccountRepo 返回 *sql.DB(可能为 nil)的占位,Close() 由 defer 统一。
type sqlDBLike = interface {
	Close() error
}

// locatorConnLike 给 mustBuildLocatorNotifier 返回 *grpc.ClientConn(可能为 nil)的占位,Close() 由 defer 统一。
type locatorConnLike = interface {
	Close() error
}

func repoEnabled(b bool) string {
	if b {
		return "redis"
	}
	return "disabled"
}

// maskDSN 把 user:password 段脱敏,只保留 host 信息便于日志诊断。
func maskDSN(dsn string) string {
	// 形如:user:password@tcp(host:port)/db?...
	// 简易处理:截到 '@' 替换前缀为 ***
	for i := 0; i < len(dsn); i++ {
		if dsn[i] == '@' {
			return "***@" + dsn[i+1:]
		}
	}
	return dsn
}
