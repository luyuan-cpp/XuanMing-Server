// hub_authoritative_test.go — Model B 原子线性化写路径测试(miniredis)。
// 覆盖 ActivateHeartbeat(promote + 分片 warming→ready + 投影 active 元组同事务 / 幂等 / stale /
// 分片缺失不 promote / 相位锁定 / authTTL 与 shardTTL 独立)、ReserveRoutableSeat(授权+路由+占座
// 原子门各分支)、CheckRoutable(只读不占座)、并发 barrier(WATCH 冲突下 promote 恰一次)。
package data

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
)

// shardRepoFor 复用授权仓底层 rdb 造一个分片仓(authKey/shardKey 同 {pod} slot,同一 miniredis)。
func shardRepoFor(repo *RedisHubAuthRepo) *RedisHubRepo { return &RedisHubRepo{rdb: repo.rdb} }

// seedWarmingShard 播种一个 warming 分片镜像(模拟拓扑种子,等首个鉴权心跳翻 ready)。
func seedWarmingShard(t *testing.T, repo *RedisHubAuthRepo, pod string, lastHbMs int64) {
	t.Helper()
	rec := sampleShard(pod, 1, lastHbMs)
	rec.State = "warming"
	if err := shardRepoFor(repo).CreateShard(context.Background(), rec, testTTL); err != nil {
		t.Fatalf("seed warming shard: %v", err)
	}
}

const activateAuthTTL = 48 * time.Hour

// activateInput 组装心跳负载:authTTL 48h(远长于 shardTTL 30m,CE8),shardTTL=testTTL。
func activateInput(playerCount int32, state string, tsMs int64) ActivateHeartbeatInput {
	playerIDs := make([]uint64, playerCount)
	for i := range playerIDs {
		playerIDs[i] = uint64(i + 1)
	}
	return ActivateHeartbeatInput{
		PlayerCount: playerCount,
		PlayerIDs:   playerIDs,
		MaxPlayers:  500,
		State:       state,
		TsMs:        tsMs,
		AuthTTL:     activateAuthTTL,
		ShardTTL:    testTTL,
	}
}

