// hub_client.go — login → hub_allocator gRPC 客户端封装(W4 ⑥,2026-06-06)。
//
// 设计(复刻 locator_client.go 弱依赖模式):
//   - data 层暴露 HubAssigner 接口,biz 只依赖接口
//   - 实际实现是 GrpcHubAssigner,内嵌 *grpc.ClientConn + HubAllocatorServiceClient
//   - main.go 用 pkg/grpcclient.MustDialInsecure 拨号
//
// 调用语义(不变量 §3 DS 票据短时效 + 不变量 §1 一人一 DS):
//   - LoginUsecase 鉴权成功后调 AssignHub(playerID, region, teamID)
//   - hub_allocator 是 hub 票据权威,response 直接带 hub_ds_addr + 已签好的 hub_ticket
//   - login 不再自签 hub 票据(addr 未配 / 调用失败时才回退自签,见 biz.resolveHub)
package data

import (
	"context"

	"google.golang.org/grpc"

	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
)

// HubAssignment 是 AssignHub 的产出(client 视角最小字段)。
type HubAssignment struct {
	HubDSAddr  string
	HubTicket  string // hub_allocator 签的 hub DSTicket(JWT)
	HubPodName string
	ShardID    uint32
	Placement placement.Binding
}

// HubAssigner 给 login.biz 分配大厅 DS 分片。
// addr 未配 → main 注入 nil,biz 检查 nil 回退自签 hub 票据 + 静态 addr。
//
// roleID(选角权威化 2026-07-08):玩家已选角色配置 ID。>0 时 allocator 把它签进
// hub 票据 claim 并更新归属镜像;0 = 未选角/保留 allocator 已存值。
type HubAssigner interface {
	AssignHub(ctx context.Context, playerID uint64, region string, teamID uint64, roleID uint32) (*HubAssignment, error)
}

type PlacementHubAssigner interface {
	AssignHubWithPlacement(ctx context.Context, playerID uint64, region string, teamID uint64, roleID uint32,
		binding placement.Binding) (*HubAssignment, error)
}

// GrpcHubAssigner 实现 HubAssigner,内嵌 grpc client。
type GrpcHubAssigner struct {
	conn   *grpc.ClientConn
	client hubv1.HubAllocatorServiceClient
}

// NewGrpcHubAssigner 用现成的 *grpc.ClientConn 包出 assigner。
//
// 调用方负责 conn 生命周期管理(main.go defer conn.Close())。
func NewGrpcHubAssigner(conn *grpc.ClientConn) *GrpcHubAssigner {
	return &GrpcHubAssigner{
		conn:   conn,
		client: hubv1.NewHubAllocatorServiceClient(conn),
	}
}

// AssignHub 调 HubAllocatorService.AssignHub,返回分片地址 + hub 票据。
//
// 登录时玩家尚未组队,teamID 一般为 0;region 由 login 配置给出(空 = 让 allocator 选最空分片)。
func (a *GrpcHubAssigner) AssignHub(ctx context.Context, playerID uint64, region string, teamID uint64, roleID uint32) (*HubAssignment, error) {
	return a.AssignHubWithPlacement(ctx, playerID, region, teamID, roleID, placement.Binding{})
}

func (a *GrpcHubAssigner) AssignHubWithPlacement(ctx context.Context, playerID uint64, region string, teamID uint64, roleID uint32,
	binding placement.Binding) (*HubAssignment, error) {
	req := &hubv1.AssignHubRequest{
		PlayerId: playerID,
		Region:   region,
		TeamId:   teamID,
		RoleId:   roleID,
		PlacementVersion: binding.Version,
		PlacementOperationId: binding.OperationID,
		SourceMatchId: binding.SourceMatchID,
	}
	resp, err := a.client.AssignHub(ctx, req)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "hub_allocator AssignHub rpc: %v", err)
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return nil, errcode.New(errcode.Code(resp.GetCode()), "hub_allocator AssignHub code=%d", resp.GetCode())
	}
	return &HubAssignment{
		HubDSAddr:  resp.GetHubDsAddr(),
		HubTicket:  resp.GetHubTicket(),
		HubPodName: resp.GetHubPodName(),
		ShardID:    resp.GetShardId(),
		Placement: placement.Binding{Version: resp.GetPlacementVersion(),
			OperationID: resp.GetPlacementOperationId(), SourceMatchID: binding.SourceMatchID},
	}, nil
}
