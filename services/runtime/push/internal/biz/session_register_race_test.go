// session_register_race_test.go — P0(INC-20260722-004)R4 复审回归:
//
//	① 建流「会话校验+注册」TOCTOU:旧会话不得在任何交错下顶掉新设备连接;
//	② 会话看门狗独立于写者:写者阻塞在 stream.Send 时,会话失效仍须在有界时间内
//	   取消流并阻止后续投递。
package biz

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	pushv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/push/v1"
)

// raceGate 复现 R4 复审①的精确交错:首次 CurrentJTI 返回预录旧值,且可在「已读到
// 结果、尚未返回」处阻塞——对应真实场景里旧会话的 Redis 读已完成、注册尚未执行的
// 窗口;后续调用返回当前值。
type raceGate struct {
	mu      sync.Mutex
	current string
	stale   string        // 首次调用返回的预录快照
	first   bool          // 首次调用是否已发生
	entered chan struct{} // 首次调用抵达阻塞点时关闭
	release chan struct{} // 首次调用阻塞至此通道关闭
}

func (g *raceGate) CurrentJTI(_ context.Context, _ uint64) (string, bool, error) {
	g.mu.Lock()
	isFirst := !g.first
	if isFirst {
		g.first = true
	}
	cur := g.current
	stale := g.stale
	g.mu.Unlock()

	if isFirst {
		close(g.entered)
		<-g.release
		return stale, true, nil
	}
	return cur, cur != "", nil
}

func (g *raceGate) setCurrent(jti string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.current = jti
}

// R4 复审①的原始复现顺序:旧会话 A 校验通过后暂停 → 新会话 B 登录轮换 jti 并订阅
// → A 恢复注册。修复后 AuthorizeAndRegister 在同玩家锁内串行,B 的「校验+注册」
// 必然排在 A 的「校验+注册」整体之后 → B 顶掉 A;B 的槽位不再可能被 A 取消。
func TestAuthorizeAndRegister_StaleSessionCannotDisplaceNewer(t *testing.T) {
	gate := &raceGate{
		current: "jti-A",
		stale:   "jti-A",
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	uc := NewPushUsecase(NewConnectionManager(), &pullRepo{})
	uc.SetSessionGate(gate, true)
	ctx := context.Background()

	var aCancelled, bCancelled atomic.Bool
	streamA := &captureStream{ctx: ctx}
	streamB := &captureStream{ctx: ctx}

	var (
		slotA *StreamSlot
		errA  error
	)
	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		slotA, errA = uc.AuthorizeAndRegister(ctx, 7, SessionInfo{JTI: "jti-A"}, streamA,
			func() { aCancelled.Store(true) })
	}()

	// A 已读到旧 jti,正处于「校验后、注册前」的窗口。
	<-gate.entered
	// 玩家在新设备登录:会话权威轮换到 jti-B,新设备随即订阅。
	gate.setCurrent("jti-B")
	var errB error
	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		_, errB = uc.AuthorizeAndRegister(ctx, 7, SessionInfo{JTI: "jti-B"}, streamB,
			func() { bCancelled.Store(true) })
	}()

	// B 必须串行在 A 的「校验+注册」之后(条带锁);旧实现此刻 B 已注册,A 恢复后会顶掉 B。
	select {
	case <-bDone:
		t.Fatal("B must serialize behind A's authorize+register critical section")
	case <-time.After(50 * time.Millisecond):
	}

	close(gate.release) // A 恢复,凭旧 jti 快照完成注册
	<-aDone
	<-bDone

	if errA != nil {
		t.Fatalf("A validated while still current, register must succeed: %v", errA)
	}
	if errB != nil {
		t.Fatalf("B is the current session, must register: %v", errB)
	}
	if bCancelled.Load() {
		t.Fatal("P0: stale session displaced the newer device connection")
	}
	if !aCancelled.Load() {
		t.Fatal("stale session slot must be superseded by the newer registration")
	}
	// A 反注册(defer Unregister 语义)不得误删 B 的槽位。
	uc.Conns().Unregister(7, slotA)
	if !uc.Conns().SendTo(7) {
		t.Fatal("newer session slot must survive stale slot's unregister")
	}
}

// gatedStream 复现慢客户端流控:Send 记录帧后阻塞,直至测试投放一个令牌。
type gatedStream struct {
	ctx     context.Context
	proceed chan struct{}

	mu   sync.Mutex
	sent []*pushv1.PushFrame
}

func (s *gatedStream) Send(frame *pushv1.PushFrame) error {
	s.mu.Lock()
	s.sent = append(s.sent, proto.Clone(frame).(*pushv1.PushFrame))
	s.mu.Unlock()
	<-s.proceed
	return nil
}

func (s *gatedStream) sentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func (*gatedStream) SetHeader(metadata.MD) error  { return nil }
func (*gatedStream) SendHeader(metadata.MD) error { return nil }
func (*gatedStream) SetTrailer(metadata.MD)       {}
func (s *gatedStream) Context() context.Context   { return s.ctx }
func (*gatedStream) SendMsg(any) error            { return nil }
func (*gatedStream) RecvMsg(any) error            { return nil }

