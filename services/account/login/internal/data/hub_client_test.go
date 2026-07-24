// hub_client_test.go — login→hub_allocator 客户端写者继任短重试测试(R9 P0-7 伴随)。
// 覆盖:可重试 ErrUnavailable(响应码/传输层)有界重试后成功、耗尽后如实上抛、
// 非可重试码零重试立即返回。
package data

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// fakeHubAllocClient 按预置脚本逐次应答 AssignHub;其余方法未实现(嵌入接口占位)。
type fakeHubAllocClient struct {
	hubv1.HubAllocatorServiceClient
	script []func() (*hubv1.AssignHubResponse, error)
	calls  int
}

func (f *fakeHubAllocClient) AssignHub(_ context.Context, _ *hubv1.AssignHubRequest, _ ...grpc.CallOption) (*hubv1.AssignHubResponse, error) {
	i := f.calls
	f.calls++
	if i >= len(f.script) {
		i = len(f.script) - 1
	}
	return f.script[i]()
}

func okResp() (*hubv1.AssignHubResponse, error) {
	return &hubv1.AssignHubResponse{
		Code: commonv1.ErrCode_OK, HubDsAddr: "hub:7777", HubTicket: "tkt", HubPodName: "pod-1", ShardId: 1,
	}, nil
}

func unavailableResp() (*hubv1.AssignHubResponse, error) {
	return &hubv1.AssignHubResponse{Code: commonv1.ErrCode(errcode.ErrUnavailable)}, nil
}

func TestAssignHub_RetriesWriterHandoverThenSucceeds(t *testing.T) {
	fake := &fakeHubAllocClient{script: []func() (*hubv1.AssignHubResponse, error){
		unavailableResp, unavailableResp, okResp,
	}}
	a := &GrpcHubAssigner{client: fake}
	got, err := a.AssignHub(context.Background(), 1001, "global", 0, 0, 0, "jti")
	if err != nil {
		t.Fatalf("handover retry must absorb transient unavailable: %v", err)
	}
	if got.HubTicket != "tkt" || fake.calls != 3 {
		t.Fatalf("want ticket after 3 attempts, got %+v calls=%d", got, fake.calls)
	}
}

func TestAssignHub_TransportUnavailableRetried(t *testing.T) {
	fake := &fakeHubAllocClient{script: []func() (*hubv1.AssignHubResponse, error){
		func() (*hubv1.AssignHubResponse, error) { return nil, status.Error(codes.Unavailable, "conn reset") },
		okResp,
	}}
	a := &GrpcHubAssigner{client: fake}
	if _, err := a.AssignHub(context.Background(), 1001, "global", 0, 0, 0, ""); err != nil {
		t.Fatalf("transport unavailable must be retried: %v", err)
	}
	if fake.calls != 2 {
		t.Fatalf("want 2 attempts, got %d", fake.calls)
	}
}

func TestAssignHub_ExhaustedRetriesReturnUnavailable(t *testing.T) {
	fake := &fakeHubAllocClient{script: []func() (*hubv1.AssignHubResponse, error){unavailableResp}}
	a := &GrpcHubAssigner{client: fake}
	_, err := a.AssignHub(context.Background(), 1001, "global", 0, 0, 0, "")
	if errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("exhausted retries must surface ErrUnavailable, got %v", err)
	}
	if fake.calls != assignHubMaxAttempts {
		t.Fatalf("want %d attempts, got %d", assignHubMaxAttempts, fake.calls)
	}
}

func TestAssignHub_NonRetryableCodeFailsFast(t *testing.T) {
	fake := &fakeHubAllocClient{script: []func() (*hubv1.AssignHubResponse, error){
		func() (*hubv1.AssignHubResponse, error) {
			return &hubv1.AssignHubResponse{Code: commonv1.ErrCode(errcode.ErrInvalidArg)}, nil
		},
	}}
	a := &GrpcHubAssigner{client: fake}
	_, err := a.AssignHub(context.Background(), 1001, "global", 0, 0, 0, "")
	if errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("non-retryable code must fail fast, got %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("non-retryable must not retry, got %d calls", fake.calls)
	}
}