func TestActivate_PromoteFlipsShardSameTx(t *testing.T) {
	ctx := context.Background()
	repo, mr := newAuthRepo(t)
	const pod = "pandora-hub-global-1"
	now := time.Now().UnixMilli()

	if _, err := repo.InitAuth(ctx, pod, "uid-A", activateAuthTTL); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := repo.StagePending(ctx, pod, credFor(pod, "uid-A", 1, 7, "j7"), activateAuthTTL); err != nil {
		t.Fatalf("stage: %v", err)
	}
	seedWarmingShard(t, repo, pod, now)

	idOK := CredentialIdentity{Gen: 7, JTI: "j7", InstanceUID: "uid-A", ProtocolEpoch: 1, TokenSHA256: "sha-j7", Kid: "kid-1", WriterEpoch: testWriterEpoch}

	// 首个合法 pending 心跳 → 同事务 promote + 分片 warming→ready + 投影 active 元组。
	res, err := repo.ActivateHeartbeat(ctx, pod, idOK, activateInput(3, "", now))
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if !res.Accepted || !res.ShardFound || res.ShardState != "ready" || res.ActiveGen != 7 {
		t.Fatalf("first heartbeat must promote+flip ready, got %+v", res)
	}
	auth, _, _ := repo.GetAuth(ctx, pod)
	if auth.Active == nil || auth.Active.Gen != 7 || auth.Pending != nil || auth.Phase != hubv1.HubAuthPhase_HUB_AUTH_PHASE_ACTIVE {
		t.Fatalf("auth after activate: active=gen7 pending=nil ACTIVE; got %+v", auth)
	}
	shard, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	if shard.State != "ready" || shard.PlayerCount != 0 || shard.ReportedConnectedCount != 3 ||
		shard.LastVerifiedGen != 7 ||
		shard.LastVerifiedJti != "j7" || shard.GameserverUid != "uid-A" || shard.AuthEpoch != 1 {
		t.Fatalf("shard must be ready + last_verified projected by active tuple, got %+v", shard)
	}

	// CE8:授权键 TTL 用 authTTL(48h),分片键 TTL 用 shardTTL(30m),互不缩短。
	authTTLLeft := mr.TTL(authKey(pod))
	shardTTLLeft := mr.TTL(shardKey(pod))
	if authTTLLeft <= testTTL {
		t.Fatalf("CE8: auth key TTL must use authTTL(48h), got %v (<=shardTTL %v)", authTTLLeft, testTTL)
	}
	if shardTTLLeft > testTTL {
		t.Fatalf("shard key TTL must use shardTTL(30m), got %v", shardTTLLeft)
	}

	// 重复心跳(同 active)→ 幂等,不再 promote,分片续跳。
	res2, err := repo.ActivateHeartbeat(ctx, pod, idOK, activateInput(5, "", now+1000))
	if err != nil {
		t.Fatalf("activate repeat: %v", err)
	}
	if res2.Accepted || !res2.ShardFound || res2.ActiveGen != 7 {
		t.Fatalf("repeat heartbeat must be idempotent (not accepted), got %+v", res2)
	}

	// stale:旧代际凭据 → fail-closed(两键零变更)。
	idStale := CredentialIdentity{Gen: 3, JTI: "j3", InstanceUID: "uid-A", ProtocolEpoch: 1, TokenSHA256: "sha-j3", Kid: "kid-1", WriterEpoch: testWriterEpoch}
	if _, err := repo.ActivateHeartbeat(ctx, pod, idStale, activateInput(9, "", now+2000)); errcode.As(err) != errcode.ErrUnauthorized {
		t.Fatalf("stale credential must be rejected ErrUnauthorized, got %v", err)
	}
	// uid 不符 → fail-closed。
	idBadUID := CredentialIdentity{Gen: 7, JTI: "j7", InstanceUID: "uid-Z", ProtocolEpoch: 1, TokenSHA256: "sha-j7", Kid: "kid-1", WriterEpoch: testWriterEpoch}
	if _, err := repo.ActivateHeartbeat(ctx, pod, idBadUID, activateInput(9, "", now+3000)); errcode.As(err) != errcode.ErrUnauthorized {
		t.Fatalf("uid mismatch must be rejected, got %v", err)
	}
	// stale/uid 拒绝后权威 player_count 仍由空 ledger 派生为 0；实报审计停在上次合法值 5。
	shard2, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	if shard2.PlayerCount != 0 || shard2.ReportedConnectedCount != 5 {
		t.Fatalf("rejected heartbeats must not mutate shard, got %+v", shard2)
	}
}

