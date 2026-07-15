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
