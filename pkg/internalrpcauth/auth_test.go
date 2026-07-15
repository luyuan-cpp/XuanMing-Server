package internalrpcauth

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
)

const (
	testSecret   = "internal-rpc-auth-test-secret-0123456789abcdef"
	testMethod   = "/pandora.match.v1.MatchService/ResolvePlayerMatchContext"
	testAudience = "matchmaker:5v5_ranked"
)

type memoryReplayStore struct {
	seen map[string]struct{}
	err  error
}

func (s *memoryReplayStore) Consume(_ context.Context, key string, _ time.Duration) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	if _, ok := s.seen[key]; ok {
		return false, nil
	}
	s.seen[key] = struct{}{}
	return true, nil
}

func signedIncoming(t *testing.T, signer *Signer, method string, subject uint64) context.Context {
	t.Helper()
	out, err := signer.SignContext(context.Background(), method, subject)
	if err != nil {
		t.Fatalf("SignContext: %v", err)
	}
	md, ok := metadata.FromOutgoingContext(out)
	if !ok {
		t.Fatal("signed context has no outgoing metadata")
	}
	return metadata.NewIncomingContext(context.Background(), md.Copy())
}

func newAuthPair(t *testing.T) (*Signer, *Verifier, *memoryReplayStore) {
	t.Helper()
	store := &memoryReplayStore{seen: map[string]struct{}{}}
	signer, err := NewSigner(testSecret, "login", testAudience)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(testSecret, "login", testAudience, 30*time.Second, store)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Unix(1_800_000_000, 123_000_000).UTC()
	signer.now = func() time.Time { return fixed }
	signer.random = bytes.NewReader(bytes.Repeat([]byte{0x5a}, nonceBytes))
	verifier.now = func() time.Time { return fixed }
	return signer, verifier, store
}

func TestVerifyConsumesNonceAndRejectsReplay(t *testing.T) {
	signer, verifier, _ := newAuthPair(t)
	ctx := signedIncoming(t, signer, testMethod, 42)
	if err := verifier.Verify(ctx, testMethod, 42); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	if err := verifier.Verify(ctx, testMethod, 42); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay error=%v, want ErrReplay", err)
	}
}

func TestVerifyBindsMethodAndSubjectBeforeReplayWrite(t *testing.T) {
	for name, verify := range map[string]func(*Verifier, context.Context) error{
		"different method":  func(v *Verifier, ctx context.Context) error { return v.Verify(ctx, "/other.Service/Read", 42) },
		"different subject": func(v *Verifier, ctx context.Context) error { return v.Verify(ctx, testMethod, 43) },
	} {
		t.Run(name, func(t *testing.T) {
			signer, verifier, store := newAuthPair(t)
			ctx := signedIncoming(t, signer, testMethod, 42)
			if err := verify(verifier, ctx); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("error=%v, want ErrUnauthorized", err)
			}
			if len(store.seen) != 0 {
				t.Fatalf("invalid signature consumed replay nonce: %d", len(store.seen))
			}
		})
	}
}

func TestVerifyBindsTargetAudience(t *testing.T) {
	signer, _, store := newAuthPair(t)
	other, err := NewVerifier(testSecret, "login", "matchmaker:pve_coop", 30*time.Second, store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := signedIncoming(t, signer, testMethod, 42)
	if err := other.Verify(ctx, testMethod, 42); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-audience error=%v, want ErrUnauthorized", err)
	}
	if len(store.seen) != 0 {
		t.Fatal("cross-audience request consumed replay nonce")
	}
}

func TestVerifyRejectsDuplicateMetadataAndStaleCredential(t *testing.T) {
	signer, verifier, store := newAuthPair(t)
	ctx := signedIncoming(t, signer, testMethod, 42)
	md, _ := metadata.FromIncomingContext(ctx)
	md = md.Copy()
	md.Append(CallerMetadataKey, "login")
	if err := verifier.Verify(metadata.NewIncomingContext(context.Background(), md), testMethod, 42); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("duplicate metadata error=%v", err)
	}
	if len(store.seen) != 0 {
		t.Fatal("duplicate metadata reached replay store")
	}

	signer, verifier, _ = newAuthPair(t)
	ctx = signedIncoming(t, signer, testMethod, 42)
	verifier.now = func() time.Time { return signer.now().Add(31 * time.Second) }
	if err := verifier.Verify(ctx, testMethod, 42); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("stale credential error=%v", err)
	}
}

func TestVerifyFailsClosedWhenReplayAuthorityFails(t *testing.T) {
	signer, verifier, store := newAuthPair(t)
	store.err = errors.New("redis unavailable")
	ctx := signedIncoming(t, signer, testMethod, 42)
	if err := verifier.Verify(ctx, testMethod, 42); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error=%v, want ErrUnavailable", err)
	}
}

func TestSignerDoesNotForwardPlayerCredential(t *testing.T) {
	signer, _, _ := newAuthPair(t)
	base := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(
		"authorization", "Bearer player-token", "x-pandora-player-id", "42"))
	signed, err := signer.SignContext(base, testMethod, 42)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := metadata.FromOutgoingContext(signed)
	if len(md.Get("authorization")) != 0 || len(md.Get("x-pandora-player-id")) != 0 {
		t.Fatal("player credential leaked into internal RPC metadata")
	}
}