func TestQuarantineExpectedFullTupleAndZeroMutationOnMismatch(t *testing.T) {
	ctx := context.Background()
	repo, mr := newAuthRepo(t)
	const pod = "pandora-hub-global-1"
	activateReady(t, repo, pod, time.Now().UnixMilli(), 3)
	expected := CredentialIdentity{
		Gen: 7, JTI: "j7", InstanceUID: "uid-A", ProtocolEpoch: 1,
		TokenSHA256: "sha-j7", Kid: "kid-1", WriterEpoch: testWriterEpoch,
	}
	wrong := expected
	wrong.JTI = "stale"
	if result, err := repo.QuarantineExpected(ctx, pod, wrong, activateAuthTTL, testTTL); err != nil || result.AuthQuarantined {
		t.Fatalf("stale quarantine result=%+v err=%v", result, err)
	}
	before, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	if before.GetState() != "ready" || before.GetPlayerCount() != 3 {
		t.Fatalf("mismatched quarantine mutated shard: %+v", before)
	}
	// auth tuple 正确但 shard 已漂到同名重建实例时也必须零修改。
	drifted := proto.Clone(before).(*hubv1.HubShardStorageRecord)
	drifted.GameserverUid = "uid-rebuilt"
	driftedRaw, err := proto.Marshal(drifted)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.rdb.Set(ctx, shardKey(pod), driftedRaw, testTTL).Err(); err != nil {
		t.Fatal(err)
	}
	authBefore, err := repo.rdb.Get(ctx, authKey(pod)).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	shardBefore, err := repo.rdb.Get(ctx, shardKey(pod)).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if result, err := repo.QuarantineExpected(ctx, pod, expected, activateAuthTTL, testTTL); err != nil ||
		!result.AuthQuarantined || result.ProjectionDrained {
		t.Fatalf("drifted projection quarantine result=%+v err=%v", result, err)
	}
	authAfter, _ := repo.rdb.Get(ctx, authKey(pod)).Bytes()
	shardAfter, _ := repo.rdb.Get(ctx, shardKey(pod)).Bytes()
	if string(authBefore) == string(authAfter) || string(shardBefore) != string(shardAfter) {
		t.Fatal("drifted projection must quarantine auth without changing shard")
	}
	originalRaw, err := proto.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.rdb.Set(ctx, shardKey(pod), originalRaw, testTTL).Err(); err != nil {
		t.Fatal(err)
	}
	if result, err := repo.QuarantineExpected(ctx, pod, expected, activateAuthTTL, testTTL); err != nil ||
		!result.AuthQuarantined || !result.ProjectionDrained {
		t.Fatalf("quarantine result=%+v err=%v", result, err)
	}
	authRecord, _, _ := repo.GetAuth(ctx, pod)
	shard, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	if authRecord.GetPhase() != hubv1.HubAuthPhase_HUB_AUTH_PHASE_QUARANTINED ||
		authRecord.GetPending() != nil || shard.GetState() != "draining" || shard.GetDrainingSinceMs() == 0 {
		t.Fatalf("auth=%+v shard=%+v", authRecord, shard)
	}
	if out, err := repo.CheckRoutable(ctx, pod, time.Now().UnixMilli(), time.Minute.Milliseconds()); err != nil || out.OK {
		t.Fatalf("quarantined shard routable=%+v err=%v", out, err)
	}
	if mr.TTL(authKey(pod)) != 0 {
		t.Fatalf("quarantine auth tombstone must be persistent, ttl=%s", mr.TTL(authKey(pod)))
	}
	tombstoneBefore, _ := repo.rdb.Get(ctx, authKey(pod)).Bytes()
	for _, uid := range []string{"uid-A", "uid-new"} {
		if _, err := repo.InitAuth(ctx, pod, uid, activateAuthTTL); errcode.As(err) != errcode.ErrUnauthorized {
			t.Fatalf("quarantine tombstone was reinitialized for uid=%s: %v", uid, err)
		}
	}
	tombstoneAfter, _ := repo.rdb.Get(ctx, authKey(pod)).Bytes()
	if !bytes.Equal(tombstoneAfter, tombstoneBefore) || mr.TTL(authKey(pod)) != 0 {
		t.Fatal("rejected quarantine reinit mutated tombstone bytes or persistence")
	}
}

