// hub_modelb_test.go — Model B「Redis 唯一授权权威」biz 层**端到端**集成测试(真实 miniredis)。
//
// 审核二轮:废弃内存 fakeAuthRepo,改用真实 data.NewRedisHubRepo + data.NewRedisHubAuthRepo 跑在
// 同一 miniredis(authKey/shardKey 同 {pod} slot),让 biz 心跳 / 分配链路真正走 ActivateHeartbeat /
// ReserveRoutableSeat / CheckRoutable 的原子事务代码,而不是被 fake 短路掉正确性。
//
// 覆盖:
//   - 心跳激活线性化点:首个合法凭据心跳把分片 warming→ready + promote + 投影 last_verified,回显 gen。
//   - Model B 下 legacy 心跳(cred==nil)一律拒(CE1/CE2 删除 legacy 回退)。
//   - 心跳 stale fail-closed:旧凭据不刷镜像、不翻 ready。
//   - AssignHub 未激活分片拒(fail-closed);激活后放行 + 归属钉 active 元组。
//   - AssignHub 复用路径:实例漂移(active 元组变化)使旧归属失效并重分配。
package biz

import (
	"bytes"
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"

	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/data"
)

const (
	modelBAuthTTL         = 48 * time.Hour
	modelBTestWriterEpoch = uint32(2)
)

// newModelBUsecase 用真实 miniredis 装配 Model B 用例:HubUsecase.repo = RedisHubRepo,
// authRepo = RedisHubAuthRepo,同一 rdb。返回 uc + 两个真实仓 + mr。
func newModelBUsecase(t *testing.T, capacity int32, shardCount int) (*HubUsecase, *data.RedisHubRepo, *data.RedisHubAuthRepo, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis run: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	cfg := testConf()
	cfg.DefaultCapacity = capacity
	cfg.MockShardCount = shardCount
	repo := data.NewRedisHubRepo(rdb)
	authRepo := data.NewRedisHubAuthRepo(rdb)
	uc := NewHubUsecase(repo, NewMockHubFleetProvider(cfg), &fakeSigner{}, cfg)
	uc.SetAuthRepo(authRepo)
	uc.SetAuthTTL(modelBAuthTTL)
	return uc, repo, authRepo, mr
}

// seedWarming 在真实仓里播种一个 warming 分片(模拟拓扑种子)。
func seedWarming(t *testing.T, repo *data.RedisHubRepo, pod string, shardID uint32, capacity int32, nowMs int64) {
	t.Helper()
	rec := &hubv1.HubShardStorageRecord{
		HubPodName:      pod,
		HubAddr:         "127.0.0.1:7778",
		Region:          "global",
		ShardId:         shardID,
		Capacity:        capacity,
		State:           "warming",
		LastHeartbeatMs: nowMs,
		CreatedAtMs:     nowMs,
	}
	if err := repo.CreateShard(context.Background(), rec, modelBAuthTTL); err != nil {
		t.Fatalf("seed warming shard: %v", err)
	}
}

// activate 走 biz 心跳把某分片激活(init+stage 授权 → HeartbeatWithCredential → warming→ready + promote)。
// 纪元由 InitAuth 权威分配(首建=1,换 uid 递增),返回实际纪元供断言。
func activate(t *testing.T, uc *HubUsecase, authRepo *data.RedisHubAuthRepo, pod, uid string, gen uint64, jti string, nowMs int64) uint32 {
	t.Helper()
	ctx := context.Background()
	rec, err := authRepo.InitAuth(ctx, pod, uid, modelBAuthTTL)
	if err != nil {
		t.Fatalf("init auth: %v", err)
	}
	epoch := rec.ProtocolEpoch
	cred := &hubv1.HubDSCredential{Gen: gen, Jti: jti, ExpMs: 2_000_000_000_000, Kid: "kid-test", InstanceUid: uid, ProtocolEpoch: epoch, TokenSha256: "sha-" + jti, WriterEpoch: modelBTestWriterEpoch}
	if _, err := authRepo.StagePending(ctx, pod, cred, modelBAuthTTL); err != nil {
		t.Fatalf("stage pending: %v", err)
	}
	res, err := uc.HeartbeatWithCredential(ctx, pod, 0, "ready", nowMs,
		&HubCredential{InstanceUID: uid, ProtocolEpoch: epoch, Gen: gen, JTI: jti, TokenSHA256: "sha-" + jti, Kid: "kid-test", WriterEpoch: modelBTestWriterEpoch})
	if err != nil {
		t.Fatalf("activate heartbeat: %v", err)
	}
	if res.AcceptedTokenGen != gen || res.AcceptedTokenJTI != jti || res.AcceptedInstanceUID != uid ||
		res.AcceptedProtocolEpoch != epoch || res.AcceptedWriterEpoch != modelBTestWriterEpoch {
		t.Fatalf("activate must echo the complete credential identity, got %+v", res)
	}
	return epoch
}

