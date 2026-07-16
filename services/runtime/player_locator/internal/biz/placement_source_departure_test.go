package biz

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
)

const (
	departureCurrentOp = "123e4567-e89b-42d3-a456-426614174000"
	departureSourceOp  = "223e4567-e89b-42d3-a456-426614174001"
	departureNextOp    = "323e4567-e89b-42d3-a456-426614174002"
	departureAdmission = "423e4567-e89b-42d3-a456-426614174003"
)

type departureAuthorities struct {
	keyring *placement.ProofKeyring
	hub     *placement.ProofSigner
	battle  *placement.ProofSigner
	match   *placement.ProofSigner
	account *placement.ProofSigner
}

func newDepartureAuthorities(t *testing.T) departureAuthorities {
	t.Helper()
	const (
		hubSecret     = "hub-transfer-and-departure-authority-v1"
		battleSecret  = "battle-departure-independent-key-v1"
		matchSecret   = "match-start-independent-authority-v1"
		accountSecret = "account-bootstrap-independent-key-v1"
	)
	keyring, err := placement.NewProofKeyring(map[int32]string{
		placement.ProofHubTransfer:      hubSecret,
		placement.ProofHubDeparture:     hubSecret,
		placement.ProofBattleDeparture:  battleSecret,
		placement.ProofMatchStart:       matchSecret,
		placement.ProofAccountBootstrap: accountSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	mustSigner := func(secret string) *placement.ProofSigner {
		signer, signerErr := placement.NewProofSigner(secret)
		if signerErr != nil {
			t.Fatal(signerErr)
		}
		return signer
	}
	return departureAuthorities{keyring: keyring, hub: mustSigner(hubSecret),
		battle: mustSigner(battleSecret), match: mustSigner(matchSecret),
		account: mustSigner(accountSecret)}
}

func hubSourceTarget() placement.Target {
	return placement.Target{PodName: "hub-source", InstanceUID: "hub-source-uid",
		InstanceEpoch: 7, AssignmentID: "hub-source-assignment", ReleaseTrack: "stable"}
}

func battleTarget() placement.Target {
	return placement.Target{PodName: "battle-target", InstanceUID: "battle-target-uid",
		InstanceEpoch: 9, AllocationID: "battle-allocation", ReleaseTrack: "canary"}
}

func hubTarget() placement.Target {
	return placement.Target{PodName: "hub-target", InstanceUID: "hub-target-uid",
		InstanceEpoch: 11, AssignmentID: "hub-target-assignment", ReleaseTrack: "stable"}
}

func pendingHubToBattle(now time.Time) *locatorv1.PlayerPlacementStorageRecord {
	source, target := hubSourceTarget(), battleTarget()
	return &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 71, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		Version:         8, OperationId: departureCurrentOp, TargetMatchId: 9001,
		LeaseDeadlineMs: now.Add(time.Hour).UnixMilli(),
		ProofType:       locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START,
		ProofId:         "match-start:9001",
		DsPodName:       target.PodName, DsInstanceUid: target.InstanceUID,
		DsInstanceEpoch: target.InstanceEpoch, AllocationId: target.AllocationID,
		ReleaseTrack:           target.ReleaseTrack,
		SourcePlacementVersion: 7, SourceOperationId: departureSourceOp,
		SourceDsPodName: source.PodName, SourceDsInstanceUid: source.InstanceUID,
		SourceDsInstanceEpoch: source.InstanceEpoch,
		SourceHubAssignmentId: source.AssignmentID, SourceReleaseTrack: source.ReleaseTrack,
	}
}

func exactHubDeparture(rec *locatorv1.PlayerPlacementStorageRecord) ConfirmSourceDepartureInput {
	return ConfirmSourceDepartureInput{
		PlayerID: rec.GetPlayerId(), Version: rec.GetVersion(), OperationID: rec.GetOperationId(),
		TargetRoute: rec.GetTargetRoute(), TargetMatchID: rec.GetTargetMatchId(),
		SourceVersion: rec.GetSourcePlacementVersion(), SourceOperationID: rec.GetSourceOperationId(),
		SourceRoute:  locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		SourceTarget: recordSourceTarget(rec),
		ProofType:    locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE,
		ProofID:      "hub-departure:assignment:71",
	}
}

func signDeparture(in *ConfirmSourceDepartureInput, signer *placement.ProofSigner) {
	in.ProofSignature = signer.SignSourceDeparture(sourceDepartureProof(*in))
}

func battleCommit(rec *locatorv1.PlayerPlacementStorageRecord) CommitPlacementInput {
	target := recordTarget(rec)
	return CommitPlacementInput{BindPlacementInput: BindPlacementInput{
		PlayerID: rec.GetPlayerId(), Version: rec.GetVersion(), OperationID: rec.GetOperationId(),
		TargetRoute: rec.GetTargetRoute(), TargetMatchID: rec.GetTargetMatchId(),
		PodName: target.PodName, InstanceUID: target.InstanceUID, InstanceEpoch: target.InstanceEpoch,
		AllocationID: target.AllocationID, ReleaseTrack: target.ReleaseTrack,
	}, AdmissionID: departureAdmission}
}

func TestBindWithoutPhysicalDepartureCannotCommit(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	rec := pendingHubToBattle(now)
	repo := &memoryPlacementRepo{rec: rec}
	uc := NewPlacementUsecase(repo, newDepartureAuthorities(t).keyring)
	uc.now = func() time.Time { return now }

	bind := battleCommit(rec).BindPlacementInput
	if _, err := uc.Bind(context.Background(), bind); err != nil {
		t.Fatalf("legacy writer must still be able to bind before rollout: %v", err)
	}
	if _, err := uc.Commit(context.Background(), CommitPlacementInput{
		BindPlacementInput: bind, AdmissionID: departureAdmission,
	}); errcode.As(err) != errcode.ErrLocatorConflict {
		t.Fatalf("Bind without physical departure committed: code=%v err=%v", errcode.As(err), err)
	}
	stored, _, _ := repo.GetPlacement(context.Background(), rec.GetPlayerId())
	if stored.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING ||
		stored.GetAdmissionId() != "" || stored.GetSourceDepartureConfirmed() {
		t.Fatalf("failed gate mutated pending placement: %+v", stored)
	}
}

func TestExactPhysicalDepartureCommitsAuditsAndStableReconnects(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	auth := newDepartureAuthorities(t)
	rec := pendingHubToBattle(now)
	repo := &memoryPlacementRepo{rec: rec}
	uc := NewPlacementUsecase(repo, auth.keyring)
	uc.now = func() time.Time { return now }
	in := exactHubDeparture(rec)
	signDeparture(&in, auth.hub)

	confirmed, err := uc.ConfirmSourceDeparture(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !confirmed.GetSourceDepartureConfirmed() || confirmed.GetSourceDepartureProofId() != in.ProofID {
		t.Fatalf("confirmation missing: %+v", confirmed)
	}
	replayed, err := uc.ConfirmSourceDeparture(context.Background(), in)
	if err != nil || !proto.Equal(confirmed, replayed) {
		t.Fatalf("exact confirmation replay=%+v err=%v", replayed, err)
	}

	committed, err := uc.Commit(context.Background(), battleCommit(rec))
	if err != nil {
		t.Fatal(err)
	}
	if committed.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE ||
		committed.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE ||
		committed.GetSourceDepartureConfirmed() || committed.GetSourceDepartureProofId() != "" ||
		committed.GetSourcePlacementVersion() != 0 || committed.GetSourceOperationId() != "" ||
		committed.GetLastSourceDepartureProofType() != in.ProofType ||
		committed.GetLastSourceDepartureProofId() != in.ProofID ||
		committed.GetLastSourceDeparturePlacementVersion() != rec.GetVersion() ||
		committed.GetLastSourceDepartureOperationId() != rec.GetOperationId() {
		t.Fatalf("committed source audit/clear mismatch: %+v", committed)
	}

	reconnect := battleCommit(rec)
	reconnect.AdmissionID = "523e4567-e89b-42d3-a456-426614174004"
	reconnected, err := uc.Commit(context.Background(), reconnect)
	if err != nil {
		t.Fatalf("same stable target reconnect failed: %v", err)
	}
	if reconnected.GetAdmissionId() != departureAdmission ||
		reconnected.GetLastSourceDepartureProofId() != in.ProofID {
		t.Fatalf("stable reconnect mutated admission/audit: %+v", reconnected)
	}
}

func TestStableReconnectRejectsMalformedCanonicalShape(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	auth := newDepartureAuthorities(t)
	pending := pendingHubToBattle(now)
	repo := &memoryPlacementRepo{rec: pending}
	uc := NewPlacementUsecase(repo, auth.keyring)
	uc.now = func() time.Time { return now }
	departure := exactHubDeparture(pending)
	signDeparture(&departure, auth.hub)
	if _, err := uc.ConfirmSourceDeparture(context.Background(), departure); err != nil {
		t.Fatal(err)
	}
	stable, err := uc.Commit(context.Background(), battleCommit(pending))
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string]func(*locatorv1.PlayerPlacementStorageRecord){
		"pending target route": func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.TargetRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE
		},
		"stale source binding": func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.SourcePlacementVersion = 1
			rec.SourceOperationId = departureSourceOp
		},
		"wrong current match": func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.MatchId++
		},
		"active departure marker": func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.SourceDepartureConfirmed = true
			rec.SourceDepartureProofType = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE
			rec.SourceDepartureProofId = "stale-departure"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			malformed := proto.Clone(stable).(*locatorv1.PlayerPlacementStorageRecord)
			mutate(malformed)
			caseRepo := &memoryPlacementRepo{rec: malformed}
			caseUC := NewPlacementUsecase(caseRepo, auth.keyring)
			caseUC.now = func() time.Time { return now }
			reconnect := battleCommit(pending)
			reconnect.AdmissionID = "523e4567-e89b-42d3-a456-426614174004"
			if _, gotErr := caseUC.Commit(context.Background(), reconnect); errcode.As(gotErr) != errcode.ErrLocatorConflict {
				t.Fatalf("malformed stable reconnect code=%v err=%v", errcode.As(gotErr), gotErr)
			}
		})
	}
}

