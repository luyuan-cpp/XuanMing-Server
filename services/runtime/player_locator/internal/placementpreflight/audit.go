package placementpreflight

import (
	"fmt"
	"strings"

	"github.com/luyuancpp/pandora/pkg/placement"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
)

// classifyPlacement is deliberately pure: release tooling and tests can apply
// the exact same fail-closed classification without touching Redis. An empty
// result means the record is safe for the locator hard-gate rollout.
func ClassifyPlacement(
	keyPlayerID uint64,
	rec *locatorv1.PlayerPlacementStorageRecord,
) []string {
	var reasons []string
	if rec == nil {
		return []string{"record is nil"}
	}
	if keyPlayerID == 0 {
		reasons = append(reasons, "placement key contains player_id=0")
	}
	if rec.GetPlayerId() != keyPlayerID {
		reasons = append(reasons, fmt.Sprintf(
			"record player_id=%d does not match key player_id=%d", rec.GetPlayerId(), keyPlayerID))
	}
	if rec.GetVersion() == 0 {
		reasons = append(reasons, "placement version is zero")
	}
	switch rec.GetTransitionState() {
	case locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE:
		// Stable records do not carry an active departure marker, but they must
		// carry a complete exact current target. Begin captures that tuple as the
		// next immutable source; admitting an incomplete legacy STABLE record
		// would make every future transition fail forever after the rollout.
		return append(reasons, classifyStable(rec)...)
	case locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING:
		// Continue below.
	default:
		return append(reasons, fmt.Sprintf("invalid transition_state=%s",
			rec.GetTransitionState().String()))
	}

	if rec.GetProofType() == locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP {
		return append(reasons, classifyBootstrapPending(rec)...)
	}
	return append(reasons, classifyTransitionPending(rec)...)
}

// classifyPlacement keeps the same-package table tests concise while the
// exported entrypoint is shared by the embedded locator release gate.
func classifyPlacement(keyPlayerID uint64, rec *locatorv1.PlayerPlacementStorageRecord) []string {
	return ClassifyPlacement(keyPlayerID, rec)
}

func classifyStable(rec *locatorv1.PlayerPlacementStorageRecord) []string {
	var reasons []string
	if !placement.ValidOperationID(rec.GetOperationId()) {
		reasons = append(reasons, "STABLE placement has invalid operation_id")
	}
	if rec.GetTargetRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_UNSPECIFIED ||
		rec.GetLeaseDeadlineMs() != 0 {
		reasons = append(reasons, "STABLE placement carries pending target route or lease")
	}
	if !sourceBindingEmpty(rec) || !sourceTarget(rec).Equal(placement.Target{}) ||
		rec.GetSourceDepartureConfirmed() ||
		rec.GetSourceDepartureProofType() != locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED ||
		rec.GetSourceDepartureProofId() != "" {
		reasons = append(reasons, "STABLE placement carries an active source-departure gate")
	}
	target := activeTarget(rec)
	switch rec.GetCurrentRoute() {
	case locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB:
		if rec.GetMatchId() != 0 || rec.GetTargetMatchId() != 0 ||
			!target.CompleteHub() || target.AllocationID != "" {
			reasons = append(reasons, "STABLE Hub placement lacks a complete exact Hub target")
		}
		// last_* is audit-only. Pre-source-departure STABLE records legitimately
		// have the whole quartet empty and are still a safe exact source for the
		// next Begin. If any history exists, however, it must be the exact tuple
		// written by Commit for this stable version/operation.
		reasons = append(reasons, classifyLastDepartureHistory(rec, false,
			rec.GetVersion(), rec.GetOperationId(),
			locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE,
			locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_BATTLE_DEPARTURE)...)
	case locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE:
		if rec.GetMatchId() == 0 || rec.GetTargetMatchId() != rec.GetMatchId() ||
			!target.CompleteBattle() || target.AssignmentID != "" {
			reasons = append(reasons, "STABLE Battle placement lacks a complete exact Battle target")
		}
		reasons = append(reasons, classifyLastDepartureHistory(rec, false,
			rec.GetVersion(), rec.GetOperationId(),
			locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE)...)
	default:
		reasons = append(reasons, "STABLE placement has no physical current route")
	}
	return reasons
}

