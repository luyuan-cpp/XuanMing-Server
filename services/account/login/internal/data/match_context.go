package data

import (
	"context"

	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/errcode"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"
)

// MatchContextState is deliberately three-valued. UNKNOWN includes transport
// errors, corrupt records and index/claim drift; callers must never collapse it
// into NONE or route the player to Hub.
type MatchContextState uint8

const (
	MatchContextUnknown MatchContextState = iota
	MatchContextNone
	MatchContextActive
)

type MatchContextStage uint8

const (
	MatchStageUnknown MatchContextStage = iota
	MatchStageStarting
	MatchStageQueued
	MatchStageConfirming
	MatchStageAllocating
	MatchStageReady
)

type MatchResumeContext struct {
	State    MatchContextState
	Stage    MatchContextStage
	TicketID uint64
	MatchID  uint64
	DSAddr   string
	BattleTicket string
	PlacementVersion uint64
	PlacementOperationID string
}

// MatchResumeReader reads canonical durable match/start-operation state. It is
// read-only: Login cannot advance, compensate or infer a match from presence.
type MatchResumeReader interface {
	ResolvePlayerMatchContext(context.Context, uint64) (MatchResumeContext, error)
}

type GrpcMatchResumeReader struct {
	client matchv1.MatchServiceClient
}

func NewGrpcMatchResumeReader(conn *grpc.ClientConn) *GrpcMatchResumeReader {
	return &GrpcMatchResumeReader{client: matchv1.NewMatchServiceClient(conn)}
}

func (r *GrpcMatchResumeReader) ResolvePlayerMatchContext(ctx context.Context, playerID uint64) (MatchResumeContext, error) {
	if r == nil || r.client == nil || playerID == 0 {
		return MatchResumeContext{State: MatchContextUnknown},
			errcode.New(errcode.ErrUnavailable, "match resume authority unavailable")
	}
	resp, err := r.client.ResolvePlayerMatchContext(ctx, &matchv1.ResolvePlayerMatchContextRequest{PlayerId: playerID})
	if err != nil {
		return MatchResumeContext{State: MatchContextUnknown},
			errcode.NewCause(errcode.ErrUnavailable, err, "match resume authority unavailable")
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return MatchResumeContext{State: MatchContextUnknown},
			errcode.New(errcode.Code(resp.GetCode()), "match resume authority rejected read")
	}
	out := MatchResumeContext{TicketID: resp.GetTicketId(), MatchID: resp.GetMatchId(),
		DSAddr: resp.GetBattleDsAddr(), BattleTicket: resp.GetBattleTicket(),
		PlacementVersion: resp.GetPlacementVersion(), PlacementOperationID: resp.GetPlacementOperationId()}
	switch resp.GetState() {
	case matchv1.PlayerMatchContextState_PLAYER_MATCH_CONTEXT_STATE_NONE:
		out.State = MatchContextNone
		if out.TicketID != 0 || out.MatchID != 0 || out.DSAddr != "" || out.BattleTicket != "" {
			return MatchResumeContext{State: MatchContextUnknown},
				errcode.New(errcode.ErrUnavailable, "NONE match context contains active identity")
		}
		return out, nil
	case matchv1.PlayerMatchContextState_PLAYER_MATCH_CONTEXT_STATE_ACTIVE:
		out.State = MatchContextActive
	default:
		return MatchResumeContext{State: MatchContextUnknown},
			errcode.New(errcode.ErrUnavailable, "match context is UNKNOWN")
	}
	switch resp.GetStage() {
	case matchv1.PlayerMatchResumeStage_PLAYER_MATCH_RESUME_STAGE_STARTING:
		out.Stage = MatchStageStarting
	case matchv1.PlayerMatchResumeStage_PLAYER_MATCH_RESUME_STAGE_QUEUED:
		out.Stage = MatchStageQueued
	case matchv1.PlayerMatchResumeStage_PLAYER_MATCH_RESUME_STAGE_CONFIRMING:
		out.Stage = MatchStageConfirming
	case matchv1.PlayerMatchResumeStage_PLAYER_MATCH_RESUME_STAGE_ALLOCATING:
		out.Stage = MatchStageAllocating
	case matchv1.PlayerMatchResumeStage_PLAYER_MATCH_RESUME_STAGE_READY:
		out.Stage = MatchStageReady
	default:
		return MatchResumeContext{State: MatchContextUnknown},
			errcode.New(errcode.ErrUnavailable, "active match context stage is UNKNOWN")
	}
	if out.TicketID == 0 || (out.Stage >= MatchStageConfirming && out.MatchID == 0) {
		return MatchResumeContext{State: MatchContextUnknown},
			errcode.New(errcode.ErrUnavailable, "active match context identity incomplete")
	}
	return out, nil
}