func TestPhysicalDepartureRejectsForgeryWrongAuthorityAndTuple(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	auth := newDepartureAuthorities(t)
	base := pendingHubToBattle(now)

	tests := []struct {
		name string
		edit func(*ConfirmSourceDepartureInput)
		sign *placement.ProofSigner
		code errcode.Code
	}{
		{name: "forged signature", edit: func(in *ConfirmSourceDepartureInput) {
			in.ProofSignature = "forged"
		}, code: errcode.ErrPermissionDeny},
		{name: "battle key cannot attest Hub", sign: auth.battle, code: errcode.ErrPermissionDeny},
		{name: "old source version", edit: func(in *ConfirmSourceDepartureInput) {
			in.SourceVersion--
		}, sign: auth.hub, code: errcode.ErrLocatorConflict},
		{name: "wrong source operation", edit: func(in *ConfirmSourceDepartureInput) {
			in.SourceOperationID = departureNextOp
		}, sign: auth.hub, code: errcode.ErrLocatorConflict},
		{name: "wrong source pod", edit: func(in *ConfirmSourceDepartureInput) {
			in.SourceTarget.PodName = "same-name-wrong-instance"
		}, sign: auth.hub, code: errcode.ErrLocatorConflict},
		{name: "wrong source uid", edit: func(in *ConfirmSourceDepartureInput) {
			in.SourceTarget.InstanceUID = "replacement-uid"
		}, sign: auth.hub, code: errcode.ErrLocatorConflict},
		{name: "wrong target match", edit: func(in *ConfirmSourceDepartureInput) {
			in.TargetMatchID++
		}, sign: auth.hub, code: errcode.ErrLocatorConflict},
		{name: "wrong target route", edit: func(in *ConfirmSourceDepartureInput) {
			in.TargetRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB
			in.TargetMatchID = 0
		}, sign: auth.hub, code: errcode.ErrLocatorConflict},
		{name: "wrong physical source route", edit: func(in *ConfirmSourceDepartureInput) {
			in.SourceRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE
			in.SourceMatchID = 9001
			in.SourceTarget = battleTarget()
			in.ProofType = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_BATTLE_DEPARTURE
		}, sign: auth.battle, code: errcode.ErrLocatorConflict},
		{name: "wrong current operation", edit: func(in *ConfirmSourceDepartureInput) {
			in.OperationID = departureNextOp
		}, sign: auth.hub, code: errcode.ErrLocatorConflict},
		{name: "wrong proof type", edit: func(in *ConfirmSourceDepartureInput) {
			in.ProofType = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_BATTLE_DEPARTURE
		}, sign: auth.battle, code: errcode.ErrInvalidArg},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := &memoryPlacementRepo{rec: proto.Clone(base).(*locatorv1.PlayerPlacementStorageRecord)}
			uc := NewPlacementUsecase(repo, auth.keyring)
			uc.now = func() time.Time { return now }
			in := exactHubDeparture(base)
			if tc.edit != nil {
				tc.edit(&in)
			}
			if tc.sign != nil {
				signDeparture(&in, tc.sign)
			}
			if _, err := uc.ConfirmSourceDeparture(context.Background(), in); errcode.As(err) != tc.code {
				t.Fatalf("code=%v want=%v err=%v", errcode.As(err), tc.code, err)
			}
			stored, _, _ := repo.GetPlacement(context.Background(), base.GetPlayerId())
			if stored.GetSourceDepartureConfirmed() {
				t.Fatalf("rejected proof mutated record: %+v", stored)
			}
		})
	}
}

