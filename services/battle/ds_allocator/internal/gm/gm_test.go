package gm

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/transport"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/middleware"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	gmv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/gm/v1"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/data"
)

type gmTestHeader map[string][]string

func (h gmTestHeader) Get(key string) string {
	if v := h[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}
func (h gmTestHeader) Set(key, value string)      { h[key] = []string{value} }
func (h gmTestHeader) Add(key, value string)      { h[key] = append(h[key], value) }
func (h gmTestHeader) Values(key string) []string { return h[key] }
func (h gmTestHeader) Keys() []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, k)
	}
	return out
}

type gmTestTransport struct{ request gmTestHeader }

func (t *gmTestTransport) Kind() transport.Kind            { return transport.KindGRPC }
func (t *gmTestTransport) Endpoint() string                { return "" }
func (t *gmTestTransport) Operation() string               { return "/pandora.gm.v1.GmService/PollCommands" }
func (t *gmTestTransport) RequestHeader() transport.Header { return t.request }
func (t *gmTestTransport) ReplyHeader() transport.Header   { return gmTestHeader{} }

func gmBearerContext(token string) context.Context {
	h := gmTestHeader{}
	h.Set("authorization", "Bearer "+token)
	h.Set(middleware.MetadataKeyDSGateway, "1")
	return transport.NewServerContext(context.Background(), &gmTestTransport{request: h})
}

func newTestService(t *testing.T) (*Service, func()) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return NewService(rdb, log.DefaultLogger), func() {
		_ = rdb.Close()
		mr.Close()
	}
}

func addItemReq(matchID uint64, playerID uint64, configID uint32, count int32) *gmv1.SendCommandRequest {
	return &gmv1.SendCommandRequest{
		MatchId: matchID,
		Payload: &gmv1.SendCommandRequest_AddItem{AddItem: &gmv1.AddItemCommand{
			PlayerId: playerID,
			ConfigId: configID,
			Count:    count,
		}},
	}
}

// TestSendThenPoll_FIFO 验证入队 → 轮询取出,顺序为 FIFO,且取即出队。
func TestSendThenPoll_FIFO(t *testing.T) {
	s, cleanup := newTestService(t)
	defer cleanup()
	ctx := context.Background()

	// 先发 3 条(config 10001/10002/10003)。
	for _, cid := range []uint32{10001, 10002, 10003} {
		resp, err := s.SendCommand(ctx, addItemReq(42, 1001, cid, 1))
		if err != nil {
			t.Fatalf("SendCommand: %v", err)
		}
		if resp.GetCode() != commonv1.ErrCode_OK || resp.GetIdempotencyKey() == "" {
			t.Fatalf("SendCommand code=%v id=%q", resp.GetCode(), resp.GetIdempotencyKey())
		}
	}

	poll, err := s.PollCommands(ctx, &gmv1.PollCommandsRequest{MatchId: 42})
	if err != nil {
		t.Fatalf("PollCommands: %v", err)
	}
	if poll.GetCode() != commonv1.ErrCode_OK {
		t.Fatalf("PollCommands code=%v", poll.GetCode())
	}
	if len(poll.GetCommands()) != 3 {
		t.Fatalf("want 3 commands, got %d", len(poll.GetCommands()))
	}
	// FIFO:先发的 10001 先出。
	wantOrder := []uint32{10001, 10002, 10003}
	for i, c := range poll.GetCommands() {
		if c.GetAddItem().GetConfigId() != wantOrder[i] {
			t.Fatalf("cmd[%d] config=%d want %d", i, c.GetAddItem().GetConfigId(), wantOrder[i])
		}
	}

	// 再轮询应为空(取即出队)。
	poll2, _ := s.PollCommands(ctx, &gmv1.PollCommandsRequest{MatchId: 42})
	if len(poll2.GetCommands()) != 0 {
		t.Fatalf("second poll want 0, got %d", len(poll2.GetCommands()))
	}
}

