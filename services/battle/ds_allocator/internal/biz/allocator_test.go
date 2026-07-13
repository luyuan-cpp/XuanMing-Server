// allocator_test.go — ds_allocator biz 层测试(miniredis 真实跑通)。
package biz

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/errcode"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/data"
)

// 生产 readyPollInterval 是 1s;单测把它调小,避免每次 AllocateBattle 都等满一个轮询周期。
func init() { readyPollInterval = 10 * time.Millisecond }

func testCfg() conf.AllocatorConf {
	return conf.AllocatorConf{
		HeartbeatTimeout:   config.Duration(15 * time.Second),
		SweepInterval:      config.Duration(5 * time.Second),
		BattleTTL:          config.Duration(2 * time.Hour),
		ReadyWaitTimeout:   config.Duration(1 * time.Second), // 测试用短超时,避免慢测
		EmptyBattleTimeout: config.Duration(5 * time.Minute),
		MockDSAddrHost:     "127.0.0.1",
		MockDSPortBase:     30000,
		MockDSPortRange:    1000,
	}
}

// allocateReady 模拟正常时序:并发跑 AllocateBattle,待 warming 镜像出现后用对应 pod 上报一次
// running 心跳,使 DS 进入 running,AllocateBattle 等到 ready 后返回。
func allocateReady(t *testing.T, uc *AllocatorUsecase, repo *data.RedisBattleRepo, matchID uint64, playerIDs []uint64, mapID uint32, gameMode string) *AllocateResult {
	t.Helper()
	ctx := context.Background()
	type out struct {
		res *AllocateResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := uc.AllocateBattle(ctx, matchID, playerIDs, mapID, gameMode)
		done <- out{res, err}
	}()
	feedReadyHeartbeat(t, uc, repo, matchID, int32(len(playerIDs)))
	r := <-done
	if r.err != nil {
		t.Fatalf("allocate match %d: %v", matchID, r.err)
	}
	return r.res
}

