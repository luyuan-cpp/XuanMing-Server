// cross_pod_delivery_fence_test.go — R5 复审 P0-2/P0-4 回归(INC-20260722-004)。
//
// P0-4:进程内 authRegMu 只能串行化同一 Pod 的「校验+注册」;跨 Pod 场景(Pod A 读到
// 旧 jti 后暂停,B 在 Pod B 登录轮换并建流,Pod A 恢复注册)下旧流仍能注册并在看门狗
// 周期内读私有帧。修复 = 每批投递前 sessionFenceDelivery:轮换后产生的帧旧流零交付。
// P0-2:流内复查先判会话代际后判到期,「已过期且已被顶号」必须回 ErrSessionSuperseded。
package biz

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// 两个 PushUsecase 实例模拟两个 Pod(各自进程锁、共享会话权威 + 共享投递缓冲)。
// 时序:A(旧 jti)在 Pod A 注册成功(当时仍现行)→ B 登录轮换 jti → 轮换后产生
// 私有帧 → Pod A 的旧流开始补推。旧流必须零帧交付并以顶号专属码关流。
func TestCrossPodStaleStream_ZeroFramesAfterRotation(t *testing.T) {
	gate := &fakeSessionGate{}
	gate.set(7, "jti-A")
	sharedRepo := &pullRepo{pageSize: 10}

	podA := NewPushUsecase(NewConnectionManager(), sharedRepo)
	podA.SetSessionGate(gate, true)
	podB := NewPushUsecase(NewConnectionManager(), sharedRepo)
	podB.SetSessionGate(gate, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	streamA := &captureStream{ctx: ctx}

	// Pod A:旧设备 A 建流时 jti-A 仍是当前一代 → 校验+注册通过(合法时序)。
	slotA, err := podA.AuthorizeAndRegister(ctx, 7, SessionInfo{JTI: "jti-A"}, streamA, func() {})
	if err != nil {
		t.Fatalf("A register while current must succeed: %v", err)
	}

	// B 在 Pod B 登录:会话权威轮换到 jti-B,并在 Pod B 注册(Pod A 的进程锁看不见)。
	gate.set(7, "jti-B")
	if _, err := podB.AuthorizeAndRegister(ctx, 7, SessionInfo{JTI: "jti-B"},
		&captureStream{ctx: ctx}, func() {}); err != nil {
		t.Fatalf("B (current session) must register on pod B: %v", err)
	}

	// 轮换之后才产生的私有帧(B 时代数据)。
	sharedRepo.add(1001, "b-era-private")

	// Pod A 的旧流开始跑投递循环:首轮补推读到该帧,投递 fence 必须在 Send 前拒绝。
	sessA := SessionInfo{JTI: "jti-A", ExpMs: time.Now().Add(time.Hour).UnixMilli()}
	errRun := podA.RunSubscribeStream(ctx, slotA, 7, 0, sessA)
	if errRun == nil {
		t.Fatal("stale stream must close once the delivery fence detects rotation")
	}
	if errcode.As(errRun) != errcode.ErrSessionSuperseded {
		t.Fatalf("fence close must be discriminable (ErrSessionSuperseded), got: %v", errRun)
	}
	if got := streamA.payloads(); len(got) != 0 {
		t.Fatalf("P0-4: stale stream delivered post-rotation private frames: %v", got)
	}
}

// 稳态流中途被顶:首轮为空(无 fence 查询),之后轮换 + 新帧 + 唤醒信号——
// 拉取路径必须经 fence 立即关流(不得退避重试给旧流保留投递机会),零帧交付。
func TestSteadyStreamRotationWakeup_FencedBeforeSend(t *testing.T) {
	gate := &fakeSessionGate{}
	gate.set(7, "jti-A")
	repo := &pullRepo{pageSize: 10}
	uc := NewPushUsecase(NewConnectionManager(), repo)
	uc.SetSessionGate(gate, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &captureStream{ctx: ctx}
	slot := newSlot(stream)

	sess := SessionInfo{JTI: "jti-A", ExpMs: time.Now().Add(time.Hour).UnixMilli()}
	done := make(chan error, 1)
	go func() { done <- uc.RunSubscribeStream(ctx, slot, 7, 0, sess) }()

	// 留出首轮空补推的时间窗,再轮换 + 产生新帧 + 触发唤醒。
	time.Sleep(50 * time.Millisecond)
	gate.set(7, "jti-B")
	repo.add(2001, "b-era-private")
	select {
	case slot.notify <- struct{}{}:
	default:
	}

	select {
	case err := <-done:
		if errcode.As(err) != errcode.ErrSessionSuperseded {
			t.Fatalf("wakeup drain must fence and close with ErrSessionSuperseded, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fenced stream did not close (fence error treated as retryable?)")
	}
	if got := stream.payloads(); len(got) != 0 {
		t.Fatalf("P0-4: fenced stream delivered frames: %v", got)
	}
}

// countdownGate 前 N 次 CurrentJTI 返回旧代际,之后返回轮换后的新代际——确定性复现
// 「fence 通过后、同批后续帧发送前发生轮换」的批内交错(R6 复审 P0-2)。
type countdownGate struct {
	mu        sync.Mutex
	passLeft  int    // 剩余"仍返回旧代际"的查询次数
	beforeJTI string // 轮换前 jti
	afterJTI  string // 轮换后 jti
	calls     int
}

func (g *countdownGate) CurrentJTI(_ context.Context, _ uint64) (string, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if g.passLeft > 0 {
		g.passLeft--
		return g.beforeJTI, true, nil
	}
	return g.afterJTI, true, nil
}

// R6 复审 P0-2:缓冲里已有 3 帧,fence 在第 1 帧通过后立即轮换——旧流只允许交付
// 第 1 帧(其 fence 通过于轮换前),第 2/3 帧必须被逐帧 fence 拦下并以顶号专属码关流。
// 修复前(每批一次 fence)整批 3 帧都会发出。
func TestDrainFence_RotationMidBatchStopsRemainingFrames(t *testing.T) {
	gate := &countdownGate{passLeft: 1, beforeJTI: "jti-A", afterJTI: "jti-B"}
	repo := &pullRepo{pageSize: 10}
	repo.add(1001, "f1")
	repo.add(1002, "f2")
	repo.add(1003, "f3")
	uc := NewPushUsecase(NewConnectionManager(), repo)
	uc.SetSessionGate(gate, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &captureStream{ctx: ctx}
	slot := newSlot(stream)

	sess := SessionInfo{JTI: "jti-A", ExpMs: time.Now().Add(time.Hour).UnixMilli()}
	err := uc.RunSubscribeStream(ctx, slot, 7, 1000, sess)
	if errcode.As(err) != errcode.ErrSessionSuperseded {
		t.Fatalf("mid-batch rotation must close the stream as superseded, got: %v", err)
	}
	got := stream.payloads()
	if len(got) != 1 || got[0] != "f1" {
		t.Fatalf("P0-2: only the pre-rotation-fenced frame may be delivered, got=%v", got)
	}
}

// 轮换发生在批首帧 fence 之前:整批零交付(含首帧)。
func TestDrainFence_RotationBeforeBatchDeliversNothing(t *testing.T) {
	gate := &countdownGate{passLeft: 0, beforeJTI: "jti-A", afterJTI: "jti-B"}
	repo := &pullRepo{pageSize: 10}
	repo.add(1001, "f1")
	repo.add(1002, "f2")
	uc := NewPushUsecase(NewConnectionManager(), repo)
	uc.SetSessionGate(gate, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &captureStream{ctx: ctx}
	slot := newSlot(stream)

	sess := SessionInfo{JTI: "jti-A", ExpMs: time.Now().Add(time.Hour).UnixMilli()}
	err := uc.RunSubscribeStream(ctx, slot, 7, 1000, sess)
	if errcode.As(err) != errcode.ErrSessionSuperseded {
		t.Fatalf("pre-batch rotation must close the stream as superseded, got: %v", err)
	}
	if got := stream.payloads(); len(got) != 0 {
		t.Fatalf("no frame may be delivered after rotation observed, got=%v", got)
	}
}

// fence 权威瞬时不可达:fail-closed 不投递、不关流、游标不动;权威恢复且会话仍现行
// 后必须续传补发(证明 fail-closed 不丢帧、不误杀现行会话)。
func TestDeliveryFence_AuthorityDownFailClosedThenRecover(t *testing.T) {
	gate := &fakeSessionGate{}
	gate.set(7, "jti-A")
	repo := &pullRepo{pageSize: 10}
	uc := NewPushUsecase(NewConnectionManager(), repo)
	uc.SetSessionGate(gate, true)

	ctx, cancel := context.WithCancel(context.Background())
	stream := &captureStream{ctx: ctx, cancel: cancel, stopAt: 1}
	slot := newSlot(stream)

	sess := SessionInfo{JTI: "jti-A", ExpMs: time.Now().Add(time.Hour).UnixMilli()}
	done := make(chan error, 1)
	go func() { done <- uc.RunSubscribeStream(ctx, slot, 7, 0, sess) }()

	time.Sleep(50 * time.Millisecond)
	// 权威宕机 + 新帧到达:本轮 fence 失败 → 不投递、不关流,走退避。
	gate.mu.Lock()
	gate.err = errcode.New(errcode.ErrUnavailable, "session authority down")
	gate.mu.Unlock()
	repo.add(3001, "frame-during-outage")
	select {
	case slot.notify <- struct{}{}:
	default:
	}
	time.Sleep(150 * time.Millisecond)
	if got := stream.payloads(); len(got) != 0 {
		t.Fatalf("fail-closed: no delivery while authority is down, got=%v", got)
	}

	// 权威恢复(会话仍现行):退避到期后的重试必须把帧补发出去。
	gate.mu.Lock()
	gate.err = nil
	gate.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case slot.notify <- struct{}{}:
		default:
		}
		if len(stream.payloads()) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("frame must be delivered after authority recovers, got=%v", stream.payloads())
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case err := <-done: // stopAt=1 触发 cancel,流以 ctx 取消正常退出
		if err != nil && errcode.As(err) == errcode.ErrSessionSuperseded {
			t.Fatalf("current session must not be fenced as superseded: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not exit after ctx cancel")
	}
}

// P0-2:「已过期且已被顶号」的复查必须裁决为顶号(ErrSessionSuperseded→ABORTED),
// 不得因先判到期而落 ErrUnauthorized(→UNAUTHENTICATED 触发 UE 自动完整 Login 反顶)。
func TestRecheckSession_SupersededTakesPrecedenceOverExpiry(t *testing.T) {
	gate := &fakeSessionGate{}
	gate.set(7, "jti-B") // 权威已轮换到新设备 B
	uc := NewPushUsecase(NewConnectionManager(), &pullRepo{})
	uc.SetSessionGate(gate, true)

	// 旧设备 A:token 已过期 + 已被顶号(双重失效)。
	expired := SessionInfo{JTI: "jti-A", ExpMs: time.Now().Add(-time.Minute).UnixMilli()}
	retryable, err := uc.recheckSession(context.Background(), 7, expired)
	if retryable || err == nil {
		t.Fatalf("expired+superseded must be a terminal close, retryable=%v err=%v", retryable, err)
	}
	if errcode.As(err) != errcode.ErrSessionSuperseded {
		t.Fatalf("P0-2: superseded must take precedence over expiry, got: %v", err)
	}

	// 对照:jti 仍是当前一代 + 已过期 → 普通未授权(自己是唯一会话,自动换新无反顶)。
	gate.set(7, "jti-A")
	retryable, err = uc.recheckSession(context.Background(), 7, expired)
	if retryable || errcode.As(err) != errcode.ErrUnauthorized {
		t.Fatalf("current-but-expired must map to ErrUnauthorized, retryable=%v err=%v", retryable, err)
	}
}
