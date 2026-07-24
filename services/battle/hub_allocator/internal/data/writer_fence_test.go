// writer_fence_test.go — 写者继任存储级 fencing 测试(R9 P0-7;miniredis)。
// 覆盖:未持有拒写、落后 token 拒写(零写入)、推进水位、幂等同 token 放行、
// 损坏 fence 值 fail-closed、nil fence 保持 legacy 行为、auth 记录仓同规则。
package data

import (
	"context"
	"testing"

	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// fakeWriterFence 是可编程 WriterFence(测试专用)。
type fakeWriterFence struct {
	token uint64
	held  bool
}

func (f *fakeWriterFence) Current() (uint64, bool) { return f.token, f.held }

func TestWriterFence_NotHeldRejectsWriteZeroMutation(t *testing.T) {
	ctx := context.Background()
	repo, mr := newRepo(t)
	const pod = "pandora-hub-global-1"
	_ = repo.CreateShard(ctx, sampleShard(pod, 1, 0), testTTL)
	repo.SetWriterFence(&fakeWriterFence{token: 0, held: false})

	err := repo.UpdateShardWithLock(ctx, pod, 3, func(s *hubv1.HubShardStorageRecord) error {
		s.PlayerCount = 99
		return nil
	}, testTTL)
	if errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("not-held write must be ErrUnavailable, got %v", err)
	}
	got, _, _ := repo.GetShard(ctx, pod)
	if got.PlayerCount != 0 {
		t.Fatalf("rejected write must not mutate shard, got count %d", got.PlayerCount)
	}
	if mr.Exists(wfenceKey(pod)) {
		t.Fatal("rejected write must not create fence key")
	}
}

func TestWriterFence_StaleTokenRejectedZeroMutation(t *testing.T) {
	ctx := context.Background()
	repo, mr := newRepo(t)
	const pod = "pandora-hub-global-1"
	_ = repo.CreateShard(ctx, sampleShard(pod, 1, 0), testTTL)
	// 继任者已把水位推进到 6;本副本还拿着第 5 届 token。
	mr.Set(wfenceKey(pod), "6")
	repo.SetWriterFence(&fakeWriterFence{token: 5, held: true})

	err := repo.UpdateShardWithLock(ctx, pod, 3, func(s *hubv1.HubShardStorageRecord) error {
		s.PlayerCount = 99
		return nil
	}, testTTL)
	if errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("stale-token write must be ErrUnavailable, got %v", err)
	}
	got, _, _ := repo.GetShard(ctx, pod)
	if got.PlayerCount != 0 {
		t.Fatalf("stale write must not mutate shard, got count %d", got.PlayerCount)
	}
	if v, _ := mr.Get(wfenceKey(pod)); v != "6" {
		t.Fatalf("stale writer must not move the fence, got %q", v)
	}

	// 心跳同规则:失主镜像变更被拒,零写入。
	if _, err := repo.HeartbeatShard(ctx, pod, 42, "ready", 0, 0, false, testTTL); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("stale-token heartbeat must be ErrUnavailable, got %v", err)
	}
	got, _, _ = repo.GetShard(ctx, pod)
	if got.PlayerCount != 0 {
		t.Fatalf("stale heartbeat must not mutate shard, got count %d", got.PlayerCount)
	}
}

