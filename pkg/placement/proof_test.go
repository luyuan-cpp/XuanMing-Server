package placement

import "testing"

func TestProofBindsEveryTransitionField(t *testing.T) {
	s, err := NewProofSigner("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	p := Proof{PlayerID: 1, ExpectedVersion: 3, SourceRoute: 2, TargetRoute: 1,
		SourceMatchID: 9, ProofType: 1, ProofID: "result:9", OperationID: "9849ab5b-2ecf-4fc3-983d-2d8df53cc009"}
	sig := s.Sign(p)
	if !s.Verify(p, sig) {
		t.Fatal("valid proof rejected")
	}
	p.SourceMatchID++
	if s.Verify(p, sig) {
		t.Fatal("mutated proof accepted")
	}
}

// transition 证明自 v1 起带 pandora-placement-transition-v1 域前缀:即便与
// retarget / source-departure 证明共用同一密钥,签名也不可跨类型重放。
func TestTransitionProofIsDomainSeparated(t *testing.T) {
	s, err := NewProofSigner("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	p := Proof{PlayerID: 42, ExpectedVersion: 7, SourceRoute: RouteHub, TargetRoute: RouteBattle,
		TargetMatchID: 90, ProofType: ProofMatchStart, ProofID: "match:90",
		OperationID: "9849ab5b-2ecf-4fc3-983d-2d8df53cc009"}
	sig := s.Sign(p)
	retarget := TargetUnavailableProof{PlayerID: p.PlayerID, PlacementVersion: p.ExpectedVersion,
		OperationID: p.OperationID, TargetRoute: p.TargetRoute, TargetMatchID: p.TargetMatchID,
		ProofType: p.ProofType, ProofID: p.ProofID}
	if s.VerifyTargetUnavailable(retarget, sig) {
		t.Fatal("transition signature replayed as retarget proof")
	}
	departure := SourceDepartureProof{PlayerID: p.PlayerID, PlacementVersion: p.ExpectedVersion,
		OperationID: p.OperationID, TargetRoute: p.TargetRoute, TargetMatchID: p.TargetMatchID,
		SourceRoute: p.SourceRoute, SourceMatchID: p.SourceMatchID,
		ProofType: p.ProofType, ProofID: p.ProofID}
	if s.VerifySourceDeparture(departure, sig) {
		t.Fatal("transition signature replayed as source-departure proof")
	}
}

func TestTargetUnavailableProofIsExactAndDomainSeparated(t *testing.T) {
	s, err := NewProofSigner("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	p := TargetUnavailableProof{PlayerID: 42, PlacementVersion: 7,
		OperationID: "11111111-1111-4111-8111-111111111111", TargetRoute: RouteHub,
		ExpectedTarget: Target{PodName: "hub-a", InstanceUID: "uid-a", InstanceEpoch: 2,
			AssignmentID: "assignment-a", ReleaseTrack: "stable"},
		ReplacementVersion: 8, ReplacementOperationID: "22222222-2222-4222-8222-222222222222",
		ReplacementTarget: Target{PodName: "hub-b", InstanceUID: "uid-b", InstanceEpoch: 3,
			AssignmentID: "assignment-b", ReleaseTrack: "stable"},
		ProofType: ProofHubTransfer, Reason: TargetUnavailableInstanceTerminated,
		ProofID: "hub-target-unavailable:uid-a"}
	sig := s.SignTargetUnavailable(p)
	if !s.VerifyTargetUnavailable(p, sig) {
		t.Fatal("exact target-unavailable proof did not verify")
	}
	mutated := p
	mutated.ReplacementTarget.InstanceUID = "uid-c"
	if s.VerifyTargetUnavailable(mutated, sig) {
		t.Fatal("proof verified after replacement target mutation")
	}
	transition := Proof{PlayerID: p.PlayerID, ExpectedVersion: p.PlacementVersion,
		TargetRoute: p.TargetRoute, ProofType: p.ProofType, ProofID: p.ProofID,
		OperationID: p.OperationID}
	if s.Verify(transition, sig) {
		t.Fatal("retarget proof replayed as transition proof")
	}
}

