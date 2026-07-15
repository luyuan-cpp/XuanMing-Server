package data

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
)

type battleExitPlacementClient interface {
	GetPlacement(context.Context, *locatorv1.GetPlacementRequest, ...grpc.CallOption) (*locatorv1.GetPlacementResponse, error)
}

// BattleExitProofRelay publishes two deliberately separate durable facts:
//   - a version-free terminal tombstone for every roster member, which prevents
//     delayed MATCH_START/Admission writers from ever resurrecting this match;
//   - a version-bound exit proof only when placement must actually move to Hub.
//
// Neither record has a TTL. The tombstone is published before placement is
// inspected, so STABLE HUB is safe rather than being a proof-less superseded
// case. Exact conflict checks never overwrite another terminal/leave identity.
type BattleExitProofRelay struct {
	locator battleExitPlacementClient
	rdb     redis.UniversalClient
	signer  *placement.ProofSigner
}

var relayBattleTerminalFenceScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
  redis.call('HSET', KEYS[1],
    ARGV[1], ARGV[2], ARGV[3], ARGV[4], ARGV[5], ARGV[6])
  return 1
end
if redis.call('HLEN', KEYS[1]) == 3
  and redis.call('HGET', KEYS[1], ARGV[1]) == ARGV[2]
  and redis.call('HGET', KEYS[1], ARGV[3]) == ARGV[4]
  and redis.call('HGET', KEYS[1], ARGV[5]) == ARGV[6] then
  return 1
