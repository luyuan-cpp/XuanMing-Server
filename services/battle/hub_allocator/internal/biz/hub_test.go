package biz

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/data"
)

// ── 测试替身 ──────────────────────────────────────────────────────────────────

// fakeRepo 是 data.HubRepo 的内存实现(无 Redis)。所有读返回克隆,避免别名污染。
type fakeRepo struct {
	mu          sync.Mutex
	shards      map[string]*hubv1.HubShardStorageRecord
	active      map[string]int64 // pod → last_heartbeat_ms
	assignments map[uint64]*hubv1.HubAssignmentStorageRecord
	teamShards  map[uint64]string
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		shards:      map[string]*hubv1.HubShardStorageRecord{},
		active:      map[string]int64{},
		assignments: map[uint64]*hubv1.HubAssignmentStorageRecord{},
		teamShards:  map[uint64]string{},
	}
}

func (f *fakeRepo) GetShard(_ context.Context, pod string) (*hubv1.HubShardStorageRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.shards[pod]
	if !ok {
		return nil, false, nil
	}
	return proto.Clone(s).(*hubv1.HubShardStorageRecord), true, nil
}

func (f *fakeRepo) ListShards(_ context.Context) ([]*hubv1.HubShardStorageRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*hubv1.HubShardStorageRecord, 0, len(f.shards))
	for _, s := range f.shards {
		out = append(out, proto.Clone(s).(*hubv1.HubShardStorageRecord))
	}
	return out, nil
}

func (f *fakeRepo) CreateShard(_ context.Context, rec *hubv1.HubShardStorageRecord, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shards[rec.HubPodName] = proto.Clone(rec).(*hubv1.HubShardStorageRecord)
	return nil
}

func (f *fakeRepo) UpdateShardWithLock(_ context.Context, pod string, _ int, fn func(*hubv1.HubShardStorageRecord) error, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.shards[pod]
	if !ok {
		return errcode.New(errcode.ErrHubNoAvailable, "shard %s not found", pod)
	}
	clone := proto.Clone(s).(*hubv1.HubShardStorageRecord)
	if err := fn(clone); err != nil {
		return err
	}
	f.shards[pod] = clone
	return nil
}

func (f *fakeRepo) HeartbeatShard(_ context.Context, pod string, playerCount int32, state string, tsMs int64, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.shards[pod]
	if !ok {
		return false, nil
	}
	s.PlayerCount = playerCount
	if state != "" {
		s.State = state
	}
	s.LastHeartbeatMs = tsMs
	f.active[pod] = tsMs
	return true, nil
}

func (f *fakeRepo) RemoveShard(_ context.Context, pod string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.shards, pod)
	delete(f.active, pod)
	return nil
}

func (f *fakeRepo) RangeStaleShards(_ context.Context, thresholdMs int64) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for pod, ts := range f.active {
		if ts > 0 && ts <= thresholdMs {
			out = append(out, pod)
		}
	}
	return out, nil
}

func (f *fakeRepo) RemoveActive(_ context.Context, pod string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.active, pod)
	return nil
}

func (f *fakeRepo) GetAssignment(_ context.Context, playerID uint64) (*hubv1.HubAssignmentStorageRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.assignments[playerID]
	if !ok {
		return nil, false, nil
	}
	return proto.Clone(a).(*hubv1.HubAssignmentStorageRecord), true, nil
}

func (f *fakeRepo) SetAssignment(_ context.Context, rec *hubv1.HubAssignmentStorageRecord, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assignments[rec.PlayerId] = proto.Clone(rec).(*hubv1.HubAssignmentStorageRecord)
	return nil
}

func (f *fakeRepo) DeleteAssignment(_ context.Context, playerID uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.assignments, playerID)
	return nil
}

func (f *fakeRepo) GetTeamShard(_ context.Context, teamID uint64) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pod, ok := f.teamShards[teamID]
	return pod, ok, nil
}

