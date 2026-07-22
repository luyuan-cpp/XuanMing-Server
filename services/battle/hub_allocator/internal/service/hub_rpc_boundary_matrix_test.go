package service

import (
	"context"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/biz"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/data"
)

type hubRPCProbe func() (commonv1.ErrCode, error)

// TestHubPublicRPCBoundaryMatrix 验证玩家入口、Model-B DS 回调、参数门和已删除接口
// 均在 nil usecase 前 fail-closed，不把未验证请求解释成可继续的默认状态。
func TestHubPublicRPCBoundaryMatrix(t *testing.T) {
	svc := NewHubService(nil)
	svc.SetModelBAuthority(true)
	ctx := context.Background()
	tests := []struct {
		name string
		want commonv1.ErrCode
		call hubRPCProbe
	}{
		{"AssignHub/player_id=0", commonv1.ErrCode_ERR_INVALID_ARG, func() (commonv1.ErrCode, error) {
			r, e := svc.AssignHub(ctx, &hubv1.AssignHubRequest{})
			return r.GetCode(), e
		}},
		{"ReleaseHub/player_id=0", commonv1.ErrCode_ERR_INVALID_ARG, func() (commonv1.ErrCode, error) {
			r, e := svc.ReleaseHub(ctx, &hubv1.ReleaseHubRequest{})
			return r.GetCode(), e
		}},
		{"TransferHub/player_id=0", commonv1.ErrCode_ERR_INVALID_ARG, func() (commonv1.ErrCode, error) {
			r, e := svc.TransferHub(ctx, &hubv1.TransferHubRequest{})
			return r.GetCode(), e
		}},
		{"EnsureHubDepartureForBattle/removed_surface", commonv1.ErrCode_ERR_SERVICE_DISABLED, func() (commonv1.ErrCode, error) {
			r, e := svc.EnsureHubDepartureForBattle(ctx, &hubv1.EnsureHubDepartureForBattleRequest{})
			return r.GetCode(), e
		}},
		{"ListHubLines/missing_player_jwt", commonv1.ErrCode_ERR_UNAUTHORIZED, func() (commonv1.ErrCode, error) {
			r, e := svc.ListHubLines(ctx, &hubv1.ListHubLinesRequest{})
			return r.GetCode(), e
		}},
		{"TransferToLine/missing_player_jwt", commonv1.ErrCode_ERR_UNAUTHORIZED, func() (commonv1.ErrCode, error) {
			r, e := svc.TransferToLine(ctx, &hubv1.TransferToLineRequest{})
			return r.GetCode(), e
		}},
		{"Heartbeat/missing_pod", commonv1.ErrCode_ERR_INVALID_ARG, func() (commonv1.ErrCode, error) {
			r, e := svc.Heartbeat(ctx, &hubv1.HeartbeatRequest{})
			return r.GetCode(), e
		}},
		{"Heartbeat/model_b_missing_credential", commonv1.ErrCode_ERR_UNAUTHORIZED, func() (commonv1.ErrCode, error) {
			r, e := svc.Heartbeat(ctx, &hubv1.HeartbeatRequest{HubPodName: "hub-0"})
			return r.GetCode(), e
		}},
		{"AcknowledgeAdmission/model_b_missing_credential", commonv1.ErrCode_ERR_UNAUTHORIZED, func() (commonv1.ErrCode, error) {
			r, e := svc.AcknowledgeAdmission(ctx, &hubv1.AcknowledgeAdmissionRequest{
				PlayerId: 1, AssignmentId: "assignment-1", HubPodName: "hub-0",
				AdmissionId: "00000000-0000-4000-8000-000000000001", AdmissionSeq: 1,
			})
			return r.GetCode(), e
		}},
		{"AcknowledgeDeparture/model_b_missing_credential", commonv1.ErrCode_ERR_UNAUTHORIZED, func() (commonv1.ErrCode, error) {
			r, e := svc.AcknowledgeDeparture(ctx, &hubv1.AcknowledgeDepartureRequest{
				PlayerId: 1, AssignmentId: "assignment-1", HubPodName: "hub-0",
				AdmissionId: "00000000-0000-4000-8000-000000000001", AdmissionSeq: 1,
			})
			return r.GetCode(), e
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.call()
			if err != nil || got != tt.want {
				t.Fatalf("code=%v err=%v, want code=%v", got, err, tt.want)
			}
		})
	}
}

type hubListBoundaryRepo struct {
	data.HubRepo
	shards []*hubv1.HubShardStorageRecord
	err    error
}

func (r *hubListBoundaryRepo) ListShards(context.Context) ([]*hubv1.HubShardStorageRecord, error) {
	if r.err != nil {
		return nil, r.err
	}
	return append([]*hubv1.HubShardStorageRecord(nil), r.shards...), nil
}

// TestHubListHubsServiceMapping 补齐无参数门的运维 RPC：既验证 region 筛选后的
// 成功响应，也验证业务错误映射到 response code 而不是 transport error。
func TestHubListHubsServiceMapping(t *testing.T) {
	repo := &hubListBoundaryRepo{shards: []*hubv1.HubShardStorageRecord{
		{HubPodName: "hub-us", Region: "us-east", State: "ready"},
		{HubPodName: "hub-eu", Region: "eu-west", State: "ready"},
	}}
	svc := NewHubService(biz.NewHubUsecase(repo, nil, nil, conf.HubConf{}))

	resp, transportErr := svc.ListHubs(context.Background(), &hubv1.ListHubsRequest{Region: "us-east"})
	if transportErr != nil {
		t.Fatalf("ListHubs success returned transport error: %v", transportErr)
	}
	if resp.GetCode() != commonv1.ErrCode_OK || len(resp.GetHubs()) != 1 || resp.GetHubs()[0].GetHubPodName() != "hub-us" {
		t.Fatalf("ListHubs success mapping: code=%v hubs=%+v", resp.GetCode(), resp.GetHubs())
	}

	repo.err = errcode.New(errcode.ErrUnavailable, "shard index unavailable")
	resp, transportErr = svc.ListHubs(context.Background(), &hubv1.ListHubsRequest{})
	if transportErr != nil {
		t.Fatalf("ListHubs business failure returned transport error: %v", transportErr)
	}
	if resp.GetCode() != commonv1.ErrCode_ERR_UNAVAILABLE || len(resp.GetHubs()) != 0 {
		t.Fatalf("ListHubs error mapping: code=%v hubs=%+v", resp.GetCode(), resp.GetHubs())
	}
}
