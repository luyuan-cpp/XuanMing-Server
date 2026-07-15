package data

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/placement"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
)

type battleExitPlacementStub struct {
	record *locatorv1.PlayerPlacementStorageRecord
}

func (s *battleExitPlacementStub) GetPlacement(context.Context, *locatorv1.GetPlacementRequest, ...grpc.CallOption) (*locatorv1.GetPlacementResponse, error) {
	return &locatorv1.GetPlacementResponse{Code: commonv1.ErrCode_OK, Found: s.record != nil, Placement: s.record}, nil
}

func TestBattleExitProofRelayIsDurableExactAndNoTTL(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	signer, err := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	stub := &battleExitPlacementStub{record: &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 42, Version: 9, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
		MatchId:         700,
	}}
	relay := NewBattleExitProofRelay(stub, rdb, signer)
	rec := BattleExitProofRecord{ID: 1, MatchID: 700, PlayerID: 42,
		Proof: placement.BattleExitProof{ProofType: placement.ProofMatchTerminal, ProofID: "result:700:match:700"}}
	proof, superseded, err := relay.PrepareTerminalProof(context.Background(), rec)
	if err != nil || superseded || proof.ExpectedVersion != 9 || !placement.ValidOperationID(proof.OperationID) ||
		!signer.Verify(proof.Statement(42, 700), proof.Signature) {
		t.Fatalf("prepared proof=%+v superseded=%v err=%v", proof, superseded, err)
	}
	if err := relay.RelayTerminalFence(context.Background(), 42, 700, rec.Proof); err != nil {
		t.Fatal(err)
	}
	if err := relay.RelayTerminalFence(context.Background(), 42, 700, rec.Proof); err != nil {
		t.Fatalf("exact terminal fence replay must be idempotent: %v", err)
	}
	if err := relay.RelayTerminalProof(context.Background(), 42, 700, proof); err != nil {
		t.Fatal(err)
	}
	if err := relay.RelayTerminalProof(context.Background(), 42, 700, proof); err != nil {
		t.Fatalf("exact replay must be idempotent: %v", err)
	}
	// Result idempotency can recreate the MySQL relay row after its first ACK.
	// Preparing that row must recover the immutable Redis payload, not mint a
	// second UUID that would conflict forever while placement remains BATTLE.
	recovered, recoveredSuperseded, err := relay.PrepareTerminalProof(context.Background(), rec)
	if err != nil || recoveredSuperseded || recovered != proof {
		t.Fatalf("idempotent preparation did not reuse durable proof: got=%+v want=%+v superseded=%v err=%v",
			recovered, proof, recoveredSuperseded, err)
	}
	key := placement.BattleExitProofKey(42, 700)
	values, err := rdb.HGetAll(context.Background(), key).Result()
	if err != nil || values[placement.BattleExitFieldOperationID] != proof.OperationID ||
		values[placement.BattleExitFieldSignature] != proof.Signature {
		t.Fatalf("relayed hash=%v err=%v", values, err)
	}
	if ttl := rdb.TTL(context.Background(), key).Val(); ttl != -1*time.Nanosecond {
		t.Fatalf("battle exit proof must not have TTL, got %s", ttl)
	}
	fenceKey := placement.BattleTerminalFenceKey(42, 700)
	fenceValues, err := rdb.HGetAll(context.Background(), fenceKey).Result()
	if err != nil || len(fenceValues) != 3 {
		t.Fatalf("terminal fence=%v err=%v", fenceValues, err)
	}
	fenceType, err := strconv.ParseInt(fenceValues[placement.BattleTerminalFenceFieldProofType], 10, 32)
	fence := placement.BattleTerminalFence{ProofType: int32(fenceType),
		ProofID:   fenceValues[placement.BattleTerminalFenceFieldProofID],
		Signature: fenceValues[placement.BattleTerminalFenceFieldSignature]}
	if err != nil || fence.ProofType != placement.ProofMatchTerminal ||
		!signer.Verify(fence.Statement(42, 700), fence.Signature) {
		t.Fatalf("terminal fence is not a valid signed result identity: %+v err=%v", fence, err)
	}
	if ttl := rdb.TTL(context.Background(), fenceKey).Val(); ttl != -1*time.Nanosecond {
		t.Fatalf("battle terminal fence must not have TTL, got %s", ttl)
	}
	conflict := proof
	conflict.OperationID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	conflict.Signature = signer.Sign(conflict.Statement(42, 700))
	if err := relay.RelayTerminalProof(context.Background(), 42, 700, conflict); err == nil {
		t.Fatal("different immutable operation overwrote existing proof")
	}
}

