// hub_auth_repo_test.go — Model B「Redis 唯一授权权威」授权记录数据层测试(miniredis)。
// 覆盖:InitAuth 建/复位/幂等、StagePending 代际单调门 + 相位锁定门。
// ActivateHeartbeat / ReserveRoutableSeat / CheckRoutable 的原子线性化点测试见 hub_authoritative_test.go。
package data

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
)

func newAuthRepo(t *testing.T) (*RedisHubAuthRepo, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis run: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisHubAuthRepo(rdb), mr
}

const testWriterEpoch uint32 = 2

func credFor(pod, uid string, epoch uint32, gen uint64, jti string) *hubv1.HubDSCredential {
	return &hubv1.HubDSCredential{
		Gen:           gen,
		Jti:           jti,
		ExpMs:         2_000_000_000_000,
		Kid:           "kid-1",
		InstanceUid:   uid,
		ProtocolEpoch: epoch,
		TokenSha256:   "sha-" + jti,
		WriterEpoch:   testWriterEpoch,
	}
}

func TestAuth_InitCreateResetIdempotent(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-global-1"

	// 首建 → BOOTSTRAP,epoch=1。
	rec, err := repo.InitAuth(ctx, pod, "uid-A", testTTL)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if rec.Phase != hubv1.HubAuthPhase_HUB_AUTH_PHASE_BOOTSTRAP {
		t.Fatalf("want BOOTSTRAP, got %v", rec.Phase)
	}
	if rec.InstanceUid != "uid-A" || rec.ProtocolEpoch != 1 {
		t.Fatalf("bad init record: %+v", rec)
	}
	if rec.RequiredWriterEpoch != testWriterEpoch {
		t.Fatalf("required writer epoch must initialize at v2, got %d", rec.RequiredWriterEpoch)
	}

	// 同 uid 幂等:epoch 不变。
	rec2, err := repo.InitAuth(ctx, pod, "uid-A", testTTL)
	if err != nil {
		t.Fatalf("init idempotent: %v", err)
	}
	if rec2.ProtocolEpoch != 1 {
		t.Fatalf("idempotent init must not bump epoch, got %d", rec2.ProtocolEpoch)
	}

	// 先放一个 pending,再换实例 uid → 复位(清 pending,epoch++,high_water 保留)。
	if _, err := repo.StagePending(ctx, pod, credFor(pod, "uid-A", 1, 5, "j5"), testTTL); err != nil {
		t.Fatalf("stage before reset: %v", err)
	}
	rec3, err := repo.InitAuth(ctx, pod, "uid-B", testTTL)
	if err != nil {
		t.Fatalf("init reset: %v", err)
	}
	if rec3.InstanceUid != "uid-B" || rec3.ProtocolEpoch != 2 {
		t.Fatalf("reset must bind new uid + bump epoch, got uid=%s epoch=%d", rec3.InstanceUid, rec3.ProtocolEpoch)
	}
	if rec3.Pending != nil || rec3.Active != nil {
		t.Fatalf("reset must clear active/pending, got %+v", rec3)
	}
	if rec3.HighWaterGen != 5 {
		t.Fatalf("reset must preserve high_water_gen monotonic, want 5 got %d", rec3.HighWaterGen)
	}
}

func TestAuth_InitRejectsLegacyRequiredWriterWithoutMutation(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-legacy-writer"
	legacy := &hubv1.HubShardAuthStorageRecord{
		PodName: pod, InstanceUid: "uid-A", ProtocolEpoch: 7,
		Phase:               hubv1.HubAuthPhase_HUB_AUTH_PHASE_BOOTSTRAP,
		RequiredWriterEpoch: 0,
	}
	payload, err := proto.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.rdb.Set(ctx, authKey(pod), payload, testTTL).Err(); err != nil {
		t.Fatal(err)
	}
	before, err := repo.rdb.Get(ctx, authKey(pod)).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	beforeTTL := repo.rdb.TTL(ctx, authKey(pod)).Val()
	if _, err := repo.InitAuth(ctx, pod, "uid-A", testTTL); errcode.As(err) != errcode.ErrUnauthorized {
		t.Fatalf("legacy required writer must fail closed, got %v", err)
	}
	after, err := repo.rdb.Get(ctx, authKey(pod)).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || repo.rdb.TTL(ctx, authKey(pod)).Val() != beforeTTL {
		t.Fatal("legacy required writer rejection mutated bytes or TTL")
	}
}