// seedResetWarming 把已存在的分片镜像强制回 warming 并清 last_verified(模拟新 DS 实例接管前的种子态)。
func seedResetWarming(t *testing.T, repo *data.RedisHubRepo, pod string, nowMs int64) {
	t.Helper()
	if err := repo.UpdateShardWithLock(context.Background(), pod, 8, func(s *hubv1.HubShardStorageRecord) error {
		s.State = "warming"
		s.LastVerifiedGen = 0
		s.LastVerifiedJti = ""
		s.GameserverUid = ""
		s.AuthEpoch = 0
		s.LastVerifiedWriterEpoch = 0
		s.LastHeartbeatMs = nowMs
		return nil
	}, modelBAuthTTL); err != nil {
		t.Fatalf("reset warming: %v", err)
	}
}

// ── 心跳激活线性化点 ────────────────────────────────────────────────────────

func TestModelB_HeartbeatActivatesShard(t *testing.T) {
	uc, repo, authRepo, _ := newModelBUsecase(t, 500, 1)
	ctx := context.Background()
	const pod = "pandora-hub-global-1"
	now := time.Now().UnixMilli()
	seedWarming(t, repo, pod, 1, 500, now)

	activate(t, uc, authRepo, pod, "uid-A", 9, "j9", now)

	s, _, _ := repo.GetShard(ctx, pod)
	if s.State != "ready" {
		t.Fatalf("first credential heartbeat must flip warming→ready, got %s", s.State)
	}
	if s.LastVerifiedGen != 9 || s.LastVerifiedJti != "j9" || s.GameserverUid != "uid-A" || s.AuthEpoch != 1 {
		t.Fatalf("shard last_verified must be projected by active tuple, got %+v", s)
	}
	auth, _, _ := authRepo.GetAuth(ctx, pod)
	if auth.Active == nil || auth.Active.Gen != 9 || auth.Pending != nil || auth.Phase != hubv1.HubAuthPhase_HUB_AUTH_PHASE_ACTIVE {
		t.Fatalf("auth must be promoted to active gen9, got %+v", auth)
	}
}

func TestModelB_HeartbeatRejectsLegacyNoCredential(t *testing.T) {
	uc, repo, _, _ := newModelBUsecase(t, 500, 1)
	const pod = "pandora-hub-global-1"
	now := time.Now().UnixMilli()
	seedWarming(t, repo, pod, 1, 500, now)

	// authRepo 已装配(Model B),但心跳不带凭据(legacy)→ 一律拒(CE1/CE2)。
	_, err := uc.Heartbeat(context.Background(), pod, 3, "ready", now, 0)
	if errcode.As(err) != errcode.ErrUnauthorized {
		t.Fatalf("model B heartbeat without credential must be rejected, got %v", err)
	}
	// 分片仍 warming(未被无凭据心跳翻 ready)。
	s, _, _ := repo.GetShard(context.Background(), pod)
	if s.State != "warming" {
		t.Fatalf("legacy heartbeat must not flip shard, got %s", s.State)
	}
}