end
return redis.error_reply('battle terminal fence identity conflict')`)

// RelayTerminalFence permanently fences this exact player/match before any
// version-dependent placement decision. Its signature is deterministic over
// the durable terminal result identity, so response loss and replay are exact.
func (r *BattleExitProofRelay) RelayTerminalFence(
	ctx context.Context,
	playerID, matchID uint64,
	identity placement.BattleExitProof,
) error {
	if r == nil || r.rdb == nil || r.signer == nil || playerID == 0 || matchID == 0 ||
		identity.ProofType != placement.ProofMatchTerminal ||
		identity.ProofID != fmt.Sprintf("result:%d:match:%d", matchID, matchID) {
		return errcode.New(errcode.ErrInvalidArg, "complete terminal result identity required")
	}
	fence := placement.BattleTerminalFence{ProofType: identity.ProofType, ProofID: identity.ProofID}
	fence.Signature = r.signer.Sign(fence.Statement(playerID, matchID))
	if fence.Signature == "" || !r.signer.Verify(fence.Statement(playerID, matchID), fence.Signature) {
		return errcode.New(errcode.ErrUnavailable, "battle terminal fence signer unavailable")
	}

	// A valid leave fence already prevents this player from re-entering the old
	// match and therefore supersedes a later terminal-result fence. Malformed or
	// conflicting terminal data remains fail-closed and is never overwritten.
	values, err := r.rdb.HGetAll(ctx, placement.BattleTerminalFenceKey(playerID, matchID)).Result()
	if err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err, "read durable battle terminal fence failed")
	}
	if len(values) != 0 {
		existing, parseErr := parseBattleTerminalFence(values)
		if parseErr != nil || !r.signer.Verify(existing.Statement(playerID, matchID), existing.Signature) {
			return errcode.New(errcode.ErrUnavailable, "durable battle terminal fence is malformed or unsigned")
		}
		if existing.ProofType == placement.ProofPlayerLeave {
			return nil
		}
		if existing != fence {
			return errcode.New(errcode.ErrUnavailable, "durable battle terminal fence identity conflicts with result")
		}
		return nil
	}

	if err := relayBattleTerminalFenceScript.Run(ctx, r.rdb,
		[]string{placement.BattleTerminalFenceKey(playerID, matchID)},
		placement.BattleTerminalFenceFieldProofType, strconv.FormatInt(int64(fence.ProofType), 10),
		placement.BattleTerminalFenceFieldProofID, fence.ProofID,
		placement.BattleTerminalFenceFieldSignature, fence.Signature,
	).Err(); err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err, "relay durable battle terminal fence failed")
	}
	return nil
}

func parseBattleTerminalFence(values map[string]string) (placement.BattleTerminalFence, error) {
	if len(values) != 3 {
		return placement.BattleTerminalFence{}, fmt.Errorf("unexpected terminal fence fields")
	}
	proofType, err := strconv.ParseInt(values[placement.BattleTerminalFenceFieldProofType], 10, 32)
	fence := placement.BattleTerminalFence{
		ProofType: int32(proofType),
		ProofID:   values[placement.BattleTerminalFenceFieldProofID],
		Signature: values[placement.BattleTerminalFenceFieldSignature],
	}
	if err != nil || (fence.ProofType != placement.ProofMatchTerminal && fence.ProofType != placement.ProofPlayerLeave) ||
		fence.ProofID == "" || fence.Signature == "" {
		return placement.BattleTerminalFence{}, fmt.Errorf("invalid terminal fence")
	}
	return fence, nil
}

func NewBattleExitProofRelay(locator battleExitPlacementClient, rdb redis.UniversalClient, signer *placement.ProofSigner) *BattleExitProofRelay {
	return &BattleExitProofRelay{locator: locator, rdb: rdb, signer: signer}
}

// PrepareTerminalProof returns superseded=true only when authoritative
// placement proves no version-bound movement is needed. The caller must have
// successfully relayed the version-free terminal tombstone first; superseded
// therefore never means "unfenced". Missing/corrupt/transport state remains
// UNKNOWN and must be retried, not silently discarded.
func (r *BattleExitProofRelay) PrepareTerminalProof(ctx context.Context, rec BattleExitProofRecord) (placement.BattleExitProof, bool, error) {
	if r == nil || r.locator == nil || r.signer == nil || rec.MatchID == 0 || rec.PlayerID == 0 ||
		rec.Proof.ProofType != placement.ProofMatchTerminal ||
		rec.Proof.ProofID != fmt.Sprintf("result:%d:match:%d", rec.MatchID, rec.MatchID) {
		return placement.BattleExitProof{}, false, errcode.New(errcode.ErrUnavailable, "battle exit proof authority unavailable")
	}
	// An idempotent BattleResult replay may recreate an outbox row after the
	// original row was ACK-deleted.  The Redis proof is deliberately durable and
	// immutable, so it is the first recovery source: re-signing a new UUID while
	// placement is still STABLE BATTLE would conflict forever with that valid
	// proof.  Reuse only a complete, locally verified statement; malformed or
	// conflicting data is UNKNOWN and must never be overwritten.
	if r.rdb != nil {
		proof, found, superseded, readErr := r.readExistingProof(ctx, rec.PlayerID, rec.MatchID, rec.Proof.ProofID)
		if readErr != nil || found {
			return proof, superseded, readErr
		}
	}
	resp, err := r.locator.GetPlacement(ctx, &locatorv1.GetPlacementRequest{PlayerId: rec.PlayerID})
	if err != nil {
		return placement.BattleExitProof{}, false, err
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return placement.BattleExitProof{}, false, errcode.New(errcode.Code(resp.GetCode()), "GetPlacement code=%d", resp.GetCode())
	}
	if !resp.GetFound() || resp.GetPlacement() == nil {
		return placement.BattleExitProof{}, false, errcode.New(errcode.ErrUnavailable, "battle placement is UNKNOWN")
	}
	p := resp.GetPlacement()
	proof := placement.BattleExitProof{ProofType: placement.ProofMatchTerminal, ProofID: rec.Proof.ProofID}
	switch {
	case p.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE &&
		p.GetCurrentRoute() == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE && p.GetMatchId() == rec.MatchID:
		if p.GetVersion() == 0 {
			return placement.BattleExitProof{}, false, errcode.New(errcode.ErrUnavailable, "stable battle placement has zero version")
		}
		proof.ExpectedVersion = p.GetVersion()
		proof.OperationID = uuid.NewString()
	case p.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
		p.GetTargetRoute() == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB && p.GetSourceMatchId() == rec.MatchID &&
		p.GetProofType() == locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_TERMINAL &&
		p.GetProofId() == rec.Proof.ProofID && placement.ValidOperationID(p.GetOperationId()) && p.GetVersion() > 1:
		// Login already began the same immutable terminal operation. Reconstruct
		// the exact statement so a crash before Redis relay remains idempotent.
		proof.ExpectedVersion = p.GetVersion() - 1
		proof.OperationID = p.GetOperationId()
	case p.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
		p.GetCurrentRoute() == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB && p.GetMatchId() == 0 &&
		p.GetTargetRoute() == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE &&
		p.GetTargetMatchId() == rec.MatchID &&
		p.GetProofType() == locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START &&
		placement.ValidOperationID(p.GetOperationId()) && p.GetProofId() != "" && p.GetVersion() > 0:
		// READY may have durably prepared/bound HUB->BATTLE but the player has not
		// crossed the Battle Admission gate yet. Terminal proof must cancel that
		// exact target, not mark recovery superseded: locator will advance version
		// once more, atomically invalidating every already-issued Battle ticket.
		proof.ExpectedVersion = p.GetVersion()
		proof.OperationID = uuid.NewString()
	case (p.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE &&
		p.GetCurrentRoute() == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB) ||
		(p.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE &&
			p.GetCurrentRoute() == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE && p.GetMatchId() != rec.MatchID) ||
		(p.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
			p.GetTargetRoute() == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB &&
			p.GetSourceMatchId() == rec.MatchID &&
			p.GetProofType() == locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_PLAYER_LEAVE &&
			placement.ValidOperationID(p.GetOperationId())) ||
		(p.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
			p.GetTargetRoute() == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB &&
			p.GetSourceMatchId() != rec.MatchID) ||
		(p.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
			p.GetTargetRoute() == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE &&
			p.GetTargetMatchId() != rec.MatchID):
		return placement.BattleExitProof{}, true, nil
	default:
		return placement.BattleExitProof{}, false, errcode.New(errcode.ErrUnavailable,
			"battle exit placement is UNKNOWN route=%s transition=%s version=%d",
			p.GetCurrentRoute(), p.GetTransitionState(), p.GetVersion())
	}
	proof.Signature = r.signer.Sign(proof.Statement(rec.PlayerID, rec.MatchID))
	return proof, false, nil
}

func (r *BattleExitProofRelay) readExistingProof(
	ctx context.Context, playerID, matchID uint64, terminalProofID string,
) (placement.BattleExitProof, bool, bool, error) {
	values, err := r.rdb.HGetAll(ctx, placement.BattleExitProofKey(playerID, matchID)).Result()
	if err != nil {
		return placement.BattleExitProof{}, false, false,
			errcode.NewCause(errcode.ErrUnavailable, err, "read durable battle exit proof failed")
	}
	if len(values) == 0 {
		return placement.BattleExitProof{}, false, false, nil
	}
	if len(values) != 5 {
		return placement.BattleExitProof{}, false, false,
			errcode.New(errcode.ErrUnavailable, "durable battle exit proof has unexpected fields")
	}
	expectedVersion, versionErr := strconv.ParseUint(values[placement.BattleExitFieldExpectedVersion], 10, 64)
	proofType, typeErr := strconv.ParseInt(values[placement.BattleExitFieldProofType], 10, 32)
	proof := placement.BattleExitProof{
		ExpectedVersion: expectedVersion,
		OperationID:     values[placement.BattleExitFieldOperationID],
		ProofType:       int32(proofType),
		ProofID:         values[placement.BattleExitFieldProofID],
		Signature:       values[placement.BattleExitFieldSignature],
	}
	if versionErr != nil || typeErr != nil || proof.ExpectedVersion == 0 ||
		!placement.ValidOperationID(proof.OperationID) || proof.ProofID == "" || proof.Signature == "" ||
		!r.signer.Verify(proof.Statement(playerID, matchID), proof.Signature) {
		return placement.BattleExitProof{}, false, false,
			errcode.New(errcode.ErrUnavailable, "durable battle exit proof is malformed or has an invalid signature")
	}
	switch proof.ProofType {
	case placement.ProofMatchTerminal:
		if proof.ProofID != terminalProofID {
			return placement.BattleExitProof{}, false, false,
				errcode.New(errcode.ErrUnavailable, "durable terminal proof identity conflicts with result")
		}
		return proof, true, false, nil
	case placement.ProofPlayerLeave:
		// A valid leave operation already owns this exact Battle→Hub transition;
		// the later terminal-result job is superseded and must not replace it.
		return placement.BattleExitProof{}, true, true, nil
	default:
		return placement.BattleExitProof{}, false, false,
			errcode.New(errcode.ErrUnavailable, "durable battle exit proof has an unsupported proof type")
	}
}

var relayBattleExitProofScript = redis.NewScript(`
if redis.call('HLEN', KEYS[1]) ~= 3
  or redis.call('HGET', KEYS[1], ARGV[1]) ~= ARGV[2]
  or redis.call('HGET', KEYS[1], ARGV[3]) ~= ARGV[4]
  or redis.call('HGET', KEYS[1], ARGV[5]) ~= ARGV[6] then
  return redis.error_reply('battle terminal fence missing or conflicting')
