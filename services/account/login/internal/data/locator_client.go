// locator_client.go — login → player_locator gRPC 客户端封装(W3 ⑤,2026-06-05)。
//
// 设计:
//   - data 层暴露 LocationNotifier 接口,biz 只依赖接口
//   - 实际实现是 GrpcLocationNotifier,内嵌 *grpc.ClientConn + PlayerLocatorServiceClient
//   - main.go 用 pkg/grpcclient.MustDialInsecure 拨号,失败 panic
//
// 调用语义:
//   - LoginUsecase 在 sessions.Set 之后调 Notify(playerID, deviceID)
//   - 内部把 state 写成 LOGIN_PENDING(2),后续 hub DS 拿到玩家后会上报到 HUB(3)
package data

import (
	"context"

	"google.golang.org/grpc"

	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// LocationNotifier 给 login.biz 上报玩家"登录中"状态。
// addr 未配 → main 注入 nil,biz 检查 nil 直接跳过。
type LocationNotifier interface {
	NotifyLoginPending(ctx context.Context, playerID uint64, deviceID string) error
}

// GrpcLocationNotifier 实现 LocationNotifier,内嵌 grpc client。
type GrpcLocationNotifier struct {
	conn   *grpc.ClientConn
	client locatorv1.PlayerLocatorServiceClient
}

// NewGrpcLocationNotifier 用现成的 *grpc.ClientConn 包出 notifier。
//
// 调用方负责 conn 生命周期管理(main.go defer conn.Close())。
func NewGrpcLocationNotifier(conn *grpc.ClientConn) *GrpcLocationNotifier {
	return &GrpcLocationNotifier{
		conn:   conn,
		client: locatorv1.NewPlayerLocatorServiceClient(conn),
	}
}

// NotifyLoginPending 调 PlayerLocatorService.SetLocation(state=LOGIN_PENDING)。
//
// 不变量 §1 入口:这一行写完,locator 就把该 player_id 标记为"正在登录",
// 后续 hub DS 拿到玩家后改 state=HUB;客户端如果重复登录会再次刷此 key + TTL。
func (n *GrpcLocationNotifier) NotifyLoginPending(ctx context.Context, playerID uint64, deviceID string) error {
	req := &locatorv1.SetLocationRequest{
		PlayerId: playerID,
		Location: &locatorv1.Location{
			State: locatorv1.LocationState_LOCATION_STATE_LOGIN_PENDING,
		},
	}
	resp, err := n.client.SetLocation(ctx, req)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "locator SetLocation rpc: %v", err)
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return errcode.New(errcode.Code(resp.GetCode()), "locator SetLocation code=%d", resp.GetCode())
	}
	_ = deviceID // 当前 LOGIN_PENDING 状态不需要 device_id,保留参数便于以后扩展
	return nil
}