func TestModelB_HeartbeatStaleFailClosed(t *testing.T) {
	uc, repo, authRepo, _ := newModelBUsecase(t, 500, 1)
	ctx := context.Background()
	const pod = "pandora-hub-global-1"
	now := time.Now().UnixMilli()
	seedWarming(t, repo, pod, 1, 500, now)
	activate(t, uc, authRepo, pod, "uid-A", 9, "j9", now)

	// 旧代际凭据(gen=3)→ fail-closed,分片不被刷。
	_, err := uc.HeartbeatWithCredential(ctx, pod, 99, "ready", now+1000,
		&HubCredential{InstanceUID: "uid-A", ProtocolEpoch: 1, Gen: 3, JTI: "j3", TokenSHA256: "sha-j3", Kid: "kid-test", WriterEpoch: modelBTestWriterEpoch})
	if errcode.As(err) != errcode.ErrUnauthorized {
		t.Fatalf("stale credential heartbeat must fail-closed, got %v", err)
	}
	s, _, _ := repo.GetShard(ctx, pod)
	if s.PlayerCount == 99 {
		t.Fatalf("stale heartbeat must not mutate player_count")
	}
	if s.LastVerifiedGen != 9 {
		t.Fatalf("stale heartbeat must not re-project last_verified, got %d", s.LastVerifiedGen)
	}
}

// ── AssignHub 原子授权终态门 ────────────────────────────────────────────────

func TestModelB_AssignBlockedWithoutActivation(t *testing.T) {
	uc, repo, _, _ := newModelBUsecase(t, 500, 1)
	const pod = "pandora-hub-global-1"
	now := time.Now().UnixMilli()
	// 分片存在且 ready(直接播种 ready,但从未经授权激活)→ 仍应被拒。
	rec := &hubv1.HubShardStorageRecord{
		HubPodName: pod, HubAddr: "127.0.0.1:7778", Region: "global", ShardId: 1,
		Capacity: 500, State: "ready", LastHeartbeatMs: now, CreatedAtMs: now,
	}
	if err := repo.CreateShard(context.Background(), rec, modelBAuthTTL); err != nil {
		t.Fatalf("seed ready shard: %v", err)
	}

	if _, err := uc.AssignHub(context.Background(), 1001, "global", 0, 0); errcode.As(err) != errcode.ErrHubNoAvailable {
		t.Fatalf("assign to never-activated shard must be blocked, got %v", err)
	}
}

func TestModelB_AssignAllowsActivatedAndBinds(t *testing.T) {
	uc, repo, authRepo, _ := newModelBUsecase(t, 500, 1)
	ctx := context.Background()
	const pod = "pandora-hub-global-1"
	now := time.Now().UnixMilli()
	seedWarming(t, repo, pod, 1, 500, now)
	epoch := activate(t, uc, authRepo, pod, "uid-A", 42, "j42", now)

	res, err := uc.AssignHub(ctx, 1001, "global", 0, 0)
	if err != nil {
		t.Fatalf("assign to activated shard must succeed: %v", err)
	}
	if res.ShardID != 1 {
		t.Fatalf("want shard 1, got %d", res.ShardID)
	}
	// 归属钉 active 元组(uid/epoch/gen)。
	a, found, _ := repo.GetAssignment(ctx, 1001)
	if !found || a.HubInstanceUid != "uid-A" || a.AuthEpoch != epoch || a.AuthGen != 42 ||
		a.AuthJti != "j42" || a.AuthWriterEpoch != modelBTestWriterEpoch || a.AssignmentId == "" {
		t.Fatalf("assignment must bind active tuple, got found=%v %+v", found, a)
	}
	// 座位被原子占用(player_count=1)。
	s, _, _ := repo.GetShard(ctx, pod)
	if s.PlayerCount != 1 {
		t.Fatalf("reserve must seat++, got %d", s.PlayerCount)
	}
}

type concurrentBindingSigner struct {
	mu       sync.Mutex
	bindings []HubTicketBinding
}

func (s *concurrentBindingSigner) SignHubTicket(_ uint64, _ uint32, binding HubTicketBinding) (string, int64, error) {
	s.mu.Lock()
	s.bindings = append(s.bindings, binding)
	s.mu.Unlock()
	return binding.HubAssignmentID, time.Now().Add(time.Minute).UnixMilli(), nil
}