func TestWriterFence_AdvancesWatermarkThenIdempotent(t *testing.T) {
	ctx := context.Background()
	repo, mr := newRepo(t)
	const pod = "pandora-hub-global-1"
	_ = repo.CreateShard(ctx, sampleShard(pod, 1, 0), testTTL)
	mr.Set(wfenceKey(pod), "6")
	repo.SetWriterFence(&fakeWriterFence{token: 7, held: true})

	if err := repo.UpdateShardWithLock(ctx, pod, 3, func(s *hubv1.HubShardStorageRecord) error {
		s.PlayerCount = 1
		return nil
	}, testTTL); err != nil {
		t.Fatalf("newer-token write must pass: %v", err)
	}
	if v, _ := mr.Get(wfenceKey(pod)); v != "7" {
		t.Fatalf("write must advance fence to 7, got %q", v)
	}
	if mr.TTL(wfenceKey(pod)) != 0 {
		t.Fatal("fence key must be persistent (no TTL): the watermark must outlive shard records")
	}
	// cur == mine:放行且不再写 fence。
	if err := repo.UpdateShardWithLock(ctx, pod, 3, func(s *hubv1.HubShardStorageRecord) error {
		s.PlayerCount = 2
		return nil
	}, testTTL); err != nil {
		t.Fatalf("same-token write must pass: %v", err)
	}
	if v, _ := mr.Get(wfenceKey(pod)); v != "7" {
		t.Fatalf("fence must stay 7, got %q", v)
	}
	// 推进后旧 token 永久出局。
	repo.SetWriterFence(&fakeWriterFence{token: 6, held: true})
	if err := repo.UpdateShardWithLock(ctx, pod, 3, func(s *hubv1.HubShardStorageRecord) error {
		s.PlayerCount = 3
		return nil
	}, testTTL); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("superseded token must stay rejected, got %v", err)
	}
	got, _, _ := repo.GetShard(ctx, pod)
	if got.PlayerCount != 2 {
		t.Fatalf("superseded write leaked: count %d", got.PlayerCount)
	}
}

func TestWriterFence_CorruptValueFailsClosed(t *testing.T) {
	ctx := context.Background()
	repo, mr := newRepo(t)
	const pod = "pandora-hub-global-1"
	_ = repo.CreateShard(ctx, sampleShard(pod, 1, 0), testTTL)
	mr.Set(wfenceKey(pod), "not-a-number")
	repo.SetWriterFence(&fakeWriterFence{token: 7, held: true})

	err := repo.UpdateShardWithLock(ctx, pod, 3, func(s *hubv1.HubShardStorageRecord) error {
		s.PlayerCount = 1
		return nil
	}, testTTL)
	if err == nil {
		t.Fatal("corrupt fence value must fail closed")
	}
	got, _, _ := repo.GetShard(ctx, pod)
	if got.PlayerCount != 0 {
		t.Fatalf("corrupt-fence write must not mutate shard, got count %d", got.PlayerCount)
	}
}

func TestWriterFence_NilFenceLegacyBehavior(t *testing.T) {
	ctx := context.Background()
	repo, mr := newRepo(t)
	const pod = "pandora-hub-global-1"
	_ = repo.CreateShard(ctx, sampleShard(pod, 1, 0), testTTL)

	if err := repo.UpdateShardWithLock(ctx, pod, 3, func(s *hubv1.HubShardStorageRecord) error {
		s.PlayerCount = 1
		return nil
	}, testTTL); err != nil {
		t.Fatalf("nil fence must keep legacy behavior: %v", err)
	}
	if mr.Exists(wfenceKey(pod)) {
		t.Fatal("nil fence must not create fence key")
	}
}