func (f *fakeRepo) SetTeamShard(_ context.Context, teamID uint64, pod string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teamShards[teamID] = pod
	return nil
}

// playerCount 是测试断言辅助。
func (f *fakeRepo) playerCount(pod string) int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.shards[pod]; ok {
		return s.PlayerCount
	}
	return -1
}

// fakeSigner 返回确定性假票据。
type fakeSigner struct{ calls int }

func (s *fakeSigner) SignHubTicket(playerID uint64) (string, int64, error) {
	s.calls++
	return "hub-ticket-fake", time.Now().Add(5 * time.Minute).UnixMilli(), nil
}

var _ data.HubRepo = (*fakeRepo)(nil)
var _ TicketSigner = (*fakeSigner)(nil)

// ── 测试夹具 ──────────────────────────────────────────────────────────────────

func testConf() conf.HubConf {
	c := conf.Config{}
	c.Defaults()
	return c.Hub
}

func newTestUsecase(capacity int32, shardCount int) (*HubUsecase, *fakeRepo, *fakeSigner) {
	cfg := testConf()
	cfg.DefaultCapacity = capacity
	cfg.MockShardCount = shardCount
	repo := newFakeRepo()
	fleet := NewMockHubFleetProvider(cfg)
	signer := &fakeSigner{}
	return NewHubUsecase(repo, fleet, signer, cfg), repo, signer
}

// ── 测试用例 ──────────────────────────────────────────────────────────────────

func TestAssignHub_LazySeedAndLeastLoaded(t *testing.T) {
	uc, repo, _ := newTestUsecase(500, 3)
	ctx := context.Background()

	res, err := uc.AssignHub(ctx, 1001, "global", 0)
	if err != nil {
		t.Fatalf("AssignHub err: %v", err)
	}
	// 空集合 lazy-seed 后,最空分片并列取 shard_id 最小 → shard 1
	if res.ShardID != 1 {
		t.Fatalf("want shard 1, got %d", res.ShardID)
	}
	if res.HubTicket == "" {
		t.Fatal("want hub ticket")
	}
	if got := repo.playerCount("pandora-hub-global-1"); got != 1 {
		t.Fatalf("want player_count 1, got %d", got)
	}
	// 共种 3 个分片
	shards, _ := repo.ListShards(ctx)
	if len(shards) != 3 {
		t.Fatalf("want 3 seeded shards, got %d", len(shards))
	}
}

func TestAssignHub_Idempotent(t *testing.T) {
	uc, repo, signer := newTestUsecase(500, 3)
	ctx := context.Background()

	r1, err := uc.AssignHub(ctx, 1001, "global", 0)
	if err != nil {
		t.Fatalf("first assign err: %v", err)
	}
	r2, err := uc.AssignHub(ctx, 1001, "global", 0)
	if err != nil {
		t.Fatalf("second assign err: %v", err)
	}
	if r1.HubPodName != r2.HubPodName {
		t.Fatalf("idempotent assign should return same pod: %s vs %s", r1.HubPodName, r2.HubPodName)
	}
	// 不重复占位:player_count 仍为 1
	if got := repo.playerCount(r1.HubPodName); got != 1 {
		t.Fatalf("idempotent should not double-count, got %d", got)
	}
	// 两次都重签票
	if signer.calls != 2 {
		t.Fatalf("want 2 sign calls, got %d", signer.calls)
	}
}

func TestAssignHub_SpreadAcrossShards(t *testing.T) {
	uc, repo, _ := newTestUsecase(500, 3)
	ctx := context.Background()

	// 3 个玩家应分散到 3 个分片(每次选最空)
	pods := map[string]bool{}
	for i := uint64(1); i <= 3; i++ {
		res, err := uc.AssignHub(ctx, i, "global", 0)
		if err != nil {
			t.Fatalf("assign p%d err: %v", i, err)
		}
		pods[res.HubPodName] = true
	}
	if len(pods) != 3 {
		t.Fatalf("want 3 distinct shards, got %d", len(pods))
	}
	for pod := range pods {
		if got := repo.playerCount(pod); got != 1 {
			t.Fatalf("shard %s want count 1, got %d", pod, got)
		}
	}
}