func TestBattleSourceRequiresIndependentBattleDepartureAuthority(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	auth := newDepartureAuthorities(t)
	source, target := battleTarget(), hubTarget()
	rec := &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 72, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE, MatchId: 9001,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		Version:         9, OperationId: departureCurrentOp, SourceMatchId: 9001,
		LeaseDeadlineMs: now.Add(time.Hour).UnixMilli(),
		DsPodName:       target.PodName, DsInstanceUid: target.InstanceUID,
		DsInstanceEpoch: target.InstanceEpoch, HubAssignmentId: target.AssignmentID,
		ReleaseTrack:           target.ReleaseTrack,
		SourcePlacementVersion: 8, SourceOperationId: departureSourceOp,
		SourceDsPodName: source.PodName, SourceDsInstanceUid: source.InstanceUID,
		SourceDsInstanceEpoch: source.InstanceEpoch, SourceAllocationId: source.AllocationID,
		SourceReleaseTrack: source.ReleaseTrack,
	}
	repo := &memoryPlacementRepo{rec: rec}
	uc := NewPlacementUsecase(repo, auth.keyring)
	uc.now = func() time.Time { return now }
	in := ConfirmSourceDepartureInput{
		PlayerID: rec.GetPlayerId(), Version: rec.GetVersion(), OperationID: rec.GetOperationId(),
		TargetRoute: rec.GetTargetRoute(), SourceVersion: rec.GetSourcePlacementVersion(),
		SourceOperationID: rec.GetSourceOperationId(), SourceRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		SourceMatchID: rec.GetMatchId(), SourceTarget: source,
		ProofType: locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_BATTLE_DEPARTURE,
		ProofID:   "battle-departure:9001:72",
	}
	signDeparture(&in, auth.hub)
	if _, err := uc.ConfirmSourceDeparture(context.Background(), in); errcode.As(err) != errcode.ErrPermissionDeny {
		t.Fatalf("Hub authority attested Battle departure: code=%v err=%v", errcode.As(err), err)
	}
	signDeparture(&in, auth.battle)
	confirmed, err := uc.ConfirmSourceDeparture(context.Background(), in)
	if err != nil || !confirmed.GetSourceDepartureConfirmed() {
		t.Fatalf("Battle authority confirmation=%+v err=%v", confirmed, err)
	}
}

