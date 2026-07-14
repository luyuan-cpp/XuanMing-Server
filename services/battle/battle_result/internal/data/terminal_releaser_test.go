package data

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/errcode"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
)

type terminalReleaseClientStub struct {
	dsv1.DSAllocatorServiceClient
	requests []*dsv1.ReleaseBattleRequest
	code     commonv1.ErrCode
	err      error
}

func (s *terminalReleaseClientStub) ReleaseBattle(
	_ context.Context,
	req *dsv1.ReleaseBattleRequest,
	_ ...grpc.CallOption,
) (*dsv1.ReleaseBattleResponse, error) {
	s.requests = append(s.requests, req)
	if s.err != nil {
		return nil, s.err
	}
	return &dsv1.ReleaseBattleResponse{Code: s.code}, nil
}

func TestGrpcTerminalReleaseRelayUsesExactTwoPhaseReasons(t *testing.T) {
	client := &terminalReleaseClientStub{code: commonv1.ErrCode_OK}
	relay := &GrpcTerminalReleaseRelay{cli: client}
	rec := TerminalReleaseRecord{
		MatchID: 11, AllocationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		DSPodName: "battle-11", GameserverUID: "uid-11", InstanceEpoch: 2,
		AuthGen: 3, AuthJTI: "j3", AuthExpMs: 99, AuthKid: "kid",
		AuthTokenSHA256: "hash", AuthWriterEpoch: 2, AuthorizedAtMs: 88,
	}
	if err := relay.ReleaseTerminal(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if err := relay.FinalizeTerminal(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if len(client.requests) != 2 || client.requests[0].GetReason() != "completed" ||
		client.requests[1].GetReason() != "completed-finalize" {
		t.Fatalf("two-phase reasons=%v", client.requests)
	}
	for _, req := range client.requests {
		if req.GetMatchId() != rec.MatchID || req.GetAllocationId() != rec.AllocationID ||
			req.GetGameserverUid() != rec.GameserverUID || req.GetInstanceEpoch() != rec.InstanceEpoch ||
			req.GetAuthJti() != rec.AuthJTI || req.GetAuthorizedAtMs() != rec.AuthorizedAtMs {
			t.Fatalf("persisted proof drifted in relay request: %+v", req)
		}
	}
}

func TestGrpcTerminalReleaseRelayNonOKIsNeverSuccess(t *testing.T) {
	client := &terminalReleaseClientStub{code: commonv1.ErrCode_ERR_DS_ALLOCATION_FAILED}
	relay := &GrpcTerminalReleaseRelay{cli: client}
	err := relay.FinalizeTerminal(context.Background(), TerminalReleaseRecord{MatchID: 12})
	if errcode.As(err) != errcode.ErrDSAllocationFailed {
		t.Fatalf("non-OK code=%v err=%v", errcode.As(err), err)
	}
}
