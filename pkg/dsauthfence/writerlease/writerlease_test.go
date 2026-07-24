package writerlease

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeTerm 是确定性任期:token 由测试注入,Lost/Resign 可控。
type fakeTerm struct {
	token    uint64
	lost     chan struct{}
	mu       sync.Mutex
	resigned bool
}

func newFakeTerm(token uint64) *fakeTerm {
	return &fakeTerm{token: token, lost: make(chan struct{})}
}

func (t *fakeTerm) Token() uint64         { return t.token }
func (t *fakeTerm) Lost() <-chan struct{} { return t.lost }
func (t *fakeTerm) Resign(context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resigned = true
	return nil
}

func (t *fakeTerm) wasResigned() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.resigned
}

// fakeBackend 按脚本依次颁发任期;脚本耗尽后 Campaign 阻塞到 ctx 取消。
type fakeBackend struct {
	mu     sync.Mutex
	terms  []*fakeTerm
	closed bool
}

func (b *fakeBackend) Campaign(ctx context.Context, _ string) (Term, error) {
	b.mu.Lock()
	if len(b.terms) > 0 {
		term := b.terms[0]
		b.terms = b.terms[1:]
		b.mu.Unlock()
		return term, nil
	}
	b.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *fakeBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestElectedExposesTokenAndHeld(t *testing.T) {
	term := newFakeTerm(42)
	backend := &fakeBackend{terms: []*fakeTerm{term}}
	lease := StartWithBackend(context.Background(), backend, Config{Election: "hub_allocator/writer"})
	defer func() { _ = lease.Close() }()

	waitFor(t, "elected", func() bool {
		token, held := lease.Current()
		return held && token == 42
	})
}

func TestLostStepsDownImmediatelyAndReelectsWithLargerToken(t *testing.T) {
	first := newFakeTerm(100)
	second := newFakeTerm(250)
	backend := &fakeBackend{terms: []*fakeTerm{first, second}}
	lease := StartWithBackend(context.Background(), backend, Config{Election: "hub_allocator/writer"})
	defer func() { _ = lease.Close() }()

	waitFor(t, "first term", func() bool {
		token, held := lease.Current()
		return held && token == 100
	})

	close(first.lost)
	// 失主必须先撤本地持有权,再清理旧任期。
	waitFor(t, "step down", func() bool {
		_, held := lease.Current()
		return !held || func() bool { token, _ := lease.Current(); return token == 250 }()
	})
	waitFor(t, "old term resigned", first.wasResigned)
	waitFor(t, "second term with strictly larger token", func() bool {
		token, held := lease.Current()
		return held && token == 250
	})
}

func TestCloseResignsAndStopsCampaign(t *testing.T) {
	term := newFakeTerm(7)
	backend := &fakeBackend{terms: []*fakeTerm{term}}
	lease := StartWithBackend(context.Background(), backend, Config{Election: "hub_allocator/writer"})

	waitFor(t, "elected", func() bool {
		_, held := lease.Current()
		return held
	})

	if err := lease.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, held := lease.Current(); held {
		t.Fatal("lease must not report held after Close")
	}
	if !term.wasResigned() {
		t.Fatal("term must be resigned on Close")
	}
	backend.mu.Lock()
	closed := backend.closed
	backend.mu.Unlock()
	if !closed {
		t.Fatal("backend must be closed on Close")
	}
	// Close 幂等。
	if err := lease.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestCampaignErrorRetriesWithoutHolding(t *testing.T) {
	// 空脚本:Campaign 阻塞到 ctx 取消,期间绝不能报告持有。
	backend := &fakeBackend{}
	lease := StartWithBackend(context.Background(), backend, Config{Election: "hub_allocator/writer"})
	defer func() { _ = lease.Close() }()

	time.Sleep(30 * time.Millisecond)
	if _, held := lease.Current(); held {
		t.Fatal("lease must not report held while campaigning")
	}
}

// failNTimesBackend:前 n 次 Campaign 返回错误,之后按脚本颁发任期。
type failNTimesBackend struct {
	fakeBackend
	mu2   sync.Mutex
	fails int
}

func (b *failNTimesBackend) Campaign(ctx context.Context, id string) (Term, error) {
	b.mu2.Lock()
	if b.fails > 0 {
		b.fails--
		b.mu2.Unlock()
		return nil, errors.New("etcd unreachable")
	}
	b.mu2.Unlock()
	return b.fakeBackend.Campaign(ctx, id)
}

// 复审 P0-6:竞选失败必须可观测——Health() 暴露连续失败计数与最近错误;
// 当选后计数清零、错误清空。
func TestCampaignFailureObservableViaHealth(t *testing.T) {
	term := newFakeTerm(9)
	backend := &failNTimesBackend{fails: 2}
	backend.terms = []*fakeTerm{term}
	lease := StartWithBackend(context.Background(), backend, Config{Election: "hub_allocator/writer"})
	defer func() { _ = lease.Close() }()

	waitFor(t, "campaign failures counted", func() bool {
		n, last := lease.Health()
		return n >= 1 && last == "etcd unreachable"
	})
	// 失败期间不得报告持有。
	if _, held := lease.Current(); held {
		t.Fatal("lease must not report held while campaign is failing")
	}
	// 退避 2s×2 后当选:计数清零、错误清空(用长等待覆盖两次退避)。
	waitFor2(t, "elected after failures", 6*time.Second, func() bool {
		_, held := lease.Current()
		return held
	})
	n, last := lease.Health()
	if n != 0 || last != "" {
		t.Fatalf("election must reset health counters, got n=%d last=%q", n, last)
	}
}

// waitFor2:自定义超时版 waitFor(竞选退避 2s,默认 3s 不够)。
func waitFor2(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestStartValidatesConfig(t *testing.T) {
	if _, err := Start(context.Background(), Config{Election: "x"}); err == nil {
		t.Fatal("empty endpoints must fail fast")
	}
	if _, err := Start(context.Background(), Config{Endpoints: []string{"127.0.0.1:2379"}}); err == nil {
		t.Fatal("empty election must fail fast")
	}
}
