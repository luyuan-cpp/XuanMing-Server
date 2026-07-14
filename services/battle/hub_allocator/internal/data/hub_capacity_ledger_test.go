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

func capacityTestCredential() CredentialIdentity {
	return CredentialIdentity{
		Gen: 7, JTI: "j7", InstanceUID: "uid-A", ProtocolEpoch: 1,
		TokenSHA256: "sha-j7", Kid: "kid-1", WriterEpoch: testWriterEpoch,
	}
}

func capacityTestReservation(playerID uint64, assignmentID string, nowMs int64) ReservationIdentity {
	return ReservationIdentity{
		PlayerID: playerID, AssignmentID: assignmentID, InstanceUID: "uid-A",
		ProtocolEpoch: 1, WriterEpoch: testWriterEpoch,
		ExpiresAtMs:           nowMs + (2 * time.Hour).Milliseconds(),
		AssignmentExpiresAtMs: nowMs + (3 * time.Hour).Milliseconds(),
	}
}

func mustReserveCapacity(t *testing.T, repo *RedisHubAuthRepo, pod string,
	reservation ReservationIdentity, nowMs int64) ReserveResult {
	t.Helper()
	result, err := repo.ReserveAssignment(context.Background(), pod, reservation, nowMs, 30_000, testTTL)
	if err != nil || !result.OK {
		t.Fatalf("reserve result=%+v err=%v", result, err)
	}
	return result
}

