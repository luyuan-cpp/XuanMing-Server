package placementpreflight

import (
	"strings"
	"testing"

	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
	"google.golang.org/protobuf/proto"
)

const (
	testOperationID       = "11111111-1111-4111-8111-111111111111"
	testSourceOperationID = "22222222-2222-4222-8222-222222222222"
	testAdmissionID       = "33333333-3333-4333-8333-333333333333"
)

func TestClassifyPlacementAllowsStableAndStrictBootstrap(t *testing.T) {
	stable := canonicalStableHubBootstrap()
	if got := classifyPlacement(7, stable); len(got) != 0 {
		t.Fatalf("stable placement findings = %v", got)
	}
	battle := canonicalStableBattle()
	if got := classifyPlacement(7, battle); len(got) != 0 {
		t.Fatalf("stable Battle placement findings = %v", got)
	}
	for _, hub := range []*locatorv1.PlayerPlacementStorageRecord{
		canonicalStableReturnedHub(locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_BATTLE_DEPARTURE),
		canonicalStableReturnedHub(locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE),
		canonicalStableHubTransfer(),
	} {
		if got := classifyPlacement(7, hub); len(got) != 0 {
			t.Fatalf("stable returned/transferred Hub placement findings = %v", got)
		}
	}

	bootstrap := strictBootstrapPending()
	if got := classifyPlacement(7, bootstrap); len(got) != 0 {
		t.Fatalf("strict unbound bootstrap findings = %v", got)
	}
	bound := proto.Clone(bootstrap).(*locatorv1.PlayerPlacementStorageRecord)
	bound.DsPodName = "hub-0"
	bound.DsInstanceUid = "hub-uid-0"
	bound.DsInstanceEpoch = 4
	bound.HubAssignmentId = "assignment-0"
	bound.ReleaseTrack = "stable"
	if got := classifyPlacement(7, bound); len(got) != 0 {
		t.Fatalf("strict bound bootstrap findings = %v", got)
	}
}

func TestClassifyPlacementRejectsStableWithoutRecoverablePhysicalTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  *locatorv1.PlayerPlacementStorageRecord
		want string
	}{
		{name: "legacy empty hub", want: "complete exact Hub target", rec: &locatorv1.PlayerPlacementStorageRecord{
			PlayerId: 7, Version: 1, OperationId: testOperationID,
			CurrentRoute:    locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
			TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
			ProofType:       locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP,
			ProofId:         "bootstrap-proof", AdmissionId: testAdmissionID, UpdatedAtMs: 100,
		}},
		{name: "battle target mismatch", want: "complete exact Battle target", rec: &locatorv1.PlayerPlacementStorageRecord{
			PlayerId: 7, Version: 9, OperationId: testOperationID,
			CurrentRoute:    locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
			TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
			MatchId:         88, TargetMatchId: 89, DsPodName: "battle-old", DsInstanceUid: "battle-uid",
			DsInstanceEpoch: 5, AllocationId: "allocation-old", ReleaseTrack: "stable",
			ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START,
			ProofId:   "match-start-proof", AdmissionId: testAdmissionID, UpdatedAtMs: 100,
			LastSourceDepartureProofType: locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE,
			LastSourceDepartureProofId:   "hub-departure-proof", LastSourceDeparturePlacementVersion: 9,
			LastSourceDepartureOperationId: testOperationID,
		}},
		{name: "active departure marker", want: "active source-departure gate", rec: &locatorv1.PlayerPlacementStorageRecord{
			PlayerId: 7, Version: 1, OperationId: testOperationID,
			CurrentRoute:    locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
			TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
			DsPodName:       "hub-stable", DsInstanceUid: "hub-stable-uid", DsInstanceEpoch: 4,
			HubAssignmentId: "assignment-stable", ReleaseTrack: "stable",
			ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP,
			ProofId:   "bootstrap-proof", AdmissionId: testAdmissionID, UpdatedAtMs: 100,
			SourceDepartureConfirmed: true,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyPlacement(7, tc.rec)
			if !hasReason(got, tc.want) {
				t.Fatalf("findings %v do not contain %q", got, tc.want)
			}
		})
	}
}

