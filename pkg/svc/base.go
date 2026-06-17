// Package svc 提供 Pandora 服务的通用 ServiceContext 模板。
//
// 各业务服务的 internal/svc/servicecontext.go 嵌入 BaseContext + 加业务字段。
//
// 用法:
//
//	type ServiceContext struct {
//	    *svc.BaseContext             // 公共:Redis / Snowflake / RedisLocker / KafkaProducer
//	    PlayerLocatorClient plpb.PlayerLocatorClient  // 业务私有
//	    MyBusinessHandler   *myHandler
//	}
//
//	func NewServiceContext(c config.Config) *ServiceContext {
//	    base := svc.MustNewBaseContext(c.Base)
//	    return &ServiceContext{
//	        BaseContext:         base,
//	        PlayerLocatorClient: plpb.NewPlayerLocatorClient(grpcclient.MustNewClient(...).Conn()),
//	    }
//	}
package svc

import (
	klog "github.com/go-kratos/kratos/v2/log"
	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/killswitch"
	"github.com/luyuancpp/pandora/pkg/redislock"
	"github.com/luyuancpp/pandora/pkg/redisx"
	"github.com/luyuancpp/pandora/pkg/snowflake"
)

// BaseContext 是所有 Pandora 服务共享的运行时上下文。
type BaseContext struct {
	// RedisClient 是该服务的主 Redis 客户端(对应 config.Node.RedisClient)。
	RedisClient *redis.Client

	// Snowflake 是该服务用的 ID 生成器,NodeID 取 config.Node.ZoneId。
	Snowflake *snowflake.Node

	// Locker 是 Redis 分布式锁实例(用 pandora:lock: 前缀)。
	Locker *redislock.RedisLocker

	// KillSwitch 是 RPC 级临时关停的开关源,进程退出时需 Close。
	KillSwitch killswitch.Source

	// Cfg 是公共配置的副本。
	Cfg config.Base
}

// MustNewBaseContext 用 config.Base 构造 BaseContext。失败 panic。
//
// 调用前必须已经 log.Setup() 过(初始化 logx)。
func MustNewBaseContext(c config.Base) *BaseContext {
	// 1. Redis client
	rdb := redisx.NewClient(c.Node.RedisClient)

	// 2. Snowflake
	sf := snowflake.NewNode(uint64(c.Node.ZoneId))

	// 3. Locker
	lk := redislock.NewRedisLocker(rdb)

	// 4. Kill-Switch(RPC 级临时关停)。fail-open:配置缺失 / 源建不起来都不阻断启动。
	ks := mustSetupKillSwitch(c.KillSwitch)

	klog.Infof("[svc] BaseContext ready zone=%d redis=%s", c.Node.ZoneId, c.Node.RedisClient.Host)

	return &BaseContext{
		RedisClient: rdb,
		Snowflake:   sf,
		Locker:      lk,
		KillSwitch:  ks,
		Cfg:         c,
	}
}

// mustSetupKillSwitch 把 config.KillSwitchConf 翻译成 killswitch.Config 并启动开关源。
//
// FailClosed=false(默认)时:即便源建不起来也只 Warn 放行,返回非 nil 的 noop Source;
// FailClosed=true 时:源建不起来直接 panic(要求开关系统必须在线的场景)。
func mustSetupKillSwitch(c config.KillSwitchConf) killswitch.Source {
	cfg := killswitch.Config{
		Enabled:         c.Enabled,
		Source:          c.Source,
		FilePath:        c.FilePath,
		EtcdEndpoints:   c.EtcdEndpoints,
		EtcdPrefix:      c.EtcdPrefix,
		EtcdDialTimeout: c.EtcdDialTimeout.Std(),
		FailOpen:        !c.FailClosed,
	}
	src, err := killswitch.Setup(cfg)
	if err != nil {
		// 只有 FailClosed=true 时 Setup 才会返回 err。
		panic("svc.MustNewBaseContext killswitch setup: " + err.Error())
	}
	return src
}

// Close 关闭 BaseContext 下属资源。业务的 ServiceContext.Close() 应调用本方法。
func (b *BaseContext) Close() error {
	if b.KillSwitch != nil {
		if err := b.KillSwitch.Close(); err != nil {
			klog.Errorf("[svc] killswitch close: %v", err)
		}
	}
	if b.RedisClient != nil {
		if err := b.RedisClient.Close(); err != nil {
			klog.Errorf("[svc] redis close: %v", err)
		}
	}
	return nil
}
