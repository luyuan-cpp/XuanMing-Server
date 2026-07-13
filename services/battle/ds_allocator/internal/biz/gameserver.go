// gameserver.go — DS pod 分配抽象 + W4 ② Mock 实现。
//
// 真 Agones 实现见 internal/data/agones_allocator.go(W4 ⑫ AgonesGameServerAllocator,
// 经 k8s apiserver REST 调 allocation.agones.dev/v1 GameServerAllocation)。本接口签名
// 保持不变,Mock / Agones 只是两个实现,main 按 agones.enabled 选装配,biz 逻辑零改动。
package biz

import (
	"context"
	"fmt"

	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/data"
)

// GameServerAllocator 向底层编排(W4 ② Mock / W4 ③ Agones)申请/释放一个战斗 DS pod。
type GameServerAllocator interface {
	// Allocate 申请一个战斗 DS,返回 pod 名 + 可连接地址(host:port)。
	Allocate(ctx context.Context, matchID uint64, mapID uint32, gameMode string) (podName, addr string, err error)
	// Release 释放(回收)一个战斗 DS pod。
	Release(ctx context.Context, podName string) error
}

// AuthoritativeGameServerAllocator 是 Agones Model B 的额外能力：分配时先取得实例 UID/RV，
// Redis stage 成功后再用 UID+RV 条件 PATCH 投递 annotation。K8s 仅是投递镜像，不是授权权威。
type AuthoritativeGameServerAllocator interface {
	AllocateAuthoritative(
		ctx context.Context,
		matchID uint64,
		allocationID string,
		mapID uint32,
		gameMode string,
	) (*data.AuthoritativeGameServerAllocation, error)
	DeliverCredential(
		ctx context.Context,
		allocation *data.AuthoritativeGameServerAllocation,
		annotations map[string]string,
	) (confirmedResourceVersion string, err error)
	// ReleaseExpected 用 Kubernetes UID delete precondition 回收本实例；同名 GameServer 已重建
	// 时必须失败且零删除，禁止旧 cleanup 按名字误杀新实例。
	ReleaseExpected(ctx context.Context, allocation *data.AuthoritativeGameServerAllocation) error
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