func classifyBootstrapPending(rec *locatorv1.PlayerPlacementStorageRecord) []string {
	var reasons []string
	if rec.GetUpdatedAtMs() <= 0 {
		reasons = append(reasons, "account bootstrap PENDING updated_at_ms must be positive")
	}
	if rec.GetVersion() != 1 {
		reasons = append(reasons, "account bootstrap PENDING must have version=1")
	}
	if !placement.ValidOperationID(rec.GetOperationId()) {
		reasons = append(reasons, "account bootstrap PENDING has invalid operation_id")
	}
	if strings.TrimSpace(rec.GetProofId()) == "" {
		reasons = append(reasons, "account bootstrap PENDING has empty proof_id")
	}
	if rec.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_UNSPECIFIED ||
		rec.GetTargetRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB ||
		rec.GetMatchId() != 0 || rec.GetSourceMatchId() != 0 || rec.GetTargetMatchId() != 0 {
		reasons = append(reasons, "account bootstrap PENDING has a non-bootstrap route/match shape")
	}
	if rec.GetLeaseDeadlineMs() <= rec.GetUpdatedAtMs() {
		reasons = append(reasons, "account bootstrap PENDING lease must be after updated_at_ms")
	}
	if rec.GetAdmissionId() != "" {
		reasons = append(reasons, "account bootstrap PENDING unexpectedly carries admission_id")
	}
	if !sourceBindingEmpty(rec) || !sourceTarget(rec).Equal(placement.Target{}) ||
		rec.GetSourceDepartureConfirmed() ||
		rec.GetSourceDepartureProofType() != locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED ||
		rec.GetSourceDepartureProofId() != "" {
		reasons = append(reasons, "account bootstrap PENDING must have no active physical source or departure proof")
	}
	if rec.GetLastSourceDepartureProofType() != locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED ||
		rec.GetLastSourceDepartureProofId() != "" ||
		rec.GetLastSourceDeparturePlacementVersion() != 0 ||
		rec.GetLastSourceDepartureOperationId() != "" {
		reasons = append(reasons, "account bootstrap PENDING unexpectedly carries source-departure history")
	}
	if rec.GetRetargetCount() != 0 || rec.GetLastRetargetProofId() != "" ||
		rec.GetLastRetargetReason() != locatorv1.PlacementTargetUnavailableReason_PLACEMENT_TARGET_UNAVAILABLE_REASON_UNSPECIFIED {
		reasons = append(reasons, "account bootstrap PENDING must not be a retargeted operation")
	}

	target := activeTarget(rec)
	if !target.Equal(placement.Target{}) &&
		(!target.CompleteHub() || target.AllocationID != "") {
		reasons = append(reasons, "account bootstrap PENDING target must be unbound or a complete Hub target")
	}
	return reasons
}

func classifyTransitionPending(rec *locatorv1.PlayerPlacementStorageRecord) []string {
	var reasons []string
	if rec.GetUpdatedAtMs() <= 0 {
		reasons = append(reasons, "non-bootstrap PENDING updated_at_ms must be positive")
	}
	if rec.GetVersion() <= 1 {
		reasons = append(reasons, "non-bootstrap PENDING must have version>1")
	}
	if !placement.ValidOperationID(rec.GetOperationId()) {
		reasons = append(reasons, "non-bootstrap PENDING has invalid operation_id")
	}
	if rec.GetTargetRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB &&
		rec.GetTargetRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE {
		reasons = append(reasons, "non-bootstrap PENDING has invalid target_route")
	}
	if rec.GetLeaseDeadlineMs() <= rec.GetUpdatedAtMs() {
		reasons = append(reasons, "non-bootstrap PENDING lease must be after updated_at_ms")
	}
	if strings.TrimSpace(rec.GetProofId()) == "" {
		reasons = append(reasons, "non-bootstrap PENDING has empty proof_id")
	}
	if rec.GetAdmissionId() != "" {
		reasons = append(reasons, "non-bootstrap PENDING unexpectedly carries admission_id")
	}

	bindingComplete := rec.GetSourcePlacementVersion() > 0 &&
		rec.GetSourcePlacementVersion() < rec.GetVersion() &&
		placement.ValidOperationID(rec.GetSourceOperationId()) &&
		rec.GetSourceOperationId() != rec.GetOperationId()
	if !bindingComplete {
		reasons = append(reasons, "non-bootstrap PENDING is missing a complete immutable source binding")
	}

	source := sourceTarget(rec)
	expectedProof := locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED
	switch rec.GetCurrentRoute() {
	case locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB:
		if rec.GetMatchId() != 0 || !source.CompleteHub() || source.AllocationID != "" {
			reasons = append(reasons, "non-bootstrap PENDING is missing a complete physical Hub source tuple")
		} else {
			expectedProof = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE
		}
	case locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE:
		if rec.GetMatchId() == 0 || rec.GetSourceMatchId() != rec.GetMatchId() ||
			!source.CompleteBattle() || source.AssignmentID != "" {
			reasons = append(reasons, "non-bootstrap PENDING is missing a complete physical Battle source tuple")
		} else {
			expectedProof = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_BATTLE_DEPARTURE
		}
	default:
		reasons = append(reasons, "non-bootstrap PENDING has no physical current route")
	}

	target := activeTarget(rec)
	targetUnbound := target.Equal(placement.Target{})
	switch rec.GetTargetRoute() {
	case locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB:
		if rec.GetTargetMatchId() != 0 {
			reasons = append(reasons, "non-bootstrap PENDING Hub target carries target_match_id")
		}
		if !targetUnbound && (!target.CompleteHub() || target.AllocationID != "") {
			reasons = append(reasons, "non-bootstrap PENDING Hub target is partially or incorrectly bound")
		}
		switch rec.GetCurrentRoute() {
		case locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE:
			if rec.GetProofType() != locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_TERMINAL &&
				rec.GetProofType() != locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_PLAYER_LEAVE {
				reasons = append(reasons, "PENDING Battle-to-Hub proof is incompatible with route")
			}
		case locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB:
			hubTransfer := rec.GetProofType() == locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_HUB_TRANSFER &&
				rec.GetSourceMatchId() == 0
			cancelBattle := (rec.GetProofType() == locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_TERMINAL ||
				rec.GetProofType() == locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_PLAYER_LEAVE) &&
				rec.GetSourceMatchId() > 0
			if !hubTransfer && !cancelBattle {
				reasons = append(reasons, "PENDING Hub-to-Hub proof/source match is incompatible with route")
			}
		}
	case locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE:
		if rec.GetTargetMatchId() == 0 {
			reasons = append(reasons, "non-bootstrap PENDING Battle target has no target_match_id")
		}
		if !targetUnbound && (!target.CompleteBattle() || target.AssignmentID != "") {
			reasons = append(reasons, "non-bootstrap PENDING Battle target is partially or incorrectly bound")
		}
		if rec.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB ||
			rec.GetProofType() != locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START ||
			rec.GetSourceMatchId() != 0 {
			reasons = append(reasons, "PENDING Hub-to-Battle proof/source match is incompatible with route")
		}
	}

	if rec.GetSourceDepartureConfirmed() {
		if strings.TrimSpace(rec.GetSourceDepartureProofId()) == "" {
			reasons = append(reasons, "confirmed non-bootstrap PENDING has empty source_departure_proof_id")
		}
		if expectedProof == locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED ||
			rec.GetSourceDepartureProofType() != expectedProof {
			reasons = append(reasons, fmt.Sprintf(
				"source departure proof type %s does not match physical source (want %s)",
				rec.GetSourceDepartureProofType().String(), expectedProof.String()))
		}
	} else if rec.GetSourceDepartureProofType() !=
		locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED ||
		rec.GetSourceDepartureProofId() != "" {
		// A canonical Begin is intentionally visible before the source DS has
		// produced its physical census/eviction proof. The preflight runs on
		// every Pod restart, so this all-empty marker must remain startable;
		// otherwise a full locator restart would prevent the later Confirm RPC
		// that is required to make progress. Partial markers are never valid.
		reasons = append(reasons, "unconfirmed non-bootstrap PENDING carries a partial source-departure marker")
	}
	reasons = append(reasons, classifyLastDepartureHistory(rec, false,
		rec.GetSourcePlacementVersion(), rec.GetSourceOperationId(),
		locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE,
		locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_BATTLE_DEPARTURE)...)
	return reasons
}

