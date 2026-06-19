// locator_client.go — friend → player_locator gRPC 客户端封装(2026-06-15)。
//
// 设计:
//   - data 层暴露 OnlineStatusReader 接口,biz 只依赖接口
//   - 实际实现 GrpcOnlineStatusReader 内嵌 *grpc.ClientConn + PlayerLocatorServiceClient
//   - main.go 用 pkg/grpcclient.MustDialInsecure 拨号;addr 未配则注入 nil(弱依赖)
//
// 调用语义:
//   - ListFriends 拿到好友 id 列表后,一次 BatchGetLocation 批量查在线状态
//   - locator 不可达 / 整体失败 → 全部按离线处理,不让 ListFriends 整体失败
//     (好友列表是只读展示,在线状态可降级)
package data

import (
	"context"

	"google.golang.org/grpc"

	plog "github.com/luyuancpp/pandora/pkg/log"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
)

// OnlineStatus 是单个玩家的在线状态(biz 据此填 FriendInfo.is_online / last_seen_ms)。
type OnlineStatus struct {
	Online     bool
	LastSeenMs int64
}

// OnlineStatusReader 给 friend.biz 查好友在线状态(弱依赖,nil 时全部按离线)。
type OnlineStatusReader interface {
	BatchOnline(ctx context.Context, playerIDs []uint64) map[uint64]OnlineStatus
}

// GrpcOnlineStatusReader 实现 OnlineStatusReader,内嵌 locator gRPC client。
type GrpcOnlineStatusReader struct {
	conn   *grpc.ClientConn
	client locatorv1.PlayerLocatorServiceClient
}

// NewGrpcOnlineStatusReader 用现成的 *grpc.ClientConn 包出 reader。
// 调用方负责 conn 生命周期(main.go defer conn.Close())。
func NewGrpcOnlineStatusReader(conn *grpc.ClientConn) *GrpcOnlineStatusReader {
	return &GrpcOnlineStatusReader{
		conn:   conn,
		client: locatorv1.NewPlayerLocatorServiceClient(conn),
	}
}

// BatchOnline 一次 BatchGetLocation 批量查在线状态。
//
// 服务端用 Redis pipeline 一次往返,替代逐好友 N 次 unary 扇出
// (见 docs/design/friend-distributed-scaling.md §13.3 BatchGetPresence)。
// 整批失败 / locator 不可达 → 返回空 map(全部按离线,弱依赖降级)。
// 响应 map 里缺席的好友按离线处理(biz 默认 false)。
// state != OFFLINE 且 != UNSPECIFIED 视为在线。
func (g *GrpcOnlineStatusReader) BatchOnline(ctx context.Context, playerIDs []uint64) map[uint64]OnlineStatus {
	out := make(map[uint64]OnlineStatus, len(playerIDs))
	if len(playerIDs) == 0 {
		return out
	}
	h := plog.With(ctx)
	resp, err := g.client.BatchGetLocation(ctx, &locatorv1.BatchGetLocationRequest{PlayerIds: playerIDs})
	if err != nil {
		h.Warnw("msg", "locator_batch_get_location_failed", "count", len(playerIDs), "err", err)
		return out
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		h.Warnw("msg", "locator_batch_get_location_not_ok", "count", len(playerIDs), "code", resp.GetCode())
		return out
	}
	for pid, loc := range resp.GetLocations() {
		state := loc.GetState()
		online := state != locatorv1.LocationState_LOCATION_STATE_OFFLINE &&
			state != locatorv1.LocationState_LOCATION_STATE_UNSPECIFIED
		out[pid] = OnlineStatus{
			Online:     online,
			LastSeenMs: loc.GetUpdatedAtMs(),
		}
	}
	return out
}
