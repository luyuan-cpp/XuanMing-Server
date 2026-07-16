package data

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/pkg/battleabort"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	"google.golang.org/protobuf/proto"
)

const abortOperationID = "550e8400-e29b-41d4-a716-446655440000"

func seedActiveAbortTarget(t *testing.T, f *battleAuthFixture, matchID uint64, playerCount int32) battleabort.Request {
	t.Helper()
	ctx := context.Background()
	const allocationID, pod, uid = "44ae8469-0244-4994-8854-9b37caf0b72e", "pod-abort", "uid-abort"
	seedModelBBattle(t, f, matchID, allocationID, pod, uid)
	_, identity := prepareAndStage(t, f, matchID, allocationID, pod, uid, true)
	input := activateInput()
	input.PlayerCount = playerCount
	input.State = "ready"
	if _, err := f.auth.ActivateHeartbeat(ctx, matchID, identity, input); err != nil {
		t.Fatalf("activate abort target: %v", err)
	}
	return battleabort.Request{MatchID: matchID, OperationID: abortOperationID, Target: placement.Target{
		PodName: pod, InstanceUID: uid, InstanceEpoch: identity.InstanceEpoch,
		AllocationID: allocationID, ReleaseTrack: "stable",
	}}
}

func TestAllocationAbortFenceIsPermanentExactAndIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newBattleAuthFixture(t)
	request := seedActiveAbortTarget(t, f, 8801, 0)

	result, err := f.auth.FenceAllocationAbortExpected(ctx, request)
	if err != nil || result.Released || result.Battle == nil {
		t.Fatalf("fence result=%+v err=%v", result, err)
	}
	if result.Battle.GetState() != BattleStateAllocationAbortPending || result.Battle.GetLastHeartbeatMs() != 0 {
		t.Fatalf("battle not fenced for immediate recovery: %+v", result.Battle)
	}
	for _, key := range []string{battleAuthKey(request.MatchID), battleKey(request.MatchID), battleAllocationAbortKey(request.MatchID)} {
		if ttl := f.rdb.TTL(ctx, key).Val(); ttl != -1 {
			t.Fatalf("%s ttl=%v, want permanent", key, ttl)
		}
	}
	authBefore := rawRedisBytes(t, f.rdb, battleAuthKey(request.MatchID))
	battleBefore := rawRedisBytes(t, f.rdb, battleKey(request.MatchID))
	journalBefore := rawRedisBytes(t, f.rdb, battleAllocationAbortKey(request.MatchID))

	again, err := f.auth.FenceAllocationAbortExpected(ctx, request)
	if err != nil || again.Released || again.Battle == nil {
		t.Fatalf("idempotent fence result=%+v err=%v", again, err)
	}
	mutated := request
	mutated.Target.InstanceUID = "uid-other"
	if _, err := f.auth.FenceAllocationAbortExpected(ctx, mutated); errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("tuple mutation err=%v code=%v", err, errcode.As(err))
	}
	if !bytes.Equal(authBefore, rawRedisBytes(t, f.rdb, battleAuthKey(request.MatchID))) ||
		!bytes.Equal(battleBefore, rawRedisBytes(t, f.rdb, battleKey(request.MatchID))) ||
		!bytes.Equal(journalBefore, rawRedisBytes(t, f.rdb, battleAllocationAbortKey(request.MatchID))) {
		t.Fatal("conflicting retry mutated abort authority")
	}
}

func TestAllocationAbortRejectsNonzeroAdmissionWithoutMutation(t *testing.T) {
	ctx := context.Background()
	f := newBattleAuthFixture(t)
	request := seedActiveAbortTarget(t, f, 8802, 1)
	authBefore := rawRedisBytes(t, f.rdb, battleAuthKey(request.MatchID))
	battleBefore := rawRedisBytes(t, f.rdb, battleKey(request.MatchID))
	if _, err := f.auth.FenceAllocationAbortExpected(ctx, request); errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("admitted target err=%v code=%v", err, errcode.As(err))
	}
	if !bytes.Equal(authBefore, rawRedisBytes(t, f.rdb, battleAuthKey(request.MatchID))) ||
		!bytes.Equal(battleBefore, rawRedisBytes(t, f.rdb, battleKey(request.MatchID))) ||
		f.mr.Exists(battleAllocationAbortKey(request.MatchID)) {
		t.Fatal("rejected admitted target mutated authority")
	}
}

