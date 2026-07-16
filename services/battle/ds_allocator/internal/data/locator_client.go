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
	placementcontract "github.com/luyuancpp/pandora/pkg/placement"
)

// GrpcLocationRefresher 用 player_locator 服务 gRPC client 实现 biz.LocationRefresher。
type GrpcLocationRefresher struct {
	conn                  *grpc.ClientConn
	client                locatorv1.PlayerLocatorServiceClient
	battleDepartureSigner *placementcontract.ProofSigner
}

// NewGrpcLocationRefresher 用现成的 *grpc.ClientConn 包出 refresher。
// 调用方负责 conn 生命周期管理(main.go defer conn.Close())。
func NewGrpcLocationRefresher(conn *grpc.ClientConn,
	battleDepartureSigner ...*placementcontract.ProofSigner,
) *GrpcLocationRefresher {
	r := &GrpcLocationRefresher{
		conn:   conn,
		client: locatorv1.NewPlayerLocatorServiceClient(conn),
	}
	if len(battleDepartureSigner) > 0 {
		r.battleDepartureSigner = battleDepartureSigner[0]
	}
	return r
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

// VerifyPendingHubBattleDeparture 是 EnsurePlayerDeparture 的线性化/ABA 门。
// 必须先由 locator Begin 把 STABLE BATTLE CAS 为 PENDING->HUB，从而在
// 等待驱逐之前就让旧 Battle ticket 过期。Begin 原子保留的 source
// version/op/exact DS tuple 则供 order 命中 in-flight admission。
func (r *GrpcLocationRefresher) VerifyPendingHubBattleDeparture(
	ctx context.Context,
	expected BattlePlayerDepartureExpected,
) error {
	if r == nil || r.client == nil {
		return errcode.New(errcode.ErrUnavailable, "locator placement reader unavailable")
	}
	resp, err := r.client.GetPlacement(ctx, &locatorv1.GetPlacementRequest{PlayerId: expected.PlayerID})
	if err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err, "locator GetPlacement for battle departure")
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return errcode.New(errcode.Code(resp.GetCode()),
			"locator GetPlacement battle departure code=%d", resp.GetCode())
	}
	placement := resp.GetPlacement()
	if !resp.GetFound() || placement == nil {
		return errcode.New(errcode.ErrUnavailable, "battle departure placement is UNKNOWN")
	}
	return validatePendingHubBattleDepartureRecord(expected, placement)
}

