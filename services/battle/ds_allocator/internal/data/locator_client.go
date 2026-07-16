// locator_client.go — ds_allocator → player_locator gRPC 客户端封装
// (断线重连,docs/design/battle-reconnect.md §2.2)。
//
// 设计:
//   - 实现 biz.LocationRefresher:心跳成功且对局 ready/running 时,把该对局玩家的位置
//     刷新为 BATTLE(顺带续期 locator TTL),使玩家整局在线期间都能被 login 检测到"在战斗中"。
//   - main.go 用 pkg/grpcclient.MustDialInsecure 拨号;locator_addr 留空则 main 注入 nil。
//   - 弱依赖:locator 不可用时 biz 仅 Warn,不阻断心跳 / 对局。
//
// 状态权属(CLAUDE.md §9.1 不变量 §1):BATTLE 态由 matchmaker 成局时首次写入,ds_allocator
// 心跳只做"同 match_id 续期"(BATTLE→BATTLE 同 match),被 locator guard 放行;不同 match_id
// 的迟到心跳(旧 DS / 旧 allocator)会被 locator guard 拒,避免覆盖当前对局位置。
package data

import (
	"context"

	"google.golang.org/grpc"

	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// GrpcLocationRefresher 用 player_locator 服务 gRPC client 实现 biz.LocationRefresher。
type GrpcLocationRefresher struct {
	conn   *grpc.ClientConn
	client locatorv1.PlayerLocatorServiceClient
}

// NewGrpcLocationRefresher 用现成的 *grpc.ClientConn 包出 refresher。
// 调用方负责 conn 生命周期管理(main.go defer conn.Close())。
func NewGrpcLocationRefresher(conn *grpc.ClientConn) *GrpcLocationRefresher {
	return &GrpcLocationRefresher{
		conn:   conn,
		client: locatorv1.NewPlayerLocatorServiceClient(conn),
	}
}

// Close 关闭底层连接。
func (r *GrpcLocationRefresher) Close() error {
	if r.conn != nil {
		return r.conn.Close()
	}
	return nil
}

// RefreshBattleLocations 把这批玩家位置刷新为 BATTLE(带 match_id + battle_pod=dsAddr),
// 顺带续期 locator TTL。逐玩家 best-effort:单个失败继续其余,返回首个错误供 biz 记 Warn。
func (r *GrpcLocationRefresher) RefreshBattleLocations(ctx context.Context, playerIDs []uint64, matchID uint64, dsAddr string) error {
	if matchID == 0 || dsAddr == "" {
		return nil
	}
	var firstErr error
	for _, pid := range playerIDs {
		if pid == 0 {
			continue
		}
		resp, err := r.client.SetLocation(ctx, &locatorv1.SetLocationRequest{
			PlayerId: pid,
			Location: &locatorv1.Location{
				State:     locatorv1.LocationState_LOCATION_STATE_BATTLE,
				MatchId:   matchID,
				BattlePod: dsAddr,
			},
		})
		if err != nil {
			if firstErr == nil {
				firstErr = errcode.New(errcode.ErrInternal, "locator SetLocation rpc: %v", err)
			}
			continue
		}
		if resp.GetCode() != commonv1.ErrCode_OK && firstErr == nil {
			firstErr = errcode.New(errcode.Code(resp.GetCode()), "locator SetLocation code=%d", resp.GetCode())
		}
	}
	return firstErr
}
