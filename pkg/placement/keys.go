package placement

import "fmt"

const (
	BattleExitFieldExpectedVersion = "expected_version"
	BattleExitFieldOperationID     = "operation_id"
	BattleExitFieldProofType       = "proof_type"
	BattleExitFieldProofID         = "proof_id"
	BattleExitFieldSignature       = "signature"

	BattleTerminalFenceFieldProofType = "proof_type"
	BattleTerminalFenceFieldProofID   = "proof_id"
	BattleTerminalFenceFieldSignature = "signature"
)

// BattleExitProofKey is written by the durable battle terminal/leave worker and
// read by login when a player explicitly requests return-to-Hub. It shares the
// player hashtag with placement but is not mutated in the placement CAS.
func BattleExitProofKey(playerID, matchID uint64) string {
	return fmt.Sprintf("pandora:placement:proof:battle-exit:{%d}:%d", playerID, matchID)
}

// BattleTerminalFenceKey is an immutable, no-TTL tombstone for one player's
// ownership of a terminal match.  Unlike BattleExitProofKey it is deliberately
// independent of the player's placement version: it prevents a delayed
// MATCH_START writer from resurrecting the old match even when the player was
// already STABLE HUB at the instant terminal processing observed placement.
// Both keys share the player hash tag with the canonical placement record.
func BattleTerminalFenceKey(playerID, matchID uint64) string {
	return fmt.Sprintf("pandora:placement:fence:battle-terminal:{%d}:%d", playerID, matchID)
}

type BattleTerminalFence struct {
	ProofType int32
	ProofID   string
	Signature string
}

// Statement is version-independent by design.  The terminal result/leave
// identity, player and match are sufficient to permanently fence every old
// MATCH_START operation for that exact match; the separate BattleExitProof
// remains version-bound and is used only when placement must actually move.
func (f BattleTerminalFence) Statement(playerID, matchID uint64) Proof {
	return Proof{PlayerID: playerID, SourceRoute: RouteBattle, TargetRoute: RouteHub,
		SourceMatchID: matchID, ProofType: f.ProofType, ProofID: f.ProofID}
}

type BattleExitProof struct {
	ExpectedVersion uint64
	OperationID     string
	ProofType       int32
	ProofID         string
	Signature       string
}

// Statement returns the exact assertion verified by player_locator. The proof
// id must identify an immutable terminal-result/leave record; callers must
// persist this struct once and replay it unchanged after a relay/RPC failure.
func (p BattleExitProof) Statement(playerID, matchID uint64) Proof {
	return Proof{PlayerID: playerID, ExpectedVersion: p.ExpectedVersion,
		SourceRoute: RouteBattle, TargetRoute: RouteHub, SourceMatchID: matchID,
		ProofType: p.ProofType, ProofID: p.ProofID, OperationID: p.OperationID}
}
