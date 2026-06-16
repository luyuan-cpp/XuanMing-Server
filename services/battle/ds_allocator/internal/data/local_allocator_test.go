// local_allocator_test.go — LocalGameServerAllocator 单测。
//
// 用注入的假进程(fakeProc)绕过真 exec UE Windows DS:fakeProc.Wait() 阻塞直到 Kill(),
// 模拟一个长驻进程,从而能确定性地测端口分配、台账、幂等、释放、Close 全杀。
package data

import (
	"context"
	"sync"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
)

// fakeProc 模拟一个运行中的 DS 进程:Wait 阻塞到 Kill 才返回。
type fakeProc struct {
	killed chan struct{}
	once   sync.Once
	mu     sync.Mutex
	killN  int
}

func newFakeProc() *fakeProc { return &fakeProc{killed: make(chan struct{})} }

func (f *fakeProc) Kill() error {
	f.mu.Lock()
	f.killN++
	f.mu.Unlock()
	f.once.Do(func() { close(f.killed) })
	return nil
}

func (f *fakeProc) Wait() error {
	<-f.killed
	return nil
}

func (f *fakeProc) killCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.killN
}

// newLocalTestAllocator 构造一个注入假进程的 allocator(不校验 executable 文件存在)。
func newLocalTestAllocator(t *testing.T, cfg conf.LocalDSConf) (*LocalGameServerAllocator, *[]*fakeProc) {
	t.Helper()
	if cfg.AdvertiseHost == "" {
		cfg.AdvertiseHost = "127.0.0.1"
	}
	l := &LocalGameServerAllocator{
		cfg:       cfg,
		procs:     make(map[string]*launchedProc),
		usedPorts: make(map[int]struct{}),
	}
	var mu sync.Mutex
	created := make([]*fakeProc, 0)
	l.startProc = func(_ string, _ int, _ uint64, _ uint32, _ string) (dsProcess, error) {
		fp := newFakeProc()
		mu.Lock()
		created = append(created, fp)
		mu.Unlock()
		return fp, nil
	}
	return l, &created
}

func TestNewLocalGameServerAllocator_RequiresExecutable(t *testing.T) {
	if _, err := NewLocalGameServerAllocator(conf.LocalDSConf{}); err == nil {
		t.Fatal("expected error when executable_path empty")
	}
}

func TestNewLocalGameServerAllocator_RejectsBadPortRange(t *testing.T) {
	// 用本测试文件自身当一个"存在的可执行路径"绕过 Stat 校验,只验 port_range。
	if _, err := NewLocalGameServerAllocator(conf.LocalDSConf{
		ExecutablePath: "local_allocator_test.go",
		PortRange:      0,
	}); err == nil {
		t.Fatal("expected error when port_range <= 0")
	}
}

func TestAllocate_ReturnsTrackedAddr(t *testing.T) {
	l, created := newLocalTestAllocator(t, conf.LocalDSConf{PortBase: 7777, PortRange: 10})
	pod, addr, err := l.Allocate(context.Background(), 42, 1, "moba5v5")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if pod != "pandora-battle-local-42" {
		t.Fatalf("pod=%q", pod)
	}
	if addr != "127.0.0.1:7777" {
		t.Fatalf("addr=%q", addr)
	}
	if len(*created) != 1 {
		t.Fatalf("expected 1 process started, got %d", len(*created))
	}
}

func TestAllocate_Idempotent(t *testing.T) {
	l, created := newLocalTestAllocator(t, conf.LocalDSConf{PortBase: 7777, PortRange: 10})
	_, addr1, err := l.Allocate(context.Background(), 7, 1, "")
	if err != nil {
		t.Fatalf("Allocate#1: %v", err)
	}
	_, addr2, err := l.Allocate(context.Background(), 7, 1, "")
	if err != nil {
		t.Fatalf("Allocate#2: %v", err)
	}
	if addr1 != addr2 {
		t.Fatalf("idempotent addr mismatch: %q vs %q", addr1, addr2)
	}
	if len(*created) != 1 {
		t.Fatalf("expected only 1 process for repeated allocate, got %d", len(*created))
	}
}

func TestAllocate_DistinctPorts(t *testing.T) {
	l, _ := newLocalTestAllocator(t, conf.LocalDSConf{PortBase: 7777, PortRange: 10})
	_, addr1, _ := l.Allocate(context.Background(), 1, 1, "")
	_, addr2, _ := l.Allocate(context.Background(), 2, 1, "")
	if addr1 == addr2 {
		t.Fatalf("expected distinct ports, both %q", addr1)
	}
}

func TestAllocate_PortExhaustion(t *testing.T) {
	l, _ := newLocalTestAllocator(t, conf.LocalDSConf{PortBase: 7777, PortRange: 2})
	if _, _, err := l.Allocate(context.Background(), 1, 1, ""); err != nil {
		t.Fatalf("Allocate#1: %v", err)
	}
	if _, _, err := l.Allocate(context.Background(), 2, 1, ""); err != nil {
		t.Fatalf("Allocate#2: %v", err)
	}
	_, _, err := l.Allocate(context.Background(), 3, 1, "")
	if err == nil {
		t.Fatal("expected ErrDSNoAvailable on port exhaustion")
	}
	if errcode.As(err) != errcode.ErrDSNoAvailable {
		t.Fatalf("expected ErrDSNoAvailable, got %v", err)
	}
}

func TestRelease_KillsAndFreesPort(t *testing.T) {
	l, created := newLocalTestAllocator(t, conf.LocalDSConf{PortBase: 7777, PortRange: 1})
	pod, _, err := l.Allocate(context.Background(), 1, 1, "")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if err := l.Release(context.Background(), pod); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if (*created)[0].killCount() == 0 {
		t.Fatal("expected process killed on Release")
	}
	// 端口已释放 → 再分配应成功(池子只有 1 个端口)。
	if _, _, err := l.Allocate(context.Background(), 2, 1, ""); err != nil {
		t.Fatalf("re-Allocate after release: %v", err)
	}
}

func TestRelease_IdempotentOnMissing(t *testing.T) {
	l, _ := newLocalTestAllocator(t, conf.LocalDSConf{PortBase: 7777, PortRange: 10})
	if err := l.Release(context.Background(), "pandora-battle-local-999"); err != nil {
		t.Fatalf("Release missing should be nil, got %v", err)
	}
	if err := l.Release(context.Background(), ""); err != nil {
		t.Fatalf("Release empty should be nil, got %v", err)
	}
}

func TestClose_KillsAll(t *testing.T) {
	l, created := newLocalTestAllocator(t, conf.LocalDSConf{PortBase: 7777, PortRange: 10})
	_, _, _ = l.Allocate(context.Background(), 1, 1, "")
	_, _, _ = l.Allocate(context.Background(), 2, 1, "")
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i, fp := range *created {
		if fp.killCount() == 0 {
			t.Fatalf("process %d not killed on Close", i)
		}
	}
}