func TestReleaseAssignmentSeatSurvivesCredentialRotationButRejectsUIDDrift(t *testing.T) {
	ctx := context.Background()
	repo, mr := newAuthRepo(t)
	const pod = "pandora-hub-global-1"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 3)

	rotated := credFor(pod, "uid-A", 1, 8, "j8")
	if _, err := repo.StagePending(ctx, pod, rotated, activateAuthTTL); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkDelivered(ctx, pod, rotated, "rv-8", activateAuthTTL); err != nil {
		t.Fatal(err)
	}
	id8 := CredentialIdentity{
		Gen: 8, JTI: "j8", InstanceUID: "uid-A", ProtocolEpoch: 1,
		TokenSHA256: "sha-j8", Kid: "kid-1", WriterEpoch: testWriterEpoch,
	}
	if _, err := repo.ActivateHeartbeat(ctx, pod, id8, activateInput(3, "ready", now+1000)); err != nil {
		t.Fatal(err)
	}
	stable := firstConnectedAssignmentIdentity(t, repo, pod)

	bad := stable
	bad.InstanceUID = "uid-rebuilt"
	before, _ := repo.rdb.Get(ctx, shardKey(pod)).Bytes()
	ttlBefore := mr.TTL(shardKey(pod))
	if released, err := repo.ReleaseAssignmentSeat(ctx, pod, bad, testTTL); err != nil || released {
		t.Fatalf("wrong UID stable release=%v err=%v", released, err)
	}
	afterBad, _ := repo.rdb.Get(ctx, shardKey(pod)).Bytes()
	if !bytes.Equal(afterBad, before) || mr.TTL(shardKey(pod)) != ttlBefore {
		t.Fatal("wrong UID stable release mutated shard bytes or TTL")
	}

	snapshot, err := repo.InspectAssignmentSeat(ctx, pod, stable)
	if err != nil || !snapshot.Connected || snapshot.AdmissionID == "" || snapshot.AdmissionSeq == 0 {
		t.Fatalf("connected owner inspection=%+v err=%v", snapshot, err)
	}
	if released, err := repo.ReleaseAssignmentSeat(ctx, pod, stable, testTTL); err != nil || released {
		t.Fatalf("live session must require physical departure, release=%v err=%v", released, err)
	}
	departure, err := repo.AcknowledgeDeparture(ctx, pod, id8, ReservationIdentity{
		PlayerID: stable.PlayerID, AssignmentID: stable.AssignmentID, InstanceUID: stable.InstanceUID,
		ProtocolEpoch: stable.ProtocolEpoch, WriterEpoch: stable.WriterEpoch,
	}, snapshot.AdmissionID, snapshot.AdmissionSeq, now+2000, testTTL)
	if err != nil || !departure.Departed {
		t.Fatalf("exact physical departure=%+v err=%v", departure, err)
	}
	shard, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	if shard.GetPlayerCount() != 2 || shard.GetLastVerifiedGen() != 8 || shard.GetLastVerifiedJti() != "j8" {
		t.Fatalf("stable release did not preserve current projection: %+v", shard)
	}
}

func TestActivate_ShardAbsentDoesNotPromote(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-global-1"
	now := time.Now().UnixMilli()

	if _, err := repo.InitAuth(ctx, pod, "uid-A", activateAuthTTL); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := repo.StagePending(ctx, pod, credFor(pod, "uid-A", 1, 7, "j7"), activateAuthTTL); err != nil {
		t.Fatalf("stage: %v", err)
	}
	// 不 seed 分片 → 分片缺失。
	idOK := CredentialIdentity{Gen: 7, JTI: "j7", InstanceUID: "uid-A", ProtocolEpoch: 1, TokenSHA256: "sha-j7", Kid: "kid-1", WriterEpoch: testWriterEpoch}
	res, err := repo.ActivateHeartbeat(ctx, pod, idOK, activateInput(3, "", now))
	if err != nil {
		t.Fatalf("activate shard-absent: %v", err)
	}
	if res.ShardFound || res.Accepted {
		t.Fatalf("shard absent → ShardFound=false & not accepted, got %+v", res)
	}
	// 关键:未 promote(pending 仍在,phase 未变 ACTIVE)——保证 promote 与 ready 恒同事务。
	auth, _, _ := repo.GetAuth(ctx, pod)
	if auth.Pending == nil || auth.Pending.Gen != 7 || auth.Active != nil {
		t.Fatalf("shard absent must NOT promote (pending kept, active nil), got %+v", auth)
	}
}

func TestActivate_QuarantineLocked(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-global-1"
	now := time.Now().UnixMilli()

	if _, err := repo.InitAuth(ctx, pod, "uid-A", activateAuthTTL); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := repo.StagePending(ctx, pod, credFor(pod, "uid-A", 1, 7, "j7"), activateAuthTTL); err != nil {
		t.Fatalf("stage: %v", err)
	}
	seedWarmingShard(t, repo, pod, now)
	// 把授权相位改成 QUARANTINED(紧急吊销)。
	quarantine(t, repo, pod)

	idOK := CredentialIdentity{Gen: 7, JTI: "j7", InstanceUID: "uid-A", ProtocolEpoch: 1, TokenSHA256: "sha-j7", Kid: "kid-1", WriterEpoch: testWriterEpoch}
	if _, err := repo.ActivateHeartbeat(ctx, pod, idOK, activateInput(3, "", now)); errcode.As(err) != errcode.ErrUnauthorized {
		t.Fatalf("quarantined phase must reject activate (CE9-iii), got %v", err)
	}
	// 分片仍 warming(未被翻 ready)。
	shard, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	if shard.State != "warming" {
		t.Fatalf("quarantine must not flip shard ready, got state=%s", shard.State)
	}
}