// ConfirmBattleSourceDeparture publishes the allocator's physical absence
// decision into the exact pending placement. It deliberately re-reads the
// immutable source from locator before signing so release_track (which is not
// client supplied by EnsurePlayerDeparture) is covered by the HMAC as well.
// A lost response is safe: departureID and the canonical proof are stable and
// locator accepts only an exact idempotent replay.
func (r *GrpcLocationRefresher) ConfirmBattleSourceDeparture(
	ctx context.Context,
	expected BattlePlayerDepartureExpected,
	departureID string,
) error {
	if r == nil || r.client == nil || r.battleDepartureSigner == nil {
		return errcode.New(errcode.ErrUnavailable,
			"Battle source-departure proof authority unavailable")
	}
	if departureID == "" {
		return errcode.New(errcode.ErrInvalidArg, "battle departure_id required")
	}
	resp, err := r.client.GetPlacement(ctx,
		&locatorv1.GetPlacementRequest{PlayerId: expected.PlayerID})
	if err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err,
			"locator GetPlacement before Battle departure confirmation")
	}
	if resp.GetCode() != commonv1.ErrCode_OK || !resp.GetFound() || resp.GetPlacement() == nil {
		return errcode.New(errcode.ErrUnavailable,
			"Battle departure placement unavailable before confirmation")
	}
	rec := resp.GetPlacement()
	if err := validatePendingHubBattleDepartureRecord(expected, rec); err != nil {
		return err
	}
	proof := placementcontract.SourceDepartureProof{
		PlayerID: expected.PlayerID, PlacementVersion: expected.PlacementVersion,
		OperationID: expected.OperationID, TargetRoute: placementcontract.RouteHub,
		TargetMatchID: 0, SourcePlacementVersion: expected.SourcePlacementVersion,
		SourceOperationID: expected.SourceOperationID,
		SourceRoute:       placementcontract.RouteBattle, SourceMatchID: expected.MatchID,
		SourceTarget: placementcontract.Target{
			PodName: expected.Source.DSPodName, InstanceUID: expected.Source.GameServerUID,
			InstanceEpoch: expected.Source.InstanceEpoch, AllocationID: expected.Source.AllocationID,
			ReleaseTrack: rec.GetSourceReleaseTrack(),
		},
		ProofType: placementcontract.ProofBattleDeparture, ProofID: departureID,
	}
	confirm, err := r.client.ConfirmPlacementSourceDeparture(ctx,
		&locatorv1.ConfirmPlacementSourceDepartureRequest{
			PlayerId: proof.PlayerID, PlacementVersion: proof.PlacementVersion,
			OperationId:            proof.OperationID,
			TargetRoute:            locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
			TargetMatchId:          proof.TargetMatchID,
			SourcePlacementVersion: proof.SourcePlacementVersion,
			SourceOperationId:      proof.SourceOperationID,
			SourceRoute:            locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
			SourceMatchId:          proof.SourceMatchID,
			SourceTarget: &locatorv1.PlacementTargetIdentity{
				DsPodName:       proof.SourceTarget.PodName,
				DsInstanceUid:   proof.SourceTarget.InstanceUID,
				DsInstanceEpoch: proof.SourceTarget.InstanceEpoch,
				AllocationId:    proof.SourceTarget.AllocationID,
				ReleaseTrack:    proof.SourceTarget.ReleaseTrack,
			},
			ProofType:      locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_BATTLE_DEPARTURE,
			ProofId:        departureID,
			ProofSignature: r.battleDepartureSigner.SignSourceDeparture(proof),
		})
	if err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err,
			"Battle source-departure confirmation result unknown")
	}
	if confirm.GetCode() != commonv1.ErrCode_OK || !confirm.GetConfirmed() ||
		confirm.GetPlacement() == nil {
		code := errcode.Code(confirm.GetCode())
		if code == 0 {
			code = errcode.ErrLocatorConflict
		}
		return errcode.New(code, "Battle source-departure confirmation rejected")
	}
	confirmed := confirm.GetPlacement()
	if confirmed.GetPlayerId() != proof.PlayerID ||
		confirmed.GetVersion() != proof.PlacementVersion ||
		confirmed.GetOperationId() != proof.OperationID ||
		confirmed.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING ||
		confirmed.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE ||
		confirmed.GetMatchId() != proof.SourceMatchID ||
		confirmed.GetTargetRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB ||
		confirmed.GetTargetMatchId() != 0 ||
		!confirmed.GetSourceDepartureConfirmed() ||
		confirmed.GetSourceDepartureProofType() != locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_BATTLE_DEPARTURE ||
		confirmed.GetSourceDepartureProofId() != departureID {
		return errcode.New(errcode.ErrLocatorConflict,
			"Battle source-departure confirmation response identity mismatch")
	}
	return validatePendingHubBattleDepartureRecord(expected, confirmed)
}

func validatePendingHubBattleDepartureRecord(expected BattlePlayerDepartureExpected,
	placement *locatorv1.PlayerPlacementStorageRecord,
) error {
	if placement == nil || !placementcontract.ValidOperationID(expected.OperationID) ||
		!placementcontract.ValidOperationID(expected.SourceOperationID) ||
		expected.SourcePlacementVersion == 0 || expected.PlacementVersion != expected.SourcePlacementVersion+1 ||
		placement.GetPlayerId() != expected.PlayerID ||
		placement.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING ||
		placement.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE ||
		placement.GetTargetRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB ||
		placement.GetVersion() != expected.PlacementVersion ||
		placement.GetOperationId() != expected.OperationID ||
		placement.GetMatchId() != expected.MatchID ||
		placement.GetSourceMatchId() != expected.MatchID ||
		placement.GetTargetMatchId() != 0 ||
		(placement.GetProofType() != locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_TERMINAL &&
			placement.GetProofType() != locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_PLAYER_LEAVE) ||
		placement.GetProofId() == "" ||
		placement.GetSourcePlacementVersion() != expected.SourcePlacementVersion ||
		placement.GetSourceOperationId() != expected.SourceOperationID ||
		placement.GetSourceDsPodName() != expected.Source.DSPodName ||
		placement.GetSourceDsInstanceUid() != expected.Source.GameServerUID ||
		placement.GetSourceDsInstanceEpoch() != expected.Source.InstanceEpoch ||
		placement.GetSourceAllocationId() != expected.Source.AllocationID ||
		placement.GetSourceReleaseTrack() == "" || placement.GetSourceHubAssignmentId() != "" {
		return errcode.New(errcode.ErrLocatorConflict,
			"battle departure request does not match exact pending Hub source lineage")
	}
	return nil
}
