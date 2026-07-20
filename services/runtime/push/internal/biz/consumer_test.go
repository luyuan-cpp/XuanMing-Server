// W3 ④(2026-06-05)KafkaConsumer.handle 单测。
//
// 直接调 handle 方法,不起真实 sarama broker;FrameSender / OfflineCacheRepo
// 用本文件内的 mock 实现,验证三条路径:在线发送、离线写入、key 非数字跳过。
package biz

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IBM/sarama"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/kafkax"
	pushv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/push/v1"

	"github.com/luyuancpp/pandora/services/runtime/push/internal/data"
)

// =============== mocks ===============

type mockSender struct {
	mu     sync.Mutex
	frames map[uint64]*pushv1.PushFrame
	online map[uint64]bool
	sendEr error

	broadcastFrames []*pushv1.PushFrame
	broadcastSent   int
	broadcastFailed int
}

func newMockSender() *mockSender {
	return &mockSender{
		frames: make(map[uint64]*pushv1.PushFrame),
		online: make(map[uint64]bool),
	}
}

func (m *mockSender) SendTo(playerID uint64, frame *pushv1.PushFrame) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.online[playerID] {
		return false, nil
	}
	if m.sendEr != nil {
		return true, m.sendEr
	}
	m.frames[playerID] = frame
	return true, nil
}

func (m *mockSender) Broadcast(frame *pushv1.PushFrame) (sent int, failed int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.broadcastFrames = append(m.broadcastFrames, frame)
	return m.broadcastSent, m.broadcastFailed
}

type mockOffline struct {
	mu        sync.Mutex
	appended  map[uint64][]*pushv1.PushFrame
	appendErr error // 非 nil 时,Append 直接返这个错(用于 R2 用例)
}

func newMockOffline() *mockOffline {
	return &mockOffline{appended: make(map[uint64][]*pushv1.PushFrame)}
}

func (o *mockOffline) Append(_ context.Context, playerID uint64, frame *pushv1.PushFrame) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.appendErr != nil {
		return o.appendErr
	}
	o.appended[playerID] = append(o.appended[playerID], frame)
	return nil
}

func (o *mockOffline) Range(_ context.Context, playerID uint64, _ int64) ([]data.OfflineFrame, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]data.OfflineFrame, 0, len(o.appended[playerID]))
	for _, f := range o.appended[playerID] {
		out = append(out, data.OfflineFrame{Frame: f, ScoreMs: f.GetTsMs()})
	}
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

// 用例 1:在线玩家 → SendTo 收到 PushFrame,offline 没写。
func TestKafkaConsumer_HandleOnline(t *testing.T) {
	sender := newMockSender()
	sender.online[42] = true
	offline := newMockOffline()
	kc := makeConsumer(t, sender, offline)

	msg := makeMsg("pandora.team.update", "42", []byte("team-event-bytes"), "trace-abc")
	if err := kc.handle(context.Background(), msg); err != nil {
		t.Fatalf("handle err=%v", err)
	}

	f, ok := sender.frames[42]
	if !ok {
		t.Fatal("expected SendTo(42) but no frame recorded")
	}
	if f.GetTopic() != "pandora.team.update" || string(f.GetPayload()) != "team-event-bytes" {
		t.Fatalf("frame=%+v", f)
	}
	if f.GetTraceId() != "trace-abc" {
		t.Fatalf("trace_id=%q want=trace-abc", f.GetTraceId())
	}
	if f.GetEventType() != 0 {
		t.Fatalf("event_type=%d want=0 when Kafka header is absent", f.GetEventType())
	}
	if len(offline.appended) != 0 {
		t.Fatalf("offline should be empty when online, got=%+v", offline.appended)
	}
}

// event_type 是域内 payload 类型判别键；push 必须把 Kafka header 原样透传到 PushFrame。
func TestKafkaConsumer_HandleEventType(t *testing.T) {
	sender := newMockSender()
	sender.online[42] = true
	offline := newMockOffline()
	kc := makeConsumer(t, sender, offline)

	msg := makeMsg("pandora.team.update", "42", []byte("team-invite-event-bytes"), "trace-invite")
	msg.Headers = append(msg.Headers, &sarama.RecordHeader{
		Key:   []byte(kafkax.HeaderEventType),
		Value: []byte("1"),
	})
	if err := kc.handle(context.Background(), msg); err != nil {
		t.Fatalf("handle err=%v", err)
	}

	f, ok := sender.frames[42]
	if !ok {
		t.Fatal("expected SendTo(42) but no frame recorded")
	}
	if f.GetEventType() != 1 {
		t.Fatalf("event_type=%d want=1", f.GetEventType())
	}
}

