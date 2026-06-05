// W3 ④ 二次修复(Opus 审查 R1)单测:验证 ConnectionManager.streamSlot.sendMu
// 把同一 stream 的 Send 串行化,挡住"KafkaConsumer.SendTo + replay 循环并发写 stream"
// 撕坏 HTTP/2 帧的隐患。
//
// 跑 race detector 时必须挂 `-race` 才能暴露原 BUG:
//
//	go test -race ./services/runtime/push/internal/biz/...
package biz

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	grpc "google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pushv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/push/v1"
)

// reentrantStream 模拟 grpc ServerStreamingServer:在 Send 中故意用 atomic.Bool 探测重入,
// 任何并发 Send 都会立即 t.Errorf。再加 sleep 也不需要,reentrance 标志比 race detector 还快。
type reentrantStream struct {
	t        *testing.T
	inFlight atomic.Bool
	count    atomic.Int64
}

// 满足 grpc.ServerStreamingServer[PushFrame]:实现 Send + grpc.ServerStream 全部 5 个方法。
func (s *reentrantStream) Send(_ *pushv1.PushFrame) error {
	if !s.inFlight.CompareAndSwap(false, true) {
		s.t.Errorf("concurrent Send detected — streamSlot.sendMu broken")
		return nil
	}
	defer s.inFlight.Store(false)
	s.count.Add(1)
	return nil
}

func (*reentrantStream) SetHeader(metadata.MD) error  { return nil }
func (*reentrantStream) SendHeader(metadata.MD) error { return nil }
func (*reentrantStream) SetTrailer(metadata.MD)       {}
func (*reentrantStream) Context() context.Context     { return context.Background() }
func (*reentrantStream) SendMsg(any) error            { return nil }
func (*reentrantStream) RecvMsg(any) error            { return nil }

// 编译期检查接口契合(否则 Register 会传不进去)。
var _ grpc.ServerStreamingServer[pushv1.PushFrame] = (*reentrantStream)(nil)

// TestSendTo_ConcurrentSafe 验证 SendTo 在多 goroutine 同时打同一 player 时,
// stream.Send 严格串行(无重入、无 race)。
//
// 不开 -race 也能查重入(借 atomic.Bool);开 -race 还能查 sendMu 是否漏锁。
func TestSendTo_ConcurrentSafe(t *testing.T) {
	cm := NewConnectionManager()
	stream := &reentrantStream{t: t}

	const playerID = uint64(42)
	cm.Register(playerID, stream, func() {})

	const (
		goroutines = 50
		perG       = 200
	)

	frame := &pushv1.PushFrame{Topic: "test", TsMs: 1, Payload: []byte("x")}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				if _, err := cm.SendTo(playerID, frame); err != nil {
					t.Errorf("SendTo err=%v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if want := int64(goroutines * perG); stream.count.Load() != want {
		t.Fatalf("Send count=%d want=%d", stream.count.Load(), want)
	}
}

// TestBroadcast_ConcurrentSafe 验证 Broadcast 与 SendTo 同时跑时也不会撕 stream。
// 实际并发模式:一组 goroutine 调 Broadcast,一组调 SendTo,共享同一玩家 slot。
func TestBroadcast_ConcurrentSafe(t *testing.T) {
	cm := NewConnectionManager()
	stream := &reentrantStream{t: t}

	const playerID = uint64(99)
	cm.Register(playerID, stream, func() {})

	frame := &pushv1.PushFrame{Topic: "test", TsMs: 1}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			cm.Broadcast(frame)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			if _, err := cm.SendTo(playerID, frame); err != nil {
				t.Errorf("SendTo err=%v", err)
				return
			}
		}
	}()
	wg.Wait()
}

// TestRegister_TopOff 验证顶号语义:同 player 再 Register,旧 closeFn 被调用,新 slot 替换。
func TestRegister_TopOff(t *testing.T) {
	cm := NewConnectionManager()
	stream1 := &reentrantStream{t: t}
	stream2 := &reentrantStream{t: t}

	const playerID = uint64(7)

	var closed1 atomic.Bool
	slot1 := cm.Register(playerID, stream1, func() { closed1.Store(true) })

	slot2 := cm.Register(playerID, stream2, func() {})

	if !closed1.Load() {
		t.Fatal("old closeFn not invoked on top-off")
	}
	if slot1 == slot2 {
		t.Fatal("Register should return different slots for different streams")
	}

	// 顶号后 SendTo 应走到 stream2
	frame := &pushv1.PushFrame{Topic: "test", TsMs: 1}
	if _, err := cm.SendTo(playerID, frame); err != nil {
		t.Fatalf("SendTo err=%v", err)
	}
	if stream2.count.Load() != 1 {
		t.Fatalf("after top-off SendTo should reach stream2, got count=%d", stream2.count.Load())
	}
	if stream1.count.Load() != 0 {
		t.Fatalf("after top-off stream1 should not receive, got count=%d", stream1.count.Load())
	}

	// Unregister 用错 slot(slot1)应被忽略(防止顶号场景下新 stream 把自己的位置删掉)
	cm.Unregister(playerID, slot1)
	if cm.Size() != 1 {
		t.Fatalf("Unregister with wrong slot should be no-op, size=%d", cm.Size())
	}

	cm.Unregister(playerID, slot2)
	if cm.Size() != 0 {
		t.Fatalf("Unregister with correct slot should remove entry, size=%d", cm.Size())
	}
}
