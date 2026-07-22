// connection_test.go — 拉取模型连接层单测(2026-07-22 v2)。
//
// 覆盖:唤醒信号合并、广播箱有界丢弃、顶号、Unregister 防误删。
package biz

import (
	"sync/atomic"
	"testing"

	pushv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/push/v1"
)

// TestSendTo_WakeCoalesces:多次 SendTo 只留一个待处理信号(写者每次醒来拉到空,
// 信号无需计数);不在线返回 false。
func TestSendTo_WakeCoalesces(t *testing.T) {
	cm := NewConnectionManager()
	slot := cm.Register(42, nil, func() {})

	for i := 0; i < 10; i++ {
		if online := cm.SendTo(42); !online {
			t.Fatal("player 42 should be online")
		}
	}
	if len(slot.notify) != 1 {
		t.Fatalf("notify pending=%d want=1 (coalesced)", len(slot.notify))
	}
	if online := cm.SendTo(999); online {
		t.Fatal("player 999 should be offline")
	}
}

// TestBroadcast_BoundedDrop:广播箱有界,满即丢(丢失容忍),不阻塞调用方。
func TestBroadcast_BoundedDrop(t *testing.T) {
	cm := NewConnectionManager()
	slot := cm.Register(1, nil, func() {})

	frame := &pushv1.PushFrame{Topic: "pandora.chat.world", TsMs: 0}
	var sent, dropped int
	for i := 0; i < broadcastQueueSize+5; i++ {
		s, f := cm.Broadcast(frame)
		sent += s
		dropped += f
	}
	if sent != broadcastQueueSize || dropped != 5 {
		t.Fatalf("sent=%d dropped=%d want %d/5", sent, dropped, broadcastQueueSize)
	}
	if len(slot.bcast) != broadcastQueueSize {
		t.Fatalf("bcast box=%d want=%d", len(slot.bcast), broadcastQueueSize)
	}
}

// TestRegister_TopOff:顶号旧 cancel 被调用;Unregister 用错 slot 是 no-op。
func TestRegister_TopOff(t *testing.T) {
	cm := NewConnectionManager()

	var closed1 atomic.Bool
	slot1 := cm.Register(7, nil, func() { closed1.Store(true) })
	slot2 := cm.Register(7, nil, func() {})

	if !closed1.Load() {
		t.Fatal("old closeFn not invoked on top-off")
	}
	if slot1 == slot2 {
		t.Fatal("Register should return different slots")
	}
	// 顶号后信号应落到 slot2
	cm.SendTo(7)
	if len(slot2.notify) != 1 || len(slot1.notify) != 0 {
		t.Fatalf("wake must land on slot2: slot2=%d slot1=%d", len(slot2.notify), len(slot1.notify))
	}

	cm.Unregister(7, slot1)
	if cm.Size() != 1 {
		t.Fatalf("Unregister with wrong slot should be no-op, size=%d", cm.Size())
	}
	cm.Unregister(7, slot2)
	if cm.Size() != 0 {
		t.Fatalf("Unregister with correct slot should remove entry, size=%d", cm.Size())
	}
}
