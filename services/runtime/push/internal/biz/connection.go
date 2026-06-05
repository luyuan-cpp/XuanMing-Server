// Package biz 是 push 服务的业务逻辑层(usecase)。
//
// 本文件实现 ConnectionManager:player_id → stream 的内存索引。
//
// W3 ④ 真实化:
//   - kafka consumer 收到事件 → 用 manager.SendTo(player_id, frame) 路由
//   - 系统公告类(pandora.system.notify)走 manager.Broadcast
//
// 设计要点(对齐 docs/design/gateway-decision.md):
//   - 一个 player_id 只允许有一个在线 stream(同账号顶号:旧 stream Close,新 stream 替换)
//   - 不变量 §3.1:玩家在线只能在一个 DS(push 服务并发场景同理:同账号一条 push 长连)
//
// W3 ④ 二次修复(Opus 审查):
//   - **gRPC ServerStream.SendMsg 非并发安全**(google.golang.org/grpc 文档明确),
//     KafkaConsumer goroutine 与 RunSubscribeStream replay goroutine 可能同时 Send 同一 stream,
//     HTTP/2 帧编码无锁 → 会撕坏 stream(对端 RST_STREAM / 解码失败)。
//   - 解法:每个 slot 自带 sendMu,SendTo / Broadcast / replay 都走 SafeSend(slot) 串行化。
//     sendMu 不能放 manager.mu(那是保护 map 的,SendTo 持太久会阻塞 Register/Unregister)。
package biz

import (
	"sync"

	grpc "google.golang.org/grpc"

	pushv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/push/v1"
)

// PushStream 是 push 服务对 gRPC server stream 的别名。
//
// 用 grpc.ServerStreamingServer[PushFrame] 是 gRPC v1.62+ 的泛型形态,
// 跟 push_grpc.pb.go 里 Subscribe 的签名一致。
type PushStream = grpc.ServerStreamingServer[pushv1.PushFrame]

// StreamSlot 持有一条玩家 stream + 串行化 Send 的互斥锁。
//
// 暴露 SafeSend(frame) 给所有发送方(KafkaConsumer.SendTo / RunSubscribeStream replay 循环);
// 不要直接 stream.Send,绕过 SafeSend 会引入并发竞态(参见 package 注释)。
//
// 不暴露 Stream 字段读取(避免外部绕开 sendMu),replay 循环需要拿到 slot 时通过 Register 返回值。
type StreamSlot struct {
	stream PushStream
	sendMu sync.Mutex
}

// SafeSend 串行化地往 stream 发一帧。同一 slot 上的所有 SafeSend 严格串行。
func (s *StreamSlot) SafeSend(frame *pushv1.PushFrame) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.Send(frame)
}

// ConnectionManager 维护 player_id → StreamSlot 的索引。
//
// 并发安全(读写锁,仅保护 map 本身;Send 的串行化在 StreamSlot.sendMu)。
type ConnectionManager struct {
	mu       sync.RWMutex
	bySlot   map[uint64]*StreamSlot // key = player_id;value = 该玩家当前 slot
	closeFns map[uint64]func()      // 旧 stream 被顶号时的 close 回调
}

// NewConnectionManager 构造空索引。
func NewConnectionManager() *ConnectionManager {
	return &ConnectionManager{
		bySlot:   make(map[uint64]*StreamSlot),
		closeFns: make(map[uint64]func()),
	}
}

// Register 把 (player_id, stream) 加入索引,返回新建的 slot 给调用方持有。
//
// 若已存在则触发旧的 closeFn(顶号语义)。closeFn 由调用方提供,用于通知旧 Subscribe goroutine
// 主动退出(接 ctx cancel)。调用方应在 Subscribe 阻塞结束后调 Unregister 反注册。
//
// 返回的 *StreamSlot 用于 RunSubscribeStream replay 循环 — 与 KafkaConsumer.SendTo 共享 sendMu。
func (m *ConnectionManager) Register(playerID uint64, stream PushStream, closeFn func()) *StreamSlot {
	slot := &StreamSlot{stream: stream}

	m.mu.Lock()
	defer m.mu.Unlock()

	if oldClose, exists := m.closeFns[playerID]; exists && oldClose != nil {
		// 顶号:先通知旧 stream 退出
		oldClose()
	}
	m.bySlot[playerID] = slot
	m.closeFns[playerID] = closeFn
	return slot
}

// Unregister 把 player_id 从索引中移除(仅当当前 slot 等于传入的 slot 时才移除,
// 避免顶号场景下新 stream 把自己的位置删掉)。
func (m *ConnectionManager) Unregister(playerID uint64, slot *StreamSlot) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if cur, ok := m.bySlot[playerID]; ok && cur == slot {
		delete(m.bySlot, playerID)
		delete(m.closeFns, playerID)
	}
}

// SendTo 给指定 player 发送一帧 PushFrame(走 slot.SafeSend 串行化)。
// 玩家不在线返回 (false, nil)(由调用方决定写离线缓存还是丢弃)。
//
// KafkaConsumer 路由 push 消息时调本方法。
func (m *ConnectionManager) SendTo(playerID uint64, frame *pushv1.PushFrame) (bool, error) {
	m.mu.RLock()
	slot, ok := m.bySlot[playerID]
	m.mu.RUnlock()

	if !ok {
		return false, nil
	}
	if err := slot.SafeSend(frame); err != nil {
		return true, err
	}
	return true, nil
}

// Broadcast 给所有在线玩家发送一帧(系统公告类用)。
// 返回成功发送数 + 失败数(失败按 stream 计,本方法不打日志)。
//
// 每个 slot 各自 Lock 调 SafeSend,不会阻塞 manager.mu。
func (m *ConnectionManager) Broadcast(frame *pushv1.PushFrame) (sent int, failed int) {
	m.mu.RLock()
	// 快照一份 slice,避免长时间持锁
	slots := make([]*StreamSlot, 0, len(m.bySlot))
	for _, s := range m.bySlot {
		slots = append(slots, s)
	}
	m.mu.RUnlock()

	for _, s := range slots {
		if err := s.SafeSend(frame); err != nil {
			failed++
		} else {
			sent++
		}
	}
	return
}

// Size 当前在线 stream 数(给 /metrics + 调试用)。
func (m *ConnectionManager) Size() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.bySlot)
}
