package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/metadata"

	"github.com/luyuancpp/pandora/pkg/internalrpcauth"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"
)

const resumeAuthTestSecret = "match-resume-service-auth-test-0123456789abcdef"
const resumeAuthTestAudience = "matchmaker:5v5_ranked"

type resumeResolverFake struct{ calls int }

func (f *resumeResolverFake) ResolvePlayerMatchContext(_ context.Context, playerID uint64) (*matchv1.ResolvePlayerMatchContextResponse, error) {
	f.calls++
	return &matchv1.ResolvePlayerMatchContextResponse{
		State: matchv1.PlayerMatchContextState_PLAYER_MATCH_CONTEXT_STATE_NONE,
	}, nil
}

type resumeReplayFake struct{ seen map[string]struct{} }

func (f *resumeReplayFake) Consume(_ context.Context, key string, _ time.Duration) (bool, error) {
	if _, ok := f.seen[key]; ok {
		return false, nil
	}
	f.seen[key] = struct{}{}
	return true, nil
}

func resumeIncomingContext(t *testing.T, signer *internalrpcauth.Signer, playerID uint64) context.Context {
	t.Helper()
	out, err := signer.SignContext(context.Background(),
		matchv1.MatchService_ResolvePlayerMatchContext_FullMethodName, playerID)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := metadata.FromOutgoingContext(out)
	return metadata.NewIncomingContext(context.Background(), md)
}

func TestResolvePlayerMatchContextRequiresServiceIdentityAndRejectsReplay(t *testing.T) {
	replay := &resumeReplayFake{seen: map[string]struct{}{}}
	verifier, err := internalrpcauth.NewVerifier(resumeAuthTestSecret, "login", resumeAuthTestAudience, 30*time.Second, replay)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := internalrpcauth.NewSigner(resumeAuthTestSecret, "login", resumeAuthTestAudience)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &resumeResolverFake{}
	svc := &MatchService{resumeResolver: resolver, resumeAuth: verifier}
	req := &matchv1.ResolvePlayerMatchContextRequest{PlayerId: 42}

	unauthorized, err := svc.ResolvePlayerMatchContext(context.Background(), req)
	if err != nil || unauthorized.GetCode() != commonv1.ErrCode_ERR_PERMISSION_DENY {
		t.Fatalf("unsigned response=%+v err=%v", unauthorized, err)
	}
	if resolver.calls != 0 {
		t.Fatalf("unsigned request reached resolver: calls=%d", resolver.calls)
	}

	ctx := resumeIncomingContext(t, signer, 42)
	first, err := svc.ResolvePlayerMatchContext(ctx, req)
	if err != nil || first.GetCode() != commonv1.ErrCode_OK || resolver.calls != 1 {
		t.Fatalf("first response=%+v calls=%d err=%v", first, resolver.calls, err)
	}
	second, err := svc.ResolvePlayerMatchContext(ctx, req)
	if err != nil || second.GetCode() != commonv1.ErrCode_ERR_PERMISSION_DENY {
		t.Fatalf("replay response=%+v err=%v", second, err)
	}
	if resolver.calls != 1 {
		t.Fatalf("replay reached resolver: calls=%d", resolver.calls)
	}
}

func TestResolvePlayerMatchContextSignatureBindsPlayerID(t *testing.T) {
	replay := &resumeReplayFake{seen: map[string]struct{}{}}
	verifier, _ := internalrpcauth.NewVerifier(resumeAuthTestSecret, "login", resumeAuthTestAudience, 30*time.Second, replay)
	signer, _ := internalrpcauth.NewSigner(resumeAuthTestSecret, "login", resumeAuthTestAudience)
	resolver := &resumeResolverFake{}
	svc := &MatchService{resumeResolver: resolver, resumeAuth: verifier}

	ctx := resumeIncomingContext(t, signer, 41)
	resp, err := svc.ResolvePlayerMatchContext(ctx,
		&matchv1.ResolvePlayerMatchContextRequest{PlayerId: 42})
	if err != nil || resp.GetCode() != commonv1.ErrCode_ERR_PERMISSION_DENY {
		t.Fatalf("response=%+v err=%v", resp, err)
	}
	if resolver.calls != 0 || len(replay.seen) != 0 {
		t.Fatalf("tampered subject caused side effect: resolver=%d replay=%d", resolver.calls, len(replay.seen))
	}
}

func TestMatchResumeReplayIsSharedAcrossReplicas(t *testing.T) {
	mr := miniredis.RunT(t)
	rdbA := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	rdbB := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdbA.Close(); _ = rdbB.Close() })
	storeA, err := internalrpcauth.NewRedisReplayStore(rdbA, "test:match-resume:nonce:")
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := internalrpcauth.NewRedisReplayStore(rdbB, "test:match-resume:nonce:")
	if err != nil {
		t.Fatal(err)
	}
	verifierA, _ := internalrpcauth.NewVerifier(resumeAuthTestSecret, "login",
		resumeAuthTestAudience, 30*time.Second, storeA)
	verifierB, _ := internalrpcauth.NewVerifier(resumeAuthTestSecret, "login",
		resumeAuthTestAudience, 30*time.Second, storeB)
	signer, _ := internalrpcauth.NewSigner(resumeAuthTestSecret, "login", resumeAuthTestAudience)
	ctx := resumeIncomingContext(t, signer, 42)
	if err := verifierA.Verify(ctx, matchv1.MatchService_ResolvePlayerMatchContext_FullMethodName, 42); err != nil {
		t.Fatalf("replica A verify: %v", err)
	}
	if err := verifierB.Verify(ctx, matchv1.MatchService_ResolvePlayerMatchContext_FullMethodName, 42); !errors.Is(err, internalrpcauth.ErrReplay) {
		t.Fatalf("replica B replay error=%v", err)
	}
}
