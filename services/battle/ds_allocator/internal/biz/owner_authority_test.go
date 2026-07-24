// owner_authority_test.go — census 代提交 Admit 缓存有界性回归测试(压测前审核 P1)。
//
// 覆盖 ownerAdmitCensusWeak 的 last-touch time.Time 值 + 本实例 census 剪枝,以及
// sweepStaleOwnerAdmitted 对死实例(UID 不再心跳续期)项的 TTL 清扫——防 ownerAdmitted
// sync.Map 随累计对局的历史 InstanceUID 无界增长导致 ds_allocator OOM。
package biz

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/data"
)

// scriptedOwnerAuthority:QueryOwner 按注入记录应答;记录 Admit 调用。
type scriptedOwnerAuthority struct {
	mu      sync.Mutex
	records map[uint64]data.OwnerRecordView
	admits  []uint64
}

func (s *scriptedOwnerAuthority) QueryOwner(_ context.Context, playerID uint64) (data.OwnerRecordView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records[playerID], nil
}

func (s *scriptedOwnerAuthority) BeginTransition(_ context.Context, _, _ uint64, _ string, _ int8, _ data.OwnerTargetView) error {
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

func pendingBattleRecord(pod, uid string, epoch uint64) data.OwnerRecordView {
	return data.OwnerRecordView{
		OwnerEpoch: epoch, OwnerType: ownerTypeBattle, Phase: ownerPhasePending,
		PodName: pod, InstanceUID: uid, InstanceEpoch: 1,
		AssignmentOrAllocationID: "a1", ReleaseTrack: "stable", OperationID: "op1",
	}
}

// 玩家离场再回流(owner epoch 推进、新 PENDING)后,census 缓存必须被剪枝并重新 Admit,
// 不得被上一纪元的 admitted 缓存误吞。修复前值为 struct{}{}、无剪枝,回流玩家会被误吞。
func TestOwnerAdmitCensus_CachePrunedOnDepartureThenReadmits(t *testing.T) {
	const pod, uid = "battle-1", "uid-1"
	auth := &scriptedOwnerAuthority{records: map[uint64]data.OwnerRecordView{
		1001: pendingBattleRecord(pod, uid, 3),
	}}
	var admitted sync.Map
	ctx := context.Background()

	// 第一轮:PENDING → Admit,进缓存。
	ownerAdmitCensusWeak(ctx, auth, &admitted, []uint64{1001}, ownerTypeBattle, pod, uid, time.Second)
	if len(auth.admits) != 1 {
		t.Fatalf("first census must admit once, got %d", len(auth.admits))
	}
	// 缓存值必须是 last-touch time.Time(不再是 struct{}{}),否则 TTL sweep 无法判老化。
	if v, ok := admitted.Load(uid + "|1001"); !ok {
		t.Fatal("census 应把 1001 写入缓存")
	} else if _, isTime := v.(time.Time); !isTime {
		t.Fatalf("缓存值必须为 time.Time(last-touch),实得 %T", v)
	}
	// 第二轮:仍在场,缓存命中,零新 Admit。
	ownerAdmitCensusWeak(ctx, auth, &admitted, []uint64{1001}, ownerTypeBattle, pod, uid, time.Second)
	if len(auth.admits) != 1 {
		t.Fatalf("cached player must not re-admit, got %d", len(auth.admits))
	}
	// 玩家离场:census 换成另一在场玩家 → 1001 被剪枝。
	auth.records[2002] = pendingBattleRecord(pod, uid, 1)
	ownerAdmitCensusWeak(ctx, auth, &admitted, []uint64{2002}, ownerTypeBattle, pod, uid, time.Second)
	if _, ok := admitted.Load(uid + "|1001"); ok {
		t.Fatal("离场玩家的 admitted 缓存项必须被剪枝")
	}
	// 回流:owner epoch 已推进、新 PENDING → 必须重新 Admit。
	auth.records[1001] = pendingBattleRecord(pod, uid, 9)
	ownerAdmitCensusWeak(ctx, auth, &admitted, []uint64{1001, 2002}, ownerTypeBattle, pod, uid, time.Second)
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

// sweepStaleOwnerAdmitted:已销毁实例(UID 不再心跳续期)的项老化超 cutoff 被清,活实例
// (刚续期)的项保留;非 time.Time 值 fail-safe 清除。防 ownerAdmitted 随历史 UID 无界增长。
func TestSweepStaleOwnerAdmitted(t *testing.T) {
	const pod, uid = "battle-1", "uid-1"
	auth := &scriptedOwnerAuthority{records: map[uint64]data.OwnerRecordView{
		1001: pendingBattleRecord(pod, uid, 3),
	}}
	var admitted sync.Map
	// 一轮 census:1001 进缓存(值为 last-touch time.Time)。
	ownerAdmitCensusWeak(context.Background(), auth, &admitted, []uint64{1001}, ownerTypeBattle, pod, uid, time.Second)
	if _, ok := admitted.Load(uid + "|1001"); !ok {
		t.Fatal("census 应把 1001 写入缓存")
	}
	// 模拟死实例遗留项(旧 UID,last-touch 很久以前)。
	admitted.Store("uid-DEAD|2002", time.Now().Add(-time.Hour))
	// cutoff = now-ownerAdmittedStaleTTL:活实例项(刚续期)保留,死实例项清除。
	sweepStaleOwnerAdmitted(&admitted, time.Now().Add(-ownerAdmittedStaleTTL))
	if _, ok := admitted.Load(uid + "|1001"); !ok {
		t.Fatal("活实例(刚续期)缓存项不应被清")
	}
	if _, ok := admitted.Load("uid-DEAD|2002"); ok {
		t.Fatal("死实例(超 TTL 未续期)缓存项必须被清")
	}
	// 非 time.Time 值(修复前的 struct{}{})也应被 fail-safe 清除。
	admitted.Store("uid-BAD|3003", struct{}{})
	sweepStaleOwnerAdmitted(&admitted, time.Now().Add(-ownerAdmittedStaleTTL))
	if _, ok := admitted.Load("uid-BAD|3003"); ok {
		t.Fatal("非 time.Time 值应被 fail-safe 清除")
	}
}
