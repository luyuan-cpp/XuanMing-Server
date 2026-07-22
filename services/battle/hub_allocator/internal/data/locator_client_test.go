// locator_client_test.go — InBattleOrMatching fail-closed 状态映射单测
// (INC-20260722-002:非 OK / OFFLINE / 未知状态一律返回 err,只有 HUB 放行)。
package data

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/errcode"

	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
)

// stubLocatorClient 只实现 GetLocation,其余方法本用例不触达(触达即 panic 暴露误用)。
type stubLocatorClient struct {
	locatorv1.PlayerLocatorServiceClient // 内嵌 nil 接口:未覆写方法被调用会 panic

	resp *locatorv1.GetLocationResponse
	err  error
}

func (s *stubLocatorClient) GetLocation(context.Context, *locatorv1.GetLocationRequest, ...grpc.CallOption) (*locatorv1.GetLocationResponse, error) {
	return s.resp, s.err
}

func locResp(code commonv1.ErrCode, state locatorv1.LocationState) *locatorv1.GetLocationResponse {
	return &locatorv1.GetLocationResponse{
		Code:     code,
		Location: &locatorv1.Location{State: state},
	}
}

func TestInBattleOrMatching_FailClosedMapping(t *testing.T) {
	cases := []struct {
		name        string
		resp        *locatorv1.GetLocationResponse
		rpcErr      error
		wantBlocked bool
		wantErr     bool
	}{
		{"matching blocks", locResp(commonv1.ErrCode_OK, locatorv1.LocationState_LOCATION_STATE_MATCHING), nil, true, false},
		{"battle blocks", locResp(commonv1.ErrCode_OK, locatorv1.LocationState_LOCATION_STATE_BATTLE), nil, true, false},
		{"hub allows", locResp(commonv1.ErrCode_OK, locatorv1.LocationState_LOCATION_STATE_HUB), nil, false, false},
		// 以下全部 fail-closed:presence 不能证明玩家不在旧 DS(§9.22)。
		{"offline (key miss / TTL 消失) fails closed", locResp(commonv1.ErrCode_OK, locatorv1.LocationState_LOCATION_STATE_OFFLINE), nil, false, true},
		{"unspecified fails closed", locResp(commonv1.ErrCode_OK, locatorv1.LocationState_LOCATION_STATE_UNSPECIFIED), nil, false, true},
		{"unknown future state fails closed", locResp(commonv1.ErrCode_OK, locatorv1.LocationState(99)), nil, false, true},
		{"non-OK response fails closed", locResp(commonv1.ErrCode_ERR_INTERNAL, locatorv1.LocationState_LOCATION_STATE_HUB), nil, false, true},
		{"rpc error fails closed", nil, errors.New("locator unreachable"), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checker := &GrpcHubLocationChecker{client: &stubLocatorClient{resp: tc.resp, err: tc.rpcErr}}
			blocked, err := checker.InBattleOrMatching(context.Background(), 1001)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if blocked != tc.wantBlocked {
				t.Fatalf("blocked=%v want=%v", blocked, tc.wantBlocked)
			}
			if tc.wantErr && tc.rpcErr == nil && errcode.As(err) != errcode.ErrUnavailable {
				t.Fatalf("fail-closed mapping must be retryable ErrUnavailable, got %v", err)
			}
		})
	}
}