func TestAssignHub_CapacityFull(t *testing.T) {
	uc, _, _ := newTestUsecase(1, 1) // 1 分片,容量 1
	ctx := context.Background()

	if _, err := uc.AssignHub(ctx, 1001, "global", 0); err != nil {
		t.Fatalf("first assign err: %v", err)
	}
	_, err := uc.AssignHub(ctx, 1002, "global", 0)
	if err == nil {
		t.Fatal("want capacity-full error")
	}
	if errcode.As(err) != errcode.ErrHubNoAvailable {
		t.Fatalf("want ErrHubNoAvailable, got code %d", errcode.As(err))
	}
}

func TestAssignHub_TeammateColocation(t *testing.T) {
	uc, repo, _ := newTestUsecase(500, 3)
	ctx := context.Background()

	r1, err := uc.AssignHub(ctx, 1001, "global", 7) // team 7
	if err != nil {
		t.Fatalf("p1 assign err: %v", err)
	}
	r2, err := uc.AssignHub(ctx, 1002, "global", 7) // same team
	if err != nil {
		t.Fatalf("p2 assign err: %v", err)
	}
	if r1.HubPodName != r2.HubPodName {
		t.Fatalf("teammates should co-locate: %s vs %s", r1.HubPodName, r2.HubPodName)
	}
	if got := repo.playerCount(r1.HubPodName); got != 2 {
		t.Fatalf("co-located shard want count 2, got %d", got)
	}
}

func TestReleaseHub_DecrementAndIdempotent(t *testing.T) {
	uc, repo, _ := newTestUsecase(500, 3)
	ctx := context.Background()

	res, err := uc.AssignHub(ctx, 1001, "global", 0)
	if err != nil {
		t.Fatalf("assign err: %v", err)
	}
	if err := uc.ReleaseHub(ctx, 1001); err != nil {
		t.Fatalf("release err: %v", err)
	}
	if got := repo.playerCount(res.HubPodName); got != 0 {
		t.Fatalf("after release want count 0, got %d", got)
	}
	// 幂等:再次 release 不报错、不变负
	if err := uc.ReleaseHub(ctx, 1001); err != nil {
		t.Fatalf("idempotent release err: %v", err)
	}
	if got := repo.playerCount(res.HubPodName); got != 0 {
		t.Fatalf("idempotent release count drift, got %d", got)
	}
}

func TestTransferHub_MoveBetweenShards(t *testing.T) {
	uc, repo, _ := newTestUsecase(500, 3)
	ctx := context.Background()

	r1, err := uc.AssignHub(ctx, 1001, "global", 0) // shard 1
	if err != nil {
		t.Fatalf("assign err: %v", err)
	}
	// 点名传送到 shard 2
	tr, err := uc.TransferHub(ctx, 1001, 2)
	if err != nil {
		t.Fatalf("transfer err: %v", err)
	}
	if tr.NewHubPodName == r1.HubPodName {
		t.Fatalf("transfer should change pod, still %s", tr.NewHubPodName)
	}
	// 旧分片退位、新分片占位
	if got := repo.playerCount(r1.HubPodName); got != 0 {
		t.Fatalf("old shard want 0, got %d", got)
	}
	if got := repo.playerCount(tr.NewHubPodName); got != 1 {
		t.Fatalf("new shard want 1, got %d", got)
	}
	// 归属更新到新分片
	a, found, _ := repo.GetAssignment(ctx, 1001)
	if !found || a.HubPodName != tr.NewHubPodName {
		t.Fatalf("assignment not moved: found=%v pod=%v", found, a)
	}
}

