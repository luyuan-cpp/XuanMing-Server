package data

import (
	"context"
	"sort"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
)

// GrpcHubDepartureCoordinator drives hub_allocator's durable exact cleanup.
// ReleaseHub must not return OK while a connected source owner still awaits an
// eviction order/Departure ACK, so retrying this loop is crash-safe.
type GrpcHubDepartureCoordinator struct {
	client hubv1.HubAllocatorServiceClient
}

func NewGrpcHubDepartureCoordinator(client hubv1.HubAllocatorServiceClient) *GrpcHubDepartureCoordinator {
	return &GrpcHubDepartureCoordinator{client: client}
}

func (g *GrpcHubDepartureCoordinator) EnsureHubDeparted(ctx context.Context, matchID uint64,
	operationID string, playerIDs []uint64, bindings map[uint64]placement.Binding,
) error {
	if g == nil || g.client == nil || matchID == 0 || !placement.ValidOperationID(operationID) ||
		len(playerIDs) == 0 || len(bindings) != len(playerIDs) {
		return errcode.New(errcode.ErrInvalidArg, "Hub departure client and roster required")
	}
	ids := append([]uint64(nil), playerIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var previous uint64
	for _, playerID := range ids {
		if playerID == 0 || playerID == previous {
			return errcode.New(errcode.ErrInvalidArg, "Hub departure roster must be unique and non-zero")
		}
		previous = playerID
		binding, ok := bindings[playerID]
		if !ok || !binding.Complete() || binding.OperationID != operationID {
			return errcode.New(errcode.ErrInvalidArg,
				"exact Battle placement binding required for Hub departure player %d", playerID)
		}
		rpcCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		resp, err := g.client.EnsureHubDepartureForBattle(rpcCtx,
			&hubv1.EnsureHubDepartureForBattleRequest{PlayerId: playerID, MatchId: matchID,
				PlacementVersion: binding.Version, PlacementOperationId: binding.OperationID})
		cancel()
		if err != nil {
			return errcode.NewCause(errcode.ErrUnavailable, err,
				"Hub departure result unknown for player %d", playerID)
		}
		if resp.GetCode() != commonv1.ErrCode_OK || !resp.GetDeparted() {
			return errcode.New(errcode.Code(resp.GetCode()),
				"Hub physical departure pending for player %d", playerID)
		}
	}
	return nil
}