func TestActivate_NoAuthRecord(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	id := CredentialIdentity{Gen: 1, JTI: "j1", InstanceUID: "uid-A", ProtocolEpoch: 1, TokenSHA256: "sha-j1", Kid: "kid-1", WriterEpoch: testWriterEpoch}
	if _, err := repo.ActivateHeartbeat(ctx, "no-such-pod", id, activateInput(1, "", 0)); errcode.As(err) != errcode.ErrUnauthorized {
		t.Fatalf("activate without auth record must be rejected, got %v", err)
	}
}

func TestReserveRoutableSeat_Gates(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-global-1"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	now = time.Now().UnixMilli() // 权威心跳使用服务端接收时间，检查时钟必须在激活之后取

	maxAge := int64(30_000)

	reservation := ReservationIdentity{
		PlayerID: 101, AssignmentID: uuid.NewString(), InstanceUID: "uid-A",
		ProtocolEpoch: 1, WriterEpoch: testWriterEpoch,
		ExpiresAtMs: now + 60_000, AssignmentExpiresAtMs: now + 120_000,
	}
	// 正常可路由 → 创建逐 assignment reservation，投影 occupancy+1。
	r1, err := repo.ReserveAssignment(ctx, pod, reservation, now, maxAge, testTTL)
	if err != nil || !r1.OK || r1.PlayerCount != 1 || r1.ActiveGen != 7 || r1.InstanceUID != "uid-A" {
		t.Fatalf("routable reserve must seat++ & return tuple, got %+v err=%v", r1, err)
	}
	// 心跳陈旧(now 远超 last_hb+maxAge)→ 不可路由。
	staleReservation := reservation
	staleReservation.AssignmentID = uuid.NewString()
	staleReservation.PlayerID++
	r2, err := repo.ReserveAssignment(ctx, pod, staleReservation, now+maxAge+1, maxAge, testTTL)
	if err != nil {
		t.Fatalf("reserve stale-hb: %v", err)
	}
	if r2.OK || r2.Reason != "heartbeat-stale" {
		t.Fatalf("stale heartbeat must block routable, got %+v", r2)
	}

	// last_verified 被改坏(实例漂移 / 未被当前 active 投影)→ 不可路由。
	bumpShard(t, repo, pod, func(s *hubv1.HubShardStorageRecord) { s.LastVerifiedGen = 999 })
	r3, _ := repo.ReserveAssignment(ctx, pod, staleReservation, now, maxAge, testTTL)
	if r3.OK || r3.Reason != "shard-not-verified-by-active" {
		t.Fatalf("shard not verified by active must block, got %+v", r3)
	}
	// 修回并把 state 改 draining → 不可路由。
	bumpShard(t, repo, pod, func(s *hubv1.HubShardStorageRecord) { s.LastVerifiedGen = 7; s.State = "draining" })
	r4, _ := repo.ReserveAssignment(ctx, pod, staleReservation, now, maxAge, testTTL)
	if r4.OK || r4.Reason != "shard-not-ready" {
		t.Fatalf("draining shard must block, got %+v", r4)
	}
	// 修回 ready，并把容量缩到现有一条 reservation → 新 assignment 被 shard-full 拒绝。
	bumpShard(t, repo, pod, func(s *hubv1.HubShardStorageRecord) {
		s.State = "ready"
		s.Capacity = 1
		s.ReportedMaxPlayers = 1
	})
	r5, _ := repo.ReserveAssignment(ctx, pod, staleReservation, now, maxAge, testTTL)
	if r5.OK || r5.Reason != "shard-full" {
		t.Fatalf("full shard must block, got %+v", r5)
	}
}