func TestPrepareTerminalProofMarksNewerPlacementSuperseded(t *testing.T) {
	signer, _ := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	relay := NewBattleExitProofRelay(&battleExitPlacementStub{record: &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 42, Version: 12, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
		MatchId:         701,
	}}, nil, signer)
	rec := BattleExitProofRecord{ID: 1, MatchID: 700, PlayerID: 42,
		Proof: placement.BattleExitProof{ProofType: placement.ProofMatchTerminal, ProofID: "result:700:match:700"}}
	proof, superseded, err := relay.PrepareTerminalProof(context.Background(), rec)
	if err != nil || !superseded || proof.OperationID != "" || proof.Signature != "" {
		t.Fatalf("newer placement proof=%+v superseded=%v err=%v", proof, superseded, err)
	}
}

func TestPrepareTerminalProofCancelsExactPendingBattleInsteadOfSuperseding(t *testing.T) {
	signer, _ := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	relay := NewBattleExitProofRelay(&battleExitPlacementStub{record: &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 42, Version: 10,
		CurrentRoute:    locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		TargetMatchId:   700, OperationId: "123e4567-e89b-42d3-a456-426614174000",
		ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START,
		ProofId:   "match-start:700",
	}}, nil, signer)
	rec := BattleExitProofRecord{ID: 1, MatchID: 700, PlayerID: 42,
		Proof: placement.BattleExitProof{ProofType: placement.ProofMatchTerminal, ProofID: "result:700:match:700"}}
	proof, superseded, err := relay.PrepareTerminalProof(context.Background(), rec)
	if err != nil || superseded || proof.ExpectedVersion != 10 ||
		!placement.ValidOperationID(proof.OperationID) ||
		!signer.Verify(proof.Statement(42, 700), proof.Signature) {
		t.Fatalf("pending Battle terminal proof=%+v superseded=%v err=%v", proof, superseded, err)
	}
}

func TestMalformedPendingBattleTerminalStateRemainsUnknown(t *testing.T) {
	signer, _ := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	relay := NewBattleExitProofRelay(&battleExitPlacementStub{record: &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 42, Version: 10,
		CurrentRoute:    locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE, // impossible pre-Admission shape
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		TargetMatchId:   700, OperationId: "123e4567-e89b-42d3-a456-426614174000",
		ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START,
		ProofId:   "match-start:700",
	}}, nil, signer)
	rec := BattleExitProofRecord{ID: 1, MatchID: 700, PlayerID: 42,
		Proof: placement.BattleExitProof{ProofType: placement.ProofMatchTerminal, ProofID: "result:700:match:700"}}
	proof, superseded, err := relay.PrepareTerminalProof(context.Background(), rec)
	if err == nil || superseded || proof.OperationID != "" {
		t.Fatalf("malformed state was discarded: proof=%+v superseded=%v err=%v", proof, superseded, err)
	}
}

