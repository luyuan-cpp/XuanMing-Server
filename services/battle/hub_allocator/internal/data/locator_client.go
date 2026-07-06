// locator_client.go — hub_allocator → player_locator gRPC 客户端封装(玩家主动切线护栏用)。
//
// 设计:
//   - data 层暴露 HubLocationChecker 接口,biz 只依赖接口(便于单测注入假实现)
//   - 实际实现 GrpcHubLocationChecker 内嵌 *grpc.ClientConn + PlayerLocatorServiceClient
//   - main.go 用 pkg/grpcclient 拨号;addr 未配则注入 nil(弱依赖)
//
// 调用语义(TransferToLine 护栏):
//   - 玩家主动切线前查其当前 Location:MATCHING / BATTLE → blocked=true(战斗匹配中禁止切大厅线路)
//   - locator 不可达 / 查询失败 → 返回 (false, err),biz 据 err 决定放行
//     (大厅切线是低危操作,locator 抖动时不应硬阻断;真正的"一人一 DS"由 DS 侧 SetLocation 强制)
package data

import (
	"context"

	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/errcode"

	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
)

// HubLocationChecker 给 hub_allocator.biz 查玩家是否在匹配/战斗中(弱依赖,nil 时跳过检查)。
type HubLocationChecker interface {
	// InBattleOrMatching 返回 true 表示玩家在匹配/战斗中(应拒绝切线)。
	// locator 不可达 / 查询失败 → 返回 (false, err),biz 据 err 决定是否放行。
	InBattleOrMatching(ctx context.Context, playerID uint64) (bool, error)
	// RefreshHubLocations 把 Hub DS 心跳捎带的在场 player_ids 转发给 locator 批量
	// 续期 HUB 位置 TTL(在线保活)。locator 侧只续 state==HUB 且 hub_pod 匹配的记录。
	// 返实际续期成功条数;失败由调用方 best-effort 处理(不影响心跳主流程)。
	RefreshHubLocations(ctx context.Context, hubPod string, playerIDs []uint64) (int, error)
}

// GrpcHubLocationChecker 实现 HubLocationChecker,内嵌 locator gRPC client。
type GrpcHubLocationChecker struct {
	conn   *grpc.ClientConn
	client locatorv1.PlayerLocatorServiceClient
}

// NewGrpcHubLocationChecker 用现成的 *grpc.ClientConn 包出 checker。
// 调用方负责 conn 生命周期(main.go defer conn.Close())。
func NewGrpcHubLocationChecker(conn *grpc.ClientConn) *GrpcHubLocationChecker {
	return &GrpcHubLocationChecker{
		conn:   conn,
		client: locatorv1.NewPlayerLocatorServiceClient(conn),
	}
}

// InBattleOrMatching 查玩家当前 Location,MATCHING / BATTLE 视为"应拒绝切线"。
func (g *GrpcHubLocationChecker) InBattleOrMatching(ctx context.Context, playerID uint64) (bool, error) {
	resp, err := g.client.GetLocation(ctx, &locatorv1.GetLocationRequest{PlayerId: playerID})
	if err != nil {
		return false, err
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return false, nil // 查不到位置按未阻断处理(玩家可能刚进大厅,locator 尚未落 HUB)
	}
	state := resp.GetLocation().GetState()
	return state == locatorv1.LocationState_LOCATION_STATE_MATCHING ||
		state == locatorv1.LocationState_LOCATION_STATE_BATTLE, nil
}

// RefreshHubLocations 转发 Hub DS 心跳捎带的在场玩家列表,批量续期 HUB 位置 TTL。
func (g *GrpcHubLocationChecker) RefreshHubLocations(ctx context.Context, hubPod string, playerIDs []uint64) (int, error) {
	resp, err := g.client.RefreshHubLocations(ctx, &locatorv1.RefreshHubLocationsRequest{
		HubPod:    hubPod,
		PlayerIds: playerIDs,
	})
	if err != nil {
		return 0, err
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return 0, errcode.New(errcode.ErrInternal, "locator refresh hub locations code=%d", resp.GetCode())
	}
	return int(resp.GetRefreshed()), nil
}
