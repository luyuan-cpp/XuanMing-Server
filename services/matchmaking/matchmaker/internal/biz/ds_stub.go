// ds_stub.go — W4 ① 的 DSAllocator 打桩实现。
//
// W4 ② ds_allocator 服务上线后,替换为 gRPC 调用 ds_allocator.AllocateBattle
// (Agones GameServerAllocation)。本桩仅返回固定 mock 地址 + 每玩家 mock 票据,
// 让撮合流水线 QUEUEING→FOUND→CONFIRM→READY 全链路可端到端跑通。
package biz

import (
	"context"
	"fmt"
	"time"

	"github.com/luyuancpp/pandora/pkg/placement"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/model"
)

// StubDSAllocator 是 DSAllocator 的打桩实现(W4 ①)。
type StubDSAllocator struct {
	// MockAddr 是返回的固定战斗 DS 地址(dev 联调用)。
	MockAddr string
}

// StubPlacementCoordinator 仅供没有真实 ds_allocator/player_locator 的本地联调。
// production main 在配置真实 allocator 时禁止使用此桩。
type StubPlacementCoordinator struct{}

func (StubPlacementCoordinator) RequireStableHub(context.Context, []uint64) error { return nil }

func (StubPlacementCoordinator) PreflightBattlePlacement(context.Context, string, uint64, []uint64) error {
	return nil
}

func (StubPlacementCoordinator) PrepareBattlePlacement(_ context.Context, operationID string, _ uint64, playerIDs []uint64, _ *model.BattleAllocation) (map[uint64]placement.Binding, error) {
	bindings := make(map[uint64]placement.Binding, len(playerIDs))
	for _, playerID := range playerIDs {
		bindings[playerID] = placement.Binding{Version: 1, OperationID: operationID}
	}
	return bindings, nil
}

// NewStubDSAllocator 构造打桩分配器。addr 为空时用占位地址。
func NewStubDSAllocator(addr string) *StubDSAllocator {
	if addr == "" {
		addr = "127.0.0.1:7777"
	}
	return &StubDSAllocator{MockAddr: addr}
}

// AllocateBattle 返回固定地址 + 每个玩家一个 mock 票据(matchID-playerID)。mapID 桩里忽略。
func (s *StubDSAllocator) AllocateBattle(_ context.Context, matchID uint64, playerIDs []uint64, _ uint32) (*model.BattleAllocation, error) {
	return &model.BattleAllocation{
		Address: s.MockAddr,
		Target: placement.Target{
			PodName:       fmt.Sprintf("mock-battle-%d", matchID),
			InstanceUID:   fmt.Sprintf("mock-uid-%d", matchID),
			InstanceEpoch: 1,
			AllocationID:  fmt.Sprintf("mock-allocation-%d", matchID),
			ReleaseTrack:  "stable",
		},
	}, nil
}

func (s *StubDSAllocator) AbortBattleAllocation(context.Context, uint64, string, *model.BattleAllocation) error {
	return nil
}

func (s *StubDSAllocator) SignBattleTickets(_ context.Context, matchID uint64, playerIDs []uint64, _ *model.BattleAllocation, _ map[uint64]placement.Binding) (map[uint64]string, error) {
	tickets := make(map[uint64]string, len(playerIDs))
	for _, pid := range playerIDs {
		tickets[pid] = fmt.Sprintf("mock-ticket-%d-%d", matchID, pid)
	}
	return tickets, nil
}

// SignBattleTicket 桩：返回带纳秒后缀的 mock 票，模拟“每次新 jti”。实现 biz.DSAllocator。
func (s *StubDSAllocator) SignBattleTicket(_ context.Context, playerID, matchID uint64, _ *model.BattleAllocation, _ placement.Binding) (string, error) {
	return fmt.Sprintf("mock-ticket-%d-%d-%d", matchID, playerID, time.Now().UnixNano()), nil
}
