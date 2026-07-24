// owner_authority_test.go — census 代提交 Admit 缓存剪枝(复审 P1-2)与
// 漂移自愈弱 Begin(复审 P1-3)单元测试。
package biz

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/data"
)

// scriptedOwnerAuthority:QueryOwner 按注入记录应答;记录 Admit/Begin 调用。
type scriptedOwnerAuthority struct {
	mu      sync.Mutex
	records map[uint64]data.OwnerRecordView
	admits  []uint64
	begins  []uint64
}

func (s *scriptedOwnerAuthority) QueryOwner(_ context.Context, playerID uint64) (data.OwnerRecordView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records[playerID], nil
}

func (s *scriptedOwnerAuthority) BeginTransition(_ context.Context, playerID, _ uint64, _ string, _ int8, target data.OwnerTargetView) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.begins = append(s.begins, playerID)
	// 模拟 owner 侧推进:记录改为指向目标、PENDING、epoch+1。
	rec := s.records[playerID]
	s.records[playerID] = data.OwnerRecordView{
		OwnerEpoch: rec.OwnerEpoch + 1, OwnerType: ownerTypeHub, Phase: ownerPhasePending,
		PodName: target.PodName, InstanceUID: target.InstanceUID, InstanceEpoch: target.InstanceEpoch,
		AssignmentOrAllocationID: target.AssignmentOrAllocationID, ReleaseTrack: target.ReleaseTrack,
	}
	return nil
}

func (s *scriptedOwnerAuthority) Admit(_ context.Context, playerID, _ uint64, _ string, _ data.OwnerTargetView) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.admits = append(s.admits, playerID)
	rec := s.records[playerID]
	rec.Phase = ownerPhaseAdmitted
	s.records[playerID] = rec
	return 0, nil
}

func pendingRecord(pod, uid string, epoch uint64) data.OwnerRecordView {
	return data.OwnerRecordView{
		OwnerEpoch: epoch, OwnerType: ownerTypeHub, Phase: ownerPhasePending,
		PodName: pod, InstanceUID: uid, InstanceEpoch: 1,
		AssignmentOrAllocationID: "a1", ReleaseTrack: "stable", OperationID: "op1",
	}
}

// 复审 P1-2:玩家离场再回流(owner epoch 推进、新 PENDING)后,census 缓存必须
// 被剪枝并重新 Admit,不得被上一纪元的 admitted 缓存误吞。
func TestOwnerAdmitCensus_CachePrunedOnDepartureThenReadmits(t *testing.T) {
	const pod, uid = "hub-1", "uid-1"
	auth := &scriptedOwnerAuthority{records: map[uint64]data.OwnerRecordView{
		1001: pendingRecord(pod, uid, 3),
	}}
	var admitted sync.Map
	ctx := context.Background()

	// 第一轮:PENDING → Admit,进缓存。
	ownerAdmitCensusWeak(ctx, auth, &admitted, []uint64{1001}, ownerTypeHub, pod, uid, time.Second, nil)
	if len(auth.admits) != 1 {
		t.Fatalf("first census must admit once, got %d", len(auth.admits))
	}
	// 第二轮:仍在场,缓存命中,零新调用。
	ownerAdmitCensusWeak(ctx, auth, &admitted, []uint64{1001}, ownerTypeHub, pod, uid, time.Second, nil)
	if len(auth.admits) != 1 {
		t.Fatalf("cached player must not re-admit, got %d", len(auth.admits))
	}
	// 玩家离场:census 不含 1001 → 缓存剪枝(用另一在场玩家触发本轮)。
	auth.records[2002] = pendingRecord(pod, uid, 1)
	ownerAdmitCensusWeak(ctx, auth, &admitted, []uint64{2002}, ownerTypeHub, pod, uid, time.Second, nil)
	// 回流:owner epoch 已推进、新 PENDING → 必须重新 Admit。
	auth.records[1001] = pendingRecord(pod, uid, 9)
	ownerAdmitCensusWeak(ctx, auth, &admitted, []uint64{1001, 2002}, ownerTypeHub, pod, uid, time.Second, nil)
	admitsFor1001 := 0
	for _, id := range auth.admits {
		if id == 1001 {
			admitsFor1001++
		}
	}
	if admitsFor1001 != 2 {
		t.Fatalf("returning player with advanced epoch must be re-admitted, got %d admits", admitsFor1001)
	}
}

// 复审 P1-3:owner 记录漂移(不指向本实例)但归属镜像仍指向本实例 → census 补弱
// Begin 自愈;下一轮 Admit 收敛。归属指向他处/缺失 → 不干预。
func TestOwnerAdmitCensus_HealsDriftedRecordViaResolver(t *testing.T) {
	const pod, uid = "hub-1", "uid-1"
	auth := &scriptedOwnerAuthority{records: map[uint64]data.OwnerRecordView{
		1001: {OwnerEpoch: 4, OwnerType: ownerTypeHub, Phase: ownerPhaseAdmitted,
			PodName: "hub-OLD", InstanceUID: "uid-OLD"}, // 漂移:签票点 Begin 失败留下的旧指向
		2002: {OwnerEpoch: 2, OwnerType: ownerTypeHub, Phase: ownerPhaseAdmitted,
			PodName: "hub-OTHER", InstanceUID: "uid-OTHER"}, // 真实迁移:归属也指向他处
	}}
	resolver := func(_ context.Context, playerID uint64) (data.OwnerTargetView, bool) {
		if playerID == 1001 {
			return data.OwnerTargetView{PodName: pod, InstanceUID: uid, InstanceEpoch: 1,
				AssignmentOrAllocationID: "a1", ReleaseTrack: "stable"}, true
		}
		return data.OwnerTargetView{}, false
	}
	var admitted sync.Map
	ctx := context.Background()

	ownerAdmitCensusWeak(ctx, auth, &admitted, []uint64{1001, 2002}, ownerTypeHub, pod, uid, time.Second, resolver)
	if len(auth.begins) != 1 || auth.begins[0] != 1001 {
		t.Fatalf("only the drifted player backed by local assignment may be healed, got begins=%v", auth.begins)
	}
	// 自愈后下一轮:记录已 PENDING 指向本实例 → Admit 收敛。
	ownerAdmitCensusWeak(ctx, auth, &admitted, []uint64{1001, 2002}, ownerTypeHub, pod, uid, time.Second, resolver)
	if len(auth.admits) != 1 || auth.admits[0] != 1001 {
		t.Fatalf("healed record must converge to admit, got admits=%v", auth.admits)
	}
	if len(auth.begins) != 1 {
		t.Fatalf("heal begin must not repeat after convergence, got begins=%v", auth.begins)
	}
}
