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
