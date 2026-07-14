package data

import (
	"context"

	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/grpcclient"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
)

// GrpcTerminalReleaseRelay 把 MySQL 持久证明交给 ds_allocator 内部控制面。
// ReleaseBattle 不暴露在 DS :8444；Redis-authority 服务端还会机械要求完整 expected tuple。
type GrpcTerminalReleaseRelay struct {
	conn *grpc.ClientConn
	cli  dsv1.DSAllocatorServiceClient
}

func NewGrpcTerminalReleaseRelay(addr string) *GrpcTerminalReleaseRelay {
	conn := grpcclient.MustDialInsecure(addr)
	return &GrpcTerminalReleaseRelay{conn: conn, cli: dsv1.NewDSAllocatorServiceClient(conn)}
}

func (g *GrpcTerminalReleaseRelay) Close() error {
	if g == nil || g.conn == nil {
		return nil
	}
	return g.conn.Close()
}

func (g *GrpcTerminalReleaseRelay) ReleaseTerminal(ctx context.Context, rec TerminalReleaseRecord) error {
	return g.releaseTerminal(ctx, rec, "completed")
}

// FinalizeTerminal 只恢复同 proof Redis 墓碑 TTL；ds_allocator 对此 reason 绝不调用 K8s。
func (g *GrpcTerminalReleaseRelay) FinalizeTerminal(ctx context.Context, rec TerminalReleaseRecord) error {
	return g.releaseTerminal(ctx, rec, "completed-finalize")
}

func (g *GrpcTerminalReleaseRelay) releaseTerminal(ctx context.Context, rec TerminalReleaseRecord, reason string) error {
	if g == nil || g.cli == nil {
		return errcode.New(errcode.ErrUnavailable, "terminal release relay is unavailable")
	}
	resp, err := g.cli.ReleaseBattle(ctx, &dsv1.ReleaseBattleRequest{
		MatchId: rec.MatchID, Reason: reason,
		AllocationId: rec.AllocationID, DsPodName: rec.DSPodName,
		GameserverUid: rec.GameserverUID, InstanceEpoch: rec.InstanceEpoch,
		AuthGen: rec.AuthGen, AuthJti: rec.AuthJTI, AuthExpMs: rec.AuthExpMs,
		AuthKid: rec.AuthKid, AuthTokenSha256: rec.AuthTokenSHA256,
		AuthWriterEpoch: rec.AuthWriterEpoch, AuthorizedAtMs: rec.AuthorizedAtMs,
	})
	if err != nil {
		return err
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return errcode.New(errcode.Code(resp.GetCode()),
			"ds_allocator.ReleaseBattle code=%d match=%d allocation=%s",
			resp.GetCode(), rec.MatchID, rec.AllocationID)
	}
	return nil
}
