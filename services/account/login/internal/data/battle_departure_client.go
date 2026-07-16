package data

import (
	"context"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
)

// BattleDepartureCoordinator proves that the exact source Battle owner has
// physically removed one player after the new PENDING->HUB version has already
// fenced every old Battle ticket.
// The implementation never accepts presence TTL or a missing battle record as
// proof; only the DS complete-snapshot ACK or an exact UID teardown journal can
// return departed=true.
type BattleDepartureCoordinator interface {
	EnsurePlayerDeparture(context.Context, uint64, uint64, placement.Binding, placement.Binding, placement.Target) error
}

type GrpcBattleDepartureCoordinator struct {
	client dsv1.DSAllocatorServiceClient
}

func NewGrpcBattleDepartureCoordinator(client dsv1.DSAllocatorServiceClient) *GrpcBattleDepartureCoordinator {
	return &GrpcBattleDepartureCoordinator{client: client}
}

func (g *GrpcBattleDepartureCoordinator) EnsurePlayerDeparture(ctx context.Context,
	playerID, matchID uint64, transition, source placement.Binding, target placement.Target,
) error {
	if g == nil || g.client == nil || playerID == 0 || matchID == 0 || !transition.Complete() || !source.Complete() ||
		!target.CompleteBattle() {
		return errcode.New(errcode.ErrInvalidArg, "complete pending Hub and source Battle departure identities required")
	}
	rpcCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := g.client.EnsurePlayerDeparture(rpcCtx, &dsv1.EnsurePlayerDepartureRequest{
		MatchId: matchID, PlayerId: playerID, PlacementVersion: transition.Version,
		OperationId: transition.OperationID, DsPodName: target.PodName,
		GameserverUid: target.InstanceUID, InstanceEpoch: target.InstanceEpoch,
		AllocationId: target.AllocationID, SourcePlacementVersion: source.Version,
		SourceOperationId: source.OperationID,
	})
	if err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err,
			"Battle physical departure result unknown")
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return errcode.New(errcode.Code(resp.GetCode()), "Battle physical departure rejected")
	}
	if !resp.GetDeparted() {
		return errcode.New(errcode.ErrUnavailable, "Battle physical departure is pending")
	}
	return nil
}