// feedReadyHeartbeat 等 warming 镜像出现后,用其记录的 pod 上报一次 running 心跳。
// 上报前确保 wall clock 已越过 AllocatedAtMs,保证 LastHeartbeatMs 严格大于分配时刻(满足 ready 判定)。
func feedReadyHeartbeat(t *testing.T, uc *AllocatorUsecase, repo *data.RedisBattleRepo, matchID uint64, playerCount int32) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(3 * time.Second)
	var rec *dsv1.BattleStorageRecord
	for {
		b, found, err := repo.GetBattle(ctx, matchID)
		if err == nil && found && b.DsPodName != "" {
			rec = b
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("warming record for match %d never appeared", matchID)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for time.Now().UnixMilli() <= rec.AllocatedAtMs {
		time.Sleep(time.Millisecond)
	}
	if _, err := uc.Heartbeat(ctx, matchID, rec.DsPodName, playerCount, "running", time.Now().UnixMilli()); err != nil {
		t.Fatalf("heartbeat match %d: %v", matchID, err)
	}
}

// newUsecaseWithAlloc 用指定分配器装配 usecase + 真实 miniredis 仓储(返回 mr 供 TTL 断言)。
func newUsecaseWithAlloc(t *testing.T, alloc GameServerAllocator) (*AllocatorUsecase, *data.RedisBattleRepo, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis run: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	repo := data.NewRedisBattleRepo(rdb)
	return NewAllocatorUsecase(repo, alloc, testCfg()), repo, mr
}

func newUsecase(t *testing.T) (*AllocatorUsecase, *data.RedisBattleRepo) {
	t.Helper()
	uc, repo, _ := newUsecaseWithAlloc(t, NewMockGameServerAllocator(testCfg()))
	return uc, repo
}

func enableModelBForTest(
	t *testing.T,
	uc *AllocatorUsecase,
	mr *miniredis.Miniredis,
) (*data.RedisBattleAuthRepo, *redis.Client) {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	authRepo := data.NewRedisBattleAuthRepo(rdb)
	signer, err := auth.NewSigner(auth.Config{
		Issuer: auth.DSCallbackIssuer, Audience: auth.DSCallbackAudience,
		Secret: []byte("battle-model-b-lifecycle-fence-test-secret"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.EnableRedisAuthority(authRepo, signer, time.Hour); err != nil {
		t.Fatal(err)
	}
	return authRepo, rdb
}

// backdate 把 match 的 last_heartbeat_ms 回拨到远古,模拟心跳超时。
func backdate(t *testing.T, repo *data.RedisBattleRepo, matchID uint64) {
	t.Helper()
	if err := repo.UpdateBattleWithLock(context.Background(), matchID, 3, func(b *dsv1.BattleStorageRecord) error {
		b.LastHeartbeatMs = 1
		return nil
	}, 2*time.Hour); err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

// countingAllocator 包 Mock 分配器并统计 Release 次数,验证补偿重试期间 pod 只回收一次。
type countingAllocator struct {
	inner    GameServerAllocator
	releases int
}

// gatedAllocator 把第一次外部分配卡在闸门上，制造两个 AllocateBattle 真并发的 Get/claim 窗口。
type gatedAllocator struct {
	inner   GameServerAllocator
	calls   atomic.Int32
	started chan struct{}
	proceed chan struct{}
}

type authoritativeTestAllocator struct {
	authoritativeCalls atomic.Int32
	legacyCalls        atomic.Int32
	releases           atomic.Int32
	delivered          chan map[string]string
	allocateResult     *data.AuthoritativeGameServerAllocation
	allocateErr        error
	releaseErr         error
	releaseCheck       func(*data.AuthoritativeGameServerAllocation) error
}

// timeoutLateApplyAllocator 模拟 apiserver 在 GSA POST 已进入处理后客户端超时，
// GameServer 稍后才被 controller 标成 Allocated。响应永远不给严格 UID/RV。
type timeoutLateApplyAllocator struct {
	authoritativeTestAllocator
	postStarted chan struct{}
	returnError chan struct{}
	lateApplied atomic.Bool
}

func (a *timeoutLateApplyAllocator) AllocateAuthoritative(
	_ context.Context,
	_ uint64,
	allocationID string,
	_ uint32,
	_ string,
) (*data.AuthoritativeGameServerAllocation, error) {
	a.authoritativeCalls.Add(1)
	select {
	case a.postStarted <- struct{}{}:
	default:
	}
	<-a.returnError
	a.lateApplied.Store(true)
	return &data.AuthoritativeGameServerAllocation{AllocationID: allocationID},
		errors.New("GSA POST timeout, controller applied later")
}

// rejectingFenceRepo 只替换 POST 前 Redis fence，其他方法仍由真实 miniredis repo 执行。
type rejectingFenceRepo struct {
	data.BattleRepo
	fenceCalls atomic.Int32
}

func (r *rejectingFenceRepo) FenceBattleAllocation(context.Context, uint64, string) (bool, error) {
	r.fenceCalls.Add(1)
	return false, nil
}

func (a *authoritativeTestAllocator) Allocate(context.Context, uint64, uint32, string) (string, string, error) {
	a.legacyCalls.Add(1)
	return "", "", errors.New("legacy allocation must not be used in Model B")
}

func (a *authoritativeTestAllocator) AllocateAuthoritative(
	_ context.Context,
	_ uint64,
	allocationID string,
	_ uint32,
	_ string,
) (*data.AuthoritativeGameServerAllocation, error) {
	a.authoritativeCalls.Add(1)
	if a.allocateResult != nil || a.allocateErr != nil {
		if a.allocateResult == nil {
			return nil, a.allocateErr
		}
		out := *a.allocateResult
		if out.AllocationID == "" {
			out.AllocationID = allocationID
		}
		return &out, a.allocateErr
	}
	return &data.AuthoritativeGameServerAllocation{
		PodName: "battle-auth-1", Addr: "10.0.0.9:7777", InstanceUID: "uid-auth-1",
		ResourceVersion: "101", AllocationID: allocationID, AnnotationsPresent: true,
	}, nil
}

func (a *authoritativeTestAllocator) DeliverCredential(
	_ context.Context,
	_ *data.AuthoritativeGameServerAllocation,
	annotations map[string]string,
) (string, error) {
	copyAnnotations := make(map[string]string, len(annotations))
	for k, v := range annotations {
		copyAnnotations[k] = v
	}
	a.delivered <- copyAnnotations
	return "102", nil
}

func (a *authoritativeTestAllocator) Release(context.Context, string) error {
	a.releases.Add(1)
	return a.releaseErr
}

func (a *authoritativeTestAllocator) ReleaseExpected(
	_ context.Context,
	allocation *data.AuthoritativeGameServerAllocation,
) error {
	if allocation == nil || (allocation.InstanceUID == "" && allocation.AllocationID == "") {
		return errors.New("missing expected UID")
	}
	a.releases.Add(1)
	if a.releaseCheck != nil {
		if err := a.releaseCheck(allocation); err != nil {
			return err
		}
	}
	return a.releaseErr
}

func (g *gatedAllocator) Allocate(ctx context.Context, matchID uint64, mapID uint32, gameMode string) (string, string, error) {
	if g.calls.Add(1) == 1 {
		close(g.started)
	}
	select {
	case <-ctx.Done():
		return "", "", ctx.Err()
	case <-g.proceed:
	}
	return g.inner.Allocate(ctx, matchID, mapID, gameMode)
}

func (g *gatedAllocator) Release(ctx context.Context, podName string) error {
	return g.inner.Release(ctx, podName)
}

func (c *countingAllocator) Allocate(ctx context.Context, matchID uint64, mapID uint32, gameMode string) (string, string, error) {
	return c.inner.Allocate(ctx, matchID, mapID, gameMode)
}

func (c *countingAllocator) Release(ctx context.Context, podName string) error {
	c.releases++
	return c.inner.Release(ctx, podName)
}

// mockLifecycle 记录 PublishLifecycle 调用;前 failFirst 次返回错误(模拟 Kafka 临时不可用)。
type mockLifecycle struct {
	failFirst int
	calls     int
	delivered []uint64
}

func (m *mockLifecycle) PublishLifecycle(_ context.Context, evt *dsv1.DSLifecycleEvent) error {
	m.calls++
	if m.calls <= m.failFirst {
		return errors.New("kafka unavailable")
	}
	m.delivered = append(m.delivered, evt.GetMatchId())
	return nil
}

func TestAllocateBattle(t *testing.T) {
	uc, repo := newUsecase(t)

	res := allocateReady(t, uc, repo, 7, []uint64{10, 20, 30}, 1, "5v5_ranked")
	if res.DSPodName != "pandora-battle-7" {
		t.Fatalf("pod = %q, want pandora-battle-7", res.DSPodName)
	}
	if res.DSAddr != "127.0.0.1:30007" {
		t.Fatalf("addr = %q, want 127.0.0.1:30007", res.DSAddr)
	}
	// AllocateBattle 返回前 DS 必须已用心跳确认 ready/running
	got, _, _ := repo.GetBattle(context.Background(), 7)
	if got.State != stateRunning {
		t.Fatalf("state = %q, want running", got.State)
	}
	if got.LastHeartbeatMs <= got.AllocatedAtMs {
		t.Fatalf("LastHeartbeatMs %d must be > AllocatedAtMs %d (real heartbeat)", got.LastHeartbeatMs, got.AllocatedAtMs)
	}
}

func TestAllocateBattleTrueConcurrencyOnlyOneExternalAllocation(t *testing.T) {
	ctx := context.Background()
	gated := &gatedAllocator{
		inner:   NewMockGameServerAllocator(testCfg()),
		started: make(chan struct{}),
		proceed: make(chan struct{}),
	}
	uc, repo, _ := newUsecaseWithAlloc(t, gated)
	type result struct {
		res *AllocateResult
		err error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			res, err := uc.AllocateBattle(ctx, 700, []uint64{1, 2}, 1, "ranked")
			results <- result{res: res, err: err}
		}()
	}
	select {
	case <-gated.started:
	case <-time.After(time.Second):
		t.Fatal("external allocation never started")
	}
	// 第一调用仍卡在外部 API；第二调用已有充足时间撞同一持久 claim。
	time.Sleep(50 * time.Millisecond)
	if got := gated.calls.Load(); got != 1 {
		t.Fatalf("external Allocate calls=%d, want exactly 1", got)
	}
	close(gated.proceed)
	feedReadyHeartbeat(t, uc, repo, 700, 2)
	var first *AllocateResult
	for i := 0; i < 2; i++ {
		select {
		case got := <-results:
			if got.err != nil {
				t.Fatalf("concurrent allocate %d: %v", i, got.err)
			}
			if first == nil {
				first = got.res
			} else if got.res.DSPodName != first.DSPodName || got.res.DSAddr != first.DSAddr {
				t.Fatalf("callers observed different allocation: first=%+v got=%+v", first, got.res)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent AllocateBattle did not return")
		}
	}
	if got := gated.calls.Load(); got != 1 {
		t.Fatalf("external Allocate calls after completion=%d, want 1", got)
	}
}

func TestBattleModelB_EndToEndStageDeliverActivateReady(t *testing.T) {
	ctx := context.Background()
	allocator := &authoritativeTestAllocator{delivered: make(chan map[string]string, 1)}
	uc, _, mr := newUsecaseWithAlloc(t, allocator)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	authRepo := data.NewRedisBattleAuthRepo(rdb)
	secret := []byte("battle-model-b-test-secret-32-bytes!!")
	signer, err := auth.NewSigner(auth.Config{
		Issuer: auth.DSCallbackIssuer, Audience: auth.DSCallbackAudience, Secret: secret,
	})
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	if err := uc.EnableRedisAuthority(authRepo, signer, 3*time.Hour); err != nil {
		t.Fatalf("EnableRedisAuthority: %v", err)
	}

	type allocationResult struct {
		res *AllocateResult
		err error
	}
	done := make(chan allocationResult, 1)
	go func() {
		res, err := uc.AllocateBattle(ctx, 800, []uint64{10, 20}, 1, "ranked")
		done <- allocationResult{res: res, err: err}
	}()

	var annotations map[string]string
	select {
	case annotations = <-allocator.delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("credential was not delivered")
	}
	if annotations[battleTokenAnnotationKey] == "" || annotations[battleTokenGenAnnotationKey] != "1" ||
		annotations[battleInstanceUIDAnnotationKey] != "uid-auth-1" || annotations[battleWriterEpochKey] != "2" {
		t.Fatalf("incomplete delivery annotations: %v", annotations)
	}

	var snapshot data.BattleAuthoritySnapshot
	deadline := time.Now().Add(time.Second)
	for {
		snapshot, err = authRepo.ReadAuthority(ctx, 800)
		if err == nil && snapshot.AuthFound && snapshot.Auth.GetDeliveredRv() == "102" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending not marked delivered: snapshot=%+v err=%v", snapshot, err)
		}
		time.Sleep(time.Millisecond)
	}
	pending := snapshot.Auth.GetPending()
	for time.Now().UnixMilli() <= snapshot.Battle.GetAllocatedAtMs() {
		time.Sleep(time.Millisecond)
	}
	hb, err := uc.HeartbeatAuthorized(ctx, 800, data.BattleCredentialIdentity{
		PodName: "battle-auth-1", InstanceUID: pending.GetInstanceUid(),
		InstanceEpoch: pending.GetInstanceEpoch(), Gen: pending.GetGen(), JTI: pending.GetJti(),
		ExpMs: pending.GetExpMs(), Kid: pending.GetKid(), TokenSHA256: pending.GetTokenSha256(),
		WriterEpoch: pending.GetWriterEpoch(),
	}, 2, stateRunning, time.Now().Add(24*time.Hour).UnixMilli()) // future ts 必须被忽略
	if err != nil {
		t.Fatalf("HeartbeatAuthorized: %v", err)
	}
	if hb.AcceptedTokenGen != pending.GetGen() || hb.AcceptedTokenJTI != pending.GetJti() ||
		hb.AcceptedInstanceUID != "uid-auth-1" || hb.AcceptedInstanceEpoch != 1 || hb.AcceptedWriterEpoch != 2 {
		t.Fatalf("incomplete activation ACK: %+v", hb)
	}

	select {
	case got := <-done:
		if got.err != nil || got.res == nil || got.res.DSPodName != "battle-auth-1" {
			t.Fatalf("AllocateBattle: res=%+v err=%v", got.res, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AllocateBattle did not pass authoritative ready gate")
	}
	if allocator.authoritativeCalls.Load() != 1 || allocator.legacyCalls.Load() != 0 || allocator.releases.Load() != 0 {
		t.Fatalf("allocator calls authoritative=%d legacy=%d releases=%d",
			allocator.authoritativeCalls.Load(), allocator.legacyCalls.Load(), allocator.releases.Load())
	}

	activeSnapshot, err := authRepo.ReadAuthority(ctx, 800)
	if err != nil {
		t.Fatalf("read active: %v", err)
	}
	ready, reason := activeSnapshot.ReadyAuthorized(time.Now().UnixMilli(), time.Minute.Milliseconds())
	if !ready {
		t.Fatalf("authority not ready: %s snapshot=%+v", reason, activeSnapshot)
	}
	// ts_ms 是未来一天，但权威时间必须接近服务器当前时间，不能被客户端延长新鲜度。
	if activeSnapshot.Auth.GetLastActiveHeartbeatMs() > time.Now().Add(time.Second).UnixMilli() {
		t.Fatalf("client ts_ms contaminated authority heartbeat: %d", activeSnapshot.Auth.GetLastActiveHeartbeatMs())
	}
}

func TestBattleModelBCleanupFencesBeforeReleaseThenPurges(t *testing.T) {
	ctx := context.Background()
	allocator := &authoritativeTestAllocator{delivered: make(chan map[string]string, 1)}
	uc, _, mr := newUsecaseWithAlloc(t, allocator)
	uc.cfg.ReadyWaitTimeout = config.Duration(30 * time.Millisecond)
	authRepo, _ := enableModelBForTest(t, uc, mr)

	allocator.releaseCheck = func(allocation *data.AuthoritativeGameServerAllocation) error {
		snapshot, err := authRepo.ReadAuthority(ctx, 820)
		if err != nil {
			return err
		}
		if !snapshot.AuthFound || !snapshot.BattleFound ||
			snapshot.Auth.GetPhase() != dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_TERMINATING ||
			snapshot.Auth.GetActive() != nil || snapshot.Auth.GetPending() != nil ||
			snapshot.Battle.GetState() != statePreactiveReleasing ||
			snapshot.Battle.GetGameserverUid() != allocation.InstanceUID {
			return errors.New("ReleaseExpected observed no exact preactive Redis fence")
		}
		if authTTL, battleTTL := mr.TTL("pandora:ds:auth:{820}"), mr.TTL("pandora:ds:battle:{820}"); authTTL != 0 || battleTTL != 0 {
			return errors.New("ReleaseExpected observed expiring preactive fence")
		}
		return nil
	}

	if _, err := uc.AllocateBattle(ctx, 820, []uint64{1, 2}, 1, "ranked"); err == nil || errcode.As(err) != errcode.ErrDSAllocationFailed {
		t.Fatalf("ready timeout err=%v code=%v", err, errcode.As(err))
	}
	if allocator.releases.Load() != 1 {
		t.Fatalf("ReleaseExpected calls=%d", allocator.releases.Load())
	}
	snapshot, err := authRepo.ReadAuthority(ctx, 820)
	if err != nil || snapshot.AuthFound || snapshot.BattleFound {
		t.Fatalf("explicit release success did not purge: snapshot=%+v err=%v", snapshot, err)
	}
}

func TestBattleModelBCleanupReleaseUnknownKeepsFenceAndCrashRetry(t *testing.T) {
	ctx := context.Background()
	allocator := &authoritativeTestAllocator{
		delivered:  make(chan map[string]string, 1),
		releaseErr: errors.New("simulated DELETE timeout with unknown result"),
	}
	uc, _, mr := newUsecaseWithAlloc(t, allocator)
	uc.cfg.ReadyWaitTimeout = config.Duration(30 * time.Millisecond)
	authRepo, rdb := enableModelBForTest(t, uc, mr)

	if _, err := uc.AllocateBattle(ctx, 821, []uint64{1}, 1, "ranked"); err == nil || errcode.As(err) != errcode.ErrDSAllocationFailed {
		t.Fatalf("ready timeout err=%v code=%v", err, errcode.As(err))
	}
	snapshot, err := authRepo.ReadAuthority(ctx, 821)
	if err != nil || !snapshot.AuthFound || !snapshot.BattleFound ||
		snapshot.Auth.GetPhase() != dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_TERMINATING ||
		snapshot.Battle.GetState() != statePreactiveReleasing {
		t.Fatalf("release timeout lost fence: snapshot=%+v err=%v", snapshot, err)
	}
	if authTTL, battleTTL := mr.TTL("pandora:ds:auth:{821}"), mr.TTL("pandora:ds:battle:{821}"); authTTL != 0 || battleTTL != 0 {
		t.Fatalf("release timeout fence must be persistent: auth=%v battle=%v", authTTL, battleTTL)
	}
	authBefore, _ := rdb.Get(ctx, "pandora:ds:auth:{821}").Bytes()
	battleBefore, _ := rdb.Get(ctx, "pandora:ds:battle:{821}").Bytes()
	if _, err := uc.AllocateBattle(ctx, 821, []uint64{1}, 1, "ranked"); err == nil || errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("retry on release fence err=%v code=%v", err, errcode.As(err))
	}
	authAfter, _ := rdb.Get(ctx, "pandora:ds:auth:{821}").Bytes()
	battleAfter, _ := rdb.Get(ctx, "pandora:ds:battle:{821}").Bytes()
	if string(authAfter) != string(authBefore) || string(battleAfter) != string(battleBefore) ||
		mr.TTL("pandora:ds:auth:{821}") != 0 || mr.TTL("pandora:ds:battle:{821}") != 0 {
		t.Fatal("Allocate retry mutated release-timeout bytes or TTL")
	}
	if allocator.authoritativeCalls.Load() != 1 || allocator.releases.Load() != 1 {
		t.Fatalf("release-timeout retry side effects: POST=%d release=%d",
			allocator.authoritativeCalls.Load(), allocator.releases.Load())
	}

	// 模拟进程崩溃后由另一副本接棒：永久 fence 仍在；幂等 UID delete
	// 得到明确成功后才 purge。这里的重试改善 liveness，安全不依赖它一定发生。
	allocator.releaseErr = nil
	if err := rdb.ZAdd(ctx, "pandora:ds:active", redis.Z{Score: 0, Member: 821}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err = authRepo.ReadAuthority(ctx, 821)
	if err != nil || snapshot.AuthFound || snapshot.BattleFound {
		t.Fatalf("confirmed retry did not purge: snapshot=%+v err=%v", snapshot, err)
	}
	if allocator.authoritativeCalls.Load() != 1 || allocator.releases.Load() != 2 {
		t.Fatalf("crash retry side effects: POST=%d release=%d",
			allocator.authoritativeCalls.Load(), allocator.releases.Load())
	}
}

func TestBattleModelBReleaseBattleUnknownKeepsPermanentTerminatingFence(t *testing.T) {
	ctx := context.Background()
	allocator := &authoritativeTestAllocator{
		delivered:  make(chan map[string]string, 1),
		releaseErr: errors.New("simulated ReleaseExpected timeout"),
	}
	uc, battleRepo, mr := newUsecaseWithAlloc(t, allocator)
	authRepo, _ := enableModelBForTest(t, uc, mr)
	const matchID = uint64(822)
	const allocationID = "alloc-822"
	claim := &dsv1.BattleStorageRecord{
		MatchId: matchID, State: stateAllocating, AllocationId: allocationID,
		AllocatedAtMs: time.Now().UnixMilli(), LastHeartbeatMs: time.Now().UnixMilli(),
	}
	if claimed, _, err := battleRepo.ClaimBattle(ctx, claim, time.Hour); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if fenced, err := battleRepo.FenceBattleAllocation(ctx, matchID, allocationID); err != nil || !fenced {
		t.Fatalf("allocation fence=%v err=%v", fenced, err)
	}
	battle := proto.Clone(claim).(*dsv1.BattleStorageRecord)
	battle.State, battle.DsPodName, battle.DsAddr = stateWarming, "battle-822", "10.0.0.82:7777"
	battle.GameserverUid = "uid-822"
	if finalized, err := battleRepo.FinalizeFencedBattleAllocation(ctx, battle, time.Hour); err != nil || !finalized {
		t.Fatalf("finalize=%v err=%v", finalized, err)
	}
	seed, err := authRepo.PrepareCredential(ctx, data.BattleAuthorityBinding{
		MatchID: matchID, AllocationID: allocationID, PodName: "battle-822", InstanceUID: "uid-822",
		RequiredWriterEpoch: data.BattleDSWriterEpochV2, AuthTTL: time.Hour, BattleTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	credential := &dsv1.BattleDSCredential{
		Gen: seed.Gen, Jti: "jti-822", ExpMs: uint64(time.Now().Add(time.Hour).UnixMilli()),
		Kid: "kid-822", InstanceUid: "uid-822", InstanceEpoch: seed.InstanceEpoch,
		TokenSha256: "sha256-822", WriterEpoch: data.BattleDSWriterEpochV2,
	}
	if _, err := authRepo.StagePending(ctx, data.BattleStageInput{
		MatchID: matchID, AllocationID: allocationID, Credential: credential, AuthTTL: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	if err := authRepo.MarkDelivered(ctx, matchID, allocationID, credential, "rv-822", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := authRepo.ActivateHeartbeat(ctx, matchID, data.BattleCredentialIdentity{
		PodName: "battle-822", InstanceUID: "uid-822", InstanceEpoch: seed.InstanceEpoch,
		Gen: seed.Gen, JTI: credential.Jti, ExpMs: credential.ExpMs, Kid: credential.Kid,
		TokenSHA256: credential.TokenSha256, WriterEpoch: credential.WriterEpoch,
	}, data.BattleHeartbeatInput{PlayerCount: 1, State: stateRunning, AuthTTL: time.Hour, BattleTTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	allocator.releaseCheck = func(allocation *data.AuthoritativeGameServerAllocation) error {
		snapshot, err := authRepo.ReadAuthority(ctx, matchID)
		if err != nil {
			return err
		}
		if snapshot.Auth.GetPhase() != dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_TERMINATING ||
			snapshot.Battle.GetState() != stateAbandoned ||
			snapshot.Battle.GetGameserverUid() != allocation.InstanceUID ||
			mr.TTL("pandora:ds:auth:{822}") != 0 || mr.TTL("pandora:ds:battle:{822}") != 0 {
			return errors.New("ReleaseBattle called ReleaseExpected before permanent TERMINATING fence")
		}
		return nil
	}

	if err := uc.ReleaseBattle(ctx, matchID, "failed"); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("release timeout code=%v err=%v", errcode.As(err), err)
	}
	snapshot, err := authRepo.ReadAuthority(ctx, matchID)
	if err != nil || !snapshot.AuthFound || !snapshot.BattleFound ||
		snapshot.Auth.GetPhase() != dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_TERMINATING ||
		snapshot.Battle.GetState() != stateAbandoned {
		t.Fatalf("ReleaseBattle timeout lost terminal fence: snapshot=%+v err=%v", snapshot, err)
	}
	if authTTL, battleTTL := mr.TTL("pandora:ds:auth:{822}"), mr.TTL("pandora:ds:battle:{822}"); authTTL != 0 || battleTTL != 0 {
		t.Fatalf("ReleaseBattle timeout fence must persist: auth=%v battle=%v", authTTL, battleTTL)
	}
	if allocator.releases.Load() != 1 {
		t.Fatalf("first release calls=%d", allocator.releases.Load())
	}

	allocator.releaseErr = nil
	if err := uc.ReleaseBattle(ctx, matchID, "failed"); err != nil {
		t.Fatalf("idempotent release retry: %v", err)
	}
	snapshot, err = authRepo.ReadAuthority(ctx, matchID)
	if err != nil || snapshot.AuthFound || snapshot.BattleFound {
		t.Fatalf("release retry did not purge: snapshot=%+v err=%v", snapshot, err)
	}
	if allocator.releases.Load() != 2 {
		t.Fatalf("release retry calls=%d", allocator.releases.Load())
	}
}

func TestBattleModelB_StrictGETMissingUIDKeepsPersistentFence(t *testing.T) {
	ctx := context.Background()
	allocator := &authoritativeTestAllocator{
		delivered: make(chan map[string]string, 1),
		allocateResult: &data.AuthoritativeGameServerAllocation{
			PodName: "battle-partial", Addr: "10.0.0.8:7777", AllocationID: "ignored-by-fake",
		},
		allocateErr: errors.New("strict GET missing UID/RV"),
	}
	uc, repo, mr := newUsecaseWithAlloc(t, allocator)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	signer, err := auth.NewSigner(auth.Config{
		Issuer: auth.DSCallbackIssuer, Audience: auth.DSCallbackAudience,
		Secret: []byte("battle-model-b-partial-secret-32bytes"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.EnableRedisAuthority(data.NewRedisBattleAuthRepo(rdb), signer, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.AllocateBattle(ctx, 802, []uint64{1, 2}, 1, "ranked"); err == nil {
		t.Fatal("strict GET failure must fail allocation")
	}
	if allocator.releases.Load() != 0 {
		t.Fatalf("strict GET failure triggered external cleanup: releases=%d", allocator.releases.Load())
	}
	claim, found, err := repo.GetBattle(ctx, 802)
	if err != nil || !found || claim.GetState() != stateAllocationUncertain {
		t.Fatalf("strict GET failure lost uncertain fence: found=%v claim=%+v err=%v", found, claim, err)
	}
	if ttl := mr.TTL("pandora:ds:battle:{802}"); ttl != 0 {
		t.Fatalf("uncertain fence must be persistent, ttl=%s", ttl)
	}
}

func TestBattleModelB_UnknownUIDKeepsClaimAndBlocksSecondPOST(t *testing.T) {
	ctx := context.Background()
	allocator := &authoritativeTestAllocator{
		delivered: make(chan map[string]string, 1),
		allocateResult: &data.AuthoritativeGameServerAllocation{
			PodName: "battle-partial", Addr: "10.0.0.8:7777", AllocationID: "alloc-unknown",
		},
		allocateErr: errors.New("strict GET timeout"),
	}
	uc, repo, mr := newUsecaseWithAlloc(t, allocator)
	uc.cfg.ReadyWaitTimeout = config.Duration(30 * time.Millisecond)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	signer, err := auth.NewSigner(auth.Config{
		Issuer: auth.DSCallbackIssuer, Audience: auth.DSCallbackAudience,
		Secret: []byte("battle-model-b-unknown-secret-32bytes"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.EnableRedisAuthority(data.NewRedisBattleAuthRepo(rdb), signer, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.AllocateBattle(ctx, 803, []uint64{1, 2}, 1, "ranked"); err == nil {
		t.Fatal("identity/delete uncertainty must fail allocation")
	}
	first, found, err := repo.GetBattle(ctx, 803)
	if err != nil || !found || first.GetState() != stateAllocationUncertain {
		t.Fatalf("uncertain allocation claim not retained: found=%v rec=%+v err=%v", found, first, err)
	}
	if ttl := mr.TTL("pandora:ds:battle:{803}"); ttl != 0 {
		t.Fatalf("uncertain allocation claim must not expire: ttl=%s", ttl)
	}
	started := time.Now()
	if _, err := uc.AllocateBattle(ctx, 803, []uint64{1, 2}, 1, "ranked"); err == nil {
		t.Fatal("retry should fail closed on retained claim")
	} else if errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("retry code=%v, want ErrUnavailable", errcode.As(err))
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("uncertain retry waited instead of failing closed: %s", elapsed)
	}
	if allocator.authoritativeCalls.Load() != 1 {
		t.Fatalf("uncertain first POST allowed second POST: calls=%d", allocator.authoritativeCalls.Load())
	}
	if allocator.releases.Load() != 0 {
		t.Fatalf("uncertain retry triggered cleanup: releases=%d", allocator.releases.Load())
	}
}

func TestBattleModelB_POSTUnknownWithoutPodStillUsesAllocationFence(t *testing.T) {
	ctx := context.Background()
	allocator := &authoritativeTestAllocator{
		delivered:      make(chan map[string]string, 1),
		allocateResult: &data.AuthoritativeGameServerAllocation{AllocationID: "alloc-unknown"},
		allocateErr:    errors.New("POST timeout after possible apply"),
	}
	uc, repo, mr := newUsecaseWithAlloc(t, allocator)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	signer, err := auth.NewSigner(auth.Config{
		Issuer: auth.DSCallbackIssuer, Audience: auth.DSCallbackAudience,
		Secret: []byte("battle-model-b-post-unknown-secret-32b"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.EnableRedisAuthority(data.NewRedisBattleAuthRepo(rdb), signer, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.AllocateBattle(ctx, 804, []uint64{1}, 1, "ranked"); err == nil {
		t.Fatal("unknown POST must fail closed")
	}
	claim, found, err := repo.GetBattle(ctx, 804)
	if err != nil || !found || claim.GetState() != stateAllocationUncertain {
		t.Fatalf("unknown POST claim was removed: claim=%+v found=%v err=%v", claim, found, err)
	}
	if ttl := mr.TTL("pandora:ds:battle:{804}"); ttl != 0 {
		t.Fatalf("unknown POST claim must be persistent: ttl=%s", ttl)
	}
	if allocator.releases.Load() != 0 {
		t.Fatalf("unknown POST triggered reconciliation side effect: releases=%d", allocator.releases.Load())
	}
}

func TestBattleModelB_FenceCASFailureNeverCallsGSA(t *testing.T) {
	ctx := context.Background()
	allocator := &authoritativeTestAllocator{delivered: make(chan map[string]string, 1)}
	uc, repo, mr := newUsecaseWithAlloc(t, allocator)
	rejecting := &rejectingFenceRepo{BattleRepo: repo}
	uc.repo = rejecting
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	signer, err := auth.NewSigner(auth.Config{
		Issuer: auth.DSCallbackIssuer, Audience: auth.DSCallbackAudience,
		Secret: []byte("battle-model-b-fence-reject-secret-32b"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.EnableRedisAuthority(data.NewRedisBattleAuthRepo(rdb), signer, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.AllocateBattle(ctx, 806, []uint64{1}, 1, "ranked"); err == nil {
		t.Fatal("fence CAS failure must fail allocation")
	} else if errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("fence failure code=%v, want ErrUnavailable", errcode.As(err))
	}
	if rejecting.fenceCalls.Load() != 1 || allocator.authoritativeCalls.Load() != 0 {
		t.Fatalf("fence_calls=%d GSA_POST_calls=%d, want 1/0",
			rejecting.fenceCalls.Load(), allocator.authoritativeCalls.Load())
	}
	claim, found, err := repo.GetBattle(ctx, 806)
	if err != nil || !found || claim.GetState() != stateAllocating {
		t.Fatalf("pre-POST claim changed unexpectedly: found=%v claim=%+v err=%v", found, claim, err)
	}
}

func TestBattleModelB_POSTTimeoutLateApplyConcurrentRetryStaysPersistent(t *testing.T) {
	ctx := context.Background()
	allocator := &timeoutLateApplyAllocator{
		authoritativeTestAllocator: authoritativeTestAllocator{delivered: make(chan map[string]string, 1)},
		postStarted:                make(chan struct{}, 1),
		returnError:                make(chan struct{}),
	}
	uc, repo, mr := newUsecaseWithAlloc(t, allocator)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	signer, err := auth.NewSigner(auth.Config{
		Issuer: auth.DSCallbackIssuer, Audience: auth.DSCallbackAudience,
		Secret: []byte("battle-model-b-late-apply-secret-32bytes"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.EnableRedisAuthority(data.NewRedisBattleAuthRepo(rdb), signer, time.Hour); err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, allocateErr := uc.AllocateBattle(ctx, 807, []uint64{1, 2}, 1, "ranked")
		firstDone <- allocateErr
	}()
	select {
	case <-allocator.postStarted:
	case <-time.After(time.Second):
		t.Fatal("first GSA POST did not start")
	}
	claim, found, err := repo.GetBattle(ctx, 807)
	if err != nil || !found || claim.GetState() != stateAllocationUncertain {
		t.Fatalf("POST began without persistent uncertain fence: found=%v claim=%+v err=%v", found, claim, err)
	}
	if ttl := mr.TTL("pandora:ds:battle:{807}"); ttl != 0 {
		t.Fatalf("POST-timeout fence must be persistent, ttl=%s", ttl)
	}

	// 第一请求仍卡在 POST 时并发重入：必须立即 unavailable，且绝不能第二次 POST。
	secondDone := make(chan error, 1)
	go func() {
		_, allocateErr := uc.AllocateBattle(ctx, 807, []uint64{1, 2}, 1, "ranked")
		secondDone <- allocateErr
	}()
	select {
	case secondErr := <-secondDone:
		if errcode.As(secondErr) != errcode.ErrUnavailable {
			t.Fatalf("concurrent retry code=%v, want ErrUnavailable", errcode.As(secondErr))
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("concurrent retry waited instead of failing closed")
	}
	if calls := allocator.authoritativeCalls.Load(); calls != 1 {
		t.Fatalf("concurrent retry issued another GSA POST: calls=%d", calls)
	}

	// 模拟 controller 在客户端超时后才应用原 POST；原请求只能返回 unavailable，
	// 不得以 DeleteCollection/LIST 空作为“未应用”并撤掉 claim。
	close(allocator.returnError)
	select {
	case firstErr := <-firstDone:
		if errcode.As(firstErr) != errcode.ErrUnavailable {
			t.Fatalf("timeout-late-apply code=%v, want ErrUnavailable", errcode.As(firstErr))
		}
	case <-time.After(time.Second):
		t.Fatal("first allocation did not return after simulated timeout")
	}
	if !allocator.lateApplied.Load() {
		t.Fatal("test did not reach simulated late apply")
	}

	// 强制派生索引进入 stale；两轮 sweep 必须严格只读：不 Release、不删 claim、
	// 不刷新/恢复 TTL，也不移出 active。
	if _, err := mr.ZAdd("pandora:ds:active", 0, "807"); err != nil {
		t.Fatalf("backdate active index: %v", err)
	}
	rawBefore, err := rdb.Get(ctx, "pandora:ds:battle:{807}").Bytes()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := uc.sweepOnce(ctx); err != nil {
			t.Fatalf("sweep %d: %v", i+1, err)
		}
	}
	rawAfter, err := rdb.Get(ctx, "pandora:ds:battle:{807}").Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(rawAfter) != string(rawBefore) {
		t.Fatal("sweep mutated persistent uncertain claim")
	}
	if ttl := mr.TTL("pandora:ds:battle:{807}"); ttl != 0 {
		t.Fatalf("sweep restored/changed uncertain TTL: ttl=%s", ttl)
	}
	if allocator.releases.Load() != 0 || allocator.authoritativeCalls.Load() != 1 {
		t.Fatalf("uncertain sweep side effects: releases=%d GSA_POST_calls=%d",
			allocator.releases.Load(), allocator.authoritativeCalls.Load())
	}
	ids, err := repo.RangeActiveBattles(ctx)
	if err != nil || len(ids) != 1 || ids[0] != 807 {
		t.Fatalf("sweep removed uncertain audit index: ids=%v err=%v", ids, err)
	}
}

func TestAllocationUncertainLegacyWriterPathsAlsoFailClosed(t *testing.T) {
	ctx := context.Background()
	allocator := &authoritativeTestAllocator{delivered: make(chan map[string]string, 1)}
	uc, repo, mr := newUsecaseWithAlloc(t, allocator) // 故意不 EnableRedisAuthority，模拟 legacy 配置副本
	claim := &dsv1.BattleStorageRecord{
		MatchId: 808, State: stateAllocating, AllocationId: "alloc-808",
		AllocatedAtMs: 1, LastHeartbeatMs: 1,
	}
	if claimed, _, err := repo.ClaimBattle(ctx, claim, time.Hour); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	if fenced, err := repo.FenceBattleAllocation(ctx, 808, "alloc-808"); err != nil || !fenced {
		t.Fatalf("fence: fenced=%v err=%v", fenced, err)
	}
	if _, err := mr.ZAdd("pandora:ds:active", 0, "808"); err != nil {
		t.Fatalf("backdate active index: %v", err)
	}
	rawBefore, err := mr.Get("pandora:ds:battle:{808}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uc.AllocateBattle(ctx, 808, []uint64{1}, 1, "ranked"); err == nil ||
		errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("legacy awaitExisting err=%v code=%v", err, errcode.As(err))
	}
	if err := uc.ReleaseBattle(ctx, 808, "failed"); err == nil || errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("legacy ReleaseBattle err=%v code=%v", err, errcode.As(err))
	}
	if hb, err := uc.Heartbeat(ctx, 808, "old-pod", 1, stateRunning, time.Now().UnixMilli()); err != nil || hb.Command != commandStop {
		t.Fatalf("legacy fenced heartbeat result=%+v err=%v", hb, err)
	}
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("legacy sweep: %v", err)
	}
	rawAfter, err := mr.Get("pandora:ds:battle:{808}")
	if err != nil {
		t.Fatal(err)
	}
	if rawAfter != rawBefore || mr.TTL("pandora:ds:battle:{808}") != 0 {
		t.Fatal("legacy path mutated/de-expired uncertain claim")
	}
	if allocator.legacyCalls.Load() != 0 || allocator.releases.Load() != 0 {
		t.Fatalf("legacy path touched external allocator: allocate=%d release=%d",
			allocator.legacyCalls.Load(), allocator.releases.Load())
	}
	ids, err := repo.RangeActiveBattles(ctx)
	if err != nil || len(ids) != 1 || ids[0] != 808 {
		t.Fatalf("legacy path removed uncertain audit index: ids=%v err=%v", ids, err)
	}
}

func TestPreactiveReleaseLegacyWriterPathsAlsoFailClosed(t *testing.T) {
	ctx := context.Background()
	allocator := &authoritativeTestAllocator{delivered: make(chan map[string]string, 1)}
	uc, repo, mr := newUsecaseWithAlloc(t, allocator) // 故意 legacy 配置
	record := &dsv1.BattleStorageRecord{
		MatchId: 809, State: statePreactiveReleasing, AllocationId: "alloc-809",
		DsPodName: "battle-809", GameserverUid: "uid-809",
		AllocatedAtMs: 1, LastHeartbeatMs: 1,
	}
	if err := repo.CreateBattle(ctx, record, 0); err != nil {
		t.Fatal(err)
	}
	rawBefore, _ := mr.Get("pandora:ds:battle:{809}")
	if _, err := uc.AllocateBattle(ctx, 809, []uint64{1}, 1, "ranked"); err == nil || errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("legacy Allocate release fence err=%v code=%v", err, errcode.As(err))
	}
	if err := uc.ReleaseBattle(ctx, 809, "failed"); err == nil || errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("legacy Release release fence err=%v code=%v", err, errcode.As(err))
	}
	if hb, err := uc.Heartbeat(ctx, 809, "battle-809", 1, stateRunning, time.Now().UnixMilli()); err != nil || hb.Command != commandStop {
		t.Fatalf("legacy Heartbeat release fence result=%+v err=%v", hb, err)
	}
	if _, err := mr.ZAdd("pandora:ds:active", 0, "809"); err != nil {
		t.Fatal(err)
	}
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	rawAfter, _ := mr.Get("pandora:ds:battle:{809}")
	if rawAfter != rawBefore || mr.TTL("pandora:ds:battle:{809}") != 0 {
		t.Fatal("legacy paths mutated/de-expired preactive release fence")
	}
	if allocator.legacyCalls.Load() != 0 || allocator.releases.Load() != 0 {
		t.Fatalf("legacy paths touched external allocator: allocate=%d release=%d",
			allocator.legacyCalls.Load(), allocator.releases.Load())
	}
}

func TestBattleModelB_SweepReconcilesCrashedAllocatingClaim(t *testing.T) {
	ctx := context.Background()
	allocator := &authoritativeTestAllocator{delivered: make(chan map[string]string, 1)}
	uc, repo, mr := newUsecaseWithAlloc(t, allocator)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	signer, err := auth.NewSigner(auth.Config{
		Issuer: auth.DSCallbackIssuer, Audience: auth.DSCallbackAudience,
		Secret: []byte("battle-model-b-crash-claim-secret-32b"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.EnableRedisAuthority(data.NewRedisBattleAuthRepo(rdb), signer, time.Hour); err != nil {
		t.Fatal(err)
	}
	claim := &dsv1.BattleStorageRecord{
		MatchId: 805, State: stateAllocating, AllocationId: "alloc-crashed",
		AllocatedAtMs:   time.Now().Add(-time.Minute).UnixMilli(),
		LastHeartbeatMs: time.Now().Add(-time.Minute).UnixMilli(),
	}
	if claimed, _, err := repo.ClaimBattle(ctx, claim, time.Hour); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found, err := repo.GetBattle(ctx, 805); err != nil || found {
		t.Fatalf("crashed claim retained: found=%v err=%v", found, err)
	}
	if allocator.releases.Load() != 0 {
		t.Fatalf("pre-POST allocating claim must not call external release, calls=%d", allocator.releases.Load())
	}
}

func TestBattleModelBSweepReliableCompensationKeepsOutbox(t *testing.T) {
	ctx := context.Background()
	allocator := &authoritativeTestAllocator{delivered: make(chan map[string]string, 1)}
	uc, battleRepo, mr := newUsecaseWithAlloc(t, allocator)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	authRepo := data.NewRedisBattleAuthRepo(rdb)
	signer, err := auth.NewSigner(auth.Config{
		Issuer: auth.DSCallbackIssuer, Audience: auth.DSCallbackAudience,
		Secret: []byte("battle-model-b-sweep-secret-32bytes!!"),
	})
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	if err := uc.EnableRedisAuthority(authRepo, signer, time.Hour); err != nil {
		t.Fatalf("EnableRedisAuthority: %v", err)
	}
	const matchID uint64 = 801
	const allocationID = "alloc-801"
	const pod = "battle-auth-801"
	claim := &dsv1.BattleStorageRecord{
		MatchId: matchID, State: stateAllocating, AllocationId: allocationID,
		AllocatedAtMs:   time.Now().Add(-time.Second).UnixMilli(),
		LastHeartbeatMs: time.Now().Add(-time.Second).UnixMilli(),
	}
	if claimed, _, err := battleRepo.ClaimBattle(ctx, claim, time.Hour); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	battle := proto.Clone(claim).(*dsv1.BattleStorageRecord)
	battle.State, battle.DsPodName, battle.DsAddr, battle.GameserverUid =
		stateWarming, pod, "10.0.0.9:7777", "uid-801"
	battle.PlayerIds, battle.MapId, battle.GameMode = []uint64{1, 2}, 1, "ranked"
	if ok, err := battleRepo.FinalizeBattleAllocation(ctx, battle, time.Hour); err != nil || !ok {
		t.Fatalf("finalize: ok=%v err=%v", ok, err)
	}
	seed, err := authRepo.PrepareCredential(ctx, data.BattleAuthorityBinding{
		MatchID: matchID, AllocationID: allocationID, PodName: pod, InstanceUID: "uid-801",
		RequiredWriterEpoch: data.BattleDSWriterEpochV2, AuthTTL: time.Hour, BattleTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	signed, err := signer.SignBattleCredential(
		matchID, pod, "uid-801", seed.InstanceEpoch, seed.Gen, uuid.NewString(), time.Hour)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	verifier, err := auth.NewVerifier(auth.Config{
		Issuer: auth.DSCallbackIssuer, Audience: auth.DSCallbackAudience,
		Secret: []byte("battle-model-b-sweep-secret-32bytes!!"),
	})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	claims, err := verifier.VerifyDSCallback(signed.Token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	stored := &dsv1.BattleDSCredential{
		Gen: seed.Gen, Jti: claims.JTI(), ExpMs: uint64(signed.ExpMs), Kid: signed.Kid,
		InstanceUid: "uid-801", InstanceEpoch: seed.InstanceEpoch,
		TokenSha256: signed.TokenSHA256, WriterEpoch: signed.WriterEpoch,
	}
	if _, err := authRepo.StagePending(ctx, data.BattleStageInput{
		MatchID: matchID, AllocationID: allocationID, Credential: stored, AuthTTL: time.Hour,
	}); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := authRepo.MarkDelivered(ctx, matchID, allocationID, stored, "102", time.Hour); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if _, err := authRepo.ActivateHeartbeat(ctx, matchID, data.BattleCredentialIdentity{
		PodName: pod, InstanceUID: "uid-801", InstanceEpoch: seed.InstanceEpoch,
		Gen: seed.Gen, JTI: stored.Jti, ExpMs: stored.ExpMs, Kid: stored.Kid,
		TokenSHA256: stored.TokenSha256, WriterEpoch: stored.WriterEpoch,
	}, data.BattleHeartbeatInput{PlayerCount: 2, State: stateRunning, AuthTTL: time.Hour, BattleTTL: time.Hour}); err != nil {
		t.Fatalf("activate: %v", err)
	}

	// 同时回拨 auth+battle 权威心跳与派生 ZSET，模拟真实失联。
	authBytes, _ := rdb.Get(ctx, "pandora:ds:auth:{801}").Bytes()
	authRec := &dsv1.BattleDSAuthStorageRecord{}
	if err := proto.Unmarshal(authBytes, authRec); err != nil {
		t.Fatalf("unmarshal auth: %v", err)
	}
	battleBytes, _ := rdb.Get(ctx, "pandora:ds:battle:{801}").Bytes()
	battleRec := &dsv1.BattleStorageRecord{}
	if err := proto.Unmarshal(battleBytes, battleRec); err != nil {
		t.Fatalf("unmarshal battle: %v", err)
	}
	authRec.LastActiveHeartbeatMs, battleRec.LastHeartbeatMs = 1, 1
	authBytes, _ = proto.Marshal(authRec)
	battleBytes, _ = proto.Marshal(battleRec)
	if err := rdb.Set(ctx, "pandora:ds:auth:{801}", authBytes, time.Hour).Err(); err != nil {
		t.Fatalf("backdate auth: %v", err)
	}
	if err := rdb.Set(ctx, "pandora:ds:battle:{801}", battleBytes, time.Hour).Err(); err != nil {
		t.Fatalf("backdate battle: %v", err)
	}
	if _, err := mr.ZAdd("pandora:ds:active", 1, "801"); err != nil {
		t.Fatalf("backdate index: %v", err)
	}
	life := &mockLifecycle{failFirst: 1}
	uc.SetLifecyclePusher(life)
	allocator.releaseErr = errors.New("simulated sweep ReleaseExpected timeout")

	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep1: %v", err)
	}
	if ids, _ := battleRepo.RangeActiveBattles(ctx); len(ids) != 1 {
		t.Fatalf("failed release lost outbox: active=%v", ids)
	}
	if allocator.releases.Load() != 1 || life.calls != 0 {
		t.Fatalf("sweep1 releases=%d lifecycle_calls=%d", allocator.releases.Load(), life.calls)
	}
	if authTTL, battleTTL := mr.TTL("pandora:ds:auth:{801}"), mr.TTL("pandora:ds:battle:{801}"); authTTL != 0 || battleTTL != 0 {
		t.Fatalf("release timeout lost permanent terminal fence: auth=%v battle=%v", authTTL, battleTTL)
	}
	allocator.releaseErr = nil
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep2: %v", err)
	}
	if ids, _ := battleRepo.RangeActiveBattles(ctx); len(ids) != 1 {
		t.Fatalf("failed lifecycle delivery lost outbox: active=%v", ids)
	}
	if allocator.releases.Load() != 2 || life.calls != 1 {
		t.Fatalf("sweep2 releases=%d lifecycle_calls=%d", allocator.releases.Load(), life.calls)
	}
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep3: %v", err)
	}
	if ids, _ := battleRepo.RangeActiveBattles(ctx); len(ids) != 0 {
		t.Fatalf("delivered outbox not removed: active=%v", ids)
	}
	// lifecycle 未确认前保留 outbox；每轮都用 UID/allocation fencing 幂等确认外部对象已回收，
	// 避免首轮 DELETE 结果未知却只补偿不再清理。
	if allocator.releases.Load() != 3 || life.calls != 2 || len(life.delivered) != 1 {
		t.Fatalf("retry missed fenced release confirmation or delivery: releases=%d calls=%d delivered=%v",
			allocator.releases.Load(), life.calls, life.delivered)
	}
}

// TestAllocateBattleReadyWaitTimeout:没有 DS 心跳 → 等待超时 → 回收 pod + 删镜像 + 返回分配失败
// (绝不把 ds_addr 回给 matchmaker)。
func TestAllocateBattleReadyWaitTimeout(t *testing.T) {
	ctx := context.Background()
	alloc := &countingAllocator{inner: NewMockGameServerAllocator(testCfg())}
	uc, repo, _ := newUsecaseWithAlloc(t, alloc)

	_, err := uc.AllocateBattle(ctx, 7, []uint64{10, 20}, 1, "5v5_ranked")
	if err == nil {
		t.Fatal("expected allocation failure on ready wait timeout")
	}
	if errcode.As(err) != errcode.ErrDSAllocationFailed {
		t.Fatalf("err code = %v, want ErrDSAllocationFailed", errcode.As(err))
	}
	if _, found, _ := repo.GetBattle(ctx, 7); found {
		t.Fatal("battle record must be deleted after ready wait timeout")
	}
	if alloc.releases != 1 {
		t.Fatalf("pod released %d times, want exactly 1", alloc.releases)
	}
}

// TestAllocateBattleRejectsMismatchedPodHeartbeat:证明 match_id ↔ pod 绑定不可绕过。
// 一个携带正确 match_id 但 pod 名不符(旧 DS / 孤儿 DS / 抢跑的别局 DS)的心跳,
// 必须被拒(返回 commandStop)、不得写回镜像、更不得打开 AllocateBattle 的就绪门控:
// 最终 AllocateBattle 仍因等不到本局 pod 的真实心跳而超时失败,绝不回 ds_addr。
func TestAllocateBattleRejectsMismatchedPodHeartbeat(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)

	type out struct {
		res *AllocateResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := uc.AllocateBattle(ctx, 7, []uint64{10, 20}, 1, "5v5_ranked")
		done <- out{res, err}
	}()

	// 等 warming 镜像出现(本局 pod = pandora-battle-7),记录其分配时刻。
	deadline := time.Now().Add(3 * time.Second)
	var rec *dsv1.BattleStorageRecord
	for {
		b, found, err := repo.GetBattle(ctx, 7)
		if err == nil && found && b.DsPodName != "" {
			rec = b
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("warming record for match 7 never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for time.Now().UnixMilli() <= rec.AllocatedAtMs {
		time.Sleep(time.Millisecond)
	}

	// 用「错误的 pod 名」上报 running 心跳:必须被门控拒绝并令其停机。
	hbRes, err := uc.Heartbeat(ctx, 7, "pandora-battle-999", 2, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("mismatched heartbeat returned hard error: %v", err)
	}
	if hbRes.Command != commandStop {
		t.Fatalf("mismatched-pod heartbeat command = %q, want stop", hbRes.Command)
	}

	// 镜像不得被异局心跳污染:仍停在 warming、未刷新到分配时刻之后。
	got, _, _ := repo.GetBattle(ctx, 7)
	if got.State != stateWarming {
		t.Fatalf("state = %q, want warming (foreign heartbeat must not flip state)", got.State)
	}
	if got.LastHeartbeatMs > got.AllocatedAtMs {
		t.Fatalf("LastHeartbeatMs %d must stay <= AllocatedAtMs %d (foreign heartbeat must not refresh)",
			got.LastHeartbeatMs, got.AllocatedAtMs)
	}

	// 门控不得放行:AllocateBattle 仍超时失败,绝不返回 ds_addr。
	r := <-done
	if r.err == nil {
		t.Fatalf("AllocateBattle must fail when only a mismatched pod heartbeat arrived, got addr=%q", r.res.DSAddr)
	}
	if errcode.As(r.err) != errcode.ErrDSAllocationFailed {
		t.Fatalf("err code = %v, want ErrDSAllocationFailed", errcode.As(r.err))
	}
	if _, found, _ := repo.GetBattle(ctx, 7); found {
		t.Fatal("battle record must be cleaned up after ready wait timeout")
	}
}

func TestAllocateBattleIdempotent(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)

	first := allocateReady(t, uc, repo, 7, []uint64{10, 20}, 1, "5v5_ranked")
	// 幂等:已 ready/running 且有有效心跳 → 第二次直接返回已分配地址(不再等心跳)
	second, err := uc.AllocateBattle(ctx, 7, []uint64{10, 20}, 1, "5v5_ranked")
	if err != nil {
		t.Fatalf("second allocate: %v", err)
	}
	if first.DSAddr != second.DSAddr || first.AllocatedAtMs != second.AllocatedAtMs {
		t.Fatalf("idempotent mismatch: %+v vs %+v", first, second)
	}
}

func TestReleaseBattleIdempotent(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)

	allocateReady(t, uc, repo, 7, []uint64{10}, 1, "5v5_ranked")
	if err := uc.ReleaseBattle(ctx, 7, "completed"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, found, _ := repo.GetBattle(ctx, 7); found {
		t.Fatal("battle 7 should be gone after release")
	}
	// 再次释放(已不存在)应幂等成功
	if err := uc.ReleaseBattle(ctx, 7, "completed"); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}
}

func TestHeartbeatUpdatesState(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)

	// allocateReady 已上报一次 running 心跳;再上报一次刷 player_count=8
	allocateReady(t, uc, repo, 7, []uint64{10, 20}, 1, "5v5_ranked")
	res, err := uc.Heartbeat(ctx, 7, "pandora-battle-7", 8, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if res.Command != "" {
		t.Fatalf("command = %q, want empty", res.Command)
	}
	got, _, _ := repo.GetBattle(ctx, 7)
	if got.State != "running" || got.PlayerCount != 8 {
		t.Fatalf("after heartbeat: %+v", got)
	}
}

// TestHeartbeatPodMismatchRejected:镜像已绑定某 pod,另一个 pod(旧/孤儿 DS)上报 → 返回 stop
// 且不写回镜像(不污染新对局的 state/心跳/player_count)。
func TestHeartbeatPodMismatchRejected(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)

	now := time.Now().UnixMilli()
	rec := &dsv1.BattleStorageRecord{
		MatchId: 7, DsPodName: "pandora-battle-7", DsAddr: "127.0.0.1:30007",
		State: stateWarming, AllocatedAtMs: now, LastHeartbeatMs: now, PlayerCount: 2,
	}
	if err := repo.CreateBattle(ctx, rec, 2*time.Hour); err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := uc.Heartbeat(ctx, 7, "pandora-battle-OLD", 9, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if res.Command != "stop" {
		t.Fatalf("command = %q, want stop", res.Command)
	}
	got, _, _ := repo.GetBattle(ctx, 7)
	if got.State != stateWarming || got.PlayerCount == 9 || got.LastHeartbeatMs != now {
		t.Fatalf("mismatched pod must not update record: %+v", got)
	}
}

func TestHeartbeatOrphanReturnsStop(t *testing.T) {
	ctx := context.Background()
	uc, _ := newUsecase(t)

	// 无对应镜像的孤儿 DS 上报心跳 → 应被告知 stop
	res, err := uc.Heartbeat(ctx, 999, "pandora-battle-999", 1, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if res.Command != "stop" {
		t.Fatalf("command = %q, want stop", res.Command)
	}
}

// recordingAllocator 记录 Release 的 podName 并经 channel 通知,供异步 kill 断言。
type recordingAllocator struct {
	inner    GameServerAllocator
	released chan string
}

func (r *recordingAllocator) Allocate(ctx context.Context, matchID uint64, mapID uint32, gameMode string) (string, string, error) {
	return r.inner.Allocate(ctx, matchID, mapID, gameMode)
}

func (r *recordingAllocator) Release(ctx context.Context, podName string) error {
	_ = r.inner.Release(ctx, podName)
	r.released <- podName
	return nil
}

// TestHeartbeatOrphanKillsStrandedDS:local 模式(killOrphanOnStop=true)下,orphan 心跳除了回 stop,
// 还必须主动 Release 幽灵 pod——UE DS 收 stop 不自杀,不主动 kill 会让它占端口污染下一局。
func TestHeartbeatOrphanKillsStrandedDS(t *testing.T) {
	ctx := context.Background()
	rec := &recordingAllocator{inner: NewMockGameServerAllocator(testCfg()), released: make(chan string, 1)}
	uc, _, _ := newUsecaseWithAlloc(t, rec)
	uc.SetKillOrphanOnStop(true)

	res, err := uc.Heartbeat(ctx, 999, "pandora-battle-999", 1, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if res.Command != commandStop {
		t.Fatalf("command = %q, want stop", res.Command)
	}
	select {
	case pod := <-rec.released:
		if pod != "pandora-battle-999" {
			t.Fatalf("released pod = %q, want pandora-battle-999", pod)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected stranded DS to be released on orphan stop")
	}
}

// TestHeartbeatPodMismatchKillsOldDS:local 模式下 pod 不匹配(旧 DS 上报)时,主动 kill 的是**上报方**
// (旧 DS 的 pod),不动镜像里绑定的新 pod。
func TestHeartbeatPodMismatchKillsOldDS(t *testing.T) {
	ctx := context.Background()
	rec := &recordingAllocator{inner: NewMockGameServerAllocator(testCfg()), released: make(chan string, 1)}
	uc, repo, _ := newUsecaseWithAlloc(t, rec)
	uc.SetKillOrphanOnStop(true)

	now := time.Now().UnixMilli()
	b := &dsv1.BattleStorageRecord{
		MatchId: 7, DsPodName: "pandora-battle-7", DsAddr: "127.0.0.1:30007",
		State: stateWarming, AllocatedAtMs: now, LastHeartbeatMs: now, PlayerCount: 2,
	}
	if err := repo.CreateBattle(ctx, b, 2*time.Hour); err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := uc.Heartbeat(ctx, 7, "pandora-battle-OLD", 9, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if res.Command != commandStop {
		t.Fatalf("command = %q, want stop", res.Command)
	}
	select {
	case pod := <-rec.released:
		if pod != "pandora-battle-OLD" {
			t.Fatalf("released pod = %q, want pandora-battle-OLD (the stale reporter)", pod)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected stale DS to be released on pod_mismatch stop")
	}
}

// TestHeartbeatOrphanNoKillWhenDisabled:killOrphanOnStop 关闭(Agones/默认)时,orphan 心跳只回
// stop,不主动 Release——孤儿 pod 回收交 Agones 生命周期,避免 Redis 抖动误判 orphan 误删正常 pod。
func TestHeartbeatOrphanNoKillWhenDisabled(t *testing.T) {
	ctx := context.Background()
	rec := &recordingAllocator{inner: NewMockGameServerAllocator(testCfg()), released: make(chan string, 1)}
	uc, _, _ := newUsecaseWithAlloc(t, rec)
	// 不调 SetKillOrphanOnStop → 默认 false

	if _, err := uc.Heartbeat(ctx, 999, "pandora-battle-999", 1, "running", time.Now().UnixMilli()); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	select {
	case pod := <-rec.released:
		t.Fatalf("must NOT release when killOrphanOnStop disabled, got %q", pod)
	case <-time.After(200 * time.Millisecond):
		// 期望:无 Release 发生
	}
}

// TestHeartbeatOnAbandonedReturnsStopNoRefresh:abandoned 对局的 DS 若继续心跳(pod release
// 失败/延迟终止),Heartbeat 必须返回 stop 且**不写回记录**——不刷新 LastHeartbeatMs/TTL,也不
// 重新 ZAdd active。否则补偿重试会被推迟、BattleTTL 上界被不断刷新(W4 ⑧ Codex 复审 P1)。
func TestHeartbeatOnAbandonedReturnsStopNoRefresh(t *testing.T) {
	ctx := context.Background()
	alloc := &countingAllocator{inner: NewMockGameServerAllocator(testCfg())}
	uc, repo, mr := newUsecaseWithAlloc(t, alloc)
	life := &mockLifecycle{failFirst: 1000} // 始终投递失败,abandoned 对局保留在 active 重试
	uc.SetLifecyclePusher(life)

	allocateReady(t, uc, repo, 7, []uint64{10, 20}, 1, "5v5_ranked")
	backdate(t, repo, 7) // LastHeartbeatMs=1

	// sweep #1:投递失败 → 标记 abandoned、回收 pod、保留在 active 待重试
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep1: %v", err)
	}

	// 把 TTL 钉到已知小值,便于检测心跳是否误刷新
	key := "pandora:ds:battle:{7}"
	mr.SetTTL(key, 90*time.Second)
	ttlBefore := mr.TTL(key)
	if ttlBefore <= 0 {
		t.Fatalf("precondition: ttl not pinned, got %v", ttlBefore)
	}

	// abandoned 后 DS 继续心跳:必须返回 stop,且不写回记录
	res, err := uc.Heartbeat(ctx, 7, "pandora-battle-7", 9, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if res.Command != "stop" {
		t.Fatalf("command = %q, want stop", res.Command)
	}

	// 记录未被写回:LastHeartbeatMs 仍是回拨值 1(active score = LastHeartbeatMs 也未刷新),
	// state 仍 abandoned,PlayerCount 未被改成 9
	got, _, _ := repo.GetBattle(ctx, 7)
	if got.LastHeartbeatMs != 1 {
		t.Fatalf("LastHeartbeatMs = %d, want 1 (terminal heartbeat must not write back)", got.LastHeartbeatMs)
	}
	if got.State != "abandoned" {
		t.Fatalf("state = %q, want abandoned", got.State)
	}
	if got.PlayerCount == 9 {
		t.Fatalf("PlayerCount refreshed to 9, terminal record must not be written")
	}

	// TTL 未被心跳刷新(仍 ≤ 钉住的 90s)
	if ttlAfter := mr.TTL(key); ttlAfter > ttlBefore {
		t.Fatalf("TTL refreshed by terminal heartbeat: before=%v after=%v", ttlBefore, ttlAfter)
	}

	// active score 仍是陈旧值 → 下一轮 sweep 仍会命中重试
	stale, _ := repo.RangeStaleBattles(ctx, 1000)
	if len(stale) != 1 || stale[0] != 7 {
		t.Fatalf("stale = %v, want [7] (active score not refreshed, sweep still retries)", stale)
	}

	// 下一轮 sweep 仍重试投递(补偿没被心跳推迟)
	callsBefore := life.calls
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep2: %v", err)
	}
	if life.calls != callsBefore+1 {
		t.Fatalf("sweep2 publish calls = %d, want %d (retry continues)", life.calls, callsBefore+1)
	}
	if alloc.releases != 1 {
		t.Fatalf("pod released %d times, want exactly 1 (no re-release)", alloc.releases)
	}
}

func TestListBattles(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)

	allocateReady(t, uc, repo, 1, []uint64{10}, 1, "5v5_ranked")
	allocateReady(t, uc, repo, 2, []uint64{20}, 1, "5v5_ranked")

	all, err := uc.ListBattles(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("list all = %d, want 2", len(all))
	}

	// 状态过滤:等到 ready 心跳后两局都是 running,ready 无
	running, _ := uc.ListBattles(ctx, "running")
	if len(running) != 2 {
		t.Fatalf("list running = %d, want 2", len(running))
	}
	ready, _ := uc.ListBattles(ctx, "ready")
	if len(ready) != 0 {
		t.Fatalf("list ready = %d, want 0", len(ready))
	}
}

func TestSweepMarksAbandoned(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)

	allocateReady(t, uc, repo, 7, []uint64{10}, 1, "5v5_ranked")
	// 手动把 last_heartbeat_ms 回拨到远古,模拟心跳超时
	if err := repo.UpdateBattleWithLock(ctx, 7, 3, func(b *dsv1.BattleStorageRecord) error {
		b.LastHeartbeatMs = 1
		return nil
	}, 2*time.Hour); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, found, _ := repo.GetBattle(ctx, 7)
	if !found {
		t.Fatal("battle should still exist (terminal record retained)")
	}
	if got.State != "abandoned" {
		t.Fatalf("state = %q, want abandoned", got.State)
	}
	// 已移出 active,不再被扫描
	ids, _ := repo.RangeActiveBattles(ctx)
	if len(ids) != 0 {
		t.Fatalf("active should be empty after sweep, got %v", ids)
	}
}

// TestSweepEndedReclaimsLocalDS:local 模式(killOrphanOnStop=true)下,正常结算(ended)且失联的 DS
// 被扫到时,除了移出 active 还必须主动 Release——battle_result 不再直杀,DS 发完 ended 心跳即停心跳
// (无第二跳触发终态 kill),local Agones Shutdown 又是 no-op 不自退,不在此兜底 taskkill 就会幽灵占端口。
func TestSweepEndedReclaimsLocalDS(t *testing.T) {
	ctx := context.Background()
	rec := &recordingAllocator{inner: NewMockGameServerAllocator(testCfg()), released: make(chan string, 1)}
	uc, repo, _ := newUsecaseWithAlloc(t, rec)
	uc.SetKillOrphanOnStop(true)

	res := allocateReady(t, uc, repo, 8, []uint64{11, 22}, 1, "5v5_ranked")
	// 上报一次 ended 心跳:state → ended(首跳不 kill),仍留在 active 待扫描收尾。
	if _, err := uc.Heartbeat(ctx, 8, res.DSPodName, 0, "ended", time.Now().UnixMilli()); err != nil {
		t.Fatalf("ended heartbeat: %v", err)
	}
	backdate(t, repo, 8) // 心跳超时,进入 sweep 扫描窗口

	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	select {
	case pod := <-rec.released:
		if pod != res.DSPodName {
			t.Fatalf("released pod = %q, want %q", pod, res.DSPodName)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected ended DS to be reclaimed on sweep (local ghost-process leak)")
	}
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 0 {
		t.Fatalf("active should be empty after ended sweep, got %v", ids)
	}
}

// TestSweepEndedNoReclaimOnAgones:Agones 模式(killOrphanOnStop=false)下,ended 对局扫到只移出
// active,绝不主动 Release——DS 已自身 Agones Shutdown,pod 回收交 Fleet 生命周期,后端不越权 kill。
func TestSweepEndedNoReclaimOnAgones(t *testing.T) {
	ctx := context.Background()
	rec := &recordingAllocator{inner: NewMockGameServerAllocator(testCfg()), released: make(chan string, 1)}
	uc, repo, _ := newUsecaseWithAlloc(t, rec)
	// 不调 SetKillOrphanOnStop → 默认 false(Agones 模式)

	res := allocateReady(t, uc, repo, 9, []uint64{33, 44}, 1, "5v5_ranked")
	if _, err := uc.Heartbeat(ctx, 9, res.DSPodName, 0, "ended", time.Now().UnixMilli()); err != nil {
		t.Fatalf("ended heartbeat: %v", err)
	}
	backdate(t, repo, 9)

	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	select {
	case pod := <-rec.released:
		t.Fatalf("must NOT release ended DS on Agones (killOrphanOnStop off), got %q", pod)
	case <-time.After(200 * time.Millisecond):
		// 期望:无 Release 发生
	}
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 0 {
		t.Fatalf("active should be empty after ended sweep, got %v", ids)
	}
}

// TestSweepDeliversAbandonedFirstTry:配置 kafka 且首次投递成功 → 发 1 次事件、移出 active、回收 1 次。
func TestSweepDeliversAbandonedFirstTry(t *testing.T) {
	ctx := context.Background()
	alloc := &countingAllocator{inner: NewMockGameServerAllocator(testCfg())}
	uc, repo, _ := newUsecaseWithAlloc(t, alloc)
	life := &mockLifecycle{}
	uc.SetLifecyclePusher(life)

	allocateReady(t, uc, repo, 5, []uint64{1, 2}, 1, "5v5_ranked")
	backdate(t, repo, 5)

	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 0 {
		t.Fatalf("active = %v, want empty after delivery", ids)
	}
	if life.calls != 1 || len(life.delivered) != 1 || life.delivered[0] != 5 {
		t.Fatalf("publish calls=%d delivered=%v, want 1 / [5]", life.calls, life.delivered)
	}
	if alloc.releases != 1 {
		t.Fatalf("releases=%d, want 1", alloc.releases)
	}
}

// TestSweepRechecksAuthorityBeforeAbandon 模拟权威 record 心跳写成功、跨 slot ZADD 失败留下
// 旧 score。sweep 必须先重读 record 修索引，绝不能误标 abandoned/Release/发补偿。
func TestSweepRechecksAuthorityBeforeAbandon(t *testing.T) {
	ctx := context.Background()
	alloc := &countingAllocator{inner: NewMockGameServerAllocator(testCfg())}
	uc, repo, mr := newUsecaseWithAlloc(t, alloc)
	life := &mockLifecycle{}
	uc.SetLifecyclePusher(life)
	allocateReady(t, uc, repo, 71, []uint64{1, 2}, 1, "ranked")

	if err := repo.UpdateBattleWithLock(ctx, 71, 3, func(b *dsv1.BattleStorageRecord) error {
		b.LastHeartbeatMs = time.Now().UnixMilli()
		return nil
	}, 2*time.Hour); err != nil {
		t.Fatalf("refresh record: %v", err)
	}
	if _, err := mr.ZAdd("pandora:ds:active", 1, "71"); err != nil {
		t.Fatalf("force stale derived index: %v", err)
	}
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, found, err := repo.GetBattle(ctx, 71)
	if err != nil || !found || got.State != stateRunning {
		t.Fatalf("fresh authority was abandoned: found=%v rec=%+v err=%v", found, got, err)
	}
	if alloc.releases != 0 || life.calls != 0 {
		t.Fatalf("stale index caused side effects: releases=%d lifecycle=%d", alloc.releases, life.calls)
	}
	stale, err := repo.RangeStaleBattles(ctx, time.Now().Add(-time.Second).UnixMilli())
	if err != nil || len(stale) != 0 {
		t.Fatalf("derived index was not repaired: stale=%v err=%v", stale, err)
	}
}

// TestSweepReliableCompensation_RetryUntilDelivered:Kafka 前两轮不可用 → abandoned 对局保留在
// active 重试,第三轮投递成功才移出;pod 只在首次转 abandoned 回收一次(不变量 §4 可靠补偿)。
func TestSweepReliableCompensation_RetryUntilDelivered(t *testing.T) {
	ctx := context.Background()
	alloc := &countingAllocator{inner: NewMockGameServerAllocator(testCfg())}
	uc, repo, _ := newUsecaseWithAlloc(t, alloc)
	life := &mockLifecycle{failFirst: 2} // 前两轮投递失败,第三轮成功
	uc.SetLifecyclePusher(life)

	allocateReady(t, uc, repo, 7, []uint64{10, 20}, 1, "5v5_ranked")
	backdate(t, repo, 7)

	// sweep #1:投递失败 → 标记 abandoned、回收 pod、保留在 active 待重试
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep1: %v", err)
	}
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 1 {
		t.Fatalf("after sweep1 active = %v, want still 1 (retry pending)", ids)
	}
	if got, _, _ := repo.GetBattle(ctx, 7); got.State != "abandoned" {
		t.Fatalf("after sweep1 state = %q, want abandoned", got.State)
	}

	// sweep #2:仍失败 → 仍保留 active,pod 不重复回收
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep2: %v", err)
	}
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 1 {
		t.Fatalf("after sweep2 active = %v, want still 1", ids)
	}

	// sweep #3:投递成功 → 移出 active
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep3: %v", err)
	}
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 0 {
		t.Fatalf("after sweep3 active = %v, want empty (delivered)", ids)
	}

	if alloc.releases != 1 {
		t.Fatalf("pod released %d times, want exactly 1 (no re-release during retry)", alloc.releases)
	}
	if life.calls != 3 {
		t.Fatalf("publish called %d times, want 3 (2 fail + 1 success)", life.calls)
	}
	if len(life.delivered) != 1 || life.delivered[0] != 7 {
		t.Fatalf("delivered = %v, want [7]", life.delivered)
	}
}

// ── 空场超时兜底(2026-07-06,全员掉线/从未连入的 DS 防空转)──────────────────────

// backdateEmptySince 把 EmptySinceMs 回拨到远古,模拟空场已持续超过 EmptyBattleTimeout。
func backdateEmptySince(t *testing.T, repo *data.RedisBattleRepo, matchID uint64) {
	t.Helper()
	if err := repo.UpdateBattleWithLock(context.Background(), matchID, 3, func(b *dsv1.BattleStorageRecord) error {
		b.EmptySinceMs = 1
		return nil
	}, 2*time.Hour); err != nil {
		t.Fatalf("backdate empty_since: %v", err)
	}
}

// TestHeartbeatEmptyBattleTimeout:running 对局 player_count==0 持续超 EmptyBattleTimeout →
// 心跳内判 abandoned + 回 stop + 回收 pod + 投递补偿事件 + 移出 active(空场兜底)。
func TestHeartbeatEmptyBattleTimeout(t *testing.T) {
	ctx := context.Background()
	alloc := &countingAllocator{inner: NewMockGameServerAllocator(testCfg())}
	uc, repo, _ := newUsecaseWithAlloc(t, alloc)
	life := &mockLifecycle{}
	uc.SetLifecyclePusher(life)

	res := allocateReady(t, uc, repo, 7, []uint64{10, 20}, 1, "5v5_ranked")

	// 第一跳空场:只盖 EmptySinceMs 起计时,不判弃
	hb, err := uc.Heartbeat(ctx, 7, res.DSPodName, 0, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if hb.Command != commandNone {
		t.Fatalf("command = %q, want none (first empty beat only starts timer)", hb.Command)
	}
	got, _, _ := repo.GetBattle(ctx, 7)
	if got.EmptySinceMs == 0 {
		t.Fatal("EmptySinceMs should be set on first empty heartbeat")
	}
	if got.State != stateRunning {
		t.Fatalf("state = %q, want still running", got.State)
	}

	// 空场持续超时(回拨 EmptySinceMs)→ 下一跳判 abandoned
	backdateEmptySince(t, repo, 7)
	hb2, err := uc.Heartbeat(ctx, 7, res.DSPodName, 0, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat2: %v", err)
	}
	if hb2.Command != commandStop {
		t.Fatalf("command = %q, want stop", hb2.Command)
	}
	got2, _, _ := repo.GetBattle(ctx, 7)
	if got2.State != stateAbandoned {
		t.Fatalf("state = %q, want abandoned", got2.State)
	}
	if alloc.releases != 1 {
		t.Fatalf("releases = %d, want 1", alloc.releases)
	}
	if len(life.delivered) != 1 || life.delivered[0] != 7 {
		t.Fatalf("delivered = %v, want [7] (段位回滚补偿事件)", life.delivered)
	}
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 0 {
		t.Fatalf("active = %v, want empty after empty-timeout abandon", ids)
	}
}

// TestHeartbeatEmptyResetWhenPlayersReturn:空场计时后有人重连回来 → EmptySinceMs 清零,不判弃
// (全员短暂掉线正在重连的局绝不能被误杀,阈值语义是「持续空场」)。
func TestHeartbeatEmptyResetWhenPlayersReturn(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)

	res := allocateReady(t, uc, repo, 7, []uint64{10, 20}, 1, "5v5_ranked")
	if _, err := uc.Heartbeat(ctx, 7, res.DSPodName, 0, "running", time.Now().UnixMilli()); err != nil {
		t.Fatalf("empty heartbeat: %v", err)
	}
	if got, _, _ := repo.GetBattle(ctx, 7); got.EmptySinceMs == 0 {
		t.Fatal("EmptySinceMs should be set")
	}
	// 玩家重连回来 → 清零
	if _, err := uc.Heartbeat(ctx, 7, res.DSPodName, 2, "running", time.Now().UnixMilli()); err != nil {
		t.Fatalf("rejoin heartbeat: %v", err)
	}
	got, _, _ := repo.GetBattle(ctx, 7)
	if got.EmptySinceMs != 0 {
		t.Fatalf("EmptySinceMs = %d, want 0 after players return", got.EmptySinceMs)
	}
	if got.State != stateRunning {
		t.Fatalf("state = %q, want running", got.State)
	}
}

// TestHeartbeatEmptyTimeoutDisabled:EmptyBattleTimeout 配负值 → 空场只计时不判弃(显式禁用)。
func TestHeartbeatEmptyTimeoutDisabled(t *testing.T) {
	ctx := context.Background()
	uc, repo := newUsecase(t)
	cfg := testCfg()
	cfg.EmptyBattleTimeout = config.Duration(-1) // 显式禁用
	ucDisabled := NewAllocatorUsecase(repo, NewMockGameServerAllocator(cfg), cfg)

	res := allocateReady(t, uc, repo, 7, []uint64{10, 20}, 1, "5v5_ranked")
	if _, err := ucDisabled.Heartbeat(ctx, 7, res.DSPodName, 0, "running", time.Now().UnixMilli()); err != nil {
		t.Fatalf("empty heartbeat: %v", err)
	}
	backdateEmptySince(t, repo, 7)
	hb, err := ucDisabled.Heartbeat(ctx, 7, res.DSPodName, 0, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if hb.Command != commandNone {
		t.Fatalf("command = %q, want none (empty timeout disabled)", hb.Command)
	}
	if got, _, _ := repo.GetBattle(ctx, 7); got.State != stateRunning {
		t.Fatalf("state = %q, want running (disabled must not abandon)", got.State)
	}
}

// TestHeartbeatEmptyTimeoutDeliveryRetry:空场判弃时 Kafka 不可用 → 保留在 active;
// 后续 sweep 以 firstAbandon=false 路径重试投递(不重复回收 pod),闭环同心跳超时补偿(不变量 §4)。
func TestHeartbeatEmptyTimeoutDeliveryRetry(t *testing.T) {
	ctx := context.Background()
	alloc := &countingAllocator{inner: NewMockGameServerAllocator(testCfg())}
	uc, repo, _ := newUsecaseWithAlloc(t, alloc)
	life := &mockLifecycle{failFirst: 1} // 心跳内首投失败,sweep 重试成功
	uc.SetLifecyclePusher(life)

	res := allocateReady(t, uc, repo, 7, []uint64{10, 20}, 1, "5v5_ranked")
	if _, err := uc.Heartbeat(ctx, 7, res.DSPodName, 0, "running", time.Now().UnixMilli()); err != nil {
		t.Fatalf("empty heartbeat: %v", err)
	}
	backdateEmptySince(t, repo, 7)
	hb, err := uc.Heartbeat(ctx, 7, res.DSPodName, 0, "running", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if hb.Command != commandStop {
		t.Fatalf("command = %q, want stop", hb.Command)
	}
	// 投递失败 → 保留在 active 等 sweep 重试
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 1 {
		t.Fatalf("active = %v, want still 1 (delivery retry pending)", ids)
	}

	// DS 收 stop 停跳 → 心跳超时后 sweep 扫到,firstAbandon=false 路径重试投递成功
	backdate(t, repo, 7)
	if err := uc.sweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 0 {
		t.Fatalf("active = %v, want empty after retry delivered", ids)
	}
	if alloc.releases != 1 {
		t.Fatalf("releases = %d, want exactly 1 (no re-release on retry)", alloc.releases)
	}
	if len(life.delivered) != 1 || life.delivered[0] != 7 {
		t.Fatalf("delivered = %v, want [7]", life.delivered)
	}
	// 终态镜像仍可查
	if rec, found, _ := repo.GetBattle(ctx, 7); !found || rec.State != "abandoned" {
		t.Fatalf("terminal record missing/wrong: found=%v rec=%+v", found, rec)
	}
}

// TestSweepReliableCompensation_KeepsTTLOnFailure:Kafka 持续不可用时,abandoned 标记 + 每轮重试
// 走 UpdateBattleKeepTTL(KEEPTTL),保留镜像原 TTL 不刷新 → BattleTTL 是补偿重试的天然上界
// (不变量 §4)。若误用刷新 TTL 的更新路径,会导致镜像永不过期、active 无限堆积。
func TestSweepReliableCompensation_KeepsTTLOnFailure(t *testing.T) {
	ctx := context.Background()
	alloc := &countingAllocator{inner: NewMockGameServerAllocator(testCfg())}
	uc, repo, mr := newUsecaseWithAlloc(t, alloc)
	life := &mockLifecycle{failFirst: 1000} // 始终投递失败
	uc.SetLifecyclePusher(life)

	allocateReady(t, uc, repo, 7, []uint64{10, 20}, 1, "5v5_ranked")
	backdate(t, repo, 7)

	// 把 TTL 钉到一个已知的小值,便于检测是否被重试刷新(CreateBattle/backdate 会先设成 BattleTTL 2h)
	key := "pandora:ds:battle:{7}"
	mr.SetTTL(key, 90*time.Second)
	ttlBefore := mr.TTL(key)
	if ttlBefore <= 0 {
		t.Fatalf("precondition: ttl not pinned, got %v", ttlBefore)
	}

	// 连续多轮 sweep,全部投递失败 → abandoned 对局保留在 active 重试
	for i := 0; i < 3; i++ {
		if err := uc.sweepOnce(ctx); err != nil {
			t.Fatalf("sweep #%d: %v", i+1, err)
		}
	}

	// 关键断言:TTL 没被重试刷新(仍 ≤ 钉住的 90s,而非回弹到 BattleTTL 2h)
	ttlAfter := mr.TTL(key)
	if ttlAfter > ttlBefore {
		t.Fatalf("TTL refreshed on retry: before=%v after=%v(KEEPTTL 未生效,BattleTTL 上界不成立)", ttlBefore, ttlAfter)
	}
	// 仍保留在 active 等待重试,状态 abandoned,pod 只回收一次
	if ids, _ := repo.RangeActiveBattles(ctx); len(ids) != 1 {
		t.Fatalf("active = %v, want still 1 (retry pending)", ids)
	}
	if got, _, _ := repo.GetBattle(ctx, 7); got.State != "abandoned" {
		t.Fatalf("state = %q, want abandoned", got.State)
	}
	if alloc.releases != 1 {
		t.Fatalf("pod released %d times, want exactly 1 (no re-release during retry)", alloc.releases)
	}
}

// ---- 断线重连:心跳续期 BATTLE 位置(docs/design/battle-reconnect.md)----

type refreshCall struct {
	players []uint64
	matchID uint64
	dsAddr  string
}

// fakeRefresher 记录 RefreshBattleLocations 调用(异步续期,用带缓冲 channel 接收)。
type fakeRefresher struct {
	calls chan refreshCall
}

func newFakeRefresher() *fakeRefresher { return &fakeRefresher{calls: make(chan refreshCall, 8)} }

func (f *fakeRefresher) RefreshBattleLocations(_ context.Context, players []uint64, matchID uint64, dsAddr string) error {
	cp := append([]uint64(nil), players...)
	f.calls <- refreshCall{players: cp, matchID: matchID, dsAddr: dsAddr}
	return nil
}

// TestHeartbeatRefreshesBattleLocation 验证:running 心跳后异步给在场玩家续期 BATTLE 位置,
// 携带正确的 match_id / ds_addr / 玩家列表(让登录侧能在整局内识别重连)。
func TestHeartbeatRefreshesBattleLocation(t *testing.T) {
	ctx := context.Background()
	uc, repo, _ := newUsecaseWithAlloc(t, NewMockGameServerAllocator(testCfg()))
	matchID := uint64(555)
	players := []uint64{1, 2, 3}
	res := allocateReady(t, uc, repo, matchID, players, 10, "5v5_ranked")

	fr := newFakeRefresher()
	uc.SetLocationRefresher(fr)

	b, found, err := repo.GetBattle(ctx, matchID)
	if err != nil || !found {
		t.Fatalf("get battle: err=%v found=%v", err, found)
	}
	if _, err := uc.Heartbeat(ctx, matchID, b.DsPodName, int32(len(players)), "running", time.Now().UnixMilli()); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	select {
	case c := <-fr.calls:
		if c.matchID != matchID {
			t.Errorf("refresh matchID = %d, want %d", c.matchID, matchID)
		}
		if c.dsAddr != res.DSAddr {
			t.Errorf("refresh dsAddr = %q, want %q", c.dsAddr, res.DSAddr)
		}
		if len(c.players) != len(players) {
			t.Errorf("refresh players = %v, want %v", c.players, players)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("running heartbeat did not trigger battle location refresh")
	}
}

// TestHeartbeatNoRefreshWhenNilRefresher 验证:未注入 refresher(dev / 未配 locator_addr)时,
// running 心跳不 panic、正常返回(弱依赖降级)。
func TestHeartbeatNoRefreshWhenNilRefresher(t *testing.T) {
	ctx := context.Background()
	uc, repo, _ := newUsecaseWithAlloc(t, NewMockGameServerAllocator(testCfg()))
	matchID := uint64(556)
	players := []uint64{1, 2}
	allocateReady(t, uc, repo, matchID, players, 10, "5v5_ranked") // 未 SetLocationRefresher

	b, _, _ := repo.GetBattle(ctx, matchID)
	if _, err := uc.Heartbeat(ctx, matchID, b.DsPodName, int32(len(players)), "running", time.Now().UnixMilli()); err != nil {
		t.Fatalf("heartbeat with nil refresher should not fail: %v", err)
	}
}