func TestTerminalCancellationUsesPhysicalHubSourceMatchZero(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	auth := newDepartureAuthorities(t)
	source, target := hubSourceTarget(), hubTarget()
	rec := &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 73, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		Version:         10, OperationId: departureCurrentOp, SourceMatchId: 9001,
		LeaseDeadlineMs: now.Add(time.Hour).UnixMilli(),
		DsPodName:       target.PodName, DsInstanceUid: target.InstanceUID,
		DsInstanceEpoch: target.InstanceEpoch, HubAssignmentId: target.AssignmentID,
		ReleaseTrack:           target.ReleaseTrack,
		SourcePlacementVersion: 8, SourceOperationId: departureSourceOp,
		SourceDsPodName: source.PodName, SourceDsInstanceUid: source.InstanceUID,
		SourceDsInstanceEpoch: source.InstanceEpoch,
		SourceHubAssignmentId: source.AssignmentID, SourceReleaseTrack: source.ReleaseTrack,
	}
	repo := &memoryPlacementRepo{rec: rec}
	uc := NewPlacementUsecase(repo, auth.keyring)
	uc.now = func() time.Time { return now }
	in := exactHubDeparture(rec)
	in.SourceMatchID = 0
	in.ProofID = "hub-departure:terminal-cancel:73"
	signDeparture(&in, auth.hub)
	confirmed, err := uc.ConfirmSourceDeparture(context.Background(), in)
	if err != nil || !confirmed.GetSourceDepartureConfirmed() {
		t.Fatalf("logical source_match_id leaked into physical proof: %+v err=%v", confirmed, err)
	}
}

