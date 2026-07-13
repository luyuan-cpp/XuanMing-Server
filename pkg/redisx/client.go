// Package redisx 提供 Pandora 统一 Redis client 构造。
package redisx

import (
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"

	"github.com/luyuancpp/pandora/pkg/config"
)

// DefaultMaintNotificationsMode 是 Pandora 默认的维护通知模式。
//
// 自建 Redis(本地 / k8s 内 Redis 7.x)不支持 CLIENT MAINT_NOTIFICATIONS,
// go-redis 默认的 auto 模式会在握手失败时打印噪音日志:
//
//	maintnotifications disabled due to handshake error
//
// 默认关闭探测;接 Redis Cloud / Enterprise 时可经
// config.RedisConf.MaintNotifications 显式改为 "auto" / "enabled"。
const DefaultMaintNotificationsMode = maintnotifications.ModeDisabled

// NewClient 按公共 RedisConf 构造 go-redis 客户端。
//
// 维护通知模式由 c.MaintNotifications 配置驱动(留空 = disabled),不再硬编码,
// 既默认消除自建 Redis 的启动噪音,又给云托管 Redis 保留开关。
func NewClient(c config.RedisConf) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:         c.Host,
		Password:     c.Password,
		DB:           int(c.DB),
		DialTimeout:  c.DialTimeout.Std(),
		ReadTimeout:  c.ReadTimeout.Std(),
		WriteTimeout: c.WriteTimeout.Std(),
		MaintNotificationsConfig: &maintnotifications.Config{
			Mode: resolveMaintMode(c.MaintNotifications),
		},
	})
}

// resolveMaintMode 把配置字符串映射成 go-redis 维护通知模式。
// 空串或非法值安全回退到 DefaultMaintNotificationsMode(disabled),不 panic。
func resolveMaintMode(s string) maintnotifications.Mode {
	if m := maintnotifications.Mode(s); m.IsValid() {
		return m
	}
	return DefaultMaintNotificationsMode
}

// NewUniversalClient 按公共 RedisConf 构造 go-redis UniversalClient,自动按拓扑选型:
//
//   - c.Addrs 为空            → 单实例 Client(等价 NewClient,Host 单点)
//   - c.MasterName 非空       → Sentinel FailoverClient(主从故障转移)
//   - c.Addrs 多节点且无 Master → Cluster ClusterClient(分片)
//
// 返回的 redis.UniversalClient 接口与 *redis.Client 同名方法兼容,业务 repo 只依赖接口即可
// 在"单实例 → Cluster"之间切换而不改业务代码。DAU 200万 / 高 CCU 阶段把单 Redis 换成
// Redis Cluster 时,只需在 yaml 填 addrs 列表,无需改 data 层。详见 docs/design/scale-cellular-20m.md。
//
// ⚠️ Cluster 模式下跨 slot 的多键操作 / 事务 / Lua 受限:同一原子操作涉及的 key 必须落同一
// slot(用 {hash_tag} 把同一玩家的相关 key 绑定到同一 slot,如 lock:{player:123})。
// redislock / 多键 ZSET 等改造前必须核对 hash tag,详见 scale-cellular-20m.md 的单 Cell Redis 口径。
func NewUniversalClient(c config.RedisConf) redis.UniversalClient {
	return newUniversalClient(c, false)
}

// NewDeadlineUniversalClient 与 NewUniversalClient 拓扑行为相同，但让 go-redis 把调用方
// context deadline 计入 socket I/O 截止时间。仅应用在分布式锁等“业务声明的硬等待上限”
// 必须真实生效的路径；保留原构造的既有行为，避免一次安全修复暗改所有服务的超时语义。
func NewDeadlineUniversalClient(c config.RedisConf) redis.UniversalClient {
	return newUniversalClient(c, true)
}

func newUniversalClient(c config.RedisConf, contextTimeoutEnabled bool) redis.UniversalClient {
	addrs := c.Addrs
	if len(addrs) == 0 {
		addrs = []string{c.Host}
	}
	return redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:        addrs,
		MasterName:   c.MasterName,
		Password:     c.Password,
		DB:           int(c.DB),
		DialTimeout:  c.DialTimeout.Std(),
		ReadTimeout:  c.ReadTimeout.Std(),
		WriteTimeout: c.WriteTimeout.Std(),
		// 默认 false 保持历史语义；拍卖 market 锁等严格截止路径显式走
		// NewDeadlineUniversalClient，防单次 Redis I/O 越过业务 maxWait/TTL。
		ContextTimeoutEnabled: contextTimeoutEnabled,
		MaintNotificationsConfig: &maintnotifications.Config{
			Mode: resolveMaintMode(c.MaintNotifications),
		},
	})
}
