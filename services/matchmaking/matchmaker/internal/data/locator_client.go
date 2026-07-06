// locator_client.go — matchmaker → player_locator gRPC 客户端封装(W4 ⑦,2026-06-06)。
//
// 设计:
//   - 实现 biz.LocationNotifier:撮合成局 → MATCHING、全员确认就绪 → BATTLE、StartMatch 前置查 IsInBattle
//   - main.go 用 pkg/grpcclient.MustDialInsecure 拨号;locator_addr 留空则 main 注入 nil
//   - 依赖强度:位置上报(Notify*)是弱依赖,失败仅 Warn 不阻断;而 StartMatch 前置
//     IsInBattle 检查默认 fail-closed(biz.ensureNoneInBattle,locator 已配却查询失败则拒绝入队),
//     只有 dev 显式 battle_gate_fail_open=true 才降级放行。本文件只负责透传结果/错误,不吞错误。
//
// 状态权属(CLAUDE.md §9.1 不变量 §1):
//   - matchmaker 是 MATCHING / BATTLE 两态的权威(掌握撮合生命周期)
//   - HUB 由 hub DS 上报;撮合失败/取消时不回写 HUB(交回 hub DS)
package data

import (
	"context"

	"google.golang.org/grpc"

	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// GrpcLocationNotifier 用 player_locator 服务 gRPC client 实现 biz.LocationNotifier。
type GrpcLocationNotifier struct {
	conn   *grpc.ClientConn
	client locatorv1.PlayerLocatorServiceClient
}

// NewGrpcLocationNotifier 用现成的 *grpc.ClientConn 包出 notifier。
// 调用方负责 conn 生命周期管理(main.go defer ln.Close())。
func NewGrpcLocationNotifier(conn *grpc.ClientConn) *GrpcLocationNotifier {
	return &GrpcLocationNotifier{
		conn:   conn,
		client: locatorv1.NewPlayerLocatorServiceClient(conn),
	}
}

// Close 关闭底层连接。
func (n *GrpcLocationNotifier) Close() error {
	if n.conn != nil {
		return n.conn.Close()
	}
	return nil
}

// NotifyMatching 把每个玩家置为 MATCHING(带 match_id)。
// 逐玩家 best-effort:单个失败继续其余,返回首个错误供 biz 记 Warn。
func (n *GrpcLocationNotifier) NotifyMatching(ctx context.Context, playerIDs []uint64, matchID uint64) error {
	var firstErr error
	for _, pid := range playerIDs {
		if err := n.setLocation(ctx, pid, &locatorv1.Location{
			State:   locatorv1.LocationState_LOCATION_STATE_MATCHING,
			MatchId: matchID,
		}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// NotifyBattle 把每个玩家置为 BATTLE(带 match_id + battle_pod)。
func (n *GrpcLocationNotifier) NotifyBattle(ctx context.Context, playerIDs []uint64, matchID uint64, battlePod string) error {
	var firstErr error
	for _, pid := range playerIDs {
		if err := n.setLocation(ctx, pid, &locatorv1.Location{
			State:     locatorv1.LocationState_LOCATION_STATE_BATTLE,
			MatchId:   matchID,
			BattlePod: battlePod,
		}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// IsInBattle 调 PlayerLocatorService.GetLocation,判断玩家当前是否正处于 battle DS 中。
//
// 战斗中禁止重复匹配(CLAUDE.md §9 不变量 §1:玩家同一时刻只能在一个 Location)。
// 只有 state==BATTLE 且 match_id!=0 且 battle_pod!="" 才认定"在战斗中";其余一律 false。
func (n *GrpcLocationNotifier) IsInBattle(ctx context.Context, playerID uint64) (bool, error) {
	resp, err := n.client.GetLocation(ctx, &locatorv1.GetLocationRequest{PlayerId: playerID})
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "locator GetLocation rpc player=%d: %v", playerID, err)
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return false, errcode.New(errcode.Code(resp.GetCode()), "locator GetLocation player=%d code=%d", playerID, resp.GetCode())
	}
	loc := resp.GetLocation()
	if loc.GetState() != locatorv1.LocationState_LOCATION_STATE_BATTLE ||
		loc.GetMatchId() == 0 || loc.GetBattlePod() == "" {
		return false, nil
	}
	return true, nil
}

// setLocation 调 PlayerLocatorService.SetLocation 并把 ErrCode 转回 error。
func (n *GrpcLocationNotifier) setLocation(ctx context.Context, playerID uint64, loc *locatorv1.Location) error {
	resp, err := n.client.SetLocation(ctx, &locatorv1.SetLocationRequest{
		PlayerId: playerID,
		Location: loc,
	})
	if err != nil {
		return errcode.New(errcode.ErrInternal, "locator SetLocation rpc player=%d: %v", playerID, err)
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return errcode.New(errcode.Code(resp.GetCode()), "locator SetLocation player=%d code=%d", playerID, resp.GetCode())
	}
	return nil
}

// FindOfflinePlayers 批量找出已离线的玩家(成局前在线校验,不变量 §1 配套在线保活)。
//
// 判定:BatchGetLocation 响应 map 里缺席(key 已过期 = 在线保活断报 ≥30s)或
// state==OFFLINE → 离线;LOGIN_PENDING/HUB/MATCHING/BATTLE 均算在线。
// 查询失败返 error 不吞,调用方(biz)按弱依赖跳过校验。
func (n *GrpcLocationNotifier) FindOfflinePlayers(ctx context.Context, playerIDs []uint64) ([]uint64, error) {
	if len(playerIDs) == 0 {
		return nil, nil
	}
	resp, err := n.client.BatchGetLocation(ctx, &locatorv1.BatchGetLocationRequest{PlayerIds: playerIDs})
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "locator BatchGetLocation rpc: %v", err)
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return nil, errcode.New(errcode.Code(resp.GetCode()), "locator BatchGetLocation code=%d", resp.GetCode())
	}
	locations := resp.GetLocations()
	var offline []uint64
	for _, pid := range playerIDs {
		loc, ok := locations[pid]
		if !ok || loc.GetState() == locatorv1.LocationState_LOCATION_STATE_OFFLINE {
			offline = append(offline, pid)
		}
	}
	return offline, nil
}
