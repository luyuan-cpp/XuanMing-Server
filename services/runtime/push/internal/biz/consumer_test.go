// KafkaConsumer.handle 单测(2026-07-22 v2 拉取模型)。
//
// 验证:每帧原子入投递缓冲(游标重铸)→ 唤醒信号;缓冲失败拒 ack(9301);
// key 非数字毒丸;广播 ts=0 + 不入缓冲;非 owner cell 毒丸不 ACK。
package biz

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IBM/sarama"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/kafkax"
	pushv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/push/v1"

	"github.com/luyuancpp/pandora/services/runtime/push/internal/data"
)

// =============== mocks ===============

type mockSender struct {
	mu     sync.Mutex
	wakes  map[uint64]int
	online map[uint64]bool

	broadcastFrames []*pushv1.PushFrame
	broadcastSent   int
	broadcastFailed int
}

func newMockSender() *mockSender {
	return &mockSender{wakes: make(map[uint64]int), online: make(map[uint64]bool)}
}

func (m *mockSender) SendTo(playerID uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.online[playerID] {
		return false
	}
	m.wakes[playerID]++
	return true
}

func (m *mockSender) Broadcast(frame *pushv1.PushFrame) (sent int, failed int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.broadcastFrames = append(m.broadcastFrames, frame)
	return m.broadcastSent, m.broadcastFailed
}

type mockOffline struct {
	mu        sync.Mutex
	buffered  map[uint64][]*pushv1.PushFrame
	cursors   map[uint64]int64
	bufferErr error
}

func newMockOffline() *mockOffline {
	return &mockOffline{buffered: make(map[uint64][]*pushv1.PushFrame), cursors: make(map[uint64]int64)}
}

func (o *mockOffline) AssignAndBuffer(_ context.Context, playerID uint64, frame *pushv1.PushFrame) (int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.bufferErr != nil {
		return 0, o.bufferErr
	}
	cursor := o.cursors[playerID] + 1
	if ts := frame.GetTsMs(); ts > cursor {
		cursor = ts
	}
	o.cursors[playerID] = cursor
	frame.TsMs = cursor
	o.buffered[playerID] = append(o.buffered[playerID], proto.Clone(frame).(*pushv1.PushFrame))
	return cursor, nil
}