func TestModelB_ConcurrentAssignSingleSeatAndBinding(t *testing.T) {
	uc, repo, authRepo, _ := newModelBUsecase(t, 500, 1)
	signer := &concurrentBindingSigner{}
	uc.signer = signer
	const pod = "pandora-hub-global-1"
	now := time.Now().UnixMilli()
	seedWarming(t, repo, pod, 1, 500, now)
	activate(t, uc, authRepo, pod, "uid-A", 42, "j42", now)

	const workers = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, err := uc.AssignHub(context.Background(), 1001, "global", 0, 0)
			errCh <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent AssignHub: %v", err)
		}
	}
	shard, _, _ := repo.GetShard(context.Background(), pod)
	assignment, found, err := repo.GetAssignment(context.Background(), 1001)
	if err != nil || !found || shard.PlayerCount != 1 {
		t.Fatalf("one player must own exactly one seat: count=%d assignment=%+v err=%v", shard.PlayerCount, assignment, err)
	}
	signer.mu.Lock()
	defer signer.mu.Unlock()
	if len(signer.bindings) != workers {
		t.Fatalf("every successful caller must receive a ticket: got=%d want=%d", len(signer.bindings), workers)
	}
	for _, binding := range signer.bindings {
		if binding.HubAssignmentID != assignment.AssignmentId || binding.PodName != assignment.HubPodName ||
			binding.InstanceUID != assignment.HubInstanceUid || binding.CredentialJTI != assignment.AuthJti ||
			binding.WriterEpoch != assignment.AuthWriterEpoch {
			t.Fatalf("ticket binding drifted from winning assignment: binding=%+v assignment=%+v", binding, assignment)
		}
	}
}

func TestModelB_AssignIdempotentReuse(t *testing.T) {
	uc, repo, authRepo, _ := newModelBUsecase(t, 500, 1)
	ctx := context.Background()
	const pod = "pandora-hub-global-1"
	now := time.Now().UnixMilli()
	seedWarming(t, repo, pod, 1, 500, now)
	activate(t, uc, authRepo, pod, "uid-A", 42, "j42", now)

	if _, err := uc.AssignHub(ctx, 1001, "global", 0, 0); err != nil {
		t.Fatalf("first assign: %v", err)
	}
	// 再分配同玩家 → 复用(不重复占位):player_count 仍 1。
	if _, err := uc.AssignHub(ctx, 1001, "global", 0, 0); err != nil {
		t.Fatalf("reuse assign: %v", err)
	}
	s, _, _ := repo.GetShard(ctx, pod)
	if s.PlayerCount != 1 {
		t.Fatalf("idempotent reuse must not double-seat, got %d", s.PlayerCount)
	}
}

func TestModelB_AssignIdempotentReuseRefreshesAssignmentTTL(t *testing.T) {
	uc, repo, authRepo, mr := newModelBUsecase(t, 500, 1)
	ctx := context.Background()
	const (
		pod      = "pandora-hub-global-1"
		playerID = uint64(1001)
	)
	now := time.Now().UnixMilli()
	seedWarming(t, repo, pod, 1, 500, now)
	activate(t, uc, authRepo, pod, "uid-A", 42, "j42", now)
	if err := repo.UpdateShardWithLock(ctx, pod, 1, func(*hubv1.HubShardStorageRecord) error { return nil }, modelBAuthTTL); err != nil {
		t.Fatalf("extend shard ttl: %v", err)
	}

	if _, err := uc.AssignHub(ctx, playerID, "global", 0, 0); err != nil {
		t.Fatalf("first assign: %v", err)
	}
	assignmentKey := "pandora:hub:player:1001"
	initialTTL := mr.TTL(assignmentKey)
	if initialTTL <= 0 {
		t.Fatalf("assignment must have positive TTL, got %s", initialTTL)
	}

	// 把归属推进到即将到期；auth/shard 使用更长测试 TTL，且 miniredis 快进不改变服务端心跳时钟。
	mr.FastForward(initialTTL - time.Second)
	if remaining := mr.TTL(assignmentKey); remaining <= 0 || remaining > time.Second {
		t.Fatalf("assignment must be near expiry before reuse, got %s", remaining)
	}
	if _, err := uc.AssignHub(ctx, playerID, "global", 0, 0); err != nil {
		t.Fatalf("reuse assign: %v", err)
	}
	if refreshed := mr.TTL(assignmentKey); refreshed < initialTTL-time.Second {
		t.Fatalf("idempotent reuse must refresh assignment TTL: got=%s initial=%s", refreshed, initialTTL)
	}
}