func TestClassifyPlacementAllowsLegacyStableAuditFieldsWhenExactSourceCanContinue(t *testing.T) {
	legacy := canonicalStableHubBootstrap()
	legacy.Version = 9
	legacy.UpdatedAtMs = 0
	legacy.ProofType = locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_UNSPECIFIED
	legacy.ProofId = ""
	legacy.AdmissionId = ""
	legacy.SourceMatchId = 88
	if got := classifyPlacement(7, legacy); len(got) != 0 {
		t.Fatalf("legacy STABLE with exact physical source must remain begin-able: findings=%v", got)
	}

	legacyBattle := canonicalStableBattle()
	legacyBattle.LastSourceDepartureProofType =
		locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED
	legacyBattle.LastSourceDepartureProofId = ""
	legacyBattle.LastSourceDeparturePlacementVersion = 0
	legacyBattle.LastSourceDepartureOperationId = ""
	if got := classifyPlacement(7, legacyBattle); len(got) != 0 {
		t.Fatalf("pre-source-departure STABLE Battle must remain begin-able: findings=%v", got)
	}

	leaseMarker := canonicalStableHubBootstrap()
	leaseMarker.LeaseDeadlineMs = 200
	if got := classifyPlacement(7, leaseMarker); !hasReason(got, "pending target route or lease") {
		t.Fatalf("findings = %v", got)
	}
}

func TestClassifyPlacementRejectsPartialOrMismatchedStableDepartureHistory(t *testing.T) {
	t.Run("Battle missing committed departure lineage", func(t *testing.T) {
		rec := canonicalStableBattle()
		rec.LastSourceDepartureProofId = ""
		if got := classifyPlacement(7, rec); !hasReason(got, "malformed source-departure audit history") {
			t.Fatalf("findings = %v", got)
		}
	})
	t.Run("Battle history from wrong version", func(t *testing.T) {
		rec := canonicalStableBattle()
		rec.LastSourceDeparturePlacementVersion--
		if got := classifyPlacement(7, rec); !hasReason(got, "does not match placement lineage") {
			t.Fatalf("findings = %v", got)
		}
	})
}

func TestClassifyPlacementRejectsNonStrictBootstrap(t *testing.T) {
	rec := strictBootstrapPending()
	rec.Version = 2
	rec.SourcePlacementVersion = 1
	rec.SourceOperationId = testSourceOperationID
	rec.SourceDsPodName = "hub-old"
	rec.SourceDsInstanceUid = "hub-old-uid"
	rec.SourceDsInstanceEpoch = 3
	rec.SourceHubAssignmentId = "old-assignment"
	rec.SourceReleaseTrack = "stable"
	rec.SourceDepartureConfirmed = true
	rec.SourceDepartureProofType = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE
	rec.SourceDepartureProofId = "departure-proof"
	rec.RetargetCount = 1

	got := classifyPlacement(7, rec)
	for _, want := range []string{"version=1", "no active physical source", "must not be a retargeted"} {
		if !hasReason(got, want) {
			t.Fatalf("findings %v do not contain %q", got, want)
		}
	}
}

func TestClassifyPlacementAllowsConfirmedHubAndBattleSources(t *testing.T) {
	hub := confirmedHubSourcePending()
	if got := classifyPlacement(7, hub); len(got) != 0 {
		t.Fatalf("Hub source findings = %v", got)
	}

	battle := confirmedBattleSourcePending()
	if got := classifyPlacement(7, battle); len(got) != 0 {
		t.Fatalf("Battle source findings = %v", got)
	}

	// Terminal cancellation of a not-yet-admitted Battle has a logical
	// terminal proof, but its physical source is still Hub. The departure proof
	// must therefore remain HUB_DEPARTURE.
	cancelled := confirmedHubSourcePending()
	cancelled.Version = 3
	cancelled.TargetRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB
	cancelled.TargetMatchId = 0
	cancelled.SourceMatchId = 88
	cancelled.ProofType = locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_TERMINAL
	if got := classifyPlacement(7, cancelled); len(got) != 0 {
		t.Fatalf("cancelled pre-admission Battle findings = %v", got)
	}
}

