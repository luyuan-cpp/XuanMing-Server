// Package conf 是 push 服务的私有配置结构。
//
// 内嵌 pkg/config.Base 拿公共字段,再加 push 自有字段。
//
// 加载方式(见 cmd/push/main.go):
//
//	c := kconfig.New(kconfig.WithSource(file.NewSource("./etc/push-dev.yaml")))
//	c.Load()
//	var cfg conf.Config
//	c.Scan(&cfg)
package conf

import (
	"time"

	"github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/kafkax"
)

// Config 是 push 服务的完整配置。
type Config struct {
	// Base 公共字段(Server/Node/Snowflake/Locker/Registry/Timeouts/Kafka)。
	config.Base `yaml:",inline" mapstructure:",squash"`

	// Push 业务字段。
	Push PushConf `yaml:"push" json:"push"`
}

// PushConf 是 push 服务私有配置。
//
// W3 ④(2026-06-05)真实化:
//   - 删除 mock_* 字段(mock tick 退役),改为 kafka consumer 真实推送
//   - 新增 Topics:订阅的 push topic 列表,默认 kafkax.PushTopics(3 个 W3 ④ 启用)
//   - 保留 OfflineCacheTTL:在线离线切换补推用 redis ZSET TTL,默认 5min
type PushConf struct {
	// Topics 订阅的 kafka topic 列表;空时用 kafkax.PushTopics 默认值。
	//
	// 每个 topic 一个独立的 KafkaConsumer,共享 cfg.Kafka.GroupID。
	// 业务侧 producer 用 kafkax.PushToPlayers helper 发送,key=player_id。
	Topics []string `yaml:"topics,omitempty" json:"topics,omitempty"`

	// OfflineCacheTTL 离线消息缓存 redis ZSET 的 TTL,默认 5min。
	//
	// 玩家不在线时 kafka 消息暂存到 pandora:push:offline:<player_id> ZSET
	// (score=ts_ms, member=PushFrame proto bytes);重连时按 last_seen_ms 补推。
	OfflineCacheTTL config.Duration `yaml:"offline_cache_ttl,omitempty" json:"offline_cache_ttl,omitempty"`
}

// Defaults 把零值填成 Pandora 标准默认值。
func (c *Config) Defaults() {
	if len(c.Push.Topics) == 0 {
		c.Push.Topics = append([]string(nil), kafkax.PushTopics...)
	}
	if c.Push.OfflineCacheTTL == 0 {
		c.Push.OfflineCacheTTL = config.Duration(5 * time.Minute)
	}
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50014"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51014"
	}
	if c.Kafka.GroupID == "" {
		c.Kafka.GroupID = "pandora-push"
	}
}