func TestAllocationAbortCompletionKeepsPermanentAckAndBoundsAuthority(t *testing.T) {
	ctx := context.Background()
	f := newBattleAuthFixture(t)
	request := seedActiveAbortTarget(t, f, 8803, 0)
	if _, err := f.auth.FenceAllocationAbortExpected(ctx, request); err != nil {
		t.Fatal(err)
	}
	if completed, err := f.auth.CompleteAllocationAbortExpected(
		ctx, request, time.Minute, 2*time.Minute); completed || errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("completion without proofs=%v err=%v", completed, err)
	}
	if err := f.battle.RecordInstanceTeardown(ctx, request.MatchID, BattleDepartureSource{
		DSPodName: request.Target.PodName, GameServerUID: request.Target.InstanceUID,
		InstanceEpoch: request.Target.InstanceEpoch, AllocationID: request.Target.AllocationID,
		PodUID: "pod-uid-abort",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.battle.RecordAllocationLifecyclePublished(ctx, request.MatchID, request.Target); err != nil {
		t.Fatal(err)
	}
	if completed, err := f.auth.CompleteAllocationAbortExpected(ctx, request, time.Minute, 2*time.Minute); err != nil || !completed {
		t.Fatalf("complete=%v err=%v", completed, err)
	}
	read, released, found, err := f.auth.ReadAllocationAbort(ctx, request.MatchID)
	if err != nil || !found || !released || string(read.Canonical()) != string(request.Canonical()) {
		t.Fatalf("read=%+v released=%v found=%v err=%v", read, released, found, err)
	}
	if ttl := f.rdb.TTL(ctx, battleAllocationAbortKey(request.MatchID)).Val(); ttl != -1 {
		t.Fatalf("journal ttl=%v, want permanent", ttl)
	}
	if ttl := f.rdb.TTL(ctx, battleAuthKey(request.MatchID)).Val(); ttl <= 0 || ttl > time.Minute {
		t.Fatalf("auth ttl=%v", ttl)
	}
	if ttl := f.rdb.TTL(ctx, battleKey(request.MatchID)).Val(); ttl <= 0 || ttl > 2*time.Minute {
		t.Fatalf("battle ttl=%v", ttl)
	}
	battle, found, err := f.battle.GetBattle(ctx, request.MatchID)
	if err != nil || !found || battle.GetState() != "abandoned" {
		t.Fatalf("battle=%+v found=%v err=%v", battle, found, err)
	}
	// ACK-loss retry succeeds from the permanent journal even if bounded
	// authority records have already expired.
	f.mr.FastForward(3 * time.Minute)
	if completed, err := f.auth.CompleteAllocationAbortExpected(ctx, request, time.Minute, 2*time.Minute); err != nil || !completed {
		t.Fatalf("post-expiry retry complete=%v err=%v", completed, err)
	}
}

func terminateAndTearDownAbortTarget(t *testing.T, f *battleAuthFixture, request battleabort.Request) {
	t.Helper()
	ctx := context.Background()
	expected := BattleExpectedInstance{
		AllocationID: request.Target.AllocationID, InstanceUID: request.Target.InstanceUID,
		InstanceEpoch: request.Target.InstanceEpoch,
	}
	if terminated, err := f.auth.TerminateExpected(
		ctx, request.MatchID, expected, "abandoned", time.Minute, time.Minute); err != nil || !terminated {
		t.Fatalf("terminate target=%v err=%v", terminated, err)
	}
	if err := f.battle.RecordInstanceTeardown(ctx, request.MatchID, BattleDepartureSource{
		DSPodName: request.Target.PodName, GameServerUID: request.Target.InstanceUID,
		InstanceEpoch: request.Target.InstanceEpoch, AllocationID: request.Target.AllocationID,
		PodUID: "pod-uid-abort",
	}); err != nil {
		t.Fatalf("record teardown: %v", err)
	}
}

func TestAllocationLifecycleMarkerRequiresExactTerminalAuthorityAndTeardown(t *testing.T) {
	ctx := context.Background()
	withoutTeardown := newBattleAuthFixture(t)
	withoutTeardownRequest := seedActiveAbortTarget(t, withoutTeardown, 8807, 0)
	withoutTeardownExpected := BattleExpectedInstance{
		AllocationID:  withoutTeardownRequest.Target.AllocationID,
		InstanceUID:   withoutTeardownRequest.Target.InstanceUID,
		InstanceEpoch: withoutTeardownRequest.Target.InstanceEpoch,
	}
	if terminated, err := withoutTeardown.auth.TerminateExpected(ctx, withoutTeardownRequest.MatchID,
		withoutTeardownExpected, "abandoned", time.Minute, time.Minute); err != nil || !terminated {
		t.Fatalf("terminate without teardown=%v err=%v", terminated, err)
	}
	if err := withoutTeardown.battle.RecordAllocationLifecyclePublished(
		ctx, withoutTeardownRequest.MatchID, withoutTeardownRequest.Target); errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("marker without teardown err=%v code=%v", err, errcode.As(err))
	}

	f := newBattleAuthFixture(t)
	request := seedActiveAbortTarget(t, f, 8804, 0)

	// Even a (simulated) teardown proof cannot mark a still-active authority as
	// lifecycle-complete. Future accidental callers must fail closed here.
	if err := f.battle.RecordInstanceTeardown(ctx, request.MatchID, BattleDepartureSource{
		DSPodName: request.Target.PodName, GameServerUID: request.Target.InstanceUID,
		InstanceEpoch: request.Target.InstanceEpoch, AllocationID: request.Target.AllocationID,
		PodUID: "pod-uid-abort",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.battle.RecordAllocationLifecyclePublished(ctx, request.MatchID, request.Target); errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("active marker err=%v code=%v", err, errcode.As(err))
	}
	if f.mr.Exists(battleAllocationLifecyclePublishedKey(request.MatchID)) {
		t.Fatal("active authority acquired a lifecycle marker")
	}

	expected := BattleExpectedInstance{AllocationID: request.Target.AllocationID,
		InstanceUID: request.Target.InstanceUID, InstanceEpoch: request.Target.InstanceEpoch}
	if terminated, err := f.auth.TerminateExpected(
		ctx, request.MatchID, expected, "abandoned", time.Minute, time.Minute); err != nil || !terminated {
		t.Fatalf("terminate=%v err=%v", terminated, err)
	}
	if err := f.battle.RecordAllocationLifecyclePublished(ctx, request.MatchID, request.Target); err != nil {
		t.Fatalf("record marker: %v", err)
	}
	// ACK-loss replay is exact and permanent.
	if err := f.battle.RecordAllocationLifecyclePublished(ctx, request.MatchID, request.Target); err != nil {
		t.Fatalf("marker replay: %v", err)
	}
	if ttl := f.rdb.TTL(ctx, battleAllocationLifecyclePublishedKey(request.MatchID)).Val(); ttl != -1 {
		t.Fatalf("marker ttl=%v, want permanent", ttl)
	}
	mutated := request.Target
	mutated.ReleaseTrack = "canary"
	if err := f.battle.RecordAllocationLifecyclePublished(ctx, request.MatchID, mutated); errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("marker tuple conflict err=%v code=%v", err, errcode.As(err))
	}
}

