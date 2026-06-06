// gameserver.go — DS pod 分配抽象 + W4 ② Mock 实现。
//
// W4 ③+ 接 Agones:实现一个 AgonesGameServerAllocator,调 K8s API
// GameServerAllocation CRD(allocation.agones.dev/v1),从返回的 status 取 pod 名 +
// address:port,Release 时给 GameServer 打 label 让 Agones 回收。本接口签名保持不变,
// 只换实现 + main 装配,biz 逻辑零改动。
package biz

import (
	"context"
	"fmt"

	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
)

// GameServerAllocator 向底层编排(W4 ② Mock / W4 ③ Agones)申请/释放一个战斗 DS pod。
type GameServerAllocator interface {
	// Allocate 申请一个战斗 DS,返回 pod 名 + 可连接地址(host:port)。
	Allocate(ctx context.Context, matchID uint64, mapID uint32, gameMode string) (podName, addr string, err error)
	// Release 释放(回收)一个战斗 DS pod。
	Release(ctx context.Context, podName string) error
}

// MockGameServerAllocator 是 W4 ② 的打桩实现:不连 k8s,按 match_id 计算确定性假地址。
//
// 端口 = MockDSPortBase + (matchID % MockDSPortRange),保证同 match 多次分配地址稳定
// (幂等场景下 biz 会先查镜像不重复 Allocate,这里只保证可复现)。
type MockGameServerAllocator struct {
	cfg conf.AllocatorConf
}

// NewMockGameServerAllocator 构造 Mock 分配器。
func NewMockGameServerAllocator(cfg conf.AllocatorConf) *MockGameServerAllocator {
	return &MockGameServerAllocator{cfg: cfg}
}

// Allocate 返回确定性假 pod / addr。
func (m *MockGameServerAllocator) Allocate(_ context.Context, matchID uint64, _ uint32, _ string) (string, string, error) {
	port := m.cfg.MockDSPortBase + int(matchID%uint64(m.cfg.MockDSPortRange))
	podName := fmt.Sprintf("pandora-battle-%d", matchID)
	addr := fmt.Sprintf("%s:%d", m.cfg.MockDSAddrHost, port)
	return podName, addr, nil
}

// Release 对 Mock 无操作(无真实 pod 可回收)。
func (m *MockGameServerAllocator) Release(_ context.Context, _ string) error {
	return nil
}