func TestAuth_StagePendingGenGate(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-global-1"
	if _, err := repo.InitAuth(ctx, pod, "uid-A", testTTL); err != nil {
		t.Fatalf("init: %v", err)
	}

	// 正常暂存 gen=3。
	rec, err := repo.StagePending(ctx, pod, credFor(pod, "uid-A", 1, 3, "j3"), testTTL)
	if err != nil {
		t.Fatalf("stage gen3: %v", err)
	}
	if rec.Pending == nil || rec.Pending.Gen != 3 || rec.HighWaterGen != 3 {
		t.Fatalf("stage must set pending+high_water=3, got %+v", rec)
	}

	// 代际回退(gen=2 <= high_water)→ 拒(fail-closed)。
	if _, err := repo.StagePending(ctx, pod, credFor(pod, "uid-A", 1, 2, "j2"), testTTL); errcode.As(err) != errcode.ErrUnauthorized {
		t.Fatalf("stale gen must be rejected with ErrUnauthorized, got %v", err)
	}

	// uid 不符 → 拒。
	if _, err := repo.StagePending(ctx, pod, credFor(pod, "uid-X", 1, 4, "j4"), testTTL); errcode.As(err) != errcode.ErrUnauthorized {
		t.Fatalf("uid mismatch must be rejected, got %v", err)
	}

	// epoch 不符 → 拒。
	if _, err := repo.StagePending(ctx, pod, credFor(pod, "uid-A", 9, 4, "j4"), testTTL); errcode.As(err) != errcode.ErrUnauthorized {
		t.Fatalf("epoch mismatch must be rejected, got %v", err)
	}
}

