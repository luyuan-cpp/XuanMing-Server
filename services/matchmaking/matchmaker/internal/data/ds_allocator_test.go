package data

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/battleabort"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/internalrpcauth"
	"github.com/luyuancpp/pandora/pkg/placement"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/model"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type allocatorResponseClient struct {
	dsv1.DSAllocatorServiceClient
	response *dsv1.AllocateBattleResponse
}

type allocatorCaptureClient struct {
	dsv1.DSAllocatorServiceClient
	request *dsv1.AllocateBattleRequest
}

func (c *allocatorCaptureClient) AllocateBattle(
	_ context.Context,
	req *dsv1.AllocateBattleRequest,
	_ ...grpc.CallOption,
) (*dsv1.AllocateBattleResponse, error) {
	c.request = req
	return &dsv1.AllocateBattleResponse{
		Code: commonv1.ErrCode_OK, DsAddr: "10.0.0.1:7777", DsPodName: "battle-1",
		GameserverUid: "uid-1", InstanceEpoch: 1,
		AllocationId: "550e8400-e29b-41d4-a716-446655440000", ReleaseTrack: "stable",
	}, nil
}

func (c allocatorResponseClient) AllocateBattle(context.Context, *dsv1.AllocateBattleRequest, ...grpc.CallOption) (*dsv1.AllocateBattleResponse, error) {
	return c.response, nil
}

func TestAllocateBattlePreservesAllocatorResponseCode(t *testing.T) {
	tests := []struct {
		name string
		code commonv1.ErrCode
		want errcode.Code
	}{
		{name: "unknown outcome stays retryable", code: commonv1.ErrCode_ERR_UNAVAILABLE, want: errcode.ErrUnavailable},
		{name: "definite failure stays definite", code: commonv1.ErrCode_ERR_DS_ALLOCATION_FAILED, want: errcode.ErrDSAllocationFailed},
		{name: "no capacity is not rewritten", code: commonv1.ErrCode_ERR_DS_NO_AVAILABLE, want: errcode.ErrDSNoAvailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allocator := &GrpcDSAllocator{cli: allocatorResponseClient{response: &dsv1.AllocateBattleResponse{Code: tt.code}}}
			if _, err := allocator.AllocateBattle(t.Context(), 9001, []uint64{1}, 1); errcode.As(err) != tt.want {
				t.Fatalf("AllocateBattle code = %d err=%v, want %d", errcode.As(err), err, tt.want)
			}
		})
	}
}

func TestAllocateBattleWithCombatFactionsSendsCanonicalIndependentMapping(t *testing.T) {
	client := &allocatorCaptureClient{}
	allocator := &GrpcDSAllocator{cli: client, gameMode: "custom"}
	_, err := allocator.AllocateBattleWithCombatFactions(
		t.Context(), 9001, []uint64{99, 7, 42}, map[uint64]uint32{7: 3, 42: 3, 99: 9}, 8)
	if err != nil {
		t.Fatal(err)
	}
	got := client.request.GetPlayerCombatFactions()
	if len(got) != 3 || got[0].GetPlayerId() != 7 || got[0].GetCombatFactionId() != 3 ||
		got[1].GetPlayerId() != 42 || got[1].GetCombatFactionId() != 3 ||
		got[2].GetPlayerId() != 99 || got[2].GetCombatFactionId() != 9 {
		t.Fatalf("player_combat_factions=%v", got)
	}
	if players := client.request.GetPlayerIds(); len(players) != 3 || players[0] != 7 || players[1] != 42 || players[2] != 99 {
		t.Fatalf("player_ids=%v", players)
	}
}

func TestSignBattleTicketWithoutSignerFailsClosed(t *testing.T) {
	g := &GrpcDSAllocator{}
	if _, err := g.SignBattleTicket(t.Context(), 42, 9001, &model.BattleAllocation{}); err == nil {
		t.Fatal("SignBattleTicket() without any signer must fail closed")
	}
}

// signFakeSessionGate 可编程会话现行性权威 fake(R7 复审 P0-2 测试用)。
type signFakeSessionGate struct {
	jti   string
	found bool
	err   error
}

func (g *signFakeSessionGate) CurrentJTI(context.Context, uint64) (string, bool, error) {
	return g.jti, g.found, g.err
}