// 用例 2:离线玩家 → SendTo 返 false,offline 写一条。
func TestKafkaConsumer_HandleOffline(t *testing.T) {
	sender := newMockSender() // 不标 online
	offline := newMockOffline()
	kc := makeConsumer(t, sender, offline)

	msg := makeMsg("pandora.match.progress", "99", []byte("match-event"), "")
	if err := kc.handle(context.Background(), msg); err != nil {
		t.Fatalf("handle err=%v", err)
	}

	if got := offline.appended[99]; len(got) != 1 {
		t.Fatalf("offline[99] len=%d want=1", len(got))
	}
	if len(sender.frames) != 0 {
		t.Fatalf("sender should not have sent any frame, got=%+v", sender.frames)
	}
}

// 用例 3:key 非数字 → 返回毒丸错误(kafkax 会投 DLQ 留证,不静默丢),
// SendTo 和 offline 都没动。
func TestKafkaConsumer_HandleInvalidKey(t *testing.T) {
	sender := newMockSender()
	sender.online[1] = true
	offline := newMockOffline()
	kc := makeConsumer(t, sender, offline)

	msg := makeMsg("pandora.team.update", "not-a-number", []byte("x"), "")
	err := kc.handle(context.Background(), msg)
	if err == nil {
		t.Fatal("invalid key should return poison error (routed to DLQ), got nil")
	}
	var poison *kafkax.PoisonError
	if !errors.As(err, &poison) {
		t.Fatalf("invalid key should be PoisonError, got=%v", err)
	}
	if len(sender.frames) != 0 || len(offline.appended) != 0 {
		t.Fatal("invalid key should not invoke sender or offline")
	}
}

// 用例 4:在线但 SendTo 返 err(stream 已断)→ handle 仍返 nil(让 kafka ack),
// 帧未交付,必须写 offline 让客户端重连后通过 last_seen_ms 补推。
// 幂等判重由客户端按 ts_ms + trace_id 处理,push 侧不应丢帧。
func TestKafkaConsumer_HandleOnlineSendFail(t *testing.T) {
	sender := newMockSender()
	sender.online[7] = true
	sender.sendEr = errors.New("stream closed")
	offline := newMockOffline()
	kc := makeConsumer(t, sender, offline)

	msg := makeMsg("pandora.team.update", "7", []byte("payload"), "")
	if err := kc.handle(context.Background(), msg); err != nil {
		t.Fatalf("handle err=%v", err)
	}
	if len(offline.appended) != 1 {
		t.Fatalf("send fail must write offline (fallback), got=%d entries", len(offline.appended))
	}
}

// 用例 5(W3 ④ 二次修复 Opus R2):offline.Append 失败 → handle 返回 errcode 9301。
//
// 防止"redis down → 只 log、kafka 仍 ack → 客户端补不回"的静默丢消息隐患。
// metric pandora_push_offline_append_failed_total 由 handle 内部 Inc,本测试通过
// errcode 断言确认调用链已经进入失败分支。
func TestKafkaConsumer_HandleOfflineFail(t *testing.T) {
	sender := newMockSender() // 离线
	offline := newMockOffline()
	offline.appendErr = errors.New("redis dial timeout")
	kc := makeConsumer(t, sender, offline)

	msg := makeMsg("pandora.chat.private", "123", []byte("hi"), "trace-r2")
	err := kc.handle(context.Background(), msg)
	if err == nil {
		t.Fatal("handle should return errcode when offline.Append fails")
	}
	if code := errcode.As(err); code != errcode.ErrPushOfflineCorrupted {
		t.Fatalf("err code=%d want=%d (9301)", code, errcode.ErrPushOfflineCorrupted)
	}
	if !strings.Contains(err.Error(), "redis dial timeout") {
		t.Fatalf("err should wrap original cause, got=%v", err)
	}
}

// 用例 6(chat 三频道补全):广播类 topic(chat.world)空 key → 走 Broadcast,
// 不按 player_id 解析(空 key 不会被当 invalid key 丢弃),也不写离线缓存。
func TestKafkaConsumer_HandleBroadcastWorld(t *testing.T) {
	sender := newMockSender()
	sender.broadcastSent = 3 // 模拟 3 个在线玩家收到
	offline := newMockOffline()
	kc := makeConsumer(t, sender, offline)
	kc.topic = "pandora.chat.world"
	kc.broadcast = true

	// 世界聊天是空 key 广播;旧逻辑会 ParseUint("") 失败丢弃。
	msg := makeMsg("pandora.chat.world", "", []byte("world-chat-bytes"), "trace-world")
	if err := kc.handle(context.Background(), msg); err != nil {
		t.Fatalf("handle err=%v", err)
	}

	if len(sender.broadcastFrames) != 1 {
		t.Fatalf("expected 1 broadcast frame, got=%d", len(sender.broadcastFrames))
	}
	f := sender.broadcastFrames[0]
	if f.GetTopic() != "pandora.chat.world" || string(f.GetPayload()) != "world-chat-bytes" {
		t.Fatalf("broadcast frame=%+v", f)
	}
	if len(sender.frames) != 0 {
		t.Fatalf("broadcast must not call SendTo, got=%+v", sender.frames)
	}
	if len(offline.appended) != 0 {
		t.Fatalf("broadcast must not write offline, got=%+v", offline.appended)
	}
}