func TestAuth_StagePendingRequiresCompleteCredential(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-complete"
	if _, err := repo.InitAuth(ctx, pod, "uid-A", testTTL); err != nil {
		t.Fatalf("init: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*hubv1.HubDSCredential)
	}{
		{name: "missing_epoch", mutate: func(c *hubv1.HubDSCredential) { c.ProtocolEpoch = 0 }},
		{name: "missing_kid", mutate: func(c *hubv1.HubDSCredential) { c.Kid = "" }},
		{name: "missing_hash", mutate: func(c *hubv1.HubDSCredential) { c.TokenSha256 = "" }},
		{name: "missing_exp", mutate: func(c *hubv1.HubDSCredential) { c.ExpMs = 0 }},
		{name: "missing_writer_epoch", mutate: func(c *hubv1.HubDSCredential) { c.WriterEpoch = 0 }},
		{name: "expired", mutate: func(c *hubv1.HubDSCredential) { c.ExpMs = 1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cred := credFor(pod, "uid-A", 1, 1, "j1")
			tc.mutate(cred)
			if _, err := repo.StagePending(ctx, pod, cred, testTTL); errcode.As(err) != errcode.ErrInvalidArg {
				t.Fatalf("incomplete credential must be invalid argument, got %v", err)
			}
		})
	}
}

func TestAuth_MarkDeliveredBindsExpectedPending(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-delivery"
	if _, err := repo.InitAuth(ctx, pod, "uid-A", testTTL); err != nil {
		t.Fatalf("init: %v", err)
	}
	g5 := credFor(pod, "uid-A", 1, 5, "j5")
	if _, err := repo.StagePending(ctx, pod, g5, testTTL); err != nil {
		t.Fatalf("stage g5: %v", err)
	}
	if err := repo.MarkDelivered(ctx, pod, g5, "rv-5", testTTL); err != nil {
		t.Fatalf("mark g5: %v", err)
	}
	rec, found, err := repo.GetAuth(ctx, pod)
	if err != nil || !found || rec.DeliveredRv != "rv-5" {
		t.Fatalf("g5 delivered rv not stored: found=%v rec=%+v err=%v", found, rec, err)
	}

	// 更高代际替换 pending 后,旧 g5 PATCH 响应晚到必须失败,且不得污染 g6。
	g6 := credFor(pod, "uid-A", 1, 6, "j6")
	if _, err := repo.StagePending(ctx, pod, g6, testTTL); err != nil {
		t.Fatalf("stage g6: %v", err)
	}
	if err := repo.MarkDelivered(ctx, pod, g5, "rv-old-late", testTTL); errcode.As(err) != errcode.ErrUnauthorized {
		t.Fatalf("late g5 mark must be rejected, got %v", err)
	}
	rec, found, err = repo.GetAuth(ctx, pod)
	if err != nil || !found || rec.Pending == nil || rec.Pending.Gen != 6 || rec.DeliveredRv != "" {
		t.Fatalf("late g5 must leave g6 undelivered: found=%v rec=%+v err=%v", found, rec, err)
	}

	// 同 gen/jti 但 kid/hash/exp 任一不同也不是同一 pending。
	wrong := proto.Clone(g6).(*hubv1.HubDSCredential)
	wrong.Kid = "other-kid"
	if err := repo.MarkDelivered(ctx, pod, wrong, "rv-wrong", testTTL); errcode.As(err) != errcode.ErrUnauthorized {
		t.Fatalf("different full tuple must be rejected, got %v", err)
	}
}

func TestAuth_MarkDeliveredConcurrentWithNewPendingNeverContaminates(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	for i := uint64(1); i <= 64; i++ {
		pod := fmt.Sprintf("pandora-hub-delivery-race-%d", i)
		if _, err := repo.InitAuth(ctx, pod, "uid-A", testTTL); err != nil {
			t.Fatalf("init iteration %d: %v", i, err)
		}
		oldCred := credFor(pod, "uid-A", 1, i*2-1, "old")
		newCred := credFor(pod, "uid-A", 1, i*2, "new")
		if _, err := repo.StagePending(ctx, pod, oldCred, testTTL); err != nil {
			t.Fatalf("stage old iteration %d: %v", i, err)
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var markErr, stageErr error
		go func() {
			defer wg.Done()
			<-start
			markErr = repo.MarkDelivered(ctx, pod, oldCred, "rv-old", testTTL)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, stageErr = repo.StagePending(ctx, pod, newCred, testTTL)
		}()
		close(start)
		wg.Wait()
		if stageErr != nil {
			t.Fatalf("stage new iteration %d: %v", i, stageErr)
		}
		if markErr != nil && errcode.As(markErr) != errcode.ErrUnauthorized {
			t.Fatalf("mark old iteration %d unexpected error: %v", i, markErr)
		}
		rec, found, err := repo.GetAuth(ctx, pod)
		if err != nil || !found || rec.Pending == nil || rec.Pending.Gen != newCred.Gen || rec.DeliveredRv != "" {
			t.Fatalf("iteration %d old response contaminated new pending: found=%v rec=%+v err=%v", i, found, rec, err)
		}
	}
}

func TestCredMatchesRequiresFullHashAndTuple(t *testing.T) {
	c := credFor("pod", "uid-A", 1, 7, "j7")
	id := CredentialIdentity{Gen: 7, JTI: "j7", InstanceUID: "uid-A", ProtocolEpoch: 1, TokenSHA256: c.TokenSha256, Kid: c.Kid, WriterEpoch: c.WriterEpoch}
	if !credMatches(c, id) {
		t.Fatal("complete matching tuple must pass")
	}
	id.TokenSHA256 = ""
	if credMatches(c, id) {
		t.Fatal("missing caller hash must fail closed")
	}
	id.TokenSHA256 = c.TokenSha256
	c.TokenSha256 = ""
	if credMatches(c, id) {
		t.Fatal("missing stored hash must fail closed")
	}
}
