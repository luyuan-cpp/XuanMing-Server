// owner_lease_test.go — 租约双写门纯单测(owner-authority.md migrate ⑥)。
package biz

import (
	"context"
	"errors"
	"testing"
)

// fakeLeaseRenewer 记录调用并按注入错误返回。
type fakeLeaseRenewer struct {
	calls int
	pod   string
	uid   string
	epoch uint32
	track string
	err   error
}

func (f *fakeLeaseRenewer) RenewInstanceLease(_ context.Context, podName, instanceUID string, instanceEpoch uint32, releaseTrack string) error {
	f.calls++
	f.pod, f.uid, f.epoch, f.track = podName, instanceUID, instanceEpoch, releaseTrack
	return f.err
}

func TestRenewOwnerLeaseGate(t *testing.T) {
	ctx := context.Background()

	// nil renewer:未启用,no-op。
	if err := renewOwnerLeaseGate(ctx, nil, true, "p", "u", 1, "stable"); err != nil {
		t.Fatalf("nil renewer 应 no-op: %v", err)
	}

	// 成功:身份原样透传。
	ok := &fakeLeaseRenewer{}
	if err := renewOwnerLeaseGate(ctx, ok, false, "pod-1", "uid-1", 3, "canary"); err != nil {
		t.Fatalf("成功路径: %v", err)
	}
	if ok.calls != 1 || ok.pod != "pod-1" || ok.uid != "uid-1" || ok.epoch != 3 || ok.track != "canary" {
		t.Fatalf("身份透传错误: %+v", ok)
	}

	// 失败 + 弱依赖(migrate 默认):告警放行,心跳不受影响。
	weak := &fakeLeaseRenewer{err: errors.New("owner unavailable")}
	if err := renewOwnerLeaseGate(ctx, weak, false, "p", "u", 1, ""); err != nil {
		t.Fatalf("弱依赖失败应放行: %v", err)
	}

	// 失败 + 强依赖(contract):心跳必须失败(DS 不延长本地租约 → 自我 fencing 时序闭合)。
	strict := &fakeLeaseRenewer{err: errors.New("owner unavailable")}
	if err := renewOwnerLeaseGate(ctx, strict, true, "p", "u", 1, ""); err == nil {
		t.Fatal("强依赖失败必须令心跳失败")
	}
}