func classifyLastDepartureHistory(rec *locatorv1.PlayerPlacementStorageRecord, required bool,
	expectedVersion uint64, expectedOperationID string,
	allowedTypes ...locatorv1.PlacementSourceDepartureProofType,
) []string {
	historyEmpty := rec.GetLastSourceDepartureProofType() ==
		locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED &&
		rec.GetLastSourceDepartureProofId() == "" && rec.GetLastSourceDeparturePlacementVersion() == 0 &&
		rec.GetLastSourceDepartureOperationId() == ""
	if historyEmpty {
		if required {
			return []string{"placement is missing complete source-departure audit history"}
		}
		return nil
	}
	var reasons []string
	typeAllowed := false
	for _, proofType := range allowedTypes {
		if rec.GetLastSourceDepartureProofType() == proofType {
			typeAllowed = true
			break
		}
	}
	if !typeAllowed || strings.TrimSpace(rec.GetLastSourceDepartureProofId()) == "" ||
		rec.GetLastSourceDeparturePlacementVersion() == 0 ||
		!placement.ValidOperationID(rec.GetLastSourceDepartureOperationId()) {
		reasons = append(reasons, "placement has malformed source-departure audit history")
	}
	if expectedVersion != 0 && (rec.GetLastSourceDeparturePlacementVersion() != expectedVersion ||
		rec.GetLastSourceDepartureOperationId() != expectedOperationID) {
		reasons = append(reasons, "source-departure audit history does not match placement lineage")
	}
	return reasons
}

func sourceBindingEmpty(rec *locatorv1.PlayerPlacementStorageRecord) bool {
	return rec.GetSourcePlacementVersion() == 0 && rec.GetSourceOperationId() == ""
}

func sourceTarget(rec *locatorv1.PlayerPlacementStorageRecord) placement.Target {
	return placement.Target{
		PodName:       rec.GetSourceDsPodName(),
		InstanceUID:   rec.GetSourceDsInstanceUid(),
		InstanceEpoch: rec.GetSourceDsInstanceEpoch(),
		AssignmentID:  rec.GetSourceHubAssignmentId(),
		AllocationID:  rec.GetSourceAllocationId(),
		ReleaseTrack:  rec.GetSourceReleaseTrack(),
	}
}

func activeTarget(rec *locatorv1.PlayerPlacementStorageRecord) placement.Target {
	return placement.Target{
		PodName:       rec.GetDsPodName(),
		InstanceUID:   rec.GetDsInstanceUid(),
		InstanceEpoch: rec.GetDsInstanceEpoch(),
		AssignmentID:  rec.GetHubAssignmentId(),
		AllocationID:  rec.GetAllocationId(),
		ReleaseTrack:  rec.GetReleaseTrack(),
	}
}