func TestAllocationAbortTerminalFallbackRequiresTeardownAndLifecycleMarker(t *testing.T) {
	ctx := context.Background()

	// Teardown alone, followed by bounded authority expiry, is UNKNOWN. It must
	// not create a successful abort journal.
	teardownOnly := newBattleAuthFixture(t)
	request := seedActiveAbortTarget(t, teardownOnly, 8805, 0)
	terminateAndTearDownAbortTarget(t, teardownOnly, request)
	if err := teardownOnly.rdb.Del(ctx, battleAuthKey(request.MatchID), battleKey(request.MatchID)).Err(); err != nil {
		t.Fatal(err)
	}
	if result, err := teardownOnly.auth.FenceAllocationAbortExpected(ctx, request); err == nil || result.Released {
		t.Fatalf("teardown-only result=%+v err=%v", result, err)
	}
	if teardownOnly.mr.Exists(battleAllocationAbortKey(request.MatchID)) {
		t.Fatal("teardown-only fallback wrote abort ACK")
	}

	// The exact terminal worker commits both independent proofs before the
	// bounded auth/battle records disappear. A later signed abort then creates
	// the permanent RELEASED operation journal atomically and is replay-safe.
	f := newBattleAuthFixture(t)
	request = seedActiveAbortTarget(t, f, 8806, 0)
	terminateAndTearDownAbortTarget(t, f, request)
	if err := f.battle.RecordAllocationLifecyclePublished(ctx, request.MatchID, request.Target); err != nil {
		t.Fatal(err)
	}
	if err := f.rdb.Del(ctx, battleAuthKey(request.MatchID), battleKey(request.MatchID)).Err(); err != nil {
		t.Fatal(err)
	}
	result, err := f.auth.FenceAllocationAbortExpected(ctx, request)
	if err != nil || !result.Released || result.Battle != nil {
		t.Fatalf("terminal fallback result=%+v err=%v", result, err)
	}
	read, released, found, err := f.auth.ReadAllocationAbort(ctx, request.MatchID)
	if err != nil || !found || !released || string(read.Canonical()) != string(request.Canonical()) {
		t.Fatalf("journal=%+v released=%v found=%v err=%v", read, released, found, err)
	}
	if ttl := f.rdb.TTL(ctx, battleAllocationAbortKey(request.MatchID)).Val(); ttl != -1 {
		t.Fatalf("fallback journal ttl=%v, want permanent", ttl)
	}
	if completed, err := f.auth.CompleteAllocationAbortExpected(
		ctx, request, time.Minute, time.Minute); err != nil || !completed {
		t.Fatalf("fallback complete=%v err=%v", completed, err)
	}
	mutated := request
	mutated.OperationID = "650e8400-e29b-41d4-a716-446655440000"
	if _, err := f.auth.FenceAllocationAbortExpected(ctx, mutated); errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("different operation inherited terminal ACK: err=%v code=%v", err, errcode.As(err))
	}
}

