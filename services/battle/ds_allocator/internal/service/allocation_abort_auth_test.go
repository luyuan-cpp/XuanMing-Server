package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/battleabort"
	"github.com/luyuancpp/pandora/pkg/internalrpcauth"
	"github.com/luyuancpp/pandora/pkg/placement"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/biz"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
)

type abortReplayStore struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func (s *abortReplayStore) Consume(_ context.Context, key string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, found := s.seen[key]; found {
		return false, nil
	}
	s.seen[key] = struct{}{}
	return true, nil
}

func abortRequestProto() (*dsv1.AbortPreactiveBattleRequest, battleabort.Request) {
	request := battleabort.Request{MatchID: 77, OperationID: "550e8400-e29b-41d4-a716-446655440000", Target: placement.Target{
		PodName: "pod-77", InstanceUID: "uid-77", InstanceEpoch: 3,
		AllocationID: "e9d0243e-04cc-4140-804b-573355e85959", ReleaseTrack: "stable",
	}}
	return &dsv1.AbortPreactiveBattleRequest{
		MatchId: request.MatchID, AllocationOperationId: request.OperationID,
		DsPodName: request.Target.PodName, GameserverUid: request.Target.InstanceUID,
		InstanceEpoch: request.Target.InstanceEpoch, AllocationId: request.Target.AllocationID,
		ReleaseTrack: request.Target.ReleaseTrack,
	}, request
}

func signedAbortIncoming(t *testing.T, signer *internalrpcauth.Signer, request battleabort.Request) context.Context {
	t.Helper()
	outgoing, err := signer.SignContextWithPayload(context.Background(),
		dsv1.DSAllocatorService_AbortPreactiveBattle_FullMethodName,
		request.MatchID, request.Canonical())
	if err != nil {
		t.Fatal(err)
	}
	md, _ := metadata.FromOutgoingContext(outgoing)
	return metadata.NewIncomingContext(context.Background(), md.Copy())
}

func TestAbortPreactiveBattleRequiresExactPayloadAndRejectsReplay(t *testing.T) {
	const secret = "allocator-abort-auth-test-secret-0123456789abcdef"
	const audience = "ds-allocator:battle-allocation-abort"
	store := &abortReplayStore{seen: make(map[string]struct{})}
	signer, err := internalrpcauth.NewSigner(secret, "matchmaker", audience)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := internalrpcauth.NewVerifier(secret, "matchmaker", audience, 30*time.Second, store)
	if err != nil {
		t.Fatal(err)
	}
	// A non-Model-B usecase returns INVALID_STATE only after service auth has
	// passed, allowing this test to exercise the endpoint without Kubernetes.
	svc := NewAllocatorService(biz.NewAllocatorUsecase(nil, nil, conf.AllocatorConf{}))
	svc.SetAllocationAbortVerifier(verifier)

	protoRequest, canonical := abortRequestProto()
	mutated := proto.Clone(protoRequest).(*dsv1.AbortPreactiveBattleRequest)
	mutated.ReleaseTrack = "canary"
	ctx := signedAbortIncoming(t, signer, canonical)
	resp, err := svc.AbortPreactiveBattle(ctx, mutated)
	if err != nil || resp.GetCode() != commonv1.ErrCode_ERR_PERMISSION_DENY {
		t.Fatalf("mutated response=%v err=%v", resp.GetCode(), err)
	}

	ctx = signedAbortIncoming(t, signer, canonical)
	resp, err = svc.AbortPreactiveBattle(ctx, protoRequest)
	if err != nil || resp.GetCode() != commonv1.ErrCode_ERR_INVALID_STATE {
		t.Fatalf("authenticated response=%v err=%v", resp.GetCode(), err)
	}
	resp, err = svc.AbortPreactiveBattle(ctx, protoRequest)
	if err != nil || resp.GetCode() != commonv1.ErrCode_ERR_PERMISSION_DENY {
		t.Fatalf("replay response=%v err=%v", resp.GetCode(), err)
	}
}

func TestAbortPreactiveBattleFailsClosedWithoutVerifier(t *testing.T) {
	svc := NewAllocatorService(biz.NewAllocatorUsecase(nil, nil, conf.AllocatorConf{}))
	request, _ := abortRequestProto()
	resp, err := svc.AbortPreactiveBattle(context.Background(), request)
	if err != nil || resp.GetCode() != commonv1.ErrCode_ERR_UNAVAILABLE {
		t.Fatalf("response=%v err=%v", resp.GetCode(), err)
	}
}