func TestTransferHub_NotInHub(t *testing.T) {
	uc, _, _ := newTestUsecase(500, 3)
	ctx := context.Background()
	_, err := uc.TransferHub(ctx, 9999, 0)
	if err == nil {
		t.Fatal("want transfer-failed for player not in hub")
	}
	if errcode.As(err) != errcode.ErrHubTransferFailed {
		t.Fatalf("want ErrHubTransferFailed, got %d", errcode.As(err))
	}
}

func TestHeartbeat_OrphanReturnsStop(t *testing.T) {
	uc, _, _ := newTestUsecase(500, 3)
	ctx := context.Background()
	// 无对应分片镜像 → stop
	res, err := uc.Heartbeat(ctx, "pandora-hub-ghost-9", 0, "ready", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat err: %v", err)
	}
	if res.Command != commandStop {
		t.Fatalf("orphan want stop, got %q", res.Command)
	}
}

func TestHeartbeat_KnownShardNoCommand(t *testing.T) {
	uc, repo, _ := newTestUsecase(500, 3)
	ctx := context.Background()
	// 先 assign 触发种子,再心跳已知分片
	if _, err := uc.AssignHub(ctx, 1001, "global", 0); err != nil {
		t.Fatalf("assign err: %v", err)
	}
	now := time.Now().UnixMilli()
	res, err := uc.Heartbeat(ctx, "pandora-hub-global-1", 42, "ready", now)
	if err != nil {
		t.Fatalf("heartbeat err: %v", err)
	}
	if res.Command != commandNone {
		t.Fatalf("known shard want no command, got %q", res.Command)
	}
	if got := repo.playerCount("pandora-hub-global-1"); got != 42 {
		t.Fatalf("heartbeat should reconcile count to 42, got %d", got)
	}
}

func TestSweepOnce_MarksStaleDraining(t *testing.T) {
	uc, repo, _ := newTestUsecase(500, 3)
	ctx := context.Background()
	if _, err := uc.AssignHub(ctx, 1001, "global", 0); err != nil {
		t.Fatalf("assign err: %v", err)
	}
	pod := "pandora-hub-global-1"
	// 心跳一个很旧的时间戳 → 进 active 且已超时
	staleTs := time.Now().Add(-1 * time.Hour).UnixMilli()
	if _, err := uc.Heartbeat(ctx, pod, 1, "ready", staleTs); err != nil {
		t.Fatalf("heartbeat err: %v", err)
	}

	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweepOnce err: %v", err)
	}
	s, _, _ := repo.GetShard(ctx, pod)
	if s.State != stateDraining {
		t.Fatalf("stale shard should be draining, got %q", s.State)
	}
	// 已移出 active(不再扫描)
	stale, _ := repo.RangeStaleShards(ctx, time.Now().UnixMilli())
	for _, p := range stale {
		if p == pod {
			t.Fatal("drained shard should be removed from active")
		}
	}
}

func TestSweepOnce_SkipsNeverHeartbeated(t *testing.T) {
	uc, repo, _ := newTestUsecase(500, 3)
	ctx := context.Background()
	// 仅 assign(Mock 种子 last_heartbeat_ms=0,从不进 active)
	if _, err := uc.AssignHub(ctx, 1001, "global", 0); err != nil {
		t.Fatalf("assign err: %v", err)
	}
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweepOnce err: %v", err)
	}
	// 种子分片不应被误标 draining
	s, _, _ := repo.GetShard(ctx, "pandora-hub-global-1")
	if s.State != stateReady {
		t.Fatalf("never-heartbeated seed should stay ready, got %q", s.State)
	}
}

func TestAssignHub_InvalidPlayer(t *testing.T) {
	uc, _, _ := newTestUsecase(500, 3)
	if _, err := uc.AssignHub(context.Background(), 0, "global", 0); err == nil {
		t.Fatal("want invalid-arg error for player_id 0")
	} else if errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("want ErrInvalidArg, got %d", errcode.As(err))
	}
}