// 继任者水位推扫(覆盖边界 ③):把分片 SET ∪ 待清理 saga 源 pod 的 fence 一次性推进
// 到本届 token;推扫后前任 token 在全部 pod 上永久出局;幂等;被继任时 fail-closed。
func TestWriterFence_AdvanceWriterFencesSweep(t *testing.T) {
	ctx := context.Background()
	repo, mr := newRepo(t)
	const podA, podB, podC = "pandora-hub-global-1", "pandora-hub-global-2", "pandora-hub-global-3"
	_ = repo.CreateShard(ctx, sampleShard(podA, 1, 0), testTTL)
	_ = repo.CreateShard(ctx, sampleShard(podB, 2, 0), testTTL)
	// podC 不在分片 SET,仅作为待清理 saga 源 pod(推扫必须并入)。
	if err := repo.RegisterTransferCleanup(ctx, podC, TransferCleanupRef{PlayerID: 1001, TargetAssignmentID: "a1"}); err != nil {
		t.Fatalf("register cleanup: %v", err)
	}
	mr.Set(wfenceKey(podA), "3") // podA 有旧水位;podB/podC 从未被触碰(懒推进盲区)
	repo.SetWriterFence(&fakeWriterFence{token: 7, held: true})

	if err := repo.AdvanceWriterFences(ctx); err != nil {
		t.Fatalf("advance sweep: %v", err)
	}
	for _, pod := range []string{podA, podB, podC} {
		if v, _ := mr.Get(wfenceKey(pod)); v != "7" {
			t.Fatalf("pod %s fence must be swept to 7, got %q", pod, v)
		}
		if mr.TTL(wfenceKey(pod)) != 0 {
			t.Fatalf("pod %s fence must be persistent", pod)
		}
	}
	// 幂等:同 token 再推不出错、水位不变。
	if err := repo.AdvanceWriterFences(ctx); err != nil {
		t.Fatalf("idempotent sweep: %v", err)
	}
	// 推扫后,前任(token 5)在从未被逐 slot 触碰过的 podB 上也永久出局。
	repo.SetWriterFence(&fakeWriterFence{token: 5, held: true})
	err := repo.UpdateShardWithLock(ctx, podB, 3, func(s *hubv1.HubShardStorageRecord) error {
		s.PlayerCount = 99
		return nil
	}, testTTL)
	if errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("stale writer on untouched pod must be rejected after sweep, got %v", err)
	}
	// 前任自己跑推扫:立即发现被继任,fail-closed 且不回退水位。
	if err := repo.AdvanceWriterFences(ctx); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("superseded sweep must be ErrUnavailable, got %v", err)
	}
	if v, _ := mr.Get(wfenceKey(podA)); v != "7" {
		t.Fatalf("superseded sweep must not regress fence, got %q", v)
	}
	// 失主:推扫直接拒。
	repo.SetWriterFence(&fakeWriterFence{token: 9, held: false})
	if err := repo.AdvanceWriterFences(ctx); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("not-held sweep must be ErrUnavailable, got %v", err)
	}
	// nil fence:no-op。
	repo.SetWriterFence(nil)
	if err := repo.AdvanceWriterFences(ctx); err != nil {
		t.Fatalf("nil fence sweep must be no-op: %v", err)
	}
}

func TestWriterFence_AuthRepoInitAndTeardownProofFenced(t *testing.T) {
	ctx := context.Background()
	repo, mr := newAuthRepo(t)
	const pod = "pandora-hub-global-1"
	mr.Set(wfenceKey(pod), "9")
	repo.SetWriterFence(&fakeWriterFence{token: 5, held: true})

	// 授权记录写路径被拒且零写入。
	if _, err := repo.InitAuth(ctx, pod, "uid-A", testTTL); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("stale-token InitAuth must be ErrUnavailable, got %v", err)
	}
	if mr.Exists(authKey(pod)) {
		t.Fatal("rejected InitAuth must not create auth record")
	}
	// teardown proof(解锁 ownership 清理的能力)同样受 fence 约束。
	if err := repo.RecordInstanceTeardownProof(ctx, pod, "uid-A", testTTL); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("stale-token teardown proof must be ErrUnavailable, got %v", err)
	}
	if mr.Exists(instanceTeardownProofKey(pod)) {
		t.Fatal("rejected teardown proof must not be recorded")
	}

	// 当前写者正常通过并推进水位。
	repo.SetWriterFence(&fakeWriterFence{token: 10, held: true})
	if _, err := repo.InitAuth(ctx, pod, "uid-A", testTTL); err != nil {
		t.Fatalf("current-writer InitAuth must pass: %v", err)
	}
	if v, _ := mr.Get(wfenceKey(pod)); v != "10" {
		t.Fatalf("InitAuth must advance fence to 10, got %q", v)
	}
	if err := repo.RecordInstanceTeardownProof(ctx, pod, "uid-A", testTTL); err != nil {
		t.Fatalf("current-writer teardown proof must pass: %v", err)
	}
}
