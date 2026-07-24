// writer_gate_test.go — biz 层写者租约业务闸门测试(R9 P0-7)。
// 覆盖:未持有租约时所有 mutating 入口在触碰存储前被拒(retryable ErrUnavailable);
// 持有租约与未注入 fence(legacy)时闸门放行。
package biz

import (
	"context"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/errcode"
)

type fakeWriterFence struct {
	token uint64
	held  bool
}

func (f *fakeWriterFence) Current() (uint64, bool) { return f.token, f.held }

func TestWriterGate_NotHeldRejectsMutatingEntrypoints(t *testing.T) {
	uc, repo, _ := newTestUsecase(500, 3)
	uc.SetWriterFence(&fakeWriterFence{held: false})
	ctx := context.Background()

	if _, err := uc.AssignHub(ctx, 1001, "global", 0, 0, 0, ""); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("AssignHub on non-writer must be ErrUnavailable, got %v", err)
	}
	if err := uc.ReleaseHub(ctx, 1001); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("ReleaseHub on non-writer must be ErrUnavailable, got %v", err)
	}
	if _, err := uc.Heartbeat(ctx, "pandora-hub-global-1", 1, "ready", 0, 0); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("Heartbeat on non-writer must be ErrUnavailable, got %v", err)
	}
	// 闸门必须在触碰存储之前拒绝:仓库不应出现任何分片/指派。
	if shards, _ := repo.ListShards(ctx); len(shards) != 0 {
		t.Fatalf("non-writer must not touch storage, got %d shards", len(shards))
	}
}

func TestWriterGate_HeldAndLegacyPassThrough(t *testing.T) {
	// 持有租约:闸门放行,后续参数校验照常发生(player_id=0 → ErrInvalidArg)。
	uc, _, _ := newTestUsecase(500, 3)
	uc.SetWriterFence(&fakeWriterFence{token: 7, held: true})
	if _, err := uc.AssignHub(context.Background(), 0, "global", 0, 0, 0, ""); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("held writer must pass gate to arg validation, got %v", err)
	}
	// 未注入 fence(单写者 legacy 拓扑):行为不变。
	uc2, _, _ := newTestUsecase(500, 3)
	if _, err := uc2.AssignHub(context.Background(), 1001, "global", 0, 0, 0, ""); err != nil {
		t.Fatalf("nil fence must keep legacy behavior: %v", err)
	}
}

// countdownFence:前 remaining 次 Current() 持有,之后失主(模拟在途请求中途失主)。
type countdownFence struct {
	token     uint64
	remaining int
}

func (f *countdownFence) Current() (uint64, bool) {
	if f.remaining > 0 {
		f.remaining--
		return f.token, true
	}
	return 0, false
}

// 出票前写者复核(writer_fence.go 覆盖边界 ④):入口持有、返回前失主的在途请求
// 绝不交付票据——assignment 单键无法进 {pod} fence 事务,票据交付以「全程持有」为准。
func TestWriterGate_TicketWithheldWhenLeaseLostMidFlight(t *testing.T) {
	uc, _, _ := newTestUsecase(500, 3)
	// 恰好放过入口 requireWriter 一次,出票前复核时已失主。
	uc.SetWriterFence(&countdownFence{token: 7, remaining: 1})
	res, err := uc.AssignHub(context.Background(), 1001, "global", 0, 0, 0, "")
	if errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("mid-flight lease loss must be ErrUnavailable, got %v", err)
	}
	if res != nil {
		t.Fatal("ticket must be withheld after mid-flight lease loss")
	}
}

// 继任者水位推扫(writer_fence.go 覆盖边界 ③):当选后 sweep 循环把全部已知 pod 的
// fence 推进一次;同一届内不重复推扫(token 未变)。
func TestWriterGate_SweepAdvancesFencesOncePerTerm(t *testing.T) {
	cfg := testConf()
	cfg.DefaultCapacity = 500
	cfg.MockShardCount = 3
	cfg.SweepInterval = config.Duration(5 * time.Millisecond)
	repo := newFakeRepo()
	uc := NewHubUsecase(repo, NewMockHubFleetProvider(cfg), &fakeSigner{}, cfg)
	uc.SetWriterFence(&fakeWriterFence{token: 7, held: true})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go uc.RunHeartbeatSweep(ctx)

	advanceCalls := func() int {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		return repo.advanceFenceCalls
	}
	deadline := time.Now().Add(2 * time.Second)
	for advanceCalls() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("sweep loop never advanced writer fences after election")
		}
		time.Sleep(2 * time.Millisecond)
	}
	// 同一届(token 不变)内继续走若干 tick,推扫不得重复执行。
	time.Sleep(50 * time.Millisecond)
	if n := advanceCalls(); n != 1 {
		t.Fatalf("fence sweep must run exactly once per term, got %d", n)
	}
}
