package data

import (
	"testing"

	"github.com/luyuancpp/pandora/pkg/placement"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/model"
)

func TestIsB1BoundBattleResponse(t *testing.T) {
	tests := []struct {
		name string
		resp *dsv1.AllocateBattleResponse
		want bool
	}{
		{name: "nil"},
		{name: "legacy", resp: &dsv1.AllocateBattleResponse{DsAddr: "127.0.0.1:7777"}},
		{name: "uid", resp: &dsv1.AllocateBattleResponse{GameserverUid: "uid-1"}, want: true},
		{name: "epoch", resp: &dsv1.AllocateBattleResponse{InstanceEpoch: 1}, want: true},
		{name: "allocation", resp: &dsv1.AllocateBattleResponse{AllocationId: "alloc-1"}, want: true},
		{name: "release-track", resp: &dsv1.AllocateBattleResponse{ReleaseTrack: "stable"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isB1BoundBattleResponse(tt.resp); got != tt.want {
				t.Fatalf("isB1BoundBattleResponse() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSignBattleTicketWithoutSignerFailsClosed(t *testing.T) {
	g := &GrpcDSAllocator{}
	if _, err := g.SignBattleTicket(t.Context(), 42, 9001, &model.BattleAllocation{}, placement.Binding{}); err == nil {
		t.Fatal("SignBattleTicket() without any signer must fail closed")
	}
}