func TestCheckRoutable_ReadOnlyNoSeat(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-global-1"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	now = time.Now().UnixMilli()

	before, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	info, err := repo.CheckRoutable(ctx, pod, now, 30_000)
	if err != nil || !info.OK || info.ActiveGen != 7 {
		t.Fatalf("check routable must pass & return active tuple, got %+v err=%v", info, err)
	}
	after, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	if after.PlayerCount != before.PlayerCount {
		t.Fatalf("CheckRoutable must NOT seat++, before=%d after=%d", before.PlayerCount, after.PlayerCount)
	}
}

func TestV2RejectsLowAndFutureWriterAcrossAllHubAuthorityPaths(t *testing.T) {
	for _, tc := range []struct {
		name          string
		requiredEpoch uint32
		writerEpoch   uint32
	}{
		{name: "low-writer-1-required-1", requiredEpoch: 1, writerEpoch: 1},
		{name: "future-writer-3-required-2", requiredEpoch: 2, writerEpoch: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo, mr := newAuthRepo(t)
			pod := "pandora-hub-" + tc.name
			activateReady(t, repo, pod, time.Now().UnixMilli(), 4)

			authRecord, found, err := repo.GetAuth(ctx, pod)
			if err != nil || !found {
				t.Fatalf("get auth: found=%v err=%v", found, err)
			}
			shard, found, err := shardRepoFor(repo).GetShard(ctx, pod)
			if err != nil || !found {
				t.Fatalf("get shard: found=%v err=%v", found, err)
			}
			authRecord.RequiredWriterEpoch = tc.requiredEpoch
			authRecord.Active.WriterEpoch = tc.writerEpoch
			authRecord.Pending = credFor(pod, "uid-A", 1, 8, "j8")
			authRecord.Pending.WriterEpoch = tc.writerEpoch
			authRecord.HighWaterGen = 8
			authRecord.Phase = hubv1.HubAuthPhase_HUB_AUTH_PHASE_ROTATING
			authRecord.DeliveredRv = "rv-8"
			shard.LastVerifiedWriterEpoch = tc.writerEpoch
			authRaw, err := proto.Marshal(authRecord)
			if err != nil {
				t.Fatal(err)
			}
			shardRaw, err := proto.Marshal(shard)
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.rdb.Set(ctx, authKey(pod), authRaw, activateAuthTTL).Err(); err != nil {
				t.Fatal(err)
			}
			if err := repo.rdb.Set(ctx, shardKey(pod), shardRaw, testTTL).Err(); err != nil {
				t.Fatal(err)
			}
			authTTLBefore, shardTTLBefore := mr.TTL(authKey(pod)), mr.TTL(shardKey(pod))

			candidate := credFor(pod, "uid-A", 1, 9, "j9")
			candidate.WriterEpoch = tc.writerEpoch
			if _, err := repo.StagePending(ctx, pod, candidate, activateAuthTTL); err == nil {
				t.Fatal("V2 StagePending accepted non-v2 writer")
			}
			if err := repo.MarkDelivered(ctx, pod, authRecord.Pending, "rv-bad", activateAuthTTL); err == nil {
				t.Fatal("V2 MarkDelivered accepted non-v2 writer")
			}
			id := CredentialIdentity{
				Gen: authRecord.Active.Gen, JTI: authRecord.Active.Jti,
				InstanceUID: "uid-A", ProtocolEpoch: 1,
				TokenSHA256: authRecord.Active.TokenSha256, Kid: authRecord.Active.Kid,
				WriterEpoch: tc.writerEpoch,
			}
			if _, err := repo.ActivateHeartbeat(ctx, pod, id, activateInput(99, "running", 0)); errcode.As(err) != errcode.ErrUnauthorized {
				t.Fatalf("V2 ActivateHeartbeat code=%v err=%v", errcode.As(err), err)
			}
			nowMs := time.Now().UnixMilli()
			reservation := ReservationIdentity{
				PlayerID: 101, AssignmentID: uuid.NewString(), InstanceUID: "uid-A",
				ProtocolEpoch: 1, WriterEpoch: testWriterEpoch,
				ExpiresAtMs: nowMs + 60_000, AssignmentExpiresAtMs: nowMs + 120_000,
			}
			if out, err := repo.ReserveAssignment(ctx, pod, reservation, nowMs, 30_000, testTTL); err != nil || out.OK {
				t.Fatalf("V2 ReserveAssignment out=%+v err=%v", out, err)
			}
			if out, err := repo.CheckRoutable(ctx, pod, nowMs, 30_000); err != nil || out.OK {
				t.Fatalf("V2 CheckRoutable out=%+v err=%v", out, err)
			}
			if released, err := repo.ReleaseAssignmentSeat(ctx, pod, AssignmentInstanceIdentity{
				PlayerID: reservation.PlayerID, AssignmentID: reservation.AssignmentID,
				InstanceUID: reservation.InstanceUID, ProtocolEpoch: reservation.ProtocolEpoch,
				WriterEpoch: reservation.WriterEpoch,
			}, testTTL); err != nil || released {
				t.Fatalf("V2 ReleaseAssignmentSeat released=%v err=%v", released, err)
			}
			if result, err := repo.QuarantineExpected(ctx, pod, id, activateAuthTTL, testTTL); err == nil || result.AuthQuarantined || result.ProjectionDrained {
				t.Fatalf("V2 QuarantineExpected result=%+v err=%v", result, err)
			}

			authAfter, err := repo.rdb.Get(ctx, authKey(pod)).Bytes()
			if err != nil {
				t.Fatal(err)
			}
			shardAfter, err := repo.rdb.Get(ctx, shardKey(pod)).Bytes()
			if err != nil {
				t.Fatal(err)
			}
			if string(authAfter) != string(authRaw) || string(shardAfter) != string(shardRaw) {
				t.Fatal("rejected non-v2 authority operation mutated auth/shard")
			}
			if mr.TTL(authKey(pod)) != authTTLBefore || mr.TTL(shardKey(pod)) != shardTTLBefore {
				t.Fatal("rejected non-v2 authority operation refreshed TTL")
			}
		})
	}
}

