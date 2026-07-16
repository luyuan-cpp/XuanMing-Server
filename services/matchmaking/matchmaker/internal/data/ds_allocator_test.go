package data

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/pkg/battleabort"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/internalrpcauth"
	"github.com/luyuancpp/pandora/pkg/placement"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/model"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type allocatorResponseClient struct {
	dsv1.DSAllocatorServiceClient
	response *dsv1.AllocateBattleResponse
}

func (c allocatorResponseClient) AllocateBattle(context.Context, *dsv1.AllocateBattleRequest, ...grpc.CallOption) (*dsv1.AllocateBattleResponse, error) {
	return c.response, nil
}

func TestAllocateBattlePreservesAllocatorResponseCode(t *testing.T) {
	tests := []struct {
		name string
		code commonv1.ErrCode
		want errcode.Code
	}{
		{name: "unknown outcome stays retryable", code: commonv1.ErrCode_ERR_UNAVAILABLE, want: errcode.ErrUnavailable},
		{name: "definite failure stays definite", code: commonv1.ErrCode_ERR_DS_ALLOCATION_FAILED, want: errcode.ErrDSAllocationFailed},
		{name: "no capacity is not rewritten", code: commonv1.ErrCode_ERR_DS_NO_AVAILABLE, want: errcode.ErrDSNoAvailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allocator := &GrpcDSAllocator{cli: allocatorResponseClient{response: &dsv1.AllocateBattleResponse{Code: tt.code}}}
			if _, err := allocator.AllocateBattle(t.Context(), 9001, []uint64{1}, 1); errcode.As(err) != tt.want {
				t.Fatalf("AllocateBattle code = %d err=%v, want %d", errcode.As(err), err, tt.want)
			}
		})
	}
}

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

type allocatorAbortReplayStore struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func (s *allocatorAbortReplayStore) Consume(_ context.Context, key string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, found := s.seen[key]; found {
		return false, nil
	}
	s.seen[key] = struct{}{}
	return true, nil
}

type allocatorAbortClient struct {
	dsv1.DSAllocatorServiceClient
	verifier *internalrpcauth.Verifier
	request  *dsv1.AbortPreactiveBattleRequest
	verified error
}

func (c *allocatorAbortClient) AbortPreactiveBattle(
	ctx context.Context,
	req *dsv1.AbortPreactiveBattleRequest,
	_ ...grpc.CallOption,
) (*dsv1.AbortPreactiveBattleResponse, error) {
	c.request = req
	md, _ := metadata.FromOutgoingContext(ctx)
	incoming := metadata.NewIncomingContext(context.Background(), md.Copy())
	request := battleabort.Request{MatchID: req.GetMatchId(), OperationID: req.GetAllocationOperationId(), Target: placement.Target{
		PodName: req.GetDsPodName(), InstanceUID: req.GetGameserverUid(),
		InstanceEpoch: req.GetInstanceEpoch(), AllocationID: req.GetAllocationId(),
		ReleaseTrack: req.GetReleaseTrack(),
	}}
	c.verified = c.verifier.VerifyWithPayload(incoming,
		dsv1.DSAllocatorService_AbortPreactiveBattle_FullMethodName,
		request.MatchID, request.Canonical())
	return &dsv1.AbortPreactiveBattleResponse{Code: commonv1.ErrCode_OK}, nil
}

func TestAbortBattleAllocationSignsCanonicalFullRequest(t *testing.T) {
	const secret = "matchmaker-allocation-abort-test-secret-0123456789"
	const audience = "ds-allocator:battle-allocation-abort"
	signer, err := internalrpcauth.NewSigner(secret, "matchmaker", audience)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := internalrpcauth.NewVerifier(secret, "matchmaker", audience, 30*time.Second,
		&allocatorAbortReplayStore{seen: make(map[string]struct{})})
	if err != nil {
		t.Fatal(err)
	}
	client := &allocatorAbortClient{verifier: verifier}
	allocator := &GrpcDSAllocator{cli: client, abortAuth: signer}
	allocation := &model.BattleAllocation{Address: "10.0.0.1:7777", Target: placement.Target{
		PodName: "battle-1", InstanceUID: "uid-1", InstanceEpoch: 9,
		AllocationID: "allocation-1", ReleaseTrack: "stable",
	}}
	const operationID = "550e8400-e29b-41d4-a716-446655440000"
	if err := allocator.AbortBattleAllocation(t.Context(), 9002, operationID, allocation); err != nil {
		t.Fatal(err)
	}
	if client.verified != nil || client.request.GetMatchId() != 9002 ||
		client.request.GetAllocationOperationId() != operationID ||
		client.request.GetDsPodName() != allocation.Target.PodName ||
		client.request.GetGameserverUid() != allocation.Target.InstanceUID ||
		client.request.GetInstanceEpoch() != allocation.Target.InstanceEpoch ||
		client.request.GetAllocationId() != allocation.Target.AllocationID ||
		client.request.GetReleaseTrack() != allocation.Target.ReleaseTrack {
		t.Fatalf("request=%+v verify=%v", client.request, client.verified)
	}
}

func TestAbortBattleAllocationFailsClosedWithoutDedicatedSigner(t *testing.T) {
	allocator := &GrpcDSAllocator{}
	if err := allocator.AbortBattleAllocation(t.Context(), 1,
		"550e8400-e29b-41d4-a716-446655440000", &model.BattleAllocation{}); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("err=%v code=%v", err, errcode.As(err))
	}
}
