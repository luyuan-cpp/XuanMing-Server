package data

import (
	"context"
	"sort"

	"github.com/luyuancpp/pandora/pkg/errcode"
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

func (g *GrpcHubDepartureCoordinator) EnsureHubDeparted(ctx context.Context, playerIDs []uint64) error {
	if g == nil || g.client == nil || len(playerIDs) == 0 {
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
		resp, err := g.client.ReleaseHub(ctx, &hubv1.ReleaseHubRequest{PlayerId: playerID})
		if err != nil {
			return errcode.NewCause(errcode.ErrUnavailable, err,
				"Hub departure result unknown for player %d", playerID)
		}
		if resp.GetCode() != commonv1.ErrCode_OK {
			return errcode.New(errcode.Code(resp.GetCode()),
				"Hub physical departure pending for player %d", playerID)
		}
	}
	return nil
}