// TestActivate_ConcurrentPromoteOnce:多个并发心跳(同一 pending 凭据),barrier 同时开跑,
// WATCH 冲突下必须恰好一个 Accepted(promote 一次),其余幂等;最终 active gen 一致,无双 promote。
func TestActivate_ConcurrentPromoteOnce(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-global-1"
	now := time.Now().UnixMilli()

	if _, err := repo.InitAuth(ctx, pod, "uid-A", activateAuthTTL); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := repo.StagePending(ctx, pod, credFor(pod, "uid-A", 1, 7, "j7"), activateAuthTTL); err != nil {
		t.Fatalf("stage: %v", err)
	}
	seedWarmingShard(t, repo, pod, now)

	idOK := CredentialIdentity{Gen: 7, JTI: "j7", InstanceUID: "uid-A", ProtocolEpoch: 1, TokenSHA256: "sha-j7", Kid: "kid-1", WriterEpoch: testWriterEpoch}
	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	acceptedCount := 0
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start // barrier:全部就位再一起冲(逼出 WATCH 冲突,不用 sleep)
			res, err := repo.ActivateHeartbeat(ctx, pod, idOK, activateInput(1, "", now))
			if err != nil {
				return
			}
			if res.Accepted {
				mu.Lock()
				acceptedCount++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if acceptedCount != 1 {
		t.Fatalf("promote must happen exactly once under concurrency, accepted=%d", acceptedCount)
	}
	auth, _, _ := repo.GetAuth(ctx, pod)
	if auth.Active == nil || auth.Active.Gen != 7 || auth.Pending != nil {
		t.Fatalf("final auth must be single active gen7, got %+v", auth)
	}
}

// —— 测试内共享辅助 ——

// activateReady 造出「active gen7 + 分片 ready + last_verified 已投影」的稳定状态,seat 初始 initialSeat。
func activateReady(t *testing.T, repo *RedisHubAuthRepo, pod string, nowMs int64, initialSeat int32) {
	t.Helper()
	ctx := context.Background()
	if _, err := repo.InitAuth(ctx, pod, "uid-A", activateAuthTTL); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := repo.StagePending(ctx, pod, credFor(pod, "uid-A", 1, 7, "j7"), activateAuthTTL); err != nil {
		t.Fatalf("stage: %v", err)
	}
	seedWarmingShard(t, repo, pod, nowMs)
	for i := int32(0); i < initialSeat; i++ {
		assignmentID := uuid.NewString()
		rec := &hubv1.HubConnectedOwnershipStorageRecord{
			PlayerId: uint64(i + 1), AssignmentId: assignmentID, AdmissionId: uuid.NewString(),
			HubPodName: pod, HubInstanceUid: "uid-A", AuthEpoch: 1, AuthWriterEpoch: testWriterEpoch,
			AdmittedAtMs: nowMs, LastSeenMs: nowMs, AdmissionSeq: uint64(i + 1),
		}
		payload, err := proto.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.rdb.HSet(ctx, sessionsKey(pod), assignmentID, payload).Err(); err != nil {
			t.Fatal(err)
		}
	}
	idOK := CredentialIdentity{Gen: 7, JTI: "j7", InstanceUID: "uid-A", ProtocolEpoch: 1, TokenSHA256: "sha-j7", Kid: "kid-1", WriterEpoch: testWriterEpoch}
	if _, err := repo.ActivateHeartbeat(ctx, pod, idOK, activateInput(initialSeat, "", nowMs)); err != nil {
		t.Fatalf("activate ready: %v", err)
	}
}

func firstConnectedAssignmentIdentity(t *testing.T, repo *RedisHubAuthRepo, pod string) AssignmentInstanceIdentity {
	t.Helper()
	rawByAssignment, err := repo.rdb.HGetAll(context.Background(), sessionsKey(pod)).Result()
	if err != nil {
		t.Fatal(err)
	}
	for assignmentID, raw := range rawByAssignment {
		rec := &hubv1.HubConnectedOwnershipStorageRecord{}
		if err := proto.Unmarshal([]byte(raw), rec); err != nil {
			t.Fatal(err)
		}
		return AssignmentInstanceIdentity{
			PlayerID: rec.GetPlayerId(), AssignmentID: assignmentID,
			InstanceUID: rec.GetHubInstanceUid(), ProtocolEpoch: rec.GetAuthEpoch(),
			WriterEpoch: rec.GetAuthWriterEpoch(),
		}
	}
	t.Fatal("expected at least one connected ownership record")
	return AssignmentInstanceIdentity{}
}

// bumpShard 就地改分片镜像字段(测试用,绕过业务门直接改存储态以构造边界)。
func bumpShard(t *testing.T, repo *RedisHubAuthRepo, pod string, fn func(*hubv1.HubShardStorageRecord)) {
	t.Helper()
	if err := shardRepoFor(repo).UpdateShardWithLock(context.Background(), pod, 8, func(s *hubv1.HubShardStorageRecord) error {
		fn(s)
		return nil
	}, testTTL); err != nil {
		t.Fatalf("bump shard: %v", err)
	}
}

// quarantine 把授权相位置 QUARANTINED(紧急吊销),用底层 rdb 直接改存储态。
func quarantine(t *testing.T, repo *RedisHubAuthRepo, pod string) {
	t.Helper()
	ctx := context.Background()
	rec, ok, err := repo.GetAuth(ctx, pod)
	if err != nil || !ok {
		t.Fatalf("quarantine get: ok=%v err=%v", ok, err)
	}
	rec.Phase = hubv1.HubAuthPhase_HUB_AUTH_PHASE_QUARANTINED
	payload, merr := proto.Marshal(rec)
	if merr != nil {
		t.Fatalf("quarantine marshal: %v", merr)
	}
	if serr := repo.rdb.Set(ctx, authKey(pod), payload, activateAuthTTL).Err(); serr != nil {
		t.Fatalf("quarantine set: %v", serr)
	}
}
