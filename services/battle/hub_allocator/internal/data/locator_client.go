// locator_client.go — hub_allocator → player_locator gRPC 客户端封装(玩家主动切线护栏用)。
//
// 设计:
//   - data 层暴露 HubLocationChecker 接口,biz 只依赖接口(便于单测注入假实现)
//   - 实际实现 GrpcHubLocationChecker 内嵌 *grpc.ClientConn + PlayerLocatorServiceClient
//   - main.go 用 pkg/grpcclient 拨号;addr 未配则注入 nil(仅限 dev 联调,生产必须配)
//
// 调用语义(TransferToLine 护栏,INC-20260722-002 修订为 fail-closed):
//   - MATCHING / BATTLE → blocked=true(战斗匹配中禁止切大厅线路);
//   - HUB → blocked=false(唯一放行态:presence 明确证明玩家在大厅);
//   - RPC 失败 / 响应非 OK / OFFLINE / 未知状态 → 返回 err:presence 投影不能证明
//     玩家不在旧 DS(§9.22 key miss/UNKNOWN 不得授权新归属),biz 必须在产生任何
//     副作用前 fail-closed 拒绝(可重试)。原"locator 抖动放行低危切线"契约已废止:
//     切线会进入另一台 Hub DS,不确定态放行 = 潜在双 DS。
package data

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/luyuancpp/pandora/pkg/errcode"

	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
)

// HubLocationChecker 给 hub_allocator.biz 查玩家是否在匹配/战斗中
// (nil 仅限 dev 联调跳过检查;生产必须装配,见 INC-20260722-002)。
type HubLocationChecker interface {
	// InBattleOrMatching 返回 true 表示玩家在匹配/战斗中(应拒绝切线);
	// false 且 err==nil 表示 presence 明确为 HUB(唯一放行态)。
	// err != nil(RPC 失败 / 非 OK / OFFLINE / 未知状态)= presence 不能证明可切线,
	// biz 必须 fail-closed 拒绝且零副作用(§9.22,INC-20260722-002)。
	InBattleOrMatching(ctx context.Context, playerID uint64) (bool, error)
	// RefreshHubLocations 把 Hub DS 心跳捎带的在场 player_ids 转发给 locator 批量
	// 续期 HUB 位置 TTL(在线保活)。locator 侧只续 state==HUB 且 hub_pod 匹配的记录。
	// 返实际续期成功条数;失败由调用方 best-effort 处理(不影响心跳主流程)。
	RefreshHubLocations(ctx context.Context, hubPod string, playerIDs []uint64, bearerToken string) (int, error)
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

// InBattleOrMatching 查玩家当前 Location,MATCHING / BATTLE 视为"应拒绝切线";
// 只有明确 HUB 才放行。非 OK 响应与 OFFLINE / 未知状态一律返回 err(fail-closed,
// INC-20260722-002:locator key miss 服务端按契约回 OK+OFFLINE,OFFLINE 只说明
// presence 不可见,不能证明玩家已离开旧 DS 的战斗/匹配)。
func (g *GrpcHubLocationChecker) InBattleOrMatching(ctx context.Context, playerID uint64) (bool, error) {
	resp, err := g.client.GetLocation(ctx, &locatorv1.GetLocationRequest{PlayerId: playerID})
	if err != nil {
		return false, err
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return false, errcode.New(errcode.ErrUnavailable, "locator get location code=%d", resp.GetCode())
	}
	switch resp.GetLocation().GetState() {
	case locatorv1.LocationState_LOCATION_STATE_MATCHING, locatorv1.LocationState_LOCATION_STATE_BATTLE:
		return true, nil
	case locatorv1.LocationState_LOCATION_STATE_HUB:
		return false, nil
	default:
		// OFFLINE(含 key miss / TTL 消失)/ UNSPECIFIED / 未来新增状态:presence 不可见
		// 或本副本不认识,一律不确定 → fail-closed。
		return false, errcode.New(errcode.ErrUnavailable,
			"player %d presence state %s cannot prove hub residency", playerID, resp.GetLocation().GetState())
	}
}

// RefreshHubLocations 转发 Hub DS 心跳捎带的在场玩家列表,批量续期 HUB 位置 TTL。
func (g *GrpcHubLocationChecker) RefreshHubLocations(ctx context.Context, hubPod string, playerIDs []uint64, bearerToken string) (int, error) {
	if bearerToken != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+bearerToken)
	}
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