func TestModelB_AssignRejectsIncompleteOrFutureWriterAssignmentWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*hubv1.HubAssignmentStorageRecord)
	}{
		{name: "missing-jti", mutate: func(a *hubv1.HubAssignmentStorageRecord) { a.AuthJti = "" }},
		{name: "legacy-writer-zero", mutate: func(a *hubv1.HubAssignmentStorageRecord) { a.AuthWriterEpoch = 0 }},
		{name: "legacy-writer-one", mutate: func(a *hubv1.HubAssignmentStorageRecord) { a.AuthWriterEpoch = 1 }},
		{name: "future-writer-three", mutate: func(a *hubv1.HubAssignmentStorageRecord) { a.AuthWriterEpoch = 3 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uc, repo, authRepo, _ := newModelBUsecase(t, 500, 1)
			ctx := context.Background()
			const pod = "pandora-hub-global-1"
			now := time.Now().UnixMilli()
			seedWarming(t, repo, pod, 1, 500, now)
			activate(t, uc, authRepo, pod, "uid-A", 42, "j42", now)
			if _, err := uc.AssignHub(ctx, 1001, "global", 0, 0); err != nil {
				t.Fatal(err)
			}
			before, found, err := repo.GetAssignment(ctx, 1001)
			if err != nil || !found {
				t.Fatalf("assignment found=%v err=%v", found, err)
			}
			poisoned := proto.Clone(before).(*hubv1.HubAssignmentStorageRecord)
			tc.mutate(poisoned)
			if swapped, err := repo.CompareAndSwapAssignment(ctx, 1001, before, poisoned, modelBAuthTTL); err != nil || !swapped {
				t.Fatalf("poison assignment swapped=%v err=%v", swapped, err)
			}
			shardBefore, _, _ := repo.GetShard(ctx, pod)
			if _, err := uc.AssignHub(ctx, 1001, "global", 0, 0); errcode.As(err) != errcode.ErrInvalidState {
				t.Fatalf("incomplete/future assignment must fail closed, code=%v err=%v", errcode.As(err), err)
			}
			after, _, _ := repo.GetAssignment(ctx, 1001)
			shardAfter, _, _ := repo.GetShard(ctx, pod)
			if !proto.Equal(after, poisoned) || shardAfter.GetPlayerCount() != shardBefore.GetPlayerCount() {
				t.Fatalf("rejected assignment mutated state: assignment=%+v shard_count=%d want=%d",
					after, shardAfter.GetPlayerCount(), shardBefore.GetPlayerCount())
			}
		})
	}
}