// TestPoll_IsolatedByMatch 验证不同 match_id 队列互相隔离。
func TestPoll_IsolatedByMatch(t *testing.T) {
	s, cleanup := newTestService(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := s.SendCommand(ctx, addItemReq(1, 1001, 10001, 5)); err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	// 另一局轮询应为空。
	poll, _ := s.PollCommands(ctx, &gmv1.PollCommandsRequest{MatchId: 2})
	if len(poll.GetCommands()) != 0 {
		t.Fatalf("match 2 want 0, got %d", len(poll.GetCommands()))
	}
	// 本局能取到。
	poll1, _ := s.PollCommands(ctx, &gmv1.PollCommandsRequest{MatchId: 1})
	if len(poll1.GetCommands()) != 1 || poll1.GetCommands()[0].GetAddItem().GetCount() != 5 {
		t.Fatalf("match 1 unexpected: %+v", poll1.GetCommands())
	}
}

// TestSendCommand_Validation 验证非法入参被拒。
func TestSendCommand_Validation(t *testing.T) {
	s, cleanup := newTestService(t)
	defer cleanup()
	ctx := context.Background()

	cases := []struct {
		name string
		req  *gmv1.SendCommandRequest
	}{
		{"zero_match", addItemReq(0, 1001, 10001, 1)},
		{"zero_player", addItemReq(1, 0, 10001, 1)},
		{"zero_config", addItemReq(1, 1001, 0, 1)},
		{"zero_count", addItemReq(1, 1001, 10001, 0)},
		{"bad_bag", &gmv1.SendCommandRequest{
			MatchId: 1,
			Payload: &gmv1.SendCommandRequest_AddItem{AddItem: &gmv1.AddItemCommand{
				PlayerId: 1001, ConfigId: 10001, Count: 1, BagType: 9,
			}},
		}},
		{"nil_payload", &gmv1.SendCommandRequest{MatchId: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := s.SendCommand(ctx, tc.req)
			if err != nil {
				t.Fatalf("SendCommand err: %v", err)
			}
			if resp.GetCode() != commonv1.ErrCode_ERR_INVALID_ARG {
				t.Fatalf("want ERR_INVALID_ARG, got %v", resp.GetCode())
			}
		})
	}
}

// TestPollMax_Clamped 验证一次拉取条数夹取到 maxPollMax。
func TestPollMax_Clamped(t *testing.T) {
	s, cleanup := newTestService(t)
	defer cleanup()
	ctx := context.Background()

	for i := 0; i < maxPollMax+10; i++ {
		if _, err := s.SendCommand(ctx, addItemReq(7, 1001, 10001, 1)); err != nil {
			t.Fatalf("SendCommand: %v", err)
		}
	}
	poll, _ := s.PollCommands(ctx, &gmv1.PollCommandsRequest{MatchId: 7, Max: 1000})
	if len(poll.GetCommands()) != maxPollMax {
		t.Fatalf("want %d (clamped), got %d", maxPollMax, len(poll.GetCommands()))
	}
}

// TestAckCommand 验证 Ack 回报的基本校验与成功路径。
func TestAckCommand(t *testing.T) {
	s, cleanup := newTestService(t)
	defer cleanup()
	ctx := context.Background()

	if resp, _ := s.AckCommand(ctx, &gmv1.AckCommandRequest{MatchId: 0, IdempotencyKey: "x"}); resp.GetCode() != commonv1.ErrCode_ERR_INVALID_ARG {
		t.Fatalf("zero match want ERR_INVALID_ARG, got %v", resp.GetCode())
	}
	if resp, _ := s.AckCommand(ctx, &gmv1.AckCommandRequest{MatchId: 1, IdempotencyKey: ""}); resp.GetCode() != commonv1.ErrCode_ERR_INVALID_ARG {
		t.Fatalf("empty id want ERR_INVALID_ARG, got %v", resp.GetCode())
	}
	if resp, _ := s.AckCommand(ctx, &gmv1.AckCommandRequest{MatchId: 1, IdempotencyKey: "abc", Ok: true}); resp.GetCode() != commonv1.ErrCode_OK {
		t.Fatalf("valid ack want OK, got %v", resp.GetCode())
	}
}

// fakeChecker 是 BattleLivenessChecker 的可控假体:found/err 由测试指定。
type fakeChecker struct {
	found bool
	err   error
}

func (f fakeChecker) GetBattle(context.Context, uint64) (*dsv1.BattleStorageRecord, bool, error) {
	if f.err != nil {
		return nil, false, f.err
	}
	if !f.found {
		return nil, false, nil
	}
	return &dsv1.BattleStorageRecord{MatchId: 1}, true, nil
}

// TestSendCommand_LivenessCheck 验证 battleChecker 三态:镜像缺失拒(NOT_FOUND)、
// 读失败 fail-open(仍入队 OK)、镜像存在放行(OK)。
func TestSendCommand_LivenessCheck(t *testing.T) {
	ctx := context.Background()

	// 缺失 → ERR_NOT_FOUND
	s1, c1 := newTestService(t)
	defer c1()
	s1.SetBattleChecker(fakeChecker{found: false})
	if resp, _ := s1.SendCommand(ctx, addItemReq(1, 1001, 10001, 1)); resp.GetCode() != commonv1.ErrCode_ERR_NOT_FOUND {
		t.Fatalf("missing match want ERR_NOT_FOUND, got %v", resp.GetCode())
	}

	// 读失败 → fail-open,仍 OK 入队
	s2, c2 := newTestService(t)
	defer c2()
	s2.SetBattleChecker(fakeChecker{err: context.DeadlineExceeded})
	if resp, _ := s2.SendCommand(ctx, addItemReq(1, 1001, 10001, 1)); resp.GetCode() != commonv1.ErrCode_OK {
		t.Fatalf("checker error want fail-open OK, got %v", resp.GetCode())
	}

	// 存在 → OK
	s3, c3 := newTestService(t)
	defer c3()
	s3.SetBattleChecker(fakeChecker{found: true})
	if resp, _ := s3.SendCommand(ctx, addItemReq(1, 1001, 10001, 1)); resp.GetCode() != commonv1.ErrCode_OK {
		t.Fatalf("active match want OK, got %v", resp.GetCode())
	}
}

func TestModelBPollAndAckRejectStaleCredentialBeforeSideEffects(t *testing.T) {
	s, cleanup := newTestService(t)
	defer cleanup()
	ctx := context.Background()
	battleRepo := data.NewRedisBattleRepo(s.rdb)
	authRepo := data.NewRedisBattleAuthRepo(s.rdb)
	const matchID uint64 = 900
	const allocationID = "alloc-900"
	const pod = "battle-auth-900"
	claim := &dsv1.BattleStorageRecord{
		MatchId: matchID, State: "allocating", AllocationId: allocationID,
		AllocatedAtMs:   time.Now().Add(-time.Second).UnixMilli(),
		LastHeartbeatMs: time.Now().Add(-time.Second).UnixMilli(),
	}
	claimed, _, err := battleRepo.ClaimBattle(ctx, claim, time.Hour)
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	battle := &dsv1.BattleStorageRecord{
		MatchId: matchID, DsPodName: pod, DsAddr: "10.0.0.9:7777", State: "warming",
		AllocationId: allocationID, GameserverUid: "uid-900",
		AllocatedAtMs: claim.AllocatedAtMs, LastHeartbeatMs: claim.LastHeartbeatMs,
	}
	if ok, err := battleRepo.FinalizeBattleAllocation(ctx, battle, time.Hour); err != nil || !ok {
		t.Fatalf("finalize: ok=%v err=%v", ok, err)
	}
	seed, err := authRepo.PrepareCredential(ctx, data.BattleAuthorityBinding{
		MatchID: matchID, AllocationID: allocationID, PodName: pod, InstanceUID: "uid-900",
		RequiredWriterEpoch: data.BattleDSWriterEpochV2, AuthTTL: time.Hour, BattleTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	secret := []byte("gm-model-b-test-secret-at-least-32-bytes")
	authCfg := auth.Config{Issuer: auth.DSCallbackIssuer, Audience: auth.DSCallbackAudience, Secret: secret}
	signer, err := auth.NewSigner(authCfg)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	verifier, err := auth.NewVerifier(authCfg)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	guard, err := middleware.NewDSCallbackGuard(verifier, middleware.DSAuthEnforce)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	jti := uuid.NewString()
	signed, err := signer.SignBattleCredential(matchID, pod, "uid-900", seed.InstanceEpoch, seed.Gen, jti, time.Hour)
	if err != nil {
		t.Fatalf("sign active: %v", err)
	}
	stored := &dsv1.BattleDSCredential{
		Gen: seed.Gen, Jti: jti, ExpMs: uint64(signed.ExpMs), Kid: signed.Kid,
		InstanceUid: "uid-900", InstanceEpoch: seed.InstanceEpoch,
		TokenSha256: signed.TokenSHA256, WriterEpoch: signed.WriterEpoch,
	}
	if _, err := authRepo.StagePending(ctx, data.BattleStageInput{
		MatchID: matchID, AllocationID: allocationID, Credential: stored, AuthTTL: time.Hour,
	}); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := authRepo.MarkDelivered(ctx, matchID, allocationID, stored, "102", time.Hour); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	identity := data.BattleCredentialIdentity{
		PodName: pod, InstanceUID: "uid-900", InstanceEpoch: seed.InstanceEpoch,
		Gen: seed.Gen, JTI: jti, ExpMs: uint64(signed.ExpMs), Kid: signed.Kid,
		TokenSHA256: signed.TokenSHA256, WriterEpoch: signed.WriterEpoch,
	}
	if _, err := authRepo.ActivateHeartbeat(ctx, matchID, identity, data.BattleHeartbeatInput{
		PlayerCount: 1, State: "running", AuthTTL: time.Hour, BattleTTL: time.Hour,
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	s.SetDSCallbackGuard(guard)
	if err := s.EnableRedisAuthority(authRepo); err != nil {
		t.Fatalf("EnableRedisAuthority: %v", err)
	}
	if resp, _ := s.SendCommand(ctx, addItemReq(matchID, 1, 10001, 1)); resp.GetCode() != commonv1.ErrCode_OK {
		t.Fatalf("enqueue: %v", resp.GetCode())
	}

	// 另一份签名完全合法、scope 也相同，但不是 Redis active；不得弹出队列或写 Ack 审计。
	staleJTI := uuid.NewString()
	stale, err := signer.SignBattleCredential(
		matchID, pod, "uid-900", seed.InstanceEpoch, seed.Gen+1, staleJTI, time.Hour)
	if err != nil {
		t.Fatalf("sign stale: %v", err)
	}
	staleCtx := gmBearerContext(stale.Token)
	poll, _ := s.PollCommands(staleCtx, &gmv1.PollCommandsRequest{
		MatchId: matchID, DsPodName: pod, Max: 1,
	})
	if poll.GetCode() != commonv1.ErrCode_ERR_UNAUTHORIZED {
		t.Fatalf("stale poll code=%v, want unauthorized", poll.GetCode())
	}
	if n, err := s.rdb.LLen(ctx, queueKey(matchID)).Result(); err != nil || n != 1 {
		t.Fatalf("stale poll mutated queue: len=%d err=%v", n, err)
	}
	ack, _ := s.AckCommand(staleCtx, &gmv1.AckCommandRequest{
		MatchId: matchID, IdempotencyKey: "command-1", Ok: true,
	})
	if ack.GetCode() != commonv1.ErrCode_ERR_UNAUTHORIZED {
		t.Fatalf("stale ack code=%v, want unauthorized", ack.GetCode())
	}

	activeCtx := gmBearerContext(signed.Token)
	poll, _ = s.PollCommands(activeCtx, &gmv1.PollCommandsRequest{
		MatchId: matchID, DsPodName: pod, Max: 1,
	})
	if poll.GetCode() != commonv1.ErrCode_OK || len(poll.GetCommands()) != 1 {
		t.Fatalf("active poll code=%v commands=%d", poll.GetCode(), len(poll.GetCommands()))
	}
}
