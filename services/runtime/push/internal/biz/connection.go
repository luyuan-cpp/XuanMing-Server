// connection.go — 玩家长连接索引(2026-07-22 审计 v2:拉取式投递)。
//
// 投递模型:Redis 投递缓冲是唯一定序与投递权威(data/offline.go)。连接写者
// (RunSubscribeStream)按「本地唤醒信号 + 定时轮询」从缓冲 Range(>游标) 拉取投递:
//   - 本 Pod 消费到该玩家消息 → SendTo 置一个唤醒信号(不传帧,帧已在缓冲);
//   - **其他 Pod** 消费到(滚动重叠 / 跨 topic consumer 落位不同)→ 本 Pod 收不到
//     信号,由写者的定时轮询(默认 1s)兜底拉到 —— 在线客户端不再依赖断线重连
//     才能看到跨 Pod 写入(审计 P1)。
//
// 单写者不变:每条 stream 只有写者 goroutine 调 stream.Send;SendTo/Broadcast 都是
// 非阻塞投递(信号 size-1 去重;广播箱有界,满则丢弃计数——广播本就丢失容忍),
// 慢客户端最多卡住自己的写者 goroutine,绝不阻塞 Kafka 分区 handler。
// 慢客户端的 stream.Send 阻塞由 gRPC keepalive/连接生命周期收敛(写者阻塞期间
// 缓冲照常累积,连接断开重连后按游标补推,不丢)。
package biz

import (
	"sync"

	grpc "google.golang.org/grpc"

	pushv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/push/v1"
)

// PushStream 是 push 服务对 gRPC server stream 的别名。
type PushStream = grpc.ServerStreamingServer[pushv1.PushFrame]

// broadcastQueueSize 每连接广播箱容量(广播丢失容忍:离线不补推,满即丢)。
const broadcastQueueSize = 64

// StreamSlot 持有一条玩家 stream + 唤醒信号 + 广播箱。stream.Send 只准写者调用。
type StreamSlot struct {
	stream PushStream
	cancel func() // 顶号踢线(关闭 Subscribe ctx)

	// notify size-1:合并多次唤醒(写者每次醒来都会把缓冲拉到空,信号无需计数)。
	notify chan struct{}
	// bcast 广播帧箱(不入投递缓冲,无游标;满即丢)。
	bcast chan *pushv1.PushFrame
}

// wake 非阻塞置唤醒信号。
func (s *StreamSlot) wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// ConnectionManager 维护 player_id → StreamSlot 的索引。
type ConnectionManager struct {
	mu     sync.RWMutex
	bySlot map[uint64]*StreamSlot // key = player_id;value = 该玩家当前 slot
}

// NewConnectionManager 构造空索引。
func NewConnectionManager() *ConnectionManager {
	return &ConnectionManager{bySlot: make(map[uint64]*StreamSlot)}
}

// Register 把 (player_id, stream) 加入索引,返回新建的 slot 给调用方持有。
// 若已存在则触发旧 slot 的 cancel(顶号语义)。
func (m *ConnectionManager) Register(playerID uint64, stream PushStream, closeFn func()) *StreamSlot {
	slot := &StreamSlot{
		stream: stream,
		cancel: closeFn,
		notify: make(chan struct{}, 1),
		bcast:  make(chan *pushv1.PushFrame, broadcastQueueSize),
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if old, exists := m.bySlot[playerID]; exists && old.cancel != nil {
		old.cancel()
	}
	m.bySlot[playerID] = slot
	return slot
}

// Unregister 把 player_id 从索引中移除(仅当当前 slot 等于传入的 slot 时才移除,
// 避免顶号场景下新 stream 把自己的位置删掉)。
func (m *ConnectionManager) Unregister(playerID uint64, slot *StreamSlot) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if cur, ok := m.bySlot[playerID]; ok && cur == slot {
		delete(m.bySlot, playerID)
	}
}

// SendTo 唤醒该玩家的连接写者去缓冲拉新帧(帧本体已在投递缓冲,这里只传信号)。
// 返回是否在线(仅作观测;不在线也不是错误——缓冲已有,重连/轮询恢复)。
func (m *ConnectionManager) SendTo(playerID uint64) (online bool) {
	m.mu.RLock()
	slot, ok := m.bySlot[playerID]
	m.mu.RUnlock()

	if !ok {
		return false
	}
	slot.wake()
	return true
}

// Broadcast 给**本 Pod** 所有在线玩家投递一帧广播(广播 topic 每 Pod 独立 consumer
// group,全 Pod 都消费到同一条,见 consumer.go;满箱即丢,丢失容忍)。
// 返回入箱数 + 丢弃数,本方法不打日志。
func (m *ConnectionManager) Broadcast(frame *pushv1.PushFrame) (sent int, failed int) {
	m.mu.RLock()
	slots := make([]*StreamSlot, 0, len(m.bySlot))
	for _, s := range m.bySlot {
		slots = append(slots, s)
	}
	m.mu.RUnlock()

	for _, s := range slots {
		select {
		case s.bcast <- frame:
			sent++
		default:
			failed++
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