func TestClassifyPlacementRejectsMissingOrMismatchedSource(t *testing.T) {
	t.Run("missing tuple", func(t *testing.T) {
		rec := confirmedHubSourcePending()
		rec.SourcePlacementVersion = 0
		rec.SourceOperationId = ""
		rec.SourceDsInstanceUid = ""
		got := classifyPlacement(7, rec)
		for _, want := range []string{"source binding", "physical Hub source tuple"} {
			if !hasReason(got, want) {
				t.Fatalf("findings %v do not contain %q", got, want)
			}
		}
	})

	t.Run("partial unconfirmed marker", func(t *testing.T) {
		rec := confirmedBattleSourcePending()
		rec.SourceDepartureConfirmed = false
		if got := classifyPlacement(7, rec); !hasReason(got, "partial source-departure marker") {
			t.Fatalf("findings = %v", got)
		}
	})

	t.Run("proof mismatches physical source", func(t *testing.T) {
		rec := confirmedBattleSourcePending()
		rec.SourceDepartureProofType = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE
		if got := classifyPlacement(7, rec); !hasReason(got, "does not match physical source") {
			t.Fatalf("findings = %v", got)
		}
	})
}

func TestClassifyPlacementAllowsCanonicalUnconfirmedPendingAcrossLocatorRestart(t *testing.T) {
	boundBattle := confirmedHubSourcePending()
	boundBattle.DsPodName = "battle-new"
	boundBattle.DsInstanceUid = "battle-new-uid"
	boundBattle.DsInstanceEpoch = 5
	boundBattle.AllocationId = "allocation-new"
	boundBattle.ReleaseTrack = "stable"
	for _, rec := range []*locatorv1.PlayerPlacementStorageRecord{
		confirmedHubSourcePending(), boundBattle, confirmedBattleSourcePending(),
	} {
		rec.SourceDepartureConfirmed = false
		rec.SourceDepartureProofType = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED
		rec.SourceDepartureProofId = ""
		if got := classifyPlacement(7, rec); len(got) != 0 {
			t.Fatalf("canonical Begin-before-Confirm must not block replacement locator: findings=%v record=%v", got, rec)
		}
	}
}

