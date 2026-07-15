package placement

import "fmt"

const (
	BattleExitFieldExpectedVersion = "expected_version"
	BattleExitFieldOperationID     = "operation_id"
	BattleExitFieldProofType       = "proof_type"
	BattleExitFieldProofID         = "proof_id"
	BattleExitFieldSignature       = "signature"
)

// BattleExitProofKey is written by the durable battle terminal/leave worker and
// read by login when a player explicitly requests return-to-Hub. It shares the
// player hashtag with placement but is not mutated in the placement CAS.
func BattleExitProofKey(playerID, matchID uint64) string {
	return fmt.Sprintf("pandora:placement:proof:battle-exit:{%d}:%d", playerID, matchID)
}

type BattleExitProof struct {
	ExpectedVersion uint64
	OperationID string
	ProofType int32
	ProofID string
	Signature string
}

// Statement returns the exact assertion verified by player_locator. The proof
// id must identify an immutable terminal-result/leave record; callers must
// persist this struct once and replay it unchanged after a relay/RPC failure.
func (p BattleExitProof) Statement(playerID, matchID uint64) Proof {
	return Proof{PlayerID: playerID, ExpectedVersion: p.ExpectedVersion,
		SourceRoute: RouteBattle, TargetRoute: RouteHub, SourceMatchID: matchID,
		ProofType: p.ProofType, ProofID: p.ProofID, OperationID: p.OperationID}
}
