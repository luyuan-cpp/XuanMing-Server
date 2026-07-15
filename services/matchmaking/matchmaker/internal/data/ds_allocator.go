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
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/grpcclient"
	"github.com/luyuancpp/pandora/pkg/placement"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/model"
)

// GrpcDSAllocator 用 ds_allocator 服务 gRPC client 实现 biz.DSAllocator。
type GrpcDSAllocator struct {
	conn   *grpc.ClientConn
	cli    dsv1.DSAllocatorServiceClient
	signer *auth.Signer
	// v2 非 nil 时启用 DSTicket v2(RS256,方案 B):battle 票绑死 DS 实例
	// (ds_uid / ds_instance_epoch / allocation_id),不再签 legacy HS256 票。
	v2       *auth.DSTicketSigner
	mapID    uint32
	gameMode string
}

// NewGrpcDSAllocator 直连 ds_allocator 服务 endpoint(host:port,内网 insecure)。
// signer 用于给每个玩家签 battle DSTicket(v2Signer 非 nil 时改签 v2 实例绑定票);
// mapID / gameMode 透传给 ds_allocator。
// allocateTimeout 是 AllocateBattle 的客户端超时(服务端阻塞等 DS ready 心跳,
// 需覆盖 agones allocate + ready_wait 预算,不能用 15s 默认值);≤0 时用 grpcclient 默认。
func NewGrpcDSAllocator(dsAllocatorAddr string, signer *auth.Signer, v2Signer *auth.DSTicketSigner, mapID uint32, gameMode string, allocateTimeout time.Duration) *GrpcDSAllocator {
	conn := grpcclient.MustDialInsecureTimeout(dsAllocatorAddr, allocateTimeout)
	return &GrpcDSAllocator{
		conn:     conn,
		cli:      dsv1.NewDSAllocatorServiceClient(conn),
		signer:   signer,
		v2:       v2Signer,
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
// mapID 为本局副本编号(来自 match 记录):非 0 时按局透传给 ds_allocator 选副本地图;
// 为 0(旧客户端 / 未选)时回退到静态默认 g.mapID,保持向后兼容。
func (g *GrpcDSAllocator) AllocateBattle(ctx context.Context, matchID uint64, playerIDs []uint64, mapID uint32) (*model.BattleAllocation, error) {
	effectiveMapID := mapID
	if effectiveMapID == 0 {
		effectiveMapID = g.mapID
	}
	resp, err := g.cli.AllocateBattle(ctx, &dsv1.AllocateBattleRequest{
		MatchId:   matchID,
		PlayerIds: playerIDs,
		MapId:     effectiveMapID,
		GameMode:  g.gameMode,
	})
	if err != nil {
		return nil, err
	}
	if resp.GetCode() != commonv1.ErrCode_OK || resp.GetDsAddr() == "" {
		return nil, errcode.New(errcode.ErrDSAllocationFailed,
			"ds_allocator returned code=%d addr=%q for match %d", resp.GetCode(), resp.GetDsAddr(), matchID)
	}
	target, terr := battleTargetFromResponse(resp, matchID)
	if terr != nil {
		return nil, terr
	}
	allocation := &model.BattleAllocation{
		Address: resp.GetDsAddr(),
		Target: placement.Target{
			PodName:       target.DSPodName,
			InstanceUID:   target.DSInstanceUID,
			InstanceEpoch: target.DSInstanceEpoch,
			AllocationID:  target.AllocationID,
			ReleaseTrack:  target.ReleaseTrack,
		},
	}
	return allocation, nil
}

func (g *GrpcDSAllocator) SignBattleTickets(
	ctx context.Context,
	matchID uint64,
	playerIDs []uint64,
	allocation *model.BattleAllocation,
	bindings map[uint64]placement.Binding,
) (map[uint64]string, error) {
	tickets := make(map[uint64]string, len(playerIDs))
	for _, playerID := range playerIDs {
		binding, ok := bindings[playerID]
		if !ok || !binding.Complete() {
			return nil, errcode.New(errcode.ErrDSAllocationFailed,
				"missing placement binding for player %d match %d", playerID, matchID)
		}
		token, err := g.SignBattleTicket(ctx, playerID, matchID, allocation, binding)
		if err != nil {
			return nil, err
		}
		tickets[playerID] = token
	}
	return tickets, nil
}

func isB1BoundBattleResponse(resp *dsv1.AllocateBattleResponse) bool {
	return resp != nil && (resp.GetGameserverUid() != "" || resp.GetInstanceEpoch() != 0 ||
		resp.GetAllocationId() != "" || resp.GetReleaseTrack() != "")
}

// battleTargetFromResponse 从 AllocateBattleResponse 提取 v2 实例绑定。
// 三个实例字段缺一即拒(旧 ds_allocator / 降级路径),保证 v2 票永远带完整绑定。
func battleTargetFromResponse(resp *dsv1.AllocateBattleResponse, matchID uint64) (auth.DSTicketTarget, error) {
	return battleTargetFromFields(resp.GetDsPodName(), resp.GetGameserverUid(), resp.GetInstanceEpoch(),
		resp.GetAllocationId(), resp.GetReleaseTrack(), matchID)
}

func battleTargetFromFields(
	podName, gameserverUID string,
	instanceEpoch uint32,
	allocationID, releaseTrack string,
	matchID uint64,
) (auth.DSTicketTarget, error) {
	if podName == "" || gameserverUID == "" || instanceEpoch == 0 || allocationID == "" ||
		(releaseTrack != auth.ReleaseTrackStable && releaseTrack != auth.ReleaseTrackCanary) {
		return auth.DSTicketTarget{}, errcode.New(errcode.ErrDSAllocationFailed,
			"ds_allocator 未回填完整 DS 目标(pod=%q uid=%q epoch=%d alloc=%q track=%q),无法签 v2 票, match %d",
			podName, gameserverUID, instanceEpoch, allocationID, releaseTrack, matchID)
	}
	return auth.DSTicketTarget{
		DSPodName:       podName,
		DSInstanceUID:   gameserverUID,
		DSInstanceEpoch: instanceEpoch,
		ReleaseTrack:    releaseTrack,
		MatchID:         matchID,
		AllocationID:    allocationID,
	}, nil
}

// SignBattleTicket 只使用 READY match 持久化的 exact target + per-player placement binding。
// 不再临时回查一个缺 binding 的 roster 目标，也不允许降级 legacy HMAC 票。
func (g *GrpcDSAllocator) SignBattleTicket(_ context.Context, playerID, matchID uint64, allocation *model.BattleAllocation, binding placement.Binding) (string, error) {
	if g.v2 == nil || allocation == nil || !allocation.Target.CompleteBattle() || !binding.Complete() {
		return "", errcode.New(errcode.ErrDSAllocationFailed,
			"complete v2 target/placement binding required, player %d match %d", playerID, matchID)
	}
	target := auth.DSTicketTarget{
		DSPodName: allocation.Target.PodName, DSInstanceUID: allocation.Target.InstanceUID,
		DSInstanceEpoch: allocation.Target.InstanceEpoch, ReleaseTrack: allocation.Target.ReleaseTrack,
		MatchID: matchID, AllocationID: allocation.Target.AllocationID, Placement: binding,
	}
	token, _, err := g.v2.SignBattleTicket(playerID, 0, 0, uuid.NewString(), target)
	if err != nil {
		return "", errcode.New(errcode.ErrDSAllocationFailed,
			"sign bound v2 battle ticket for player %d match %d failed: %v", playerID, matchID, err)
	}
	return token, nil
}