func TestModelB_FutureWriterAssignmentRejectedByAllConsumerPaths(t *testing.T) {
	for _, operation := range []string{"release", "transfer", "transfer-line", "list-lines", "drain-migrate"} {
		t.Run(operation, func(t *testing.T) {
			uc, repo, authRepo, mr := newModelBUsecase(t, 500, 2)
			ctx := context.Background()
			now := time.Now().UnixMilli()
			const pod1, pod2 = "pandora-hub-global-1", "pandora-hub-global-2"
			seedWarming(t, repo, pod1, 1, 500, now)
			seedWarming(t, repo, pod2, 2, 500, now)
			activate(t, uc, authRepo, pod1, "uid-A", 42, "j42", now)
			activate(t, uc, authRepo, pod2, "uid-B", 52, "j52", now)
			if _, err := uc.AssignHub(ctx, 1001, "global", 0, 0); err != nil {
				t.Fatal(err)
			}
			before, found, err := repo.GetAssignment(ctx, 1001)
			if err != nil || !found || before.GetHubPodName() != pod1 {
				t.Fatalf("assignment=%+v found=%v err=%v", before, found, err)
			}
			poisoned := proto.Clone(before).(*hubv1.HubAssignmentStorageRecord)
			poisoned.AuthWriterEpoch = 3
			if swapped, err := repo.CompareAndSwapAssignment(ctx, 1001, before, poisoned, modelBAuthTTL); err != nil || !swapped {
				t.Fatalf("poison assignment swapped=%v err=%v", swapped, err)
			}
			fromBefore, _, _ := repo.GetShard(ctx, pod1)
			targetBefore, _, _ := repo.GetShard(ctx, pod2)
			membersBefore, _ := repo.ListShardMembers(ctx, pod1)

			switch operation {
			case "release":
				if err := uc.ReleaseHub(ctx, 1001); errcode.As(err) != errcode.ErrInvalidState {
					t.Fatalf("future writer release code=%v err=%v", errcode.As(err), err)
				}
			case "transfer":
				if _, err := uc.TransferHub(ctx, 1001, 2); errcode.As(err) != errcode.ErrHubTransferFailed {
					t.Fatalf("future writer transfer code=%v err=%v", errcode.As(err), err)
				}
			case "transfer-line":
				if _, err := uc.TransferToLineForPlayer(ctx, 1001, 2); errcode.As(err) != errcode.ErrInvalidState {
					t.Fatalf("future writer line transfer code=%v err=%v", errcode.As(err), err)
				}
				if mr.Exists("pandora:hub:transfer_cd:1001") {
					t.Fatal("future writer rejection must happen before cooldown SET")
				}
			case "list-lines":
				if _, err := uc.ListHubLinesForPlayer(ctx, 1001, ""); errcode.As(err) != errcode.ErrInvalidState {
					t.Fatalf("future writer list lines code=%v err=%v", errcode.As(err), err)
				}
			case "drain-migrate":
				from, _, _ := repo.GetShard(ctx, pod1)
				target, _, _ := repo.GetShard(ctx, pod2)
				if uc.migratePlayer(ctx, 1001, from, target) {
					t.Fatal("future writer assignment must not be migrated")
				}
			}

			after, _, _ := repo.GetAssignment(ctx, 1001)
			fromAfter, _, _ := repo.GetShard(ctx, pod1)
			targetAfter, _, _ := repo.GetShard(ctx, pod2)
			membersAfter, _ := repo.ListShardMembers(ctx, pod1)
			if !proto.Equal(after, poisoned) || fromAfter.GetPlayerCount() != fromBefore.GetPlayerCount() ||
				targetAfter.GetPlayerCount() != targetBefore.GetPlayerCount() ||
				!slices.Equal(membersAfter, membersBefore) {
				t.Fatalf("rejected path mutated state: assignment=%+v from=%d/%d target=%d/%d", after,
					fromAfter.GetPlayerCount(), fromBefore.GetPlayerCount(),
					targetAfter.GetPlayerCount(), targetBefore.GetPlayerCount())
			}
		})
	}
}

