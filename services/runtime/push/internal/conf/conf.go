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
)

// Config 是 push 服务的完整配置。
type Config struct {
	// Base 公共字段(Server/Node/Snowflake/Locker/Registry/Timeouts/Kafka)。
	config.Base `yaml:",inline" mapstructure:",squash"`

	// Push 业务字段。
	Push PushConf `yaml:"push" json:"push"`
}

// PushConf 是 push 服务私有配置。
type PushConf struct {
	// MockTickInterval W2 mock 阶段每个 Subscribe stream 的推送间隔。
	// W3 ⑥(2026-06-05):字段改用 config.Duration,etc yaml 可写 "5s" 字符串。
	MockTickInterval config.Duration `yaml:"mock_tick_interval,omitempty" json:"mock_tick_interval,omitempty"`

	// MockTopic W2 mock 推送的 PushFrame.topic 字段。
	// 默认 "pandora.system.notify"(infra.md §4 推送 topic 之一)。
	MockTopic string `yaml:"mock_topic,omitempty" json:"mock_topic,omitempty"`

	// MockPayload W2 mock 推送的 PushFrame.payload 字段(原样转字节)。
	// 默认 "hello"。W3 接 kafka 后,payload 是业务 Event message 的 protobuf 序列化字节。
	MockPayload string `yaml:"mock_payload,omitempty" json:"mock_payload,omitempty"`

	// OfflineCacheTTL 离线消息缓存 redis ZSET 的 TTL(W2 不用,W3 真实化时启用)。
	OfflineCacheTTL config.Duration `yaml:"offline_cache_ttl,omitempty" json:"offline_cache_ttl,omitempty"`
}

// Defaults 把零值填成 Pandora 标准默认值(W2 mock 阶段用)。
func (c *Config) Defaults() {
	if c.Push.MockTickInterval == 0 {
		c.Push.MockTickInterval = config.Duration(5 * time.Second)
	}
	if c.Push.MockTopic == "" {
		c.Push.MockTopic = "pandora.system.notify"
	}
	if c.Push.MockPayload == "" {
		c.Push.MockPayload = "hello"
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
}
