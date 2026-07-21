// experience_client.go — battle_result 调 player.AddExperience 把击杀经验幂等入账
// (实时成长,2026-07-20)。
//
// 接线对齐 inventory_client:内网 insecure 直连,无 JWT(player.AddExperience 是系统 RPC,
// callerID==0 才放行)。幂等键 = progress:{match_id}:{seq}:{player_id}:exp,
// 同一批末 seq 的经验聚合行只入账一次(player exp_history uk 去重)。
package data

import (
	"context"

	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/grpcclient"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	playerv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/player/v1"
)

// GrpcExperienceGranter 用 player 服务 gRPC client 实现 biz.ExperienceGranter。
type GrpcExperienceGranter struct {
	conn *grpc.ClientConn
	cli  playerv1.PlayerServiceClient
}

// NewGrpcExperienceGranter 直连 player 服务 endpoint(host:port,内网 insecure)。
func NewGrpcExperienceGranter(playerAddr string) *GrpcExperienceGranter {
	conn := grpcclient.MustDialInsecure(playerAddr)
	return &GrpcExperienceGranter{conn: conn, cli: playerv1.NewPlayerServiceClient(conn)}
}

// Close 关闭底层连接。
func (g *GrpcExperienceGranter) Close() error {
	if g.conn != nil {
		return g.conn.Close()
	}
	return nil
}

// AddExperience 调 player.AddExperience 幂等入账经验;
// 非 OK 透传错误(发布器保留出箱行下轮重试),gRPC 错误原样返回。
func (g *GrpcExperienceGranter) AddExperience(ctx context.Context, playerID uint64, expDelta uint64, reason, idempotencyKey string) error {
	resp, err := g.cli.AddExperience(ctx, &playerv1.AddExperienceRequest{
		PlayerId:       playerID,
		ExpDelta:       expDelta,
		Reason:         reason,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return err
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return errcode.New(errcode.Code(resp.GetCode()), "player add experience code=%d", resp.GetCode())
	}
	return nil
}
