// fleet.go — Hub DS Fleet 分片拓扑抽象 + W4 ⑤ Mock 实现。
//
// W4+ 接 Agones:实现一个 AgonesHubFleetProvider,查 Agones Fleet/GameServer 列表
// (label region=...),把 Ready 的 GameServer 映射成 ShardCandidate。本接口签名保持不变,
// 只换实现 + main 装配,biz 逻辑零改动。
package biz

import (
	"context"
	"fmt"

	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/conf"
)

// ShardCandidate 是一个候选大厅 DS 分片(拓扑信息,不含实时负载)。
// 实时 player_count / state 以 Redis 分片镜像为准。
type ShardCandidate struct {
	PodName  string
	Addr     string
	Region   string
	ShardID  uint32
	Capacity int32
}

// HubFleetProvider 返回某 region 的候选分片拓扑(W4 ⑤ Mock / W4+ Agones Fleet)。
type HubFleetProvider interface {
	// ListShards 返回 region 下的全部候选分片(静态拓扑,不含实时负载)。
	ListShards(ctx context.Context, region string) ([]ShardCandidate, error)
}

// MockHubFleetProvider 是 W4 ⑤ 的打桩实现:不连 k8s,按 region 生成 MockShardCount 个确定性假分片。
//
// pod   = pandora-hub-<region>-<i>(i 从 1 起)
// addr  = MockHubAddrHost:(MockHubPortBase + i)
// 保证同 region 多次列举分片拓扑稳定(实时负载在 Redis,这里只给拓扑)。
type MockHubFleetProvider struct {
	cfg conf.HubConf
}

// NewMockHubFleetProvider 构造 Mock 分片拓扑提供者。
func NewMockHubFleetProvider(cfg conf.HubConf) *MockHubFleetProvider {
	return &MockHubFleetProvider{cfg: cfg}
}

// ListShards 返回 region 的确定性假分片拓扑。
func (m *MockHubFleetProvider) ListShards(_ context.Context, region string) ([]ShardCandidate, error) {
	out := make([]ShardCandidate, 0, m.cfg.MockShardCount)
	for i := 1; i <= m.cfg.MockShardCount; i++ {
		out = append(out, ShardCandidate{
			PodName:  fmt.Sprintf("pandora-hub-%s-%d", region, i),
			Addr:     fmt.Sprintf("%s:%d", m.cfg.MockHubAddrHost, m.cfg.MockHubPortBase+i),
			Region:   region,
			ShardID:  uint32(i),
			Capacity: m.cfg.DefaultCapacity,
		})
	}
	return out, nil
}