// battleTicketSJTI 从签出的 JWT 里解出 sjti claim(不验签,测试只关心签发内容)。
func battleTicketSJTI(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token is not a JWT: %q", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims struct {
		SessJTI string `json:"sjti"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	return claims.SessJTI
}

// R7 复审 P0-2:装配 session gate 后,READY 票必须把签发时刻玩家的当前会话 jti 签进
// sjti;权威不可达 fail-closed 拒签;已登出玩家扣票。确定性:gate 状态逐步切换断言。
func TestSignBattleTicketBindsCurrentSessionJTI(t *testing.T) {
	pemBytes, _, kid, err := auth.GenerateDSTicketKeyPair()
	if err != nil {
		t.Fatalf("GenerateDSTicketKeyPair: %v", err)
	}
	v2, err := auth.NewDSTicketSigner(auth.DSTicketSignerConfig{PrivateKeyPEM: pemBytes, ActiveKid: kid})
	if err != nil {
		t.Fatalf("NewDSTicketSigner: %v", err)
	}
	allocation := &model.BattleAllocation{Target: placement.Target{
		PodName: "battle-1", InstanceUID: "uid-1", InstanceEpoch: 3,
		AllocationID: "550e8400-e29b-41d4-a716-446655440000", ReleaseTrack: "stable",
	}}
	gate := &signFakeSessionGate{jti: "jti-A", found: true}
	g := &GrpcDSAllocator{v2: v2, sessGate: gate}

	// ① 有现行会话:sjti = 签发时刻的当前 jti。
	token, err := g.SignBattleTicket(t.Context(), 42, 9001, allocation)
	if err != nil {
		t.Fatalf("SignBattleTicket: %v", err)
	}
	if got := battleTicketSJTI(t, token); got != "jti-A" {
		t.Fatalf("sjti = %q, want jti-A", got)
	}
	// ② 会话轮换后签发:新票绑定新 jti(旧票在兑换点由 login 权威拒绝)。
	gate.jti = "jti-B"
	token, err = g.SignBattleTicket(t.Context(), 42, 9001, allocation)
	if err != nil {
		t.Fatalf("SignBattleTicket after rotation: %v", err)
	}
	if got := battleTicketSJTI(t, token); got != "jti-B" {
		t.Fatalf("sjti after rotation = %q, want jti-B", got)
	}
	// ③ 权威不可达:fail-closed 拒签,不得盲签无会话绑定的票。
	gate.err = errors.New("redis down")
	if _, err := g.SignBattleTicket(t.Context(), 42, 9001, allocation); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("gate outage must fail closed, code=%v err=%v", errcode.As(err), err)
	}
	gate.err = nil
	// ④ 已登出(无会话):扣票,重登链会用新会话重签。
	gate.found = false
	if _, err := g.SignBattleTicket(t.Context(), 42, 9001, allocation); errcode.As(err) != errcode.ErrUnauthorized {
		t.Fatalf("logged-out player must be withheld, code=%v err=%v", errcode.As(err), err)
	}
}

type allocatorAbortReplayStore struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func (s *allocatorAbortReplayStore) Consume(_ context.Context, key string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, found := s.seen[key]; found {
		return false, nil
	}
	s.seen[key] = struct{}{}
	return true, nil
}

type allocatorAbortClient struct {
	dsv1.DSAllocatorServiceClient
	verifier *internalrpcauth.Verifier
	request  *dsv1.AbortPreactiveBattleRequest
	verified error
}

func (c *allocatorAbortClient) AbortPreactiveBattle(
	ctx context.Context,
	req *dsv1.AbortPreactiveBattleRequest,
	_ ...grpc.CallOption,
) (*dsv1.AbortPreactiveBattleResponse, error) {
	c.request = req
	md, _ := metadata.FromOutgoingContext(ctx)
	incoming := metadata.NewIncomingContext(context.Background(), md.Copy())
	request := battleabort.Request{MatchID: req.GetMatchId(), OperationID: req.GetAllocationOperationId(), Target: placement.Target{
		PodName: req.GetDsPodName(), InstanceUID: req.GetGameserverUid(),
		InstanceEpoch: req.GetInstanceEpoch(), AllocationID: req.GetAllocationId(),
		ReleaseTrack: req.GetReleaseTrack(),
	}}
	c.verified = c.verifier.VerifyWithPayload(incoming,
		dsv1.DSAllocatorService_AbortPreactiveBattle_FullMethodName,
		request.MatchID, request.Canonical())
	return &dsv1.AbortPreactiveBattleResponse{Code: commonv1.ErrCode_OK}, nil
}

func TestAbortBattleAllocationSignsCanonicalFullRequest(t *testing.T) {
	const secret = "matchmaker-allocation-abort-test-secret-0123456789"
	const audience = "ds-allocator:battle-allocation-abort"
	signer, err := internalrpcauth.NewSigner(secret, "matchmaker", audience)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := internalrpcauth.NewVerifier(secret, "matchmaker", audience, 30*time.Second,
		&allocatorAbortReplayStore{seen: make(map[string]struct{})})
	if err != nil {
		t.Fatal(err)
	}
	client := &allocatorAbortClient{verifier: verifier}
	allocator := &GrpcDSAllocator{cli: client, abortAuth: signer}
	allocation := &model.BattleAllocation{Address: "10.0.0.1:7777", Target: placement.Target{
		PodName: "battle-1", InstanceUID: "uid-1", InstanceEpoch: 9,
		AllocationID: "allocation-1", ReleaseTrack: "stable",
	}}
	const operationID = "550e8400-e29b-41d4-a716-446655440000"
	if err := allocator.AbortBattleAllocation(t.Context(), 9002, operationID, allocation); err != nil {
		t.Fatal(err)
	}
	if client.verified != nil || client.request.GetMatchId() != 9002 ||
		client.request.GetAllocationOperationId() != operationID ||
		client.request.GetDsPodName() != allocation.Target.PodName ||
		client.request.GetGameserverUid() != allocation.Target.InstanceUID ||
		client.request.GetInstanceEpoch() != allocation.Target.InstanceEpoch ||
		client.request.GetAllocationId() != allocation.Target.AllocationID ||
		client.request.GetReleaseTrack() != allocation.Target.ReleaseTrack {
		t.Fatalf("request=%+v verify=%v", client.request, client.verified)
	}
}

func TestAbortBattleAllocationFailsClosedWithoutDedicatedSigner(t *testing.T) {
	allocator := &GrpcDSAllocator{}
	if err := allocator.AbortBattleAllocation(t.Context(), 1,
		"550e8400-e29b-41d4-a716-446655440000", &model.BattleAllocation{}); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("err=%v code=%v", err, errcode.As(err))
	}
}
