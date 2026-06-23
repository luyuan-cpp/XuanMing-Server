// match_releaser.go 实现 biz.MatchReleaser:通过 gRPC 调 matchmaker 内部接口
// ReleaseMatch,在一场对局结算/废弃落库后释放残留的撮合状态。
//
// 设计(修复"结算返回 Hub 后无法再次匹配"):
//   - matchmaker 对局走完 READY → 进战斗 → 结算后,故意保留 player→ticket 归属(SETNX claim)
//   - 票据 + match 镜像到 30min TTL;期间玩家回 Hub 再次 StartMatch 撞上残留 claim 报
//     ErrMatchAlreadyMatching(4002)。battle_result 落库后调此接口主动彻底释放。
//   - 内部服务间调用:matchmaker gRPC server 用 AuthOptional,本调用不带玩家 JWT,按 match_id
//     操作,合法。
//   - 弱依赖语义:matchmaker 地址未配 / 调用失败时由 biz.releaseMatch 仅 Warn,不阻断落库。
package data

import (
	"context"

	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/grpcclient"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"
)

// GrpcMatchReleaser 用 matchmaker 服务 gRPC client 实现 biz.MatchReleaser。
type GrpcMatchReleaser struct {
	conn *grpc.ClientConn
	cli  matchv1.MatchServiceClient
}

// NewGrpcMatchReleaser 直连 matchmaker 服务 endpoint(host:port,内网 insecure)。
func NewGrpcMatchReleaser(matchmakerAddr string) *GrpcMatchReleaser {
	conn := grpcclient.MustDialInsecure(matchmakerAddr)
	return &GrpcMatchReleaser{
		conn: conn,
		cli:  matchv1.NewMatchServiceClient(conn),
	}
}

// Close 关闭底层连接。
func (g *GrpcMatchReleaser) Close() error {
	if g.conn != nil {
		return g.conn.Close()
	}
	return nil
}

// ReleaseMatch 调 matchmaker.ReleaseMatch 释放本局撮合状态(幂等)。
func (g *GrpcMatchReleaser) ReleaseMatch(ctx context.Context, matchID uint64, playerIDs []uint64) error {
	resp, err := g.cli.ReleaseMatch(ctx, &matchv1.ReleaseMatchRequest{
		MatchId:   matchID,
		PlayerIds: playerIDs,
	})
	if err != nil {
		return err
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return errcode.New(errcode.Code(resp.GetCode()), "matchmaker.ReleaseMatch code=%d match=%d", resp.GetCode(), matchID)
	}
	return nil
}