func TestSourceDepartureProofBindsExactLineageAndIsDomainSeparated(t *testing.T) {
	hubSigner, err := NewProofSigner("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	battleSigner, err := NewProofSigner("abcdef0123456789abcdef0123456789")
	if err != nil {
		t.Fatal(err)
	}
	p := SourceDepartureProof{PlayerID: 42, PlacementVersion: 8,
		OperationID: "22222222-2222-4222-8222-222222222222",
		TargetRoute: RouteBattle, TargetMatchID: 90,
		SourcePlacementVersion: 7,
		SourceOperationID:      "11111111-1111-4111-8111-111111111111",
		SourceRoute:            RouteHub,
		SourceTarget: Target{PodName: "hub-a", InstanceUID: "uid-a", InstanceEpoch: 2,
			AssignmentID: "assignment-a", ReleaseTrack: "stable"},
		ProofType: ProofHubDeparture, ProofID: "hub-departure:assignment-a:17"}
	sig := hubSigner.SignSourceDeparture(p)
	if !hubSigner.VerifySourceDeparture(p, sig) {
		t.Fatal("exact source-departure proof did not verify")
	}

	mutations := []struct {
		name   string
		mutate func(*SourceDepartureProof)
	}{
		{"player", func(v *SourceDepartureProof) { v.PlayerID++ }},
		{"placement-version", func(v *SourceDepartureProof) { v.PlacementVersion++ }},
		{"operation", func(v *SourceDepartureProof) { v.OperationID = "33333333-3333-4333-8333-333333333333" }},
		{"target-route", func(v *SourceDepartureProof) { v.TargetRoute = RouteHub }},
		{"target-match", func(v *SourceDepartureProof) { v.TargetMatchID++ }},
		{"source-version", func(v *SourceDepartureProof) { v.SourcePlacementVersion++ }},
		{"source-operation", func(v *SourceDepartureProof) { v.SourceOperationID = "44444444-4444-4444-8444-444444444444" }},
		{"source-route", func(v *SourceDepartureProof) { v.SourceRoute = RouteBattle }},
		{"source-match", func(v *SourceDepartureProof) { v.SourceMatchID++ }},
		{"source-pod", func(v *SourceDepartureProof) { v.SourceTarget.PodName = "hub-b" }},
		{"source-uid", func(v *SourceDepartureProof) { v.SourceTarget.InstanceUID = "uid-b" }},
		{"source-epoch", func(v *SourceDepartureProof) { v.SourceTarget.InstanceEpoch++ }},
		{"source-assignment", func(v *SourceDepartureProof) { v.SourceTarget.AssignmentID = "assignment-b" }},
		{"source-allocation", func(v *SourceDepartureProof) { v.SourceTarget.AllocationID = "unexpected" }},
		{"source-track", func(v *SourceDepartureProof) { v.SourceTarget.ReleaseTrack = "canary" }},
		{"proof-type", func(v *SourceDepartureProof) { v.ProofType = ProofBattleDeparture }},
		{"proof-id", func(v *SourceDepartureProof) { v.ProofID += ":other" }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			mutated := p
			tc.mutate(&mutated)
			if hubSigner.VerifySourceDeparture(mutated, sig) {
				t.Fatal("mutated source-departure proof verified")
			}
		})
	}
	if battleSigner.VerifySourceDeparture(p, sig) {
		t.Fatal("source-departure proof verified with the wrong authority key")
	}
	transition := Proof{PlayerID: p.PlayerID, ExpectedVersion: p.SourcePlacementVersion,
		SourceRoute: p.SourceRoute, TargetRoute: p.TargetRoute, TargetMatchID: p.TargetMatchID,
		ProofType: p.ProofType, ProofID: p.ProofID, OperationID: p.OperationID}
	if hubSigner.Verify(transition, sig) {
		t.Fatal("source-departure signature replayed as transition proof")
	}
	retarget := TargetUnavailableProof{PlayerID: p.PlayerID, PlacementVersion: p.PlacementVersion,
		OperationID: p.OperationID, TargetRoute: p.TargetRoute, TargetMatchID: p.TargetMatchID,
		ProofType: p.ProofType, ProofID: p.ProofID}
	if hubSigner.VerifyTargetUnavailable(retarget, sig) {
		t.Fatal("source-departure signature replayed as retarget proof")
	}
}

func TestProofKeyringSeparatesHubAndBattleDepartureAuthorities(t *testing.T) {
	keyring, err := NewProofKeyring(map[int32]string{
		ProofHubDeparture:    "0123456789abcdef0123456789abcdef",
		ProofBattleDeparture: "abcdef0123456789abcdef0123456789",
	})
	if err != nil {
		t.Fatal(err)
	}
	p := SourceDepartureProof{PlayerID: 1, PlacementVersion: 2,
		OperationID: "22222222-2222-4222-8222-222222222222", TargetRoute: RouteBattle,
		TargetMatchID: 3, SourcePlacementVersion: 1,
		SourceOperationID: "11111111-1111-4111-8111-111111111111", SourceRoute: RouteHub,
		SourceTarget: Target{PodName: "hub", InstanceUID: "uid", InstanceEpoch: 1,
			AssignmentID: "assignment", ReleaseTrack: "stable"},
		ProofType: ProofHubDeparture, ProofID: "departure"}
	hubSigner, _ := NewProofSigner("0123456789abcdef0123456789abcdef")
	if !keyring.VerifySourceDeparture(p, hubSigner.SignSourceDeparture(p)) {
		t.Fatal("keyring rejected Hub departure authority")
	}
	p.ProofType = ProofBattleDeparture
	if keyring.VerifySourceDeparture(p, hubSigner.SignSourceDeparture(p)) {
		t.Fatal("Hub key authenticated a Battle departure proof")
	}
}