func (o *mockOffline) Range(_ context.Context, playerID uint64, afterCursor int64) ([]data.OfflineFrame, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []data.OfflineFrame
	for _, f := range o.buffered[playerID] {
		if f.GetTsMs() > afterCursor {
			out = append(out, data.OfflineFrame{Frame: proto.Clone(f).(*pushv1.PushFrame), ScoreMs: f.GetTsMs()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ScoreMs < out[j].ScoreMs })
	return out, nil
}

// =============== helpers ===============

func makeConsumer(t *testing.T, sender FrameRouter, offline data.OfflineCacheRepo) *KafkaConsumer {
	t.Helper()
	// 不调 NewKafkaConsumer(会拨号 broker);直接构造 struct,只用于 handle 测试
	return &KafkaConsumer{
		topic:   "pandora.team.update",
		cm:      sender,
		offline: offline,
	}
}

func makeMsg(topic, key string, value []byte, traceID string) *sarama.ConsumerMessage {
	headers := []*sarama.RecordHeader{}
	if traceID != "" {
		headers = append(headers, &sarama.RecordHeader{Key: []byte("trace_id"), Value: []byte(traceID)})
	}
	return &sarama.ConsumerMessage{
		Topic:     topic,
		Key:       []byte(key),
		Value:     value,
		Timestamp: time.UnixMilli(1700000000000),
		Headers:   headers,
	}
}

// =============== cases ===============

// 用例 1:在线玩家 → 帧入投递缓冲(ts 重铸为游标)+ 唤醒一次。
func TestKafkaConsumer_HandleOnline(t *testing.T) {
	sender := newMockSender()
	sender.online[42] = true
	offline := newMockOffline()
	kc := makeConsumer(t, sender, offline)

	msg := makeMsg("pandora.team.update", "42", []byte("team-event-bytes"), "trace-abc")
	if err := kc.handle(context.Background(), msg); err != nil {
		t.Fatalf("handle err=%v", err)
	}

	got := offline.buffered[42]
	if len(got) != 1 {
		t.Fatalf("buffered=%d want=1", len(got))
	}
	f := got[0]
	if f.GetTopic() != "pandora.team.update" || string(f.GetPayload()) != "team-event-bytes" ||
		f.GetTraceId() != "trace-abc" || f.GetEventType() != 0 {
		t.Fatalf("frame=%+v", f)
	}
	if f.GetTsMs() != offline.cursors[42] {
		t.Fatalf("frame ts must be recast to cursor: ts=%d cursor=%d", f.GetTsMs(), offline.cursors[42])
	}
	if sender.wakes[42] != 1 {
		t.Fatalf("wakes=%d want=1", sender.wakes[42])
	}
}

// event_type 透传。
func TestKafkaConsumer_HandleEventType(t *testing.T) {
	sender := newMockSender()
	sender.online[42] = true
	offline := newMockOffline()
	kc := makeConsumer(t, sender, offline)

	msg := makeMsg("pandora.team.update", "42", []byte("invite"), "trace-invite")
	msg.Headers = append(msg.Headers, &sarama.RecordHeader{
		Key: []byte(kafkax.HeaderEventType), Value: []byte("1"),
	})
	if err := kc.handle(context.Background(), msg); err != nil {
		t.Fatalf("handle err=%v", err)
	}
	if f := offline.buffered[42][0]; f.GetEventType() != 1 {
		t.Fatalf("event_type=%d want=1", f.GetEventType())
	}
}

// 用例 2:离线玩家 → 只入缓冲,无唤醒;游标严格递增。
func TestKafkaConsumer_HandleOfflineAndCursor(t *testing.T) {
	sender := newMockSender() // 不标 online
	offline := newMockOffline()
	kc := makeConsumer(t, sender, offline)

	for i := 0; i < 3; i++ {
		msg := makeMsg("pandora.match.progress", "99", []byte("e"), "")
		if err := kc.handle(context.Background(), msg); err != nil {
			t.Fatalf("i=%d handle err=%v", i, err)
		}
	}
	frames := offline.buffered[99]
	if len(frames) != 3 {
		t.Fatalf("buffered=%d want=3", len(frames))
	}
	for i := 1; i < len(frames); i++ {
		if frames[i].GetTsMs() <= frames[i-1].GetTsMs() {
			t.Fatalf("cursor must strictly increase: %d then %d", frames[i-1].GetTsMs(), frames[i].GetTsMs())
		}
	}
	if len(sender.wakes) != 0 {
		t.Fatalf("offline player must not be woken, wakes=%+v", sender.wakes)
	}
}

// 用例 3:key 非数字 → 毒丸投 DLQ,零副作用。
func TestKafkaConsumer_HandleInvalidKey(t *testing.T) {
	sender := newMockSender()
	sender.online[1] = true
	offline := newMockOffline()
	kc := makeConsumer(t, sender, offline)

	msg := makeMsg("pandora.team.update", "not-a-number", []byte("x"), "")
	err := kc.handle(context.Background(), msg)
	var poison *kafkax.PoisonError
	if err == nil || !errors.As(err, &poison) {
		t.Fatalf("invalid key should be PoisonError, got=%v", err)
	}
	if len(sender.wakes) != 0 || len(offline.buffered) != 0 {
		t.Fatal("invalid key should not touch sender or buffer")
	}
}

// 用例 4:缓冲写入失败 → 返回 errcode 9301 拒 ack(缓冲是交付权威)。
func TestKafkaConsumer_HandleBufferFail(t *testing.T) {
	sender := newMockSender()
	offline := newMockOffline()
	offline.bufferErr = errors.New("redis dial timeout")
	kc := makeConsumer(t, sender, offline)

	msg := makeMsg("pandora.chat.private", "123", []byte("hi"), "trace-r2")
	err := kc.handle(context.Background(), msg)
	if err == nil || errcode.As(err) != errcode.ErrPushOfflineCorrupted {
		t.Fatalf("want 9301, got=%v", err)
	}
	if !strings.Contains(err.Error(), "redis dial timeout") {
		t.Fatalf("err should wrap cause, got=%v", err)
	}
	if len(sender.wakes) != 0 {
		t.Fatal("buffer failure must not wake")
	}
}

// 用例 5:广播 topic → ts_ms 置 0(不污染客户端恢复游标,审计 P1)、不入缓冲、
// 空 key 不按 player 解析。
func TestKafkaConsumer_HandleBroadcastWorld(t *testing.T) {
	sender := newMockSender()
	sender.broadcastSent = 3
	offline := newMockOffline()
	kc := makeConsumer(t, sender, offline)
	kc.topic = "pandora.chat.world"
	kc.broadcast = true

	msg := makeMsg("pandora.chat.world", "", []byte("world-chat-bytes"), "trace-world")
	if err := kc.handle(context.Background(), msg); err != nil {
		t.Fatalf("handle err=%v", err)
	}

	if len(sender.broadcastFrames) != 1 {
		t.Fatalf("broadcast frames=%d want=1", len(sender.broadcastFrames))
	}
	f := sender.broadcastFrames[0]
	if f.GetTsMs() != 0 {
		t.Fatalf("broadcast ts_ms=%d want=0(防游标污染)", f.GetTsMs())
	}
	if f.GetTopic() != "pandora.chat.world" || string(f.GetPayload()) != "world-chat-bytes" {
		t.Fatalf("broadcast frame=%+v", f)
	}
	if len(offline.buffered) != 0 {
		t.Fatalf("broadcast must not enter buffer, got=%+v", offline.buffered)
	}
}
