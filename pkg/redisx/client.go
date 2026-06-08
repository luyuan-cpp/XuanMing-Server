// Package redisx 提供 Pandora 统一 Redis client 构造。
package redisx

import (
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"

	"github.com/luyuancpp/pandora/pkg/config"
)

// NewClient 按公共 RedisConf 构造 go-redis 客户端。
//
// 本地 Redis 7.4 不支持 CLIENT MAINT_NOTIFICATIONS,go-redis auto 模式会打印
// fallback 噪音日志。Pandora 本地/自建 Redis 当前不依赖该云厂商维护通知能力,统一禁用探测。
func NewClient(c config.RedisConf) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:         c.Host,
		Password:     c.Password,
		DB:           int(c.DB),
		DialTimeout:  c.DialTimeout.Std(),
		ReadTimeout:  c.ReadTimeout.Std(),
		WriteTimeout: c.WriteTimeout.Std(),
		MaintNotificationsConfig: &maintnotifications.Config{
			Mode: maintnotifications.ModeDisabled,
		},
	})
}
