// capacity_test.go — CapacityWatcher 水位状态机 + 事件降噪单测(2026-07-10)。
package biz

import (
	"context"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/data"
)

// fakeLister 返回预设容量快照,记录被调次数。
type fakeLister struct {
	caps []data.FleetCapacity
	err  error
	n    int
}

func (f *fakeLister) ListFleetCapacities(context.Context) ([]data.FleetCapacity, error) {
	f.n++
	return f.caps, f.err
}

func newWatcher(warn float64) *CapacityWatcher {
	return NewCapacityWatcher(&fakeLister{}, conf.AgonesConf{
		CapacityWatchInterval: config.Duration(30 * time.Second),
		CapacityWarnRatio:     warn,
	})
}

func TestNewCapacityWatcher_DisabledOnNegativeInterval(t *testing.T) {
	w := NewCapacityWatcher(&fakeLister{}, conf.AgonesConf{
		CapacityWatchInterval: config.Duration(-1),
	})
	if w != nil {
		t.Fatalf("expected nil watcher when interval<0, got %v", w)
	}
}

func TestLevelFor(t *testing.T) {
	cases := []struct {
		name string
		c    data.FleetCapacity
		want capacityLevel
	}{
		{"idle", data.FleetCapacity{Replicas: 10, Ready: 8, Allocated: 2}, capacityOK},
		{"near", data.FleetCapacity{Replicas: 10, Ready: 2, Allocated: 8}, capacityWarn},
		{"exhausted_ready0", data.FleetCapacity{Replicas: 10, Ready: 0, Allocated: 10}, capacityExhausted},
		{"scaled_to_zero", data.FleetCapacity{Replicas: 0, Ready: 0, Allocated: 0}, capacityExhausted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := levelFor(tc.c, 0.8); got != tc.want {
				t.Errorf("levelFor(%+v)=%d want %d", tc.c, got, tc.want)
			}
		})
	}
}

func TestUsageRatio_ZeroReplicasIsFull(t *testing.T) {
	if got := usageRatio(data.FleetCapacity{Replicas: 0}); got != 1.0 {
		t.Errorf("usageRatio(replicas=0)=%v want 1.0", got)
	}
	if got := usageRatio(data.FleetCapacity{Replicas: 4, Allocated: 1}); got != 0.25 {
		t.Errorf("usageRatio=%v want 0.25", got)
	}
}

// TestObserve_StateMachine 覆盖:首轮 ok 不报 → 升 warn 报 → 同档降噪 → 升 exhausted 报 →
// 回落 ok 报 recovered。
func TestObserve_StateMachine(t *testing.T) {
	w := newWatcher(0.8)
	base := time.Unix(1_700_000_000, 0)
	w.now = func() time.Time { return base }
	f := "pandora-battle"

	// 首轮正常水位:不报
	if ev := w.observe(data.FleetCapacity{Fleet: f, Replicas: 10, Ready: 8, Allocated: 2}); ev != eventNone {
		t.Fatalf("first ok: got %q want none", ev)
	}
	// 升到接近上限:报 near_limit
	if ev := w.observe(data.FleetCapacity{Fleet: f, Replicas: 10, Ready: 2, Allocated: 8}); ev != eventNearLimit {
		t.Fatalf("rise to warn: got %q want near_limit", ev)
	}
	// 同档 30s 后:降噪不报(未到 rewarnInterval)
	w.now = func() time.Time { return base.Add(30 * time.Second) }
	if ev := w.observe(data.FleetCapacity{Fleet: f, Replicas: 10, Ready: 1, Allocated: 9}); ev != eventNone {
		t.Fatalf("same warn level within rewarn: got %q want none", ev)
	}
	// 升到 exhausted:立即报(升档不受降噪限制)
	if ev := w.observe(data.FleetCapacity{Fleet: f, Replicas: 10, Ready: 0, Allocated: 10}); ev != eventExhausted {
		t.Fatalf("rise to exhausted: got %q want exhausted", ev)
	}
	// 回落到正常:报 recovered
	if ev := w.observe(data.FleetCapacity{Fleet: f, Replicas: 10, Ready: 9, Allocated: 1}); ev != eventRecovered {
		t.Fatalf("recover: got %q want recovered", ev)
	}
	// 持续正常:不再报
	if ev := w.observe(data.FleetCapacity{Fleet: f, Replicas: 10, Ready: 9, Allocated: 1}); ev != eventNone {
		t.Fatalf("stay ok: got %q want none", ev)
	}
}

// TestObserve_RewarnAfterInterval 同档持续超限,过了 rewarnInterval 才重报一次。
func TestObserve_RewarnAfterInterval(t *testing.T) {
	w := newWatcher(0.8)
	base := time.Unix(1_700_000_000, 0)
	w.now = func() time.Time { return base }
	f := "pandora-battle"

	if ev := w.observe(data.FleetCapacity{Fleet: f, Replicas: 10, Ready: 1, Allocated: 9}); ev != eventNearLimit {
		t.Fatalf("first warn: got %q want near_limit", ev)
	}
	// 未到 rewarnInterval:不报
	w.now = func() time.Time { return base.Add(rewarnInterval - time.Second) }
	if ev := w.observe(data.FleetCapacity{Fleet: f, Replicas: 10, Ready: 1, Allocated: 9}); ev != eventNone {
		t.Fatalf("before rewarn: got %q want none", ev)
	}
	// 超过 rewarnInterval:重报一次
	w.now = func() time.Time { return base.Add(rewarnInterval + time.Second) }
	if ev := w.observe(data.FleetCapacity{Fleet: f, Replicas: 10, Ready: 1, Allocated: 9}); ev != eventNearLimit {
		t.Fatalf("after rewarn: got %q want near_limit", ev)
	}
}

// TestPollOnce_QueryErrorNoPanic lister 返回 err 时不 panic,指标不更新也安全。
func TestPollOnce_QueryErrorNoPanic(t *testing.T) {
	w := NewCapacityWatcher(&fakeLister{err: context.DeadlineExceeded}, conf.AgonesConf{
		CapacityWatchInterval: config.Duration(30 * time.Second),
		CapacityWarnRatio:     0.8,
	})
	w.pollOnce(context.Background()) // 不 panic 即通过
}
