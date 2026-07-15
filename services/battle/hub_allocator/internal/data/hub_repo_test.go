// hub_repo_test.go — hub_allocator data 层 Redis 实现测试(miniredis)。
package data

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/errcode"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
)

const testTTL = 30 * time.Minute

func newRepo(t *testing.T) (*RedisHubRepo, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis run: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisHubRepo(rdb), mr
}

func sampleShard(pod string, shardID uint32, lastHbMs int64) *hubv1.HubShardStorageRecord {
	return &hubv1.HubShardStorageRecord{
		HubPodName:      pod,
		HubAddr:         "127.0.0.1:7778",
		Region:          "global",
		ShardId:         shardID,
		PlayerCount:     0,
		Capacity:        500,
		State:           "ready",
		LastHeartbeatMs: lastHbMs,
		CreatedAtMs:     1000,
	}
}

func TestShard_CreateGetRoundtrip(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	if err := repo.CreateShard(ctx, sampleShard("pandora-hub-global-1", 1, 0), testTTL); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, found, err := repo.GetShard(ctx, "pandora-hub-global-1")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if got.ShardId != 1 || got.Capacity != 500 || got.State != "ready" {
		t.Fatalf("shard mismatch: %+v", got)
	}
	// 新建不进 active(等首次心跳)
	stale, err := repo.RangeStaleShards(ctx, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("range stale: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("freshly-created shard must not be in active, got %v", stale)
	}
}

func TestShard_ListShards(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	_ = repo.CreateShard(ctx, sampleShard("pandora-hub-global-1", 1, 0), testTTL)
	_ = repo.CreateShard(ctx, sampleShard("pandora-hub-global-2", 2, 0), testTTL)

	shards, err := repo.ListShards(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(shards) != 2 {
		t.Fatalf("want 2 shards, got %d", len(shards))
	}
}

func TestShard_UpdateWithLock(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	_ = repo.CreateShard(ctx, sampleShard("pandora-hub-global-1", 1, 0), testTTL)

	err := repo.UpdateShardWithLock(ctx, "pandora-hub-global-1", 3, func(s *hubv1.HubShardStorageRecord) error {
		s.PlayerCount += 5
		return nil
	}, testTTL)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _, _ := repo.GetShard(ctx, "pandora-hub-global-1")
	if got.PlayerCount != 5 {
		t.Fatalf("want count 5, got %d", got.PlayerCount)
	}
}

func TestShard_HeartbeatKnownAndOrphan(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	_ = repo.CreateShard(ctx, sampleShard("pandora-hub-global-1", 1, 0), testTTL)

	now := time.Now().UnixMilli()
	found, err := repo.HeartbeatShard(ctx, "pandora-hub-global-1", 100, "ready", now, 0, false, testTTL)
	if err != nil || !found {
		t.Fatalf("heartbeat known: found=%v err=%v", found, err)
	}
	got, _, _ := repo.GetShard(ctx, "pandora-hub-global-1")
	if got.PlayerCount != 100 || got.LastHeartbeatMs != now {
		t.Fatalf("heartbeat not reconciled: %+v", got)
	}
	// 心跳后进 active
	stale, _ := repo.RangeStaleShards(ctx, now)
	if len(stale) != 1 || stale[0] != "pandora-hub-global-1" {
		t.Fatalf("active after heartbeat = %v", stale)
	}

	// 孤儿 pod
	found, err = repo.HeartbeatShard(ctx, "pandora-hub-ghost-9", 1, "ready", now, 0, false, testTTL)
	if err != nil {
		t.Fatalf("heartbeat orphan err: %v", err)
	}
	if found {
		t.Fatal("orphan heartbeat should return found=false")
	}
}

func TestShard_RangeStaleExcludesNeverHeartbeated(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	_ = repo.CreateShard(ctx, sampleShard("pandora-hub-global-1", 1, 0), testTTL)

	// 从未心跳(score 0,不在 active),不应出现在 stale
	stale, err := repo.RangeStaleShards(ctx, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("never-heartbeated must not be stale, got %v", stale)
	}
}

func TestShard_RemoveAndRemoveActive(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	_ = repo.CreateShard(ctx, sampleShard("pandora-hub-global-1", 1, 0), testTTL)
	now := time.Now().UnixMilli()
	_, _ = repo.HeartbeatShard(ctx, "pandora-hub-global-1", 1, "ready", now, 0, false, testTTL)

	if err := repo.RemoveActive(ctx, "pandora-hub-global-1"); err != nil {
		t.Fatalf("remove active: %v", err)
	}
	stale, _ := repo.RangeStaleShards(ctx, now)
	if len(stale) != 0 {
		t.Fatalf("after remove active, want none, got %v", stale)
	}

	if err := repo.RemoveShard(ctx, "pandora-hub-global-1"); err != nil {
		t.Fatalf("remove shard: %v", err)
	}
	_, found, _ := repo.GetShard(ctx, "pandora-hub-global-1")
	if found {
		t.Fatal("shard should be gone after remove")
	}
	shards, _ := repo.ListShards(ctx)
	if len(shards) != 0 {
		t.Fatalf("shards set should be empty, got %d", len(shards))
	}
}

// genShard 造一个带指定当前代际(CurrentTokenGen)与状态的分片镜像,供代际门测试用。
func genShard(pod string, gen uint64, state string) *hubv1.HubShardStorageRecord {
	s := sampleShard(pod, 1, 0)
	s.State = state
	s.CurrentTokenGen = gen
	return s
}

// 审核 P1(5/6/7/8):enforce 代际门下,过期/缺失代际的心跳必须在任何镜像变更前 fail-closed ——
// player_count/state/last_heartbeat_ms/TTL 全不动,也不进 active 索引。
func TestShard_HeartbeatStaleGenRejectedZeroMutation(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	_ = repo.CreateShard(ctx, genShard("pandora-hub-global-1", 5, "warming"), testTTL)

	now := time.Now().UnixMilli()
	// 携带旧代际 3(镜像当前代际 5)→ stale。
	found, err := repo.HeartbeatShard(ctx, "pandora-hub-global-1", 100, "ready", now, 3, true, testTTL)
	if err == nil {
		t.Fatal("stale-gen heartbeat must return error")
	}
	if errcode.As(err) != errcode.ErrUnauthorized {
		t.Fatalf("want ErrUnauthorized, got code=%d (%v)", errcode.As(err), err)
	}
	_ = found
	got, _, _ := repo.GetShard(ctx, "pandora-hub-global-1")
	if got.PlayerCount != 0 || got.State != "warming" || got.LastHeartbeatMs != 0 {
		t.Fatalf("stale heartbeat mutated shard: %+v", got)
	}
	// 未进 active 索引(不被保活)。
	stale, _ := repo.RangeStaleShards(ctx, now)
	if len(stale) != 0 {
		t.Fatalf("stale heartbeat must not enter active index, got %v", stale)
	}
}

// 审核 P1(1):enforce(genRequired)下 legacy gen0 心跳(tokenGen==0)一律拒,即便镜像尚未绑定代际。
func TestShard_HeartbeatMissingGenUnderEnforceRejected(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	_ = repo.CreateShard(ctx, genShard("pandora-hub-global-1", 0, "warming"), testTTL)

	now := time.Now().UnixMilli()
	_, err := repo.HeartbeatShard(ctx, "pandora-hub-global-1", 1, "ready", now, 0, true, testTTL)
	if err == nil {
		t.Fatal("gen0 heartbeat under enforce must be rejected")
	}
	got, _, _ := repo.GetShard(ctx, "pandora-hub-global-1")
	if got.State != "warming" || got.LastHeartbeatMs != 0 {
		t.Fatalf("rejected heartbeat mutated shard: %+v", got)
	}
}

// 代际匹配的心跳正常放行:warming→ready 且刷 player_count/心跳时刻。
func TestShard_HeartbeatMatchingGenFlipsReady(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	_ = repo.CreateShard(ctx, genShard("pandora-hub-global-1", 7, "warming"), testTTL)

	now := time.Now().UnixMilli()
	found, err := repo.HeartbeatShard(ctx, "pandora-hub-global-1", 42, "ready", now, 7, true, testTTL)
	if err != nil || !found {
		t.Fatalf("matching-gen heartbeat: found=%v err=%v", found, err)
	}
	got, _, _ := repo.GetShard(ctx, "pandora-hub-global-1")
	if got.State != "ready" || got.PlayerCount != 42 || got.LastHeartbeatMs != now {
		t.Fatalf("matching-gen heartbeat not applied: %+v", got)
	}
}

// permissive 向后兼容:未开代际门(genRequired=false)且镜像无代际(0)→ gen0 心跳照常翻 ready。
func TestShard_HeartbeatGen0PermissiveBackCompat(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	_ = repo.CreateShard(ctx, genShard("pandora-hub-global-1", 0, "warming"), testTTL)

	now := time.Now().UnixMilli()
	found, err := repo.HeartbeatShard(ctx, "pandora-hub-global-1", 3, "ready", now, 0, false, testTTL)
	if err != nil || !found {
		t.Fatalf("permissive gen0 heartbeat: found=%v err=%v", found, err)
	}
	got, _, _ := repo.GetShard(ctx, "pandora-hub-global-1")
	if got.State != "ready" {
		t.Fatalf("permissive gen0 heartbeat should flip ready: %+v", got)
	}
}

func TestAssignment_Roundtrip(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	rec := &hubv1.HubAssignmentStorageRecord{
		PlayerId:     1001,
		HubPodName:   "pandora-hub-global-1",
		HubAddr:      "127.0.0.1:7778",
		ShardId:      1,
		Region:       "global",
		TeamId:       7,
		AssignedAtMs: 2000,
	}
	if err := repo.SetAssignment(ctx, rec, testTTL); err != nil {
		t.Fatalf("set assignment: %v", err)
	}
	got, found, err := repo.GetAssignment(ctx, 1001)
	if err != nil || !found {
		t.Fatalf("get assignment: found=%v err=%v", found, err)
	}
	if got.HubPodName != "pandora-hub-global-1" || got.TeamId != 7 {
		t.Fatalf("assignment mismatch: %+v", got)
	}

	// pod 不匹配 → 不删(并发新归属保护)。
	if deleted, err := repo.DeleteAssignmentIfPodMatches(ctx, 1001, "pandora-hub-global-9"); err != nil || deleted {
		t.Fatalf("mismatched pod must not delete: deleted=%v err=%v", deleted, err)
	}
	if _, found, _ := repo.GetAssignment(ctx, 1001); !found {
		t.Fatal("assignment must survive mismatched delete")
	}

	if deleted, err := repo.DeleteAssignmentIfPodMatches(ctx, 1001, "pandora-hub-global-1"); err != nil || !deleted {
		t.Fatalf("delete: deleted=%v err=%v", deleted, err)
	}
	if _, found, _ := repo.GetAssignment(ctx, 1001); found {
		t.Fatal("assignment should be deleted")
	}
	// 已不存在 → (false, nil) 幂等。
	if deleted, err := repo.DeleteAssignmentIfPodMatches(ctx, 1001, "pandora-hub-global-1"); err != nil || deleted {
		t.Fatalf("missing assignment: deleted=%v err=%v", deleted, err)
	}
}

func TestAssignmentCAS_ConcurrentCreateSingleWinner(t *testing.T) {
	repo, _ := newRepo(t)
	ctx := context.Background()
	const workers = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	errs := make([]error, 0)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			next := &hubv1.HubAssignmentStorageRecord{
				PlayerId: 1001, HubPodName: fmt.Sprintf("hub-%02d", i), AssignmentId: fmt.Sprintf("a-%02d", i),
			}
			ok, err := repo.CompareAndSwapAssignment(ctx, 1001, nil, next, testTTL)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			} else if ok {
				winners++
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(errs) != 0 || winners != 1 {
		t.Fatalf("CAS create must have exactly one winner: winners=%d errs=%v", winners, errs)
	}
	got, found, err := repo.GetAssignment(ctx, 1001)
	if err != nil || !found || got.AssignmentId == "" {
		t.Fatalf("winning assignment missing: found=%v got=%+v err=%v", found, got, err)
	}
}

func TestTeamShard_Roundtrip(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	if _, found, _ := repo.GetTeamShard(ctx, 7); found {
		t.Fatal("missing team shard should be not-found")
	}
	if err := repo.SetTeamShard(ctx, 7, "pandora-hub-global-2", testTTL); err != nil {
		t.Fatalf("set team shard: %v", err)
	}
	pod, found, err := repo.GetTeamShard(ctx, 7)
	if err != nil || !found {
		t.Fatalf("get team shard: found=%v err=%v", found, err)
	}
	if pod != "pandora-hub-global-2" {
		t.Fatalf("team shard mismatch: %s", pod)
	}
}

func TestShardMemberZeroTTLPersistsUntilExactRemoval(t *testing.T) {
	ctx := context.Background()
	repo, mr := newRepo(t)
	const pod = "pandora-hub-global-1"
	if err := repo.AddShardMember(ctx, pod, 1001, 0); err != nil {
		t.Fatalf("add persistent member: %v", err)
	}
	key := membersKey(pod)
	if ttl := mr.TTL(key); ttl != 0 {
		t.Fatalf("strict member index must have no TTL, got %s", ttl)
	}
	mr.FastForward(24 * time.Hour)
	if members, err := repo.ListShardMembers(ctx, pod); err != nil || len(members) != 1 || members[0] != 1001 {
		t.Fatalf("long-connected member vanished: members=%v err=%v", members, err)
	}
	if err := repo.RemoveShardMember(ctx, pod, 1001); err != nil {
		t.Fatalf("exact member removal: %v", err)
	}
	if members, err := repo.ListShardMembers(ctx, pod); err != nil || len(members) != 0 {
		t.Fatalf("removed member remained: members=%v err=%v", members, err)
	}
}

func TestTransferCleanupIndexRegisterListRemoveAndOrphanPrecision(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	const sourceA = "pandora-hub-global-source-a"
	const sourceB = "pandora-hub-global-source-b"
	refsA := []TransferCleanupRef{
		{PlayerID: 1001, TargetAssignmentID: "target-assignment-a"},
		{PlayerID: 1001, TargetAssignmentID: "target-assignment-b"},
		{PlayerID: 1002, TargetAssignmentID: "target-assignment-a"},
	}
	refB := TransferCleanupRef{PlayerID: 2001, TargetAssignmentID: "target-assignment-c"}

	for _, ref := range refsA {
		if err := repo.RegisterTransferCleanup(ctx, sourceA, ref); err != nil {
			t.Fatalf("register source A ref=%+v: %v", ref, err)
		}
	}
	// Registration is a SET operation. An ACK retry must not duplicate work.
	if err := repo.RegisterTransferCleanup(ctx, sourceA, refsA[0]); err != nil {
		t.Fatalf("duplicate register: %v", err)
	}
	if err := repo.RegisterTransferCleanup(ctx, sourceB, refB); err != nil {
		t.Fatalf("register source B: %v", err)
	}

	// The index is deliberately written before assignment CAS. These entries
	// are therefore valid, discoverable orphans and must not invent assignments.
	for _, playerID := range []uint64{1001, 1002, 2001} {
		if assignment, found, err := repo.GetAssignment(ctx, playerID); err != nil || found || assignment != nil {
			t.Fatalf("cleanup registration created assignment player=%d found=%v assignment=%+v err=%v",
				playerID, found, assignment, err)
		}
	}

	pods, err := repo.ListTransferCleanupPods(ctx)
	if err != nil {
		t.Fatal(err)
	}
	podSet := make(map[string]bool, len(pods))
	for _, pod := range pods {
		podSet[pod] = true
	}
	if len(podSet) != 2 || !podSet[sourceA] || !podSet[sourceB] {
		t.Fatalf("cleanup pod index=%v", pods)
	}

	assertRefs := func(want ...TransferCleanupRef) {
		t.Helper()
		got, listErr := repo.ListTransferCleanups(ctx, sourceA)
		if listErr != nil {
			t.Fatal(listErr)
		}
		gotSet := make(map[string]bool, len(got))
		for _, ref := range got {
			gotSet[encodeTransferCleanupRef(ref)] = true
		}
		wantSet := make(map[string]bool, len(want))
		for _, ref := range want {
			wantSet[encodeTransferCleanupRef(ref)] = true
		}
		if len(gotSet) != len(wantSet) {
			t.Fatalf("cleanup refs=%+v want=%+v", got, want)
		}
		for encoded := range wantSet {
			if !gotSet[encoded] {
				t.Fatalf("cleanup refs=%+v missing=%q", got, encoded)
			}
		}
	}
	assertRefs(refsA...)

	// Same player/different target and different player/same target are distinct
	// refs. Removing one must never use a prefix match or delete its neighbours.
	if err := repo.RemoveTransferCleanup(ctx, sourceA, refsA[0]); err != nil {
		t.Fatalf("remove exact ref: %v", err)
	}
	assertRefs(refsA[1], refsA[2])
	if err := repo.RemoveTransferCleanup(ctx, sourceA, refsA[0]); err != nil {
		t.Fatalf("idempotent remove: %v", err)
	}
	assertRefs(refsA[1], refsA[2])

	for _, ref := range refsA[1:] {
		if err := repo.RemoveTransferCleanup(ctx, sourceA, ref); err != nil {
			t.Fatalf("remove remaining ref=%+v: %v", ref, err)
		}
	}
	assertRefs()

	// An empty per-pod set intentionally leaves the global pod as a persistent
	// superset/orphan. This avoids a last-remove racing a concurrent register;
	// a later registration under the same source remains discoverable.
	pods, err = repo.ListTransferCleanupPods(ctx)
	if err != nil {
		t.Fatal(err)
	}
	podSet = make(map[string]bool, len(pods))
	for _, pod := range pods {
		podSet[pod] = true
	}
	if !podSet[sourceA] || !podSet[sourceB] {
		t.Fatalf("empty source pod disappeared from superset index: %v", pods)
	}
	if err := repo.RegisterTransferCleanup(ctx, sourceA, refsA[0]); err != nil {
		t.Fatalf("register after orphan: %v", err)
	}
	assertRefs(refsA[0])
}