func TestStableHubStillGetsPermanentTerminalTombstone(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	signer, _ := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	relay := NewBattleExitProofRelay(&battleExitPlacementStub{record: &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 42, Version: 15, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
	}}, rdb, signer)
	rec := BattleExitProofRecord{ID: 1, MatchID: 700, PlayerID: 42,
		Proof: placement.BattleExitProof{ProofType: placement.ProofMatchTerminal, ProofID: "result:700:match:700"}}

	// Publisher order is fence first, route inspection second. STABLE HUB means no
	// version-bound movement is needed, not that the old match may be resurrected.
	if err := relay.RelayTerminalFence(context.Background(), 42, 700, rec.Proof); err != nil {
		t.Fatal(err)
	}
	if err := relay.RelayTerminalFence(context.Background(), 42, 700, rec.Proof); err != nil {
		t.Fatalf("stable-Hub fence replay: %v", err)
	}
	proof, superseded, err := relay.PrepareTerminalProof(context.Background(), rec)
	if err != nil || !superseded || proof != (placement.BattleExitProof{}) {
		t.Fatalf("stable HUB route decision proof=%+v superseded=%v err=%v", proof, superseded, err)
	}
	key := placement.BattleTerminalFenceKey(42, 700)
	values, err := rdb.HGetAll(context.Background(), key).Result()
	if err != nil || len(values) != 3 || rdb.Exists(context.Background(), placement.BattleExitProofKey(42, 700)).Val() != 0 {
		t.Fatalf("stable HUB tombstone/proof state fence=%v err=%v", values, err)
	}
	if ttl := rdb.TTL(context.Background(), key).Val(); ttl != -1*time.Nanosecond {
		t.Fatalf("stable HUB terminal tombstone has TTL %s", ttl)
	}
}

func TestMalformedTerminalTombstoneFailsClosedAndIsNeverOverwritten(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	signer, _ := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	relay := NewBattleExitProofRelay(nil, rdb, signer)
	ctx := context.Background()
	fenceKey := placement.BattleTerminalFenceKey(42, 700)
	if err := rdb.HSet(ctx, fenceKey, placement.BattleTerminalFenceFieldProofID, "corrupt").Err(); err != nil {
		t.Fatal(err)
	}
	identity := placement.BattleExitProof{ProofType: placement.ProofMatchTerminal, ProofID: "result:700:match:700"}
	if err := relay.RelayTerminalFence(ctx, 42, 700, identity); err == nil {
		t.Fatal("malformed tombstone was silently overwritten")
	}
	values, err := rdb.HGetAll(ctx, fenceKey).Result()
	if err != nil || len(values) != 1 || values[placement.BattleTerminalFenceFieldProofID] != "corrupt" {
		t.Fatalf("malformed fail-closed tombstone changed: %v err=%v", values, err)
	}
	proof := placement.BattleExitProof{ExpectedVersion: 9,
		OperationID: "9849ab5b-2ecf-4fc3-983d-2d8df53cc009",
		ProofType:   placement.ProofMatchTerminal, ProofID: identity.ProofID}
	proof.Signature = signer.Sign(proof.Statement(42, 700))
	if err := relay.RelayTerminalProof(ctx, 42, 700, proof); err == nil {
		t.Fatal("version proof bypassed malformed terminal tombstone")
	}
	if rdb.Exists(ctx, placement.BattleExitProofKey(42, 700)).Val() != 0 {
		t.Fatal("version proof was written despite malformed tombstone")
	}
}

func TestVersionBoundExitProofRequiresMatchingTerminalTombstone(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	signer, _ := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	relay := NewBattleExitProofRelay(nil, rdb, signer)
	proof := placement.BattleExitProof{ExpectedVersion: 9,
		OperationID: "9849ab5b-2ecf-4fc3-983d-2d8df53cc009",
		ProofType:   placement.ProofMatchTerminal, ProofID: "result:700:match:700"}
	proof.Signature = signer.Sign(proof.Statement(42, 700))
	if err := relay.RelayTerminalProof(context.Background(), 42, 700, proof); err == nil {
		t.Fatal("version-bound exit proof was written without its permanent tombstone")
	}
	if rdb.Exists(context.Background(), placement.BattleExitProofKey(42, 700)).Val() != 0 {
		t.Fatal("orphan version-bound exit proof exists")
	}
}