// TestModelB_ReuseInvalidatesOnInstanceDrift:玩家已分配后,分片被新 DS 实例顶替(uid 变、
// epoch 递增、active gen 前进),旧归属钉的元组不再等于当前 active → 复用失效 → 退旧重分配,
// 新归属钉新元组(审核二轮 CE1/CE2 misassignment 防护)。
func TestModelB_ReuseInvalidatesOnInstanceDrift(t *testing.T) {
	uc, repo, authRepo, _ := newModelBUsecase(t, 500, 1)
	ctx := context.Background()
	const pod = "pandora-hub-global-1"
	now := time.Now().UnixMilli()
	seedWarming(t, repo, pod, 1, 500, now)
	ep1 := activate(t, uc, authRepo, pod, "uid-A", 9, "j9", now)

	if _, err := uc.AssignHub(ctx, 1001, "global", 0, 0); err != nil {
		t.Fatalf("first assign: %v", err)
	}
	a1, _, _ := repo.GetAssignment(ctx, 1001)
	if a1.HubInstanceUid != "uid-A" || a1.AuthEpoch != ep1 || a1.AuthGen != 9 {
		t.Fatalf("first assignment tuple mismatch: %+v", a1)
	}

	// DS 实例漂移:换 uid-B(InitAuth 递增 epoch),复位分片回 warming 模拟新实例首跳,重新 stage+activate 新 gen。
	seedResetWarming(t, repo, pod, now)
	ep2 := activate(t, uc, authRepo, pod, "uid-B", 20, "j20", now)
	if ep2 == ep1 {
		t.Fatalf("instance drift must bump protocol epoch, still %d", ep2)
	}

	// 再分配同玩家:旧归属元组 (uid-A,ep1,gen9) != 当前 active (uid-B,ep2,gen20)
	// → reuseRoutable=false → 退旧重分配,新归属钉新元组。
	if _, err := uc.AssignHub(ctx, 1001, "global", 0, 0); err != nil {
		t.Fatalf("reassign after drift: %v", err)
	}
	a2, _, _ := repo.GetAssignment(ctx, 1001)
	if a2.HubInstanceUid != "uid-B" || a2.AuthEpoch != ep2 || a2.AuthGen != 20 {
		t.Fatalf("assignment must rebind to new active tuple after drift, got %+v", a2)
	}
}

func TestModelB_SameInstanceRotationRebindsWithoutDoubleSeatAndKeepsUnknown(t *testing.T) {
	uc, repo, authRepo, _ := newModelBUsecase(t, 500, 1)
	ctx := context.Background()
	const pod = "pandora-hub-global-1"
	now := time.Now().UnixMilli()
	seedWarming(t, repo, pod, 1, 500, now)
	epoch := activate(t, uc, authRepo, pod, "uid-A", 9, "j9", now)
	if _, err := uc.AssignHub(ctx, 1001, "global", 0, 0); err != nil {
		t.Fatal(err)
	}
	old, found, err := repo.GetAssignment(ctx, 1001)
	if err != nil || !found {
		t.Fatalf("assignment found=%v err=%v", found, err)
	}
	oldID := old.GetAssignmentId()
	unknown := protowire.AppendTag(nil, 2047, protowire.VarintType)
	unknown = protowire.AppendVarint(unknown, 12345)
	next := proto.Clone(old).(*hubv1.HubAssignmentStorageRecord)
	next.ProtoReflect().SetUnknown(unknown)
	if swapped, err := repo.CompareAndSwapAssignment(ctx, 1001, old, next, modelBAuthTTL); err != nil || !swapped {
		t.Fatalf("inject future field swapped=%v err=%v", swapped, err)
	}

	rotated := &hubv1.HubDSCredential{
		Gen: 10, Jti: "j10", ExpMs: 2_000_000_000_000, Kid: "kid-test",
		InstanceUid: "uid-A", ProtocolEpoch: epoch, TokenSha256: "sha-j10",
		WriterEpoch: modelBTestWriterEpoch,
	}
	if _, err := authRepo.StagePending(ctx, pod, rotated, modelBAuthTTL); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.HeartbeatWithCredential(ctx, pod, 1, "ready", now+1000, &HubCredential{
		InstanceUID: "uid-A", ProtocolEpoch: epoch, Gen: 10, JTI: "j10",
		TokenSHA256: "sha-j10", Kid: "kid-test", WriterEpoch: modelBTestWriterEpoch,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.AssignHub(ctx, 1001, "global", 0, 0); err != nil {
		t.Fatal(err)
	}
	got, _, _ := repo.GetAssignment(ctx, 1001)
	shard, _, _ := repo.GetShard(ctx, pod)
	if shard.GetPlayerCount() != 1 {
		t.Fatalf("credential rebind double-seated player_count=%d", shard.GetPlayerCount())
	}
	if got.GetAuthGen() != 10 || got.GetAuthJti() != "j10" || got.GetAssignmentId() != oldID {
		t.Fatalf("assignment was not rebound in place: %+v", got)
	}
	if !bytes.Equal(got.ProtoReflect().GetUnknown(), unknown) {
		t.Fatalf("future assignment fields lost: got=%x want=%x", got.ProtoReflect().GetUnknown(), unknown)
	}
}