func TestRetargetClearsConfirmationAndRejectsOldProof(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	auth := newDepartureAuthorities(t)
	rec := pendingHubToBattle(now)
	repo := &memoryPlacementRepo{rec: rec}
	uc := NewPlacementUsecase(repo, auth.keyring)
	uc.now = func() time.Time { return now }
	oldConfirm := exactHubDeparture(rec)
	signDeparture(&oldConfirm, auth.hub)
	if _, err := uc.ConfirmSourceDeparture(context.Background(), oldConfirm); err != nil {
		t.Fatal(err)
	}

	replacement := placement.Target{PodName: "battle-replacement", InstanceUID: "battle-replacement-uid",
		InstanceEpoch: 10, AllocationID: "battle-replacement-allocation", ReleaseTrack: "stable"}
	retarget := RetargetPlacementInput{
		PlayerID: rec.GetPlayerId(), Version: rec.GetVersion(), OperationID: rec.GetOperationId(),
		TargetRoute: rec.GetTargetRoute(), TargetMatchID: rec.GetTargetMatchId(),
		ExpectedTarget: recordTarget(rec), ReplacementVersion: rec.GetVersion() + 1,
		ReplacementOperationID: departureNextOp, ReplacementTarget: replacement,
		ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START,
		Reason:    locatorv1.PlacementTargetUnavailableReason_PLACEMENT_TARGET_UNAVAILABLE_REASON_INSTANCE_TERMINATED,
		ProofID:   "target-unavailable:battle:9001", LeaseDeadlineMs: now.Add(2 * time.Hour).UnixMilli(),
	}
	retarget.ProofSignature = auth.match.SignTargetUnavailable(retargetProof(retarget))
	retargeted, err := uc.Retarget(context.Background(), retarget)
	if err != nil {
		t.Fatal(err)
	}
	if retargeted.GetSourceDepartureConfirmed() || retargeted.GetSourceDepartureProofId() != "" {
		t.Fatalf("retarget preserved stale confirmation: %+v", retargeted)
	}
	if _, err := uc.ConfirmSourceDeparture(context.Background(), oldConfirm); errcode.As(err) != errcode.ErrLocatorConflict {
		t.Fatalf("old confirmation replay code=%v err=%v", errcode.As(err), err)
	}
	if _, err := uc.Commit(context.Background(), CommitPlacementInput{BindPlacementInput: BindPlacementInput{
		PlayerID: rec.GetPlayerId(), Version: retarget.ReplacementVersion,
		OperationID: retarget.ReplacementOperationID, TargetRoute: rec.GetTargetRoute(),
		TargetMatchID: rec.GetTargetMatchId(), PodName: replacement.PodName,
		InstanceUID: replacement.InstanceUID, InstanceEpoch: replacement.InstanceEpoch,
		AllocationID: replacement.AllocationID, ReleaseTrack: replacement.ReleaseTrack,
	}, AdmissionID: departureAdmission}); errcode.As(err) != errcode.ErrLocatorConflict {
		t.Fatalf("retarget admitted without new confirmation: code=%v err=%v", errcode.As(err), err)
	}
	newConfirm := oldConfirm
	newConfirm.Version = retarget.ReplacementVersion
	newConfirm.OperationID = retarget.ReplacementOperationID
	newConfirm.ProofID = "hub-departure:retarget:71"
	signDeparture(&newConfirm, auth.hub)
	if _, err := uc.ConfirmSourceDeparture(context.Background(), newConfirm); err != nil {
		t.Fatalf("new exact confirmation failed: %v", err)
	}
}

