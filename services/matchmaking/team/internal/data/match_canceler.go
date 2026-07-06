// match_canceler.go 实现 biz.MatchCanceler:通过 gRPC 调 matchmaker CancelMatch,
// 在成员离队 / 被踢时联动撤销其所在的匹配票据。
//
// 设计(修复"排队中离队不取消票据"的跨服务不一致,原 biz TODO):
//   - team 只知道队伍成员,不知道 matchmaker 的 ticket;按 player_id 撤销即可——matchmaker
//     由 player→ticket 归属(SETNX claim)定位该成员所在整张票据:
//     仍在排队 → CAS 删票 + 释放全队 claim;已进确认期 → 等价该玩家拒绝确认(match 失败退票)。
//   - 内部服务间调用:matchmaker gRPC server 用 AuthOptional,本调用不带玩家 JWT
//     (callerID==0),身份走 CancelMatchRequest.player_id 内部路径,合法。
//   - 弱依赖语义:matchmaker 地址未配 / 调用失败时由 biz.cancelMatchmaking 仅 Warn,
//     不阻断离队;残留票据由确认期超时 / TTL 兜底回收。
package data

import (
	"context"

	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/grpcclient"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"
)

// GrpcMatchCanceler 用 matchmaker 服务 gRPC client 实现 biz.MatchCanceler。
type GrpcMatchCanceler struct {
	conn *grpc.ClientConn
	cli  matchv1.MatchServiceClient
}

// NewGrpcMatchCanceler 直连 matchmaker 服务 endpoint(host:port,内网 insecure)。
func NewGrpcMatchCanceler(matchmakerAddr string) *GrpcMatchCanceler {
	conn := grpcclient.MustDialInsecure(matchmakerAddr)
	return &GrpcMatchCanceler{
		conn: conn,
		cli:  matchv1.NewMatchServiceClient(conn),
	}
}

// Close 关闭底层连接。
func (g *GrpcMatchCanceler) Close() error {
	if g.conn != nil {
		return g.conn.Close()
	}
	return nil
}

// CancelMatch 撤销 playerID 当前所在的匹配票据(整张票据,含全体队友)。
// 玩家未在排队时 matchmaker 返回 ErrMatchNotFound(4004),由调用方按常态忽略。
func (g *GrpcMatchCanceler) CancelMatch(ctx context.Context, playerID uint64) error {
	resp, err := g.cli.CancelMatch(ctx, &matchv1.CancelMatchRequest{PlayerId: playerID})
	if err != nil {
		return err
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return errcode.New(errcode.Code(resp.GetCode()), "matchmaker.CancelMatch code=%d player=%d", resp.GetCode(), playerID)
	}
	return nil
}
