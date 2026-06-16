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
)

// StubDSAllocator 是 DSAllocator 的打桩实现(W4 ①)。
type StubDSAllocator struct {
	// MockAddr 是返回的固定战斗 DS 地址(dev 联调用)。
	MockAddr string
}

// NewStubDSAllocator 构造打桩分配器。addr 为空时用占位地址。
func NewStubDSAllocator(addr string) *StubDSAllocator {
	if addr == "" {
		addr = "127.0.0.1:7777"
	}
	return &StubDSAllocator{MockAddr: addr}
}

// AllocateBattle 返回固定地址 + 每个玩家一个 mock 票据(matchID-playerID)。
func (s *StubDSAllocator) AllocateBattle(_ context.Context, matchID uint64, playerIDs []uint64) (string, map[uint64]string, error) {
	tickets := make(map[uint64]string, len(playerIDs))
	for _, pid := range playerIDs {
		tickets[pid] = fmt.Sprintf("mock-ticket-%d-%d", matchID, pid)
	}
	return s.MockAddr, tickets, nil
}

// SignBattleTicket 桩：返回带纳秒后缀的 mock 票，模拟“每次新 jti”。实现 biz.DSAllocator。
func (s *StubDSAllocator) SignBattleTicket(_ context.Context, playerID, matchID uint64) (string, error) {
	return fmt.Sprintf("mock-ticket-%d-%d-%d", matchID, playerID, time.Now().UnixNano()), nil
}