func TestClassifyPlacementRejectsPendingThatCannotBindOrCommit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*locatorv1.PlayerPlacementStorageRecord)
		want   string
	}{
		{name: "zero updated timestamp", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.UpdatedAtMs = 0
		}, want: "updated_at_ms"},
		{name: "zero lease", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.LeaseDeadlineMs = 0
		}, want: "lease must be after"},
		{name: "lease before update", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.LeaseDeadlineMs = rec.UpdatedAtMs
		}, want: "lease must be after"},
		{name: "empty logical proof", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.ProofId = ""
		}, want: "empty proof_id"},
		{name: "wrong target proof", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.ProofType = locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_HUB_TRANSFER
		}, want: "Hub-to-Battle proof"},
		{name: "missing target match", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.TargetMatchId = 0
		}, want: "no target_match_id"},
		{name: "partial Battle target", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.DsPodName = "battle-new"
		}, want: "partially or incorrectly bound"},
		{name: "Battle target carries Hub assignment", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.DsPodName = "battle-new"
			rec.DsInstanceUid = "battle-new-uid"
			rec.DsInstanceEpoch = 5
			rec.AllocationId = "allocation-new"
			rec.ReleaseTrack = "stable"
			rec.HubAssignmentId = "foreign-assignment"
		}, want: "partially or incorrectly bound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := confirmedHubSourcePending()
			tc.mutate(rec)
			if got := classifyPlacement(7, rec); !hasReason(got, tc.want) {
				t.Fatalf("findings %v do not contain %q", got, tc.want)
			}
		})
	}

	t.Run("complete bound Battle target remains recoverable", func(t *testing.T) {
		rec := confirmedHubSourcePending()
		rec.DsPodName = "battle-new"
		rec.DsInstanceUid = "battle-new-uid"
		rec.DsInstanceEpoch = 5
		rec.AllocationId = "allocation-new"
		rec.ReleaseTrack = "stable"
		if got := classifyPlacement(7, rec); len(got) != 0 {
			t.Fatalf("findings = %v", got)
		}
	})

	t.Run("partial Hub target is permanently unbindable", func(t *testing.T) {
		rec := confirmedBattleSourcePending()
		rec.DsInstanceUid = "hub-new-uid"
		if got := classifyPlacement(7, rec); !hasReason(got, "partially or incorrectly bound") {
			t.Fatalf("findings = %v", got)
		}
	})

	t.Run("audit-only retarget metadata cannot strand a safe pending commit", func(t *testing.T) {
		rec := confirmedHubSourcePending()
		rec.RetargetCount = 1
		rec.LastRetargetProofId = "retarget-proof"
		rec.LastRetargetReason = locatorv1.PlacementTargetUnavailableReason(99)
		if got := classifyPlacement(7, rec); len(got) != 0 {
			t.Fatalf("findings = %v", got)
		}
	})
}

func TestClassifyPlacementRejectsMalformedRecordIdentity(t *testing.T) {
	rec := confirmedHubSourcePending()
	rec.Version = 0
	rec.TransitionState = locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_UNSPECIFIED
	got := classifyPlacement(8, rec)
	for _, want := range []string{"does not match key", "version is zero", "invalid transition_state"} {
		if !hasReason(got, want) {
			t.Fatalf("findings %v do not contain %q", got, want)
		}
	}
}

func TestParsePlacementRecordKey(t *testing.T) {
	if id, record, err := parsePlacementRecordKey("pandora:placement:{42}"); err != nil || !record || id != 42 {
		t.Fatalf("valid placement key: id=%d record=%t err=%v", id, record, err)
	}
	for _, key := range []string{
		"pandora:placement:proof:battle-exit:{42}:9",
		"pandora:placement:fence:battle-terminal:{42}:9",
	} {
		if id, record, err := parsePlacementRecordKey(key); err != nil || record || id != 0 {
			t.Fatalf("reserved key %q: id=%d record=%t err=%v", key, id, record, err)
		}
	}
	if _, _, err := parsePlacementRecordKey("pandora:placement:legacy:42"); err == nil {
		t.Fatal("unexpected placement namespace key must fail closed")
	}
	if _, record, err := parsePlacementRecordKey("pandora:placement:{18446744073709551616}"); err == nil || !record {
		t.Fatalf("overflow key: record=%t err=%v", record, err)
	}
}

func strictBootstrapPending() *locatorv1.PlayerPlacementStorageRecord {
	return &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 7, Version: 1, OperationId: testOperationID,
		CurrentRoute:    locatorv1.PlacementRoute_PLACEMENT_ROUTE_UNSPECIFIED,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		ProofType:       locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP,
		ProofId:         "bootstrap-proof", UpdatedAtMs: 100, LeaseDeadlineMs: 200,
	}
}

func confirmedHubSourcePending() *locatorv1.PlayerPlacementStorageRecord {
	return &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 7, Version: 2, OperationId: testOperationID,
		CurrentRoute:             locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TargetRoute:              locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState:          locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		TargetMatchId:            88,
		ProofType:                locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START,
		ProofId:                  "match-start-proof",
		UpdatedAtMs:              100,
		LeaseDeadlineMs:          200,
		SourcePlacementVersion:   1,
		SourceOperationId:        testSourceOperationID,
		SourceDsPodName:          "hub-old",
		SourceDsInstanceUid:      "hub-old-uid",
		SourceDsInstanceEpoch:    3,
		SourceHubAssignmentId:    "old-assignment",
		SourceReleaseTrack:       "stable",
		SourceDepartureConfirmed: true,
		SourceDepartureProofType: locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE,
		SourceDepartureProofId:   "hub-departure-proof",
	}
}