func TestAllocationAbortReleasedAckRetryBoundsPermanentAuthority(t *testing.T) {
	ctx := context.Background()
	f := newBattleAuthFixture(t)
	request := seedActiveAbortTarget(t, f, 8808, 0)
	if _, err := f.auth.FenceAllocationAbortExpected(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := f.battle.RecordInstanceTeardown(ctx, request.MatchID, BattleDepartureSource{
		DSPodName: request.Target.PodName, GameServerUID: request.Target.InstanceUID,
		InstanceEpoch: request.Target.InstanceEpoch, AllocationID: request.Target.AllocationID,
		PodUID: "pod-uid-abort",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.battle.RecordAllocationLifecyclePublished(ctx, request.MatchID, request.Target); err != nil {
		t.Fatal(err)
	}
	// Simulate a retry that recognizes both permanent proofs and commits the
	// RELEASED journal, followed by a process crash before local TTL cleanup.
	result, err := f.auth.FenceAllocationAbortExpected(ctx, request)
	if err != nil || !result.Released {
		t.Fatalf("proof retry result=%+v err=%v", result, err)
	}
	for _, key := range []string{battleAuthKey(request.MatchID), battleKey(request.MatchID)} {
		if ttl := f.rdb.TTL(ctx, key).Val(); ttl != -1 {
			t.Fatalf("pre-cleanup %s ttl=%v, want permanent crash window", key, ttl)
		}
	}
	if completed, err := f.auth.CompleteAllocationAbortExpected(
		ctx, request, time.Minute, 2*time.Minute); err != nil || !completed {
		t.Fatalf("released cleanup=%v err=%v", completed, err)
	}
	if ttl := f.rdb.TTL(ctx, battleAuthKey(request.MatchID)).Val(); ttl <= 0 || ttl > time.Minute {
		t.Fatalf("auth cleanup ttl=%v", ttl)
	}
	if ttl := f.rdb.TTL(ctx, battleKey(request.MatchID)).Val(); ttl <= 0 || ttl > 2*time.Minute {
		t.Fatalf("battle cleanup ttl=%v", ttl)
	}
}

func TestAllocationAbortReleasedAckRejectsDifferentRemainingAuthority(t *testing.T) {
	ctx := context.Background()
	f := newBattleAuthFixture(t)
	request := seedActiveAbortTarget(t, f, 8809, 0)
	if _, err := f.auth.FenceAllocationAbortExpected(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := f.battle.RecordInstanceTeardown(ctx, request.MatchID, BattleDepartureSource{
		DSPodName: request.Target.PodName, GameServerUID: request.Target.InstanceUID,
		InstanceEpoch: request.Target.InstanceEpoch, AllocationID: request.Target.AllocationID,
		PodUID: "pod-uid-abort",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.battle.RecordAllocationLifecyclePublished(ctx, request.MatchID, request.Target); err != nil {
		t.Fatal(err)
	}
	if result, err := f.auth.FenceAllocationAbortExpected(ctx, request); err != nil || !result.Released {
		t.Fatalf("release journal result=%+v err=%v", result, err)
	}
	// Simulate a later/different authority record appearing under the same
	// match key. A permanent old journal may never expire or de-index it.
	payload := rawRedisBytes(t, f.rdb, battleKey(request.MatchID))
	battle := &dsv1.BattleStorageRecord{}
	if err := proto.Unmarshal(payload, battle); err != nil {
		t.Fatal(err)
	}
	battle.ReleaseTrack = "canary"
	payload, err := proto.Marshal(battle)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.rdb.Set(ctx, battleKey(request.MatchID), payload, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if completed, err := f.auth.CompleteAllocationAbortExpected(
		ctx, request, time.Minute, time.Minute); completed || errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("mismatched cleanup=%v err=%v", completed, err)
	}
	if _, err := f.rdb.ZScore(ctx, activeKey, "8809").Result(); err != nil {
		t.Fatalf("mismatched authority was removed from active index: %v", err)
	}
	if ttl := f.rdb.TTL(ctx, battleKey(request.MatchID)).Val(); ttl != -1 {
		t.Fatalf("mismatched battle ttl changed to %v", ttl)
	}
}
