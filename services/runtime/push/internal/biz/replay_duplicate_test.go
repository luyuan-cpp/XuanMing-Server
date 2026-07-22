// replay_duplicate_test.go — RunSubscribeStream 拉取式投递单测(2026-07-22 v2)。
//
// 覆盖:首轮补推(含 cursor=0 首连拉登录窗帧,审计 P1)、分页拉到空、唤醒信号驱动
// 增量拉取(跨"消费-连接"路径)、拉取失败首轮断流/后续重试不丢、广播直通。
package biz

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	pushv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/push/v1"
	"github.com/luyuancpp/pandora/services/runtime/push/internal/data"
)

// pullRepo 复刻投递缓冲(可并发追加;Range 严格 >,分页)。
type pullRepo struct {
	mu       sync.Mutex
	frames   []data.OfflineFrame
	pageSize int
	rangeErr error // 消费一次后清零(模拟瞬时故障)
	calls    int
	lost     int64 // LostSince 注入结果:已确定不可交付的最高游标(R4 P1-3/复审 P1-2)
	lostErr  error // LostSince 注入错误
}

func (r *pullRepo) AssignAndBuffer(_ context.Context, _ uint64, frame *pushv1.PushFrame) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cursor := int64(1)
	if n := len(r.frames); n > 0 {
		cursor = r.frames[n-1].ScoreMs + 1
	}
	if frame.GetTsMs() > cursor {
		cursor = frame.GetTsMs()
	}
	f := proto.Clone(frame).(*pushv1.PushFrame)
	f.TsMs = cursor
	r.frames = append(r.frames, data.OfflineFrame{Frame: f, ScoreMs: cursor})
	return cursor, nil
}

func (r *pullRepo) add(cursor int64, payload string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, data.OfflineFrame{
		Frame:   &pushv1.PushFrame{Topic: "t", Payload: []byte(payload), TsMs: cursor},
		ScoreMs: cursor,
	})
}

func (r *pullRepo) Range(_ context.Context, _ uint64, afterCursor int64) ([]data.OfflineFrame, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.rangeErr != nil {
		err := r.rangeErr
		r.rangeErr = nil
		return nil, err
	}
	sort.Slice(r.frames, func(i, j int) bool { return r.frames[i].ScoreMs < r.frames[j].ScoreMs })
	var out []data.OfflineFrame
	for _, f := range r.frames {
		if f.ScoreMs > afterCursor {
			out = append(out, data.OfflineFrame{Frame: proto.Clone(f.Frame).(*pushv1.PushFrame), ScoreMs: f.ScoreMs})
			if r.pageSize > 0 && len(out) == r.pageSize {
				break
			}
		}
	}
	return out, nil
}

type captureStream struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	frames []*pushv1.PushFrame
	stopAt int
}

func (s *captureStream) Send(frame *pushv1.PushFrame) error {
	s.mu.Lock()
	s.frames = append(s.frames, proto.Clone(frame).(*pushv1.PushFrame))
	n := len(s.frames)
	s.mu.Unlock()
	if n == s.stopAt {
		s.cancel()
	}
	return nil
}

func (s *captureStream) payloads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.frames))
	for i, f := range s.frames {
		out[i] = string(f.GetPayload())
	}
	return out
}

func (*captureStream) SetHeader(metadata.MD) error  { return nil }
func (*captureStream) SendHeader(metadata.MD) error { return nil }
func (*captureStream) SetTrailer(metadata.MD)       {}
func (s *captureStream) Context() context.Context   { return s.ctx }
func (*captureStream) SendMsg(any) error            { return nil }
func (*captureStream) RecvMsg(any) error            { return nil }

func newSlot(stream PushStream) *StreamSlot {
	return &StreamSlot{
		stream: stream,
		notify: make(chan struct{}, 1),
		bcast:  make(chan *pushv1.PushFrame, broadcastQueueSize),
	}
}

