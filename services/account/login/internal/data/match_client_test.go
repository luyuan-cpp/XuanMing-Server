// match_client_test.go — GrpcMatchContextResolver 单测(P0 修复 2026-07-15)。
//
// 覆盖:
//   - internalrpcauth 签名:每次调用带 x-pandora-service-* 元数据(caller=login),
//     matchmaker 侧 resume auth 才会放行(裸调会被 ERR_PERMISSION_DENY 拒)。
//   - GameMode 映射:权威 game_mode(pve_coop/5v5_ranked)必须原样透传,
//     丢字段就是复现"rejecting unknown authoritative game_mode ”"客户端死循环。
//   - 非 OK code → errcode 映射(fail-closed,不得静默当 NONE)。
package data

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/internalrpcauth"
)

// fakeMatchServiceClient 只覆写 ResolvePlayerMatchContext,其余方法走内嵌接口(未实现即 panic,
// 本测试不会触达)。
type fakeMatchServiceClient struct {
	matchv1.MatchServiceClient
	resp   *matchv1.ResolvePlayerMatchContextResponse
	err    error
	gotCtx context.Context
	gotReq *matchv1.ResolvePlayerMatchContextRequest
}

func (f *fakeMatchServiceClient) ResolvePlayerMatchContext(
	ctx context.Context, req *matchv1.ResolvePlayerMatchContextRequest, _ ...grpc.CallOption,
) (*matchv1.ResolvePlayerMatchContextResponse, error) {
	f.gotCtx = ctx
	f.gotReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func TestGrpcMatchContextResolver_SignsRequestAndMapsGameMode(t *testing.T) {
	signer, err := internalrpcauth.NewSigner(
		"pandora-dev-match-resume-auth-key-v1!", "login", "matchmaker:5v5_ranked")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	fake := &fakeMatchServiceClient{resp: &matchv1.ResolvePlayerMatchContextResponse{
		Code:         commonv1.ErrCode_OK,
		State:        matchv1.PlayerMatchContextState_PLAYER_MATCH_CONTEXT_STATE_ACTIVE,
		Stage:        matchv1.PlayerMatchResumeStage_PLAYER_MATCH_RESUME_STAGE_READY,
		MatchId:      9001,
		BattleDsAddr: "10.1.2.3:7000",
		GameMode:     "pve_coop",
	}}
	r := &GrpcMatchContextResolver{client: fake, signer: signer}

	ma, err := r.ResolvePlayerMatchContext(context.Background(), 42)
	if err != nil {
		t.Fatalf("ResolvePlayerMatchContext: %v", err)
	}
	if ma.GameMode != "pve_coop" {
		t.Fatalf("GameMode = %q, want canonical pve_coop passthrough", ma.GameMode)
	}
	if ma.State != matchv1.PlayerMatchContextState_PLAYER_MATCH_CONTEXT_STATE_ACTIVE ||
		ma.Stage != matchv1.PlayerMatchResumeStage_PLAYER_MATCH_RESUME_STAGE_READY ||
		ma.MatchID != 9001 || ma.BattleDSAddr != "10.1.2.3:7000" {
		t.Fatalf("authority mapping drifted: %+v", ma)
	}
	if fake.gotReq.GetPlayerId() != 42 {
		t.Fatalf("player_id = %d, want 42", fake.gotReq.GetPlayerId())
	}
	md, ok := metadata.FromOutgoingContext(fake.gotCtx)
	if !ok {
		t.Fatalf("outgoing call carries no metadata; matchmaker resume auth would reject it")
	}
	if got := md.Get(internalrpcauth.CallerMetadataKey); len(got) != 1 || got[0] != "login" {
		t.Fatalf("caller metadata = %v, want [login]", got)
	}
	for _, k := range []string{
		internalrpcauth.AudienceMetadataKey,
		internalrpcauth.TimestampMetadataKey,
		internalrpcauth.NonceMetadataKey,
		internalrpcauth.SignatureMetadataKey,
	} {
		if got := md.Get(k); len(got) != 1 || got[0] == "" {
			t.Fatalf("signed metadata %q missing: %v", k, got)
		}
	}
}

func TestGrpcMatchContextResolver_NilSignerStillMaps(t *testing.T) {
	fake := &fakeMatchServiceClient{resp: &matchv1.ResolvePlayerMatchContextResponse{
		Code:     commonv1.ErrCode_OK,
		State:    matchv1.PlayerMatchContextState_PLAYER_MATCH_CONTEXT_STATE_ACTIVE,
		Stage:    matchv1.PlayerMatchResumeStage_PLAYER_MATCH_RESUME_STAGE_QUEUED,
		GameMode: "5v5_ranked",
	}}
	r := &GrpcMatchContextResolver{client: fake} // signer 未配(裸 dev 兼容)

	ma, err := r.ResolvePlayerMatchContext(context.Background(), 42)
	if err != nil {
		t.Fatalf("ResolvePlayerMatchContext: %v", err)
	}
	if ma.GameMode != "5v5_ranked" {
		t.Fatalf("GameMode = %q, want 5v5_ranked", ma.GameMode)
	}
	if md, ok := metadata.FromOutgoingContext(fake.gotCtx); ok {
		if got := md.Get(internalrpcauth.SignatureMetadataKey); len(got) != 0 {
			t.Fatalf("nil signer must not attach signature metadata, got %v", got)
		}
	}
}

func TestGrpcMatchContextResolver_NonOKCodeFailsClosed(t *testing.T) {
	fake := &fakeMatchServiceClient{resp: &matchv1.ResolvePlayerMatchContextResponse{
		Code: commonv1.ErrCode_ERR_PERMISSION_DENY,
	}}
	r := &GrpcMatchContextResolver{client: fake}

	_, err := r.ResolvePlayerMatchContext(context.Background(), 42)
	if errcode.As(err) != errcode.ErrPermissionDeny {
		t.Fatalf("code = %v err = %v, want ErrPermissionDeny passthrough (never silent NONE)",
			errcode.As(err), err)
	}
}