end
if redis.call('EXISTS', KEYS[2]) == 0 then
  redis.call('HSET', KEYS[2],
    ARGV[7], ARGV[8], ARGV[9], ARGV[10], ARGV[11], ARGV[12],
    ARGV[13], ARGV[14], ARGV[15], ARGV[16])
  return 1
end
if redis.call('HLEN', KEYS[2]) == 5
  and redis.call('HGET', KEYS[2], ARGV[7]) == ARGV[8]
  and redis.call('HGET', KEYS[2], ARGV[9]) == ARGV[10]
  and redis.call('HGET', KEYS[2], ARGV[11]) == ARGV[12]
  and redis.call('HGET', KEYS[2], ARGV[13]) == ARGV[14]
  and redis.call('HGET', KEYS[2], ARGV[15]) == ARGV[16] then
  return 1
end
return redis.error_reply('battle exit proof identity conflict')`)

func (r *BattleExitProofRelay) RelayTerminalProof(ctx context.Context, playerID, matchID uint64, proof placement.BattleExitProof) error {
	if r == nil || r.rdb == nil || r.signer == nil || playerID == 0 || matchID == 0 ||
		proof.ExpectedVersion == 0 || !placement.ValidOperationID(proof.OperationID) ||
		proof.ProofType != placement.ProofMatchTerminal ||
		proof.ProofID != fmt.Sprintf("result:%d:match:%d", matchID, matchID) || proof.Signature == "" ||
		!r.signer.Verify(proof.Statement(playerID, matchID), proof.Signature) {
		return errcode.New(errcode.ErrInvalidArg, "complete signed terminal battle exit proof required")
	}
	fence := placement.BattleTerminalFence{ProofType: proof.ProofType, ProofID: proof.ProofID}
	fence.Signature = r.signer.Sign(fence.Statement(playerID, matchID))
	return relayBattleExitProofScript.Run(ctx, r.rdb, []string{
		placement.BattleTerminalFenceKey(playerID, matchID),
		placement.BattleExitProofKey(playerID, matchID),
	},
		placement.BattleTerminalFenceFieldProofType, strconv.FormatInt(int64(fence.ProofType), 10),
		placement.BattleTerminalFenceFieldProofID, fence.ProofID,
		placement.BattleTerminalFenceFieldSignature, fence.Signature,
		placement.BattleExitFieldExpectedVersion, strconv.FormatUint(proof.ExpectedVersion, 10),
		placement.BattleExitFieldOperationID, proof.OperationID,
		placement.BattleExitFieldProofType, strconv.FormatInt(int64(proof.ProofType), 10),
		placement.BattleExitFieldProofID, proof.ProofID,
		placement.BattleExitFieldSignature, proof.Signature,
	).Err()
}