// 首轮补推分页拉到空 + 顺序;cursor=1000 严格 > 续传。
func TestRunSubscribeStream_PaginatedReplay(t *testing.T) {
	repo := &pullRepo{pageSize: 2}
	for i, p := range []string{"a", "b", "c", "d", "e"} {
		repo.add(int64(1001+i), p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &captureStream{ctx: ctx, cancel: cancel, stopAt: 5}
	uc := NewPushUsecase(NewConnectionManager(), repo)

	if err := uc.RunSubscribeStream(ctx, newSlot(stream), 77, 1000, SessionInfo{}); err != nil {
		t.Fatalf("err: %v", err)
	}
	got := stream.payloads()
	want := []string{"a", "b", "c", "d", "e"}
	if len(got) != 5 {
		t.Fatalf("got=%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("i=%d got=%q want=%q", i, got[i], want[i])
		}
	}
}

// 首连 cursor=0 也补推缓冲现存帧(审计 P1:登录→订阅窗口内已入缓冲的帧不得跳过)。
func TestRunSubscribeStream_FirstConnectReplaysBuffer(t *testing.T) {
	repo := &pullRepo{pageSize: 10}
	repo.add(1001, "login-window-frame")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &captureStream{ctx: ctx, cancel: cancel, stopAt: 1}
	uc := NewPushUsecase(NewConnectionManager(), repo)

	if err := uc.RunSubscribeStream(ctx, newSlot(stream), 77, 0, SessionInfo{}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := stream.payloads(); len(got) != 1 || got[0] != "login-window-frame" {
		t.Fatalf("first connect must replay buffered frames, got=%v", got)
	}
}

// 唤醒信号驱动增量拉取:补推完成后新帧入缓冲 + wake → 写者拉到并投递。
func TestRunSubscribeStream_WakePullsNewFrames(t *testing.T) {
	repo := &pullRepo{pageSize: 10}
	repo.add(1001, "old")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &captureStream{ctx: ctx, cancel: cancel, stopAt: 2}
	slot := newSlot(stream)
	uc := NewPushUsecase(NewConnectionManager(), repo)

	done := make(chan error, 1)
	go func() { done <- uc.RunSubscribeStream(ctx, slot, 77, 1000, SessionInfo{}) }()

	// 等首轮补推完成(收到 old)再注入新帧 + 唤醒。
	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(stream.payloads()) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first replay did not finish in time")
		}
		time.Sleep(time.Millisecond)
	}
	repo.add(1002, "fresh")
	slot.wake()

	if err := <-done; err != nil {
		t.Fatalf("err: %v", err)
	}
	got := stream.payloads()
	if len(got) != 2 || got[1] != "fresh" {
		t.Fatalf("wake must pull new frame, got=%v", got)
	}
}

// 首轮拉取失败 → 断流返回错误(游标未动,客户端重连不漏)。
func TestRunSubscribeStream_FirstReplayFailureClosesStream(t *testing.T) {
	repo := &pullRepo{pageSize: 10, rangeErr: errors.New("redis down")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &captureStream{ctx: ctx, cancel: cancel, stopAt: 99}
	uc := NewPushUsecase(NewConnectionManager(), repo)

	if err := uc.RunSubscribeStream(ctx, newSlot(stream), 77, 1000, SessionInfo{}); err == nil {
		t.Fatal("first replay failure must close stream")
	}
}

// 后续拉取失败不断流:下一次唤醒重试,帧不丢。
func TestRunSubscribeStream_LaterPullFailureRetries(t *testing.T) {
	repo := &pullRepo{pageSize: 10}
	repo.add(1001, "old")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &captureStream{ctx: ctx, cancel: cancel, stopAt: 2}
	slot := newSlot(stream)
	uc := NewPushUsecase(NewConnectionManager(), repo)

	done := make(chan error, 1)
	go func() { done <- uc.RunSubscribeStream(ctx, slot, 77, 1000, SessionInfo{}) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(stream.payloads()) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first replay did not finish in time")
		}
		time.Sleep(time.Millisecond)
	}
	// 注入瞬时故障 + 新帧:第一次唤醒拉取失败(不断流,进入 1s 退避),
	// 退避窗过后再次唤醒成功投递。
	repo.mu.Lock()
	repo.rangeErr = errors.New("transient")
	repo.mu.Unlock()
	repo.add(1002, "after-transient")
	slot.wake()
	time.Sleep(1100 * time.Millisecond) // 覆盖首档退避(1s),验证退避后恢复
	slot.wake()

	if err := <-done; err != nil {
		t.Fatalf("err: %v", err)
	}
	got := stream.payloads()
	if len(got) != 2 || got[1] != "after-transient" {
		t.Fatalf("transient pull failure must not lose frames, got=%v", got)
	}
}

// 广播帧经广播箱直通(不参与游标)。
func TestRunSubscribeStream_BroadcastPassthrough(t *testing.T) {
	repo := &pullRepo{pageSize: 10}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &captureStream{ctx: ctx, cancel: cancel, stopAt: 1}
	slot := newSlot(stream)
	slot.bcast <- &pushv1.PushFrame{Topic: "pandora.chat.world", Payload: []byte("bcast"), TsMs: 0}
	uc := NewPushUsecase(NewConnectionManager(), repo)

	if err := uc.RunSubscribeStream(ctx, slot, 77, 0, SessionInfo{}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := stream.payloads(); len(got) != 1 || got[0] != "bcast" {
		t.Fatalf("broadcast must pass through, got=%v", got)
	}
}

// ── 会话现行性门(P0,INC-20260722-004)────────────────────────────────────────

type fakeSessionGate struct {
	mu  sync.Mutex
	jti map[uint64]string
	err error
}

func (g *fakeSessionGate) CurrentJTI(_ context.Context, playerID uint64) (string, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return "", false, g.err
	}
	j, ok := g.jti[playerID]
	return j, ok && j != "", nil
}

func (g *fakeSessionGate) set(playerID uint64, jti string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.jti == nil {
		g.jti = map[uint64]string{}
	}
	g.jti[playerID] = jti
}

// 建流门:当前一代放行;旧 jti(被顶号)拒;无会话拒;权威不可达 fail-closed;
// require 档下缺 jti 拒。
func TestAuthorizeSubscribe_SessionCurrency(t *testing.T) {
	gate := &fakeSessionGate{}
	gate.set(7, "jti-new")
	uc := NewPushUsecase(NewConnectionManager(), &pullRepo{})
	uc.SetSessionGate(gate, true)
	ctx := context.Background()

	if err := uc.AuthorizeSubscribe(ctx, 7, SessionInfo{JTI: "jti-new"}); err != nil {
		t.Fatalf("current session must pass: %v", err)
	}
	if err := uc.AuthorizeSubscribe(ctx, 7, SessionInfo{JTI: "jti-old"}); err == nil {
		t.Fatal("superseded session must be rejected (P0: 旧 token 不得建流)")
	}
	if err := uc.AuthorizeSubscribe(ctx, 8, SessionInfo{JTI: "any"}); err == nil {
		t.Fatal("logged-out player must be rejected")
	}
	if err := uc.AuthorizeSubscribe(ctx, 7, SessionInfo{}); err == nil {
		t.Fatal("require 档缺 jti 必须拒(绕网关)")
	}
	gate.err = errors.New("redis down")
	if err := uc.AuthorizeSubscribe(ctx, 7, SessionInfo{JTI: "jti-new"}); err == nil {
		t.Fatal("session authority down must fail-closed")
	}
	// 宽松档(dev):无 jti 放行,有 jti 仍校验。
	gate.err = nil
	uc2 := NewPushUsecase(NewConnectionManager(), &pullRepo{})
	uc2.SetSessionGate(gate, false)
	if err := uc2.AuthorizeSubscribe(ctx, 7, SessionInfo{}); err != nil {
		t.Fatalf("dev 档无 jti 应放行: %v", err)
	}
	if err := uc2.AuthorizeSubscribe(ctx, 7, SessionInfo{JTI: "jti-old"}); err == nil {
		t.Fatal("dev 档携带旧 jti 仍必须拒")
	}
}

// 流内复查:token 到期 → 关流(exp 只在建流时被 Envoy 验一次,长连必须自查)。
func TestRecheckSession_ExpiryClosesStream(t *testing.T) {
	uc := NewPushUsecase(NewConnectionManager(), &pullRepo{})
	retryable, err := uc.recheckSession(context.Background(), 7,
		SessionInfo{JTI: "j", ExpMs: time.Now().UnixMilli() - 1000})
	if err == nil || retryable {
		t.Fatalf("expired token must close stream non-retryably: retryable=%v err=%v", retryable, err)
	}
}

// 流内复查:jti 被轮换(顶号,含跨 Pod 旧流场景)→ 关流;权威失败 → retryable。
func TestRecheckSession_SupersededAndRetryable(t *testing.T) {
	gate := &fakeSessionGate{}
	gate.set(7, "jti-A")
	uc := NewPushUsecase(NewConnectionManager(), &pullRepo{})
	uc.SetSessionGate(gate, true)
	ctx := context.Background()
	sess := SessionInfo{JTI: "jti-A", ExpMs: time.Now().Add(time.Hour).UnixMilli()}

	if retryable, err := uc.recheckSession(ctx, 7, sess); err != nil || retryable {
		t.Fatalf("current session recheck should pass: %v", err)
	}
	gate.set(7, "jti-B") // 顶号(可能发生在另一个 Pod 的新建流)
	if retryable, err := uc.recheckSession(ctx, 7, sess); err == nil || retryable {
		t.Fatalf("superseded recheck must close non-retryably: retryable=%v err=%v", retryable, err)
	}
	gate.set(7, "jti-A")
	gate.err = errors.New("redis blip")
	if retryable, err := uc.recheckSession(ctx, 7, sess); err == nil || !retryable {
		t.Fatalf("authority failure must be retryable (连败 fail-closed 由调用方计数): retryable=%v err=%v", retryable, err)
	}
}

// ── gap 检测 + resync 信号(R4 P1-3;R4 复审 P1-2:拉空后终检 + fail-closed)──────

// 带游标重连且缓冲已丢帧:补推幸存帧之后必须收到 pandora.push.resync 合成帧
// (终检在拉空后执行,消除「检查后~分页间隙修剪」的漏报窗口),且同一段丢失只信号一次。
func TestRunSubscribeStream_GapSignalsResyncAfterReplay(t *testing.T) {
	repo := &pullRepo{pageSize: 10, lost: 3000}
	repo.add(2001, "tail")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &captureStream{ctx: ctx, cancel: cancel, stopAt: 99}
	slot := newSlot(stream)
	uc := NewPushUsecase(NewConnectionManager(), repo)

	done := make(chan error, 1)
	go func() { done <- uc.RunSubscribeStream(ctx, slot, 77, 1000, SessionInfo{}) }()

	deadline := time.Now().Add(2 * time.Second)
	for len(stream.payloads()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("replay+resync did not arrive in time")
		}
		time.Sleep(time.Millisecond)
	}
	// 游标已跳到丢失上界(3000):同一段丢失再次唤醒不得重复发 resync。
	repo.add(3500, "late")
	slot.wake()
	deadline = time.Now().Add(2 * time.Second)
	for len(stream.payloads()) < 3 {
		if time.Now().After(deadline) {
			t.Fatal("post-resync frame did not arrive in time")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("err: %v", err)
	}

	stream.mu.Lock()
	frames := append([]*pushv1.PushFrame(nil), stream.frames...)
	stream.mu.Unlock()
	if len(frames) != 3 {
		t.Fatalf("want exactly tail+resync+late, got=%+v", frames)
	}
	if string(frames[0].GetPayload()) != "tail" {
		t.Fatalf("surviving frames must be replayed before resync, got=%+v", frames[0])
	}
	if frames[1].GetTopic() != ResyncTopic || frames[1].GetTsMs() != 0 {
		t.Fatalf("second frame must be resync signal (topic=%s ts=0), got=%+v", ResyncTopic, frames[1])
	}
	if string(frames[2].GetPayload()) != "late" {
		t.Fatalf("resync must be signaled once per lost range, got=%+v", frames[2])
	}
}

// 重连时缓冲已被整段修剪(拉空无幸存帧):同样必须发 resync,不能静默当无事发生。
func TestRunSubscribeStream_GapSignalsResyncWhenAllTrimmed(t *testing.T) {
	repo := &pullRepo{pageSize: 10, lost: 1500}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &captureStream{ctx: ctx, cancel: cancel, stopAt: 1}
	uc := NewPushUsecase(NewConnectionManager(), repo)

	if err := uc.RunSubscribeStream(ctx, newSlot(stream), 77, 1000, SessionInfo{}); err != nil {
		t.Fatalf("err: %v", err)
	}
	stream.mu.Lock()
	frames := append([]*pushv1.PushFrame(nil), stream.frames...)
	stream.mu.Unlock()
	if len(frames) != 1 || frames[0].GetTopic() != ResyncTopic {
		t.Fatalf("trimmed-out reconnect must emit resync, got=%+v", frames)
	}
}

// gap 检测失败必须 fail-closed(R4 复审:旧实现告警放行,游标越过缺口后 resync
// 永远无法触发):首轮即断流,游标不推进,客户端重连重试。
func TestRunSubscribeStream_GapCheckFailClosed(t *testing.T) {
	repo := &pullRepo{pageSize: 10, lostErr: errors.New("redis blip")}
	repo.add(2001, "only")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &captureStream{ctx: ctx, cancel: cancel, stopAt: 99}
	uc := NewPushUsecase(NewConnectionManager(), repo)

	if err := uc.RunSubscribeStream(ctx, newSlot(stream), 77, 1000, SessionInfo{}); err == nil {
		t.Fatal("gap check failure on first replay must close the stream (fail-closed)")
	}
}

// 无丢失 / 首连:不得发 resync 帧(首连空缓冲连检测都不做——新客户端无增量历史;
// 首连有帧时丢失上界 ≤ 已投递游标同样不触发)。
func TestRunSubscribeStream_NoResyncWhenNoGap(t *testing.T) {
	for name, repo := range map[string]*pullRepo{
		"no-gap":               {pageSize: 10},
		"first-connect-hasold": {pageSize: 10, lost: 1500}, // lost < 已投递游标 2001
	} {
		repo.add(2001, "only")
		ctx, cancel := context.WithCancel(context.Background())
		stream := &captureStream{ctx: ctx, cancel: cancel, stopAt: 1}
		uc := NewPushUsecase(NewConnectionManager(), repo)
		after := int64(1000)
		if name == "first-connect-hasold" {
			after = 0
		}
		if err := uc.RunSubscribeStream(ctx, newSlot(stream), 77, after, SessionInfo{}); err != nil {
			t.Fatalf("%s: err: %v", name, err)
		}
		if got := stream.payloads(); len(got) != 1 || got[0] != "only" {
			t.Fatalf("%s: must not emit resync frame, got=%v", name, got)
		}
		cancel()
	}
	// 首连且缓冲为空:cursor 恒 0,不做检测(即便 lost 注入非 0 也不得发信号)。
	repo := &pullRepo{pageSize: 10, lost: 9999}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &captureStream{ctx: ctx, cancel: cancel, stopAt: 1}
	slot := newSlot(stream)
	slot.bcast <- &pushv1.PushFrame{Topic: "pandora.chat.world", Payload: []byte("bcast"), TsMs: 0}
	uc := NewPushUsecase(NewConnectionManager(), repo)
	if err := uc.RunSubscribeStream(ctx, slot, 77, 0, SessionInfo{}); err != nil {
		t.Fatalf("first connect err: %v", err)
	}
	if got := stream.payloads(); len(got) != 1 || got[0] != "bcast" {
		t.Fatalf("empty first connect must not emit resync, got=%v", got)
	}
}