func confirmedBattleSourcePending() *locatorv1.PlayerPlacementStorageRecord {
	return &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 7, Version: 3, OperationId: testOperationID,
		CurrentRoute:    locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		MatchId:         88, SourceMatchId: 88,
		ProofType:                locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_TERMINAL,
		ProofId:                  "terminal-proof",
		UpdatedAtMs:              100,
		LeaseDeadlineMs:          200,
		SourcePlacementVersion:   2,
		SourceOperationId:        testSourceOperationID,
		SourceDsPodName:          "battle-old",
		SourceDsInstanceUid:      "battle-old-uid",
		SourceDsInstanceEpoch:    5,
		SourceAllocationId:       "allocation-old",
		SourceReleaseTrack:       "stable",
		SourceDepartureConfirmed: true,
		SourceDepartureProofType: locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_BATTLE_DEPARTURE,
		SourceDepartureProofId:   "battle-departure-proof",
	}
}

func canonicalStableHubBootstrap() *locatorv1.PlayerPlacementStorageRecord {
	return &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 7, Version: 1, OperationId: testOperationID,
		CurrentRoute:    locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
		DsPodName:       "hub-stable", DsInstanceUid: "hub-stable-uid",
		DsInstanceEpoch: 4, HubAssignmentId: "assignment-stable", ReleaseTrack: "stable",
		UpdatedAtMs: 100, ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP,
		ProofId: "bootstrap-proof", AdmissionId: testAdmissionID,
	}
}

func canonicalStableBattle() *locatorv1.PlayerPlacementStorageRecord {
	return &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 7, Version: 2, OperationId: testOperationID,
		CurrentRoute:    locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
		MatchId:         88, TargetMatchId: 88,
		DsPodName: "battle-stable", DsInstanceUid: "battle-stable-uid", DsInstanceEpoch: 5,
		AllocationId: "allocation-stable", ReleaseTrack: "stable", UpdatedAtMs: 100,
		ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START,
		ProofId:   "match-start-proof", AdmissionId: testAdmissionID,
		LastSourceDepartureProofType: locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE,
		LastSourceDepartureProofId:   "hub-departure-proof", LastSourceDeparturePlacementVersion: 2,
		LastSourceDepartureOperationId: testOperationID,
	}
}

func canonicalStableReturnedHub(proofType locatorv1.PlacementSourceDepartureProofType) *locatorv1.PlayerPlacementStorageRecord {
	return &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 7, Version: 3, OperationId: testOperationID,
		CurrentRoute:    locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
		SourceMatchId:   88,
		DsPodName:       "hub-return", DsInstanceUid: "hub-return-uid", DsInstanceEpoch: 6,
		HubAssignmentId: "assignment-return", ReleaseTrack: "stable", UpdatedAtMs: 100,
		ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_TERMINAL,
		ProofId:   "terminal-proof", AdmissionId: testAdmissionID,
		LastSourceDepartureProofType: proofType,
		LastSourceDepartureProofId:   "departure-proof", LastSourceDeparturePlacementVersion: 3,
		LastSourceDepartureOperationId: testOperationID,
	}
}

func canonicalStableHubTransfer() *locatorv1.PlayerPlacementStorageRecord {
	rec := canonicalStableReturnedHub(
		locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE)
	rec.SourceMatchId = 0
	rec.ProofType = locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_HUB_TRANSFER
	rec.ProofId = "hub-transfer-proof"
	return rec
}

func hasReason(got []string, want string) bool {
	for _, reason := range got {
		if strings.Contains(reason, want) {
			return true
		}
	}
	return false
}
