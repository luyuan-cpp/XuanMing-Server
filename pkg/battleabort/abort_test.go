package battleabort

import (
	"testing"

	"github.com/luyuancpp/pandora/pkg/placement"
)

func TestRequestCanonicalBindsEveryField(t *testing.T) {
	base := Request{MatchID: 42, OperationID: "550e8400-e29b-41d4-a716-446655440000", Target: placement.Target{
		PodName: "battle-42", InstanceUID: "uid-42", InstanceEpoch: 7,
		AllocationID: "alloc-42", ReleaseTrack: "stable",
	}}
	if !base.Complete() {
		t.Fatal("complete request rejected")
	}
	want := string(base.Canonical())
	mutations := []Request{
		{MatchID: 43, OperationID: base.OperationID, Target: base.Target},
		{MatchID: base.MatchID, OperationID: "550e8400-e29b-41d4-a716-446655440001", Target: base.Target},
		{MatchID: base.MatchID, OperationID: base.OperationID, Target: placement.Target{PodName: "other", InstanceUID: base.Target.InstanceUID, InstanceEpoch: base.Target.InstanceEpoch, AllocationID: base.Target.AllocationID, ReleaseTrack: base.Target.ReleaseTrack}},
		{MatchID: base.MatchID, OperationID: base.OperationID, Target: placement.Target{PodName: base.Target.PodName, InstanceUID: "other", InstanceEpoch: base.Target.InstanceEpoch, AllocationID: base.Target.AllocationID, ReleaseTrack: base.Target.ReleaseTrack}},
		{MatchID: base.MatchID, OperationID: base.OperationID, Target: placement.Target{PodName: base.Target.PodName, InstanceUID: base.Target.InstanceUID, InstanceEpoch: 8, AllocationID: base.Target.AllocationID, ReleaseTrack: base.Target.ReleaseTrack}},
		{MatchID: base.MatchID, OperationID: base.OperationID, Target: placement.Target{PodName: base.Target.PodName, InstanceUID: base.Target.InstanceUID, InstanceEpoch: base.Target.InstanceEpoch, AllocationID: "other", ReleaseTrack: base.Target.ReleaseTrack}},
		{MatchID: base.MatchID, OperationID: base.OperationID, Target: placement.Target{PodName: base.Target.PodName, InstanceUID: base.Target.InstanceUID, InstanceEpoch: base.Target.InstanceEpoch, AllocationID: base.Target.AllocationID, ReleaseTrack: "canary"}},
	}
	for i, mutation := range mutations {
		if got := string(mutation.Canonical()); got == want {
			t.Fatalf("mutation %d did not change canonical body", i)
		}
	}
}

func TestRequestCompleteRejectsPartialOrHubTarget(t *testing.T) {
	valid := Request{MatchID: 1, OperationID: "550e8400-e29b-41d4-a716-446655440000", Target: placement.Target{
		PodName: "pod", InstanceUID: "uid", InstanceEpoch: 1, AllocationID: "allocation", ReleaseTrack: "stable",
	}}
	invalid := []Request{
		{},
		{MatchID: valid.MatchID, OperationID: "not-an-operation", Target: valid.Target},
		{MatchID: valid.MatchID, OperationID: valid.OperationID, Target: placement.Target{PodName: "pod"}},
		{MatchID: valid.MatchID, OperationID: valid.OperationID, Target: placement.Target{PodName: "pod", InstanceUID: "uid", InstanceEpoch: 1, AllocationID: "allocation", ReleaseTrack: "stable", AssignmentID: "hub-assignment"}},
		{MatchID: valid.MatchID, OperationID: valid.OperationID, Target: placement.Target{PodName: "pod\nshift", InstanceUID: "uid", InstanceEpoch: 1, AllocationID: "allocation", ReleaseTrack: "stable"}},
		{MatchID: valid.MatchID, OperationID: valid.OperationID, Target: placement.Target{PodName: "pod", InstanceUID: "uid", InstanceEpoch: 1, AllocationID: "allocation", ReleaseTrack: "future"}},
	}
	for i, request := range invalid {
		if request.Complete() {
			t.Fatalf("invalid request %d accepted", i)
		}
	}
}

func TestCanonicalLengthPrefixPreventsFieldBoundaryCollision(t *testing.T) {
	left := Request{MatchID: 1, OperationID: "550e8400-e29b-41d4-a716-446655440000", Target: placement.Target{
		PodName: "a\nb", InstanceUID: "c", InstanceEpoch: 1, AllocationID: "allocation", ReleaseTrack: "stable",
	}}
	right := left
	right.Target.PodName = "a"
	right.Target.InstanceUID = "b\nc"
	if string(left.Canonical()) == string(right.Canonical()) {
		t.Fatal("canonical encoding collided after shifting a delimiter across fields")
	}
	if left.Complete() || right.Complete() {
		t.Fatal("control characters must also be rejected before signing")
	}
}
