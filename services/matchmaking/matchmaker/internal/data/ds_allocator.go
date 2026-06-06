// ds_allocator.go 实现 biz.DSAllocator:通过 gRPC 调 ds_allocator 服务申请战斗 DS,
// 并在 matchmaker 侧为每个玩家签发 battle DSTicket(JWT,不变量 §3 短时效 5min)。
//
// 设计(W4 ②,2026-06-06):
//   - ds_allocator 服务只负责"拉一个 DS pod"并返回 ds_addr / pod_name,不签票据
//     (战斗结果 MMR 在 battle_result 算,DS 不可信,不变量 §6;票据由可信后端签)
//   - DSTicket 由 matchmaker 用 pkg/auth.Signer 签(dsType=battle + match_id),
//     客户端拿票据连 DS,DS 转交后端校验
package data

import (
	"context"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/grpcclient"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// GrpcDSAllocator 用 ds_allocator 服务 gRPC client 实现 biz.DSAllocator。
type GrpcDSAllocator struct {
	conn     *grpc.ClientConn
	cli      dsv1.DSAllocatorServiceClient
	signer   *auth.Signer
	mapID    uint32
	gameMode string
}

// NewGrpcDSAllocator 直连 ds_allocator 服务 endpoint(host:port,内网 insecure)。
// signer 用于给每个玩家签 battle DSTicket;mapID / gameMode 透传给 ds_allocator。
func NewGrpcDSAllocator(dsAllocatorAddr string, signer *auth.Signer, mapID uint32, gameMode string) *GrpcDSAllocator {
	conn := grpcclient.MustDialInsecure(dsAllocatorAddr)
	return &GrpcDSAllocator{
		conn:     conn,
		cli:      dsv1.NewDSAllocatorServiceClient(conn),
		signer:   signer,
		mapID:    mapID,
		gameMode: gameMode,
	}
}

// Close 关闭底层连接。
func (g *GrpcDSAllocator) Close() error {
	if g.conn != nil {
		return g.conn.Close()
	}
	return nil
}

// AllocateBattle 调 ds_allocator.AllocateBattle 拉战斗 DS,再为每个玩家签 battle DSTicket。
func (g *GrpcDSAllocator) AllocateBattle(ctx context.Context, matchID uint64, playerIDs []uint64) (string, map[uint64]string, error) {
	resp, err := g.cli.AllocateBattle(ctx, &dsv1.AllocateBattleRequest{
		MatchId:   matchID,
		PlayerIds: playerIDs,
		MapId:     g.mapID,
		GameMode:  g.gameMode,
	})
	if err != nil {
		return "", nil, err
	}
	if resp.GetCode() != commonv1.ErrCode_OK || resp.GetDsAddr() == "" {
		return "", nil, errcode.New(errcode.ErrDSAllocationFailed,
			"ds_allocator returned code=%d addr=%q for match %d", resp.GetCode(), resp.GetDsAddr(), matchID)
	}

	tickets := make(map[uint64]string, len(playerIDs))
	for _, pid := range playerIDs {
		token, _, serr := g.signer.SignDSTicket(pid, auth.DSTypeBattle, matchID, uuid.NewString())
		if serr != nil {
			return "", nil, errcode.New(errcode.ErrDSAllocationFailed,
				"sign battle ticket for player %d match %d failed: %v", pid, matchID, serr)
		}
		tickets[pid] = token
	}
	return resp.GetDsAddr(), tickets, nil
}