func TestCapacityLedgerReservationSurvivesZeroHeartbeatsUntilAbsoluteExpiry(t *testing.T) {
	ctx := context.Background()
	repo, mr := newAuthRepo(t)
	const pod = "pandora-hub-capacity-reservation"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	now = time.Now().UnixMilli() + 1
	reservation := capacityTestReservation(101, uuid.NewString(), now)
	mustReserveCapacity(t, repo, pod, reservation, now)

	// reservation HASH/ZSET retention 取最晚单项绝对到期+guard，不能被较短 shardTTL 截断。
	if ttl := mr.TTL(reservationsKey(pod)); ttl <= testTTL {
		t.Fatalf("reservation key ttl=%s must outlive shard ttl=%s", ttl, testTTL)
	}
	for i := 0; i < 3; i++ {
		if _, err := repo.ActivateHeartbeat(ctx, pod, capacityTestCredential(),
			activateInput(0, "ready", now+int64(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	shard, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	if shard.GetPlayerCount() != 1 || shard.GetReservedCount() != 1 ||
		shard.GetConnectedOwnershipCount() != 0 || shard.GetReportedConnectedCount() != 0 {
		t.Fatalf("zero-player heartbeats deleted reservation or corrupted projection: %+v", shard)
	}
	if count, _ := repo.rdb.HLen(ctx, reservationsKey(pod)).Result(); count != 1 {
		t.Fatalf("reservation disappeared after heartbeat: count=%d", count)
	}

	// 直接把单项绝对期限推进到过去，下一次合法 heartbeat 才可机械回收。
	raw, err := repo.rdb.HGet(ctx, reservationsKey(pod), reservation.AssignmentID).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	record := &hubv1.HubReservationStorageRecord{}
	if err := proto.Unmarshal(raw, record); err != nil {
		t.Fatal(err)
	}
	record.ExpiresAtMs = time.Now().UnixMilli() - 1
	raw, _ = proto.Marshal(record)
	if err := repo.rdb.HSet(ctx, reservationsKey(pod), reservation.AssignmentID, raw).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ActivateHeartbeat(ctx, pod, capacityTestCredential(), activateInput(0, "ready", now+10)); err != nil {
		t.Fatal(err)
	}
	shard, _, _ = shardRepoFor(repo).GetShard(ctx, pod)
	if shard.GetPlayerCount() != 0 || shard.GetReservedCount() != 0 ||
		shard.GetConnectedOwnershipCount() != 0 {
		t.Fatalf("expired reservation was not reclaimed: %+v", shard)
	}
}

func TestAdmissionSequenceResponseLossDelayedOldAckAndLogout(t *testing.T) {
	ctx := context.Background()
	repo, mr := newAuthRepo(t)
	const pod = "pandora-hub-admission-sequence"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	now = time.Now().UnixMilli() + 1
	reservation := capacityTestReservation(101, uuid.NewString(), now)
	mustReserveCapacity(t, repo, pod, reservation, now)
	credential := capacityTestCredential()
	oldAdmissionID, newAdmissionID := uuid.NewString(), uuid.NewString()

	first, err := repo.AcknowledgeAdmission(ctx, pod, credential, reservation, oldAdmissionID, 10, now+1, testTTL)
	if err != nil || !first.Admitted || first.AlreadyAdmitted || first.CapacityOccupancy != 1 {
		t.Fatalf("first admission=%+v err=%v", first, err)
	}
	retry, err := repo.AcknowledgeAdmission(ctx, pod, credential, reservation, oldAdmissionID, 10, now+2, testTTL)
	if err != nil || !retry.Admitted || !retry.AlreadyAdmitted || retry.CapacityOccupancy != 1 {
		t.Fatalf("response-loss retry=%+v err=%v", retry, err)
	}
	replacement, err := repo.AcknowledgeAdmission(ctx, pod, credential, reservation, newAdmissionID, 20, now+3, testTTL)
	if err != nil || !replacement.Admitted || replacement.CapacityOccupancy != 1 {
		t.Fatalf("new connection replacement=%+v err=%v", replacement, err)
	}

	sessionBefore, _ := repo.rdb.HGet(ctx, sessionsKey(pod), reservation.AssignmentID).Bytes()
	shardBefore, _ := repo.rdb.Get(ctx, shardKey(pod)).Bytes()
	shardTTLBefore := mr.TTL(shardKey(pod))
	if delayed, err := repo.AcknowledgeAdmission(ctx, pod, credential, reservation,
		oldAdmissionID, 10, now+4, testTTL); err != nil || !delayed.Conflict || delayed.Admitted {
		t.Fatalf("delayed old admission=%+v err=%v", delayed, err)
	}
	if got, _ := repo.rdb.HGet(ctx, sessionsKey(pod), reservation.AssignmentID).Bytes(); !bytes.Equal(got, sessionBefore) {
		t.Fatal("delayed old admission mutated the new owner")
	}
	if oldLogout, err := repo.AcknowledgeDeparture(ctx, pod, credential, reservation,
		oldAdmissionID, 10, now+5, testTTL); err != nil || !oldLogout.Conflict || oldLogout.Departed {
		t.Fatalf("old logout=%+v err=%v", oldLogout, err)
	}
	if got, _ := repo.rdb.HGet(ctx, sessionsKey(pod), reservation.AssignmentID).Bytes(); !bytes.Equal(got, sessionBefore) {
		t.Fatal("old logout deleted or rewrote the new owner")
	}
	if got, _ := repo.rdb.Get(ctx, shardKey(pod)).Bytes(); !bytes.Equal(got, shardBefore) ||
		mr.TTL(shardKey(pod)) != shardTTLBefore {
		t.Fatal("conflict path mutated shard bytes or refreshed ttl")
	}

	departed, err := repo.AcknowledgeDeparture(ctx, pod, credential, reservation,
		newAdmissionID, 20, now+6, testTTL)
	if err != nil || !departed.Departed || departed.Conflict || departed.CapacityOccupancy != 0 {
		t.Fatalf("new owner departure=%+v err=%v", departed, err)
	}
	idempotent, err := repo.AcknowledgeDeparture(ctx, pod, credential, reservation,
		newAdmissionID, 20, now+7, testTTL)
	if err != nil || !idempotent.Departed || !idempotent.AlreadyDeparted || idempotent.Conflict {
		t.Fatalf("departure response-loss retry=%+v err=%v", idempotent, err)
	}
}

func TestReportedPlayersAreAuditOnlyWithUntrackedSafetyGate(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-reported-audit"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	credential := capacityTestCredential()

	if _, err := repo.ActivateHeartbeat(ctx, pod, credential, activateInput(1, "ready", now+1)); err != nil {
		t.Fatal(err)
	}
	if route, err := repo.CheckRoutable(ctx, pod, time.Now().UnixMilli(), 30_000); err != nil ||
		route.OK || route.Reason != "untracked-connected-players" {
		t.Fatalf("reported=1 ledger=0 must not route: result=%+v err=%v", route, err)
	}
	shard, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	if shard.GetPlayerCount() != 0 || shard.GetConnectedOwnershipCount() != 0 ||
		shard.GetReportedConnectedCount() != 1 {
		t.Fatalf("reported player was incorrectly backfilled into authority: %+v", shard)
	}

	// 下一次实报回到 0 后可分配；接纳后 heartbeat 漏报 0 也不得删除 connected owner。
	if _, err := repo.ActivateHeartbeat(ctx, pod, credential, activateInput(0, "ready", now+2)); err != nil {
		t.Fatal(err)
	}
	reservation := capacityTestReservation(101, uuid.NewString(), time.Now().UnixMilli())
	mustReserveCapacity(t, repo, pod, reservation, time.Now().UnixMilli())
	if admission, err := repo.AcknowledgeAdmission(ctx, pod, credential, reservation,
		uuid.NewString(), 1, time.Now().UnixMilli(), testTTL); err != nil || !admission.Admitted {
		t.Fatalf("admission=%+v err=%v", admission, err)
	}
	if _, err := repo.ActivateHeartbeat(ctx, pod, credential, activateInput(0, "ready", now+3)); err != nil {
		t.Fatal(err)
	}
	shard, _, _ = shardRepoFor(repo).GetShard(ctx, pod)
	if shard.GetPlayerCount() != 1 || shard.GetConnectedOwnershipCount() != 1 ||
		shard.GetReportedConnectedCount() != 0 {
		t.Fatalf("heartbeat omission deleted connected owner: %+v", shard)
	}
	if route, err := repo.CheckRoutable(ctx, pod, time.Now().UnixMilli(), 30_000); err != nil || !route.OK {
		t.Fatalf("reported omission below owned count must remain routable: result=%+v err=%v", route, err)
	}
}

func TestHeartbeatMaxPlayersAndPlayerListRejectionsHaveZeroSideEffects(t *testing.T) {
	ctx := context.Background()
	repo, mr := newAuthRepo(t)
	const pod = "pandora-hub-heartbeat-zero-side-effects"
	now := time.Now().UnixMilli()
	if _, err := repo.InitAuth(ctx, pod, "uid-A", activateAuthTTL); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.StagePending(ctx, pod, credFor(pod, "uid-A", 1, 7, "j7"), activateAuthTTL); err != nil {
		t.Fatal(err)
	}
	seedWarmingShard(t, repo, pod, now)
	credential := capacityTestCredential()
	authBefore, _ := repo.rdb.Get(ctx, authKey(pod)).Bytes()
	shardBefore, _ := repo.rdb.Get(ctx, shardKey(pod)).Bytes()
	authTTLBefore, shardTTLBefore := mr.TTL(authKey(pod)), mr.TTL(shardKey(pod))

	badInputs := []ActivateHeartbeatInput{
		{PlayerCount: 0, MaxPlayers: 16, State: "ready", AuthTTL: activateAuthTTL, ShardTTL: testTTL},
		{PlayerCount: 1, PlayerIDs: nil, MaxPlayers: 500, State: "ready", AuthTTL: activateAuthTTL, ShardTTL: testTTL},
		{PlayerCount: 2, PlayerIDs: []uint64{1, 1}, MaxPlayers: 500, State: "ready", AuthTTL: activateAuthTTL, ShardTTL: testTTL},
		{PlayerCount: 1, PlayerIDs: []uint64{0}, MaxPlayers: 500, State: "ready", AuthTTL: activateAuthTTL, ShardTTL: testTTL},
	}
	for index, input := range badInputs {
		if _, err := repo.ActivateHeartbeat(ctx, pod, credential, input); err == nil {
			t.Fatalf("bad heartbeat %d unexpectedly accepted", index)
		}
		authAfter, _ := repo.rdb.Get(ctx, authKey(pod)).Bytes()
		shardAfter, _ := repo.rdb.Get(ctx, shardKey(pod)).Bytes()
		if !bytes.Equal(authAfter, authBefore) || !bytes.Equal(shardAfter, shardBefore) ||
			mr.TTL(authKey(pod)) != authTTLBefore || mr.TTL(shardKey(pod)) != shardTTLBefore {
			t.Fatalf("bad heartbeat %d mutated auth/shard bytes or ttl", index)
		}
	}
	auth, _, _ := repo.GetAuth(ctx, pod)
	shard, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	if auth.GetActive() != nil || auth.GetPending() == nil || shard.GetState() != "warming" ||
		shard.GetReportedMaxPlayers() != 0 {
		t.Fatalf("rejected heartbeat promoted or readied shard: auth=%+v shard=%+v", auth, shard)
	}
}

func TestConcurrentReservationsAndAssignmentIdentityConflict(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-concurrent-capacity"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	now = time.Now().UnixMilli() + 1
	bumpShard(t, repo, pod, func(shard *hubv1.HubShardStorageRecord) {
		shard.Capacity = 1
		shard.ReportedMaxPlayers = 1
	})
	reservations := []ReservationIdentity{
		capacityTestReservation(101, uuid.NewString(), now),
		capacityTestReservation(102, uuid.NewString(), now),
	}
	type outcome struct {
		index  int
		result ReserveResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, len(reservations))
	var wg sync.WaitGroup
	for index := range reservations {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			result, err := repo.ReserveAssignment(ctx, pod, reservations[index], now, 30_000, testTTL)
			outcomes <- outcome{index: index, result: result, err: err}
		}(index)
	}
	close(start)
	wg.Wait()
	close(outcomes)
	winner := -1
	for result := range outcomes {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.result.OK {
			if winner >= 0 {
				t.Fatal("capacity=1 admitted two concurrent reservations")
			}
			winner = result.index
		} else if result.result.Reason != "shard-full" {
			t.Fatalf("loser reason=%q", result.result.Reason)
		}
	}
	if winner < 0 {
		t.Fatal("no concurrent reservation won")
	}
	if duplicate, err := repo.ReserveAssignment(ctx, pod, reservations[winner], now, 30_000, testTTL); err != nil || !duplicate.OK || duplicate.PlayerCount != 1 {
		t.Fatalf("exact reservation retry=%+v err=%v", duplicate, err)
	}
	conflictIdentity := reservations[winner]
	conflictIdentity.PlayerID += 1000
	if conflict, err := repo.ReserveAssignment(ctx, pod, conflictIdentity, now, 30_000, testTTL); err != nil || conflict.OK || conflict.Reason != "assignment-reservation-conflict" {
		t.Fatalf("same assignment/different player conflict=%+v err=%v", conflict, err)
	}
	shard, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	if shard.GetPlayerCount() != 1 || shard.GetReservedCount() != 1 {
		t.Fatalf("concurrent reservation projection=%+v", shard)
	}
}

func TestNewGameServerUIDHeartbeatPrunesOldOwnershipLedger(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-uid-rebuild"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	now = time.Now().UnixMilli() + 1
	reservation := capacityTestReservation(101, uuid.NewString(), now)
	mustReserveCapacity(t, repo, pod, reservation, now)
	if admission, err := repo.AcknowledgeAdmission(ctx, pod, capacityTestCredential(), reservation,
		uuid.NewString(), 1, now+1, testTTL); err != nil || !admission.Admitted {
		t.Fatalf("old uid admission=%+v err=%v", admission, err)
	}

	auth, err := repo.InitAuth(ctx, pod, "uid-B", activateAuthTTL)
	if err != nil || auth.GetProtocolEpoch() != 2 {
		t.Fatalf("uid rebuild auth=%+v err=%v", auth, err)
	}
	newCredential := credFor(pod, "uid-B", 2, 8, "j8")
	if _, err := repo.StagePending(ctx, pod, newCredential, activateAuthTTL); err != nil {
		t.Fatal(err)
	}
	newIdentity := CredentialIdentity{
		Gen: 8, JTI: "j8", InstanceUID: "uid-B", ProtocolEpoch: 2,
		TokenSHA256: "sha-j8", Kid: "kid-1", WriterEpoch: testWriterEpoch,
	}
	if _, err := repo.ActivateHeartbeat(ctx, pod, newIdentity, activateInput(0, "ready", now+2)); err != nil {
		t.Fatal(err)
	}
	shard, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	sessions, _ := repo.rdb.HLen(ctx, sessionsKey(pod)).Result()
	if sessions != 0 || shard.GetPlayerCount() != 0 || shard.GetConnectedOwnershipCount() != 0 ||
		shard.GetGameserverUid() != "uid-B" || shard.GetAuthEpoch() != 2 {
		t.Fatalf("new uid did not atomically prune old ownership: sessions=%d shard=%+v", sessions, shard)
	}
}

func TestExpiredReservationReleaseStillCommitsExactCleanup(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-expired-release"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	now = time.Now().UnixMilli() + 1
	reservation := capacityTestReservation(101, uuid.NewString(), now)
	mustReserveCapacity(t, repo, pod, reservation, now)
	raw, _ := repo.rdb.HGet(ctx, reservationsKey(pod), reservation.AssignmentID).Bytes()
	record := &hubv1.HubReservationStorageRecord{}
	if err := proto.Unmarshal(raw, record); err != nil {
		t.Fatal(err)
	}
	record.ExpiresAtMs = time.Now().UnixMilli() - 1
	raw, _ = proto.Marshal(record)
	if err := repo.rdb.HSet(ctx, reservationsKey(pod), reservation.AssignmentID, raw).Err(); err != nil {
		t.Fatal(err)
	}
	released, err := repo.ReleaseAssignmentSeat(ctx, pod, AssignmentInstanceIdentity{
		PlayerID: reservation.PlayerID, AssignmentID: reservation.AssignmentID,
		InstanceUID: reservation.InstanceUID, ProtocolEpoch: reservation.ProtocolEpoch,
		WriterEpoch: reservation.WriterEpoch,
	}, testTTL)
	if err != nil || !released {
		t.Fatalf("expired exact release=%v err=%v", released, err)
	}
	if exists, _ := repo.rdb.HExists(ctx, reservationsKey(pod), reservation.AssignmentID).Result(); exists {
		t.Fatal("expired reservation remained after exact release")
	}
	shard, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	if shard.GetPlayerCount() != 0 || shard.GetReservedCount() != 0 {
		t.Fatalf("expired release did not repair projection: %+v", shard)
	}
}

func TestMaxPlayersMismatchUsesInvalidStateCode(t *testing.T) {
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-max-code"
	now := time.Now().UnixMilli()
	if _, err := repo.InitAuth(context.Background(), pod, "uid-A", activateAuthTTL); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.StagePending(context.Background(), pod,
		credFor(pod, "uid-A", 1, 7, "j7"), activateAuthTTL); err != nil {
		t.Fatal(err)
	}
	seedWarmingShard(t, repo, pod, now)
	input := activateInput(0, "ready", now)
	input.MaxPlayers = 16
	if _, err := repo.ActivateHeartbeat(context.Background(), pod, capacityTestCredential(), input); errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("max mismatch code=%v err=%v", errcode.As(err), err)
	}
}