// R4 复审②:写者阻塞在 Send 上时,旧实现的会话复查(与写者同一 select)永远轮不到,
// 「30s 内关闭旧流」不成立。修复后看门狗独立裁决:顶号发生后,即便写者仍阻塞,
// 流上下文也被取消;写者解除阻塞后不得再投任何帧,并以会话错误关流。
func TestRunSubscribeStream_WatchdogClosesBlockedWriter(t *testing.T) {
	repo := &pullRepo{pageSize: 10}
	repo.add(1001, "one")
	repo.add(1002, "two")

	gate := &fakeSessionGate{}
	gate.set(7, "jti-A")
	uc := NewPushUsecase(NewConnectionManager(), repo)
	uc.SetSessionGate(gate, true)
	uc.sessionRecheckEvery = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &gatedStream{ctx: ctx, proceed: make(chan struct{})}
	sess := SessionInfo{JTI: "jti-A", ExpMs: time.Now().Add(time.Hour).UnixMilli()}

	done := make(chan error, 1)
	go func() { done <- uc.RunSubscribeStream(ctx, newSlot(stream), 7, 1000, sess) }()

	// 等写者进入第一帧 Send 并阻塞(慢客户端)。
	deadline := time.Now().Add(2 * time.Second)
	for stream.sentCount() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("writer did not reach the first Send in time")
		}
		time.Sleep(time.Millisecond)
	}

	gate.set(7, "jti-B") // 顶号:另一设备登录轮换会话
	// 给看门狗足够的复查周期在「写者仍阻塞」期间完成裁决。
	time.Sleep(300 * time.Millisecond)

	stream.proceed <- struct{}{} // 客户端终于读走第一帧,写者解除阻塞

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("superseded session must close the stream with an error")
		}
		if errcode.As(err) != errcode.ErrSessionSuperseded {
			// R4 P0 互踢循环:顶号关流必须用专属码(→ABORTED),不得与自然过期的
			// ErrUnauthorized(→UNAUTHENTICATED)混同,否则被顶设备自动重登反顶新设备。
			t.Fatalf("close reason must map to ErrSessionSuperseded, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not close after writer unblocked (watchdog starved?)")
	}
	if got := stream.sentCount(); got != 1 {
		t.Fatalf("no further frames may be delivered after session invalidation, sent=%d", got)
	}
}

// R4 P0(顶号互踢循环)双设备「稳定赢家」回归:B 登录顶掉 A 后,A 的旧 token 无论
// 自动重连多少次都必须被拒、且每次都以 ErrSessionSuperseded 判别(客户端据此转交互
// 登录,不再自动完整 Login 反顶 B)。全程 B 的连接槽不得被 A 的重试取消或替换;
// A 的迟到反注册也不得误删赢家槽位。
func TestKickedStaleSessionRetriesNeverDisplaceWinner(t *testing.T) {
	gate := &fakeSessionGate{}
	gate.set(7, "jti-A")
	uc := NewPushUsecase(NewConnectionManager(), &pullRepo{})
	uc.SetSessionGate(gate, true)
	ctx := context.Background()

	var aCancelled, bCancelled atomic.Bool
	slotA, err := uc.AuthorizeAndRegister(ctx, 7, SessionInfo{JTI: "jti-A"},
		&captureStream{ctx: ctx}, func() { aCancelled.Store(true) })
	if err != nil {
		t.Fatalf("device A initial subscribe must succeed: %v", err)
	}

	// 设备 B 登录:login 轮换会话权威到 jti-B,B 随即建流(顶号语义取消 A)。
	gate.set(7, "jti-B")
	if _, err := uc.AuthorizeAndRegister(ctx, 7, SessionInfo{JTI: "jti-B"},
		&captureStream{ctx: ctx}, func() { bCancelled.Store(true) }); err != nil {
		t.Fatalf("device B (current session) must register: %v", err)
	}
	if !aCancelled.Load() {
		t.Fatal("device A's stale slot must be superseded when B registers")
	}

	// A 客户端的自动重连风暴:旧 token 重试建流,每次都必须拒 + 可判别为顶号。
	for i := 0; i < 5; i++ {
		slot, rerr := uc.AuthorizeAndRegister(ctx, 7, SessionInfo{JTI: "jti-A"},
			&captureStream{ctx: ctx}, func() {})
		if rerr == nil {
			uc.Conns().Unregister(7, slot)
			t.Fatalf("retry %d: stale token re-subscribe must be rejected", i)
		}
		if errcode.As(rerr) != errcode.ErrSessionSuperseded {
			t.Fatalf("retry %d: kick must be discriminable (ErrSessionSuperseded), got: %v", i, rerr)
		}
	}
	if bCancelled.Load() {
		t.Fatal("P0: winner (device B) connection must be stable across stale retries")
	}
	// A 的流退出时 defer Unregister(迟到反注册)不得误删 B 的槽位。
	uc.Conns().Unregister(7, slotA)
	if !uc.Conns().SendTo(7) {
		t.Fatal("winner slot must survive stale slot's late unregister")
	}
}