func TestBeginSourceDepartureConfirmationResetRules(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	auth := newDepartureAuthorities(t)
	t.Run("exact pending retry preserves confirmation", func(t *testing.T) {
		rec := pendingHubToBattle(now)
		rec.SourceDepartureConfirmed = true
		rec.SourceDepartureProofType = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE
		rec.SourceDepartureProofId = "hub-departure:already-confirmed"
		repo := &memoryPlacementRepo{rec: rec}
		uc := NewPlacementUsecase(repo, auth.keyring)
		uc.now = func() time.Time { return now }
		in := BeginPlacementInput{PlayerID: rec.GetPlayerId(), ExpectedVersion: rec.GetVersion() - 1,
			TargetRoute: rec.GetTargetRoute(), OperationID: rec.GetOperationId(),
			TargetMatchID: rec.GetTargetMatchId(), ProofType: rec.GetProofType(), ProofID: rec.GetProofId(),
			LeaseDeadlineMs: now.Add(2 * time.Hour).UnixMilli()}
		in.ProofSignature = auth.match.Sign(placement.Proof{PlayerID: in.PlayerID,
			ExpectedVersion: in.ExpectedVersion, SourceRoute: placement.RouteHub,
			TargetRoute: placement.RouteBattle, TargetMatchID: in.TargetMatchID,
			ProofType: placement.ProofMatchStart, ProofID: in.ProofID, OperationID: in.OperationID})
		got, err := uc.Begin(context.Background(), in)
		if err != nil || !got.GetSourceDepartureConfirmed() ||
			got.GetSourceDepartureProofId() != rec.GetSourceDepartureProofId() {
			t.Fatalf("same pending Begin lost confirmation: %+v err=%v", got, err)
		}
	})

	t.Run("new transition clears stale confirmation", func(t *testing.T) {
		source := hubSourceTarget()
		rec := &locatorv1.PlayerPlacementStorageRecord{
			PlayerId: 75, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
			TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
			Version:         7, OperationId: departureSourceOp, AdmissionId: departureAdmission,
			DsPodName: source.PodName, DsInstanceUid: source.InstanceUID,
			DsInstanceEpoch: source.InstanceEpoch, HubAssignmentId: source.AssignmentID,
			ReleaseTrack:             source.ReleaseTrack,
			SourceDepartureConfirmed: true,
			SourceDepartureProofType: locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE,
			SourceDepartureProofId:   "stale-confirmation",
		}
		repo := &memoryPlacementRepo{rec: rec}
		uc := NewPlacementUsecase(repo, auth.keyring)
		uc.now = func() time.Time { return now }
		in := BeginPlacementInput{PlayerID: rec.GetPlayerId(), ExpectedVersion: rec.GetVersion(),
			TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
			OperationID: departureCurrentOp, ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_HUB_TRANSFER,
			ProofID: "hub-transfer:75", LeaseDeadlineMs: now.Add(time.Hour).UnixMilli()}
		in.ProofSignature = auth.hub.Sign(placement.Proof{PlayerID: in.PlayerID,
			ExpectedVersion: in.ExpectedVersion, SourceRoute: placement.RouteHub,
			TargetRoute: placement.RouteHub, ProofType: placement.ProofHubTransfer,
			ProofID: in.ProofID, OperationID: in.OperationID})
		got, err := uc.Begin(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if got.GetSourceDepartureConfirmed() || got.GetSourceDepartureProofId() != "" ||
			got.GetSourceDepartureProofType() != locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED {
			t.Fatalf("new Begin preserved stale confirmation: %+v", got)
		}
	})
}

func TestAccountBootstrapCommitsWithoutPhysicalSource(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	auth := newDepartureAuthorities(t)
	repo := &memoryPlacementRepo{}
	uc := NewPlacementUsecase(repo, auth.keyring)
	uc.now = func() time.Time { return now }
	bootstrap := BootstrapPlacementInput{PlayerID: 74, OperationID: departureCurrentOp,
		ProofID: "account-bootstrap:74", LeaseDeadlineMs: now.Add(time.Hour).UnixMilli()}
	bootstrap.ProofSignature = auth.account.Sign(placement.Proof{PlayerID: bootstrap.PlayerID,
		TargetRoute: placement.RouteHub, ProofType: placement.ProofAccountBootstrap,
		ProofID: bootstrap.ProofID, OperationID: bootstrap.OperationID})
	if _, err := uc.Bootstrap(context.Background(), bootstrap); err != nil {
		t.Fatal(err)
	}
	target := hubTarget()
	bind := BindPlacementInput{PlayerID: bootstrap.PlayerID, Version: 1,
		OperationID: bootstrap.OperationID, TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		PodName: target.PodName, InstanceUID: target.InstanceUID, InstanceEpoch: target.InstanceEpoch,
		AssignmentID: target.AssignmentID, ReleaseTrack: target.ReleaseTrack}
	if _, err := uc.Bind(context.Background(), bind); err != nil {
		t.Fatal(err)
	}
	committed, err := uc.Commit(context.Background(), CommitPlacementInput{
		BindPlacementInput: bind, AdmissionID: departureAdmission,
	})
	if err != nil || committed.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB ||
		committed.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE {
		t.Fatalf("bootstrap commit=%+v err=%v", committed, err)
	}
}
