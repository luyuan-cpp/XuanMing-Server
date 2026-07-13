package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-kratos/kratos/v2/transport"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/middleware"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
	loginv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/login/v1"
	"github.com/luyuancpp/pandora/services/account/login/internal/biz"
	"github.com/luyuancpp/pandora/services/account/login/internal/data"
)

type admissionHeader map[string][]string

func (h admissionHeader) Get(key string) string {
	if v := h[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}
func (h admissionHeader) Set(key, value string) { h[key] = []string{value} }
func (h admissionHeader) Add(key, value string) { h[key] = append(h[key], value) }
func (h admissionHeader) Keys() []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, k)
	}
	return out
}
func (h admissionHeader) Values(key string) []string { return h[key] }

type admissionTransport struct{ header admissionHeader }

func (t *admissionTransport) Kind() transport.Kind { return transport.KindGRPC }
func (t *admissionTransport) Endpoint() string     { return "" }
func (t *admissionTransport) Operation() string {
	return "/pandora.login.v1.LoginService/VerifyDSTicket"
}
func (t *admissionTransport) RequestHeader() transport.Header { return t.header }
func (t *admissionTransport) ReplyHeader() transport.Header   { return admissionHeader{} }

func admissionServiceContext(token string) context.Context {
	h := admissionHeader{}
	h.Set(middleware.MetadataKeyDSGateway, "1")
	h.Set("authorization", "Bearer "+token)
	return transport.NewServerContext(context.Background(), &admissionTransport{header: h})
}

type activeAdmissionChecker struct {
	err     error
	matchID uint64
	calls   atomic.Int32
}

func (c *activeAdmissionChecker) CheckActive(_ context.Context, pod string, credential *middleware.VerifiedCredential) (data.DSAdmissionBinding, error) {
	c.calls.Add(1)
	if c.err != nil {
		return data.DSAdmissionBinding{}, c.err
	}
	matchID := credential.MatchID
	if c.matchID != 0 {
		matchID = c.matchID
	}
	return data.DSAdmissionBinding{
		DSType: credential.DSType, MatchID: matchID, PodName: pod,
		InstanceUID: credential.InstanceUID, ProtocolEpoch: credential.ProtocolEpoch,
		CredentialGen: credential.Gen, CredentialJTI: credential.JTI, ExpMs: credential.ExpMs,
		Kid: credential.Kid, TokenSHA256: credential.TokenSHA256, WriterEpoch: credential.WriterEpoch,
		PlayerIDs: func() []uint64 {
			if credential.DSType == auth.DSTypeBattle {
				return []uint64{1001, 1002}
			}
			return nil
		}(),
	}, nil
}

func newAdmissionService(t *testing.T, checker data.DSAdmissionChecker) (*LoginService, *auth.Signer, *miniredis.Miniredis) {
	t.Helper()
	playerCfg := auth.Config{Secret: []byte("pandora-player-ticket-test-secret-32!!")}
	playerSigner, err := auth.NewSigner(playerCfg)
	if err != nil {
		t.Fatal(err)
	}
	playerVerifier, err := auth.NewVerifier(playerCfg)
	if err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ticketUC := biz.NewTicketUsecase(playerSigner, playerVerifier, data.NewRedisTicketJTIRepo(rdb))

	dsCfg := auth.Config{Issuer: auth.DSCallbackIssuer, Audience: auth.DSCallbackAudience,
		Secret: []byte("pandora-ds-admission-test-secret-32!!!")}
	dsSigner, err := auth.NewSigner(dsCfg)
	if err != nil {
		t.Fatal(err)
	}
	dsVerifier, err := auth.NewVerifier(dsCfg)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := middleware.NewDSCallbackGuard(dsVerifier, middleware.DSAuthEnforce)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewLoginService(nil, ticketUC)
	svc.SetRedisDSAdmissionAuthority(guard, checker)
	return svc, dsSigner, mr
}

func TestVerifyDSTicketRedisAdmissionOrderingAndIdempotency(t *testing.T) {
	checker := &activeAdmissionChecker{}
	svc, dsSigner, mr := newAdmissionService(t, checker)
	dsCredential, err := dsSigner.SignBattleCredential(9001, "battle-1", "uid-b", 4, 8, "credential-jti", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// TicketUsecase signer 不导出；用相同玩家配置重建签发器。
	entrySigner, _ := auth.NewSigner(auth.Config{Secret: []byte("pandora-player-ticket-test-secret-32!!")})
	ticket, _, err := entrySigner.SignDSTicket(1001, auth.DSTypeBattle, 9001, "entry-jti")
	if err != nil {
		t.Fatal(err)
	}
	req := &loginv1.VerifyDSTicketRequest{Ticket: ticket, DsPodName: "battle-1", AdmissionId: "123e4567-e89b-42d3-a456-426614174000"}
	ctx := admissionServiceContext(dsCredential.Token)

	for i := 0; i < 2; i++ {
		res, rpcErr := svc.VerifyDSTicket(ctx, req)
		if rpcErr != nil || res.GetCode() != commonv1.ErrCode_OK {
			t.Fatalf("attempt %d code=%v rpcErr=%v", i, res.GetCode(), rpcErr)
		}
	}
	if checker.calls.Load() != 2 {
		t.Fatalf("active checker calls=%d", checker.calls.Load())
	}
	if !mr.Exists("pandora:ticket:entry-jti") {
		t.Fatal("successful admission did not create marker")
	}
}

func TestVerifyDSTicketRejectsBeforeJTISideEffect(t *testing.T) {
	entrySigner, _ := auth.NewSigner(auth.Config{Secret: []byte("pandora-player-ticket-test-secret-32!!")})
	ticket, _, _ := entrySigner.SignDSTicket(1001, auth.DSTypeBattle, 9001, "zero-side-effect-jti")
	const admissionID = "123e4567-e89b-42d3-a456-426614174000"

	for _, tc := range []struct {
		name       string
		checker    data.DSAdmissionChecker
		requestPod string
		want       commonv1.ErrCode
	}{
		{"wrong-match", &activeAdmissionChecker{matchID: 9002}, "battle-1", commonv1.ErrCode_ERR_LOGIN_TICKET_INVALID},
		{"stale-caller", &activeAdmissionChecker{err: errcode.New(errcode.ErrUnauthorized, "stale")}, "battle-1", commonv1.ErrCode_ERR_UNAUTHORIZED},
		{"redis-failure", &activeAdmissionChecker{err: errcode.New(errcode.ErrUnavailable, "redis down")}, "battle-1", commonv1.ErrCode_ERR_UNAVAILABLE},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, dsSigner, mr := newAdmissionService(t, tc.checker)
			cred, err := dsSigner.SignBattleCredential(9001, "battle-1", "uid-b", 4, 8, "credential-jti", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			res, rpcErr := svc.VerifyDSTicket(admissionServiceContext(cred.Token), &loginv1.VerifyDSTicketRequest{
				Ticket: ticket, DsPodName: tc.requestPod, AdmissionId: admissionID,
			})
			if rpcErr != nil || res.GetCode() != tc.want {
				t.Fatalf("code=%v want=%v rpcErr=%v", res.GetCode(), tc.want, rpcErr)
			}
			if mr.Exists("pandora:ticket:zero-side-effect-jti") {
				t.Fatal("rejected admission consumed jti")
			}
		})
	}
}

func TestVerifyDSTicketRedisAuthorityMissingCheckerFailsClosed(t *testing.T) {
	svc, dsSigner, mr := newAdmissionService(t, nil)
	cred, _ := dsSigner.SignBattleCredential(9001, "battle-1", "uid-b", 4, 8, "credential-jti", time.Hour)
	res, err := svc.VerifyDSTicket(admissionServiceContext(cred.Token), &loginv1.VerifyDSTicketRequest{
		Ticket: "ignored-before-ticket-parse", DsPodName: "battle-1", AdmissionId: "123e4567-e89b-42d3-a456-426614174000",
	})
	if err != nil || res.GetCode() != commonv1.ErrCode_ERR_UNAVAILABLE {
		t.Fatalf("code=%v err=%v", res.GetCode(), err)
	}
	if len(mr.Keys()) != 0 {
		t.Fatalf("missing checker mutated Redis: %v", mr.Keys())
	}
}

func TestVerifyDSTicketHubProjectionRejectsBeforeJTIMarker(t *testing.T) {
	const (
		pod         = "hub-1"
		uid         = "hub-uid"
		entryJTI    = "hub-projection-entry-jti"
		admissionID = "123e4567-e89b-42d3-a456-426614174000"
	)
	for _, tc := range []struct {
		name       string
		writeShard bool
		preseed    bool
		mutateAuth func(*hubv1.HubShardAuthStorageRecord)
		mutate     func(*hubv1.HubShardStorageRecord)
	}{
		{name: "missing", writeShard: false},
		{name: "draining", writeShard: true, mutate: func(r *hubv1.HubShardStorageRecord) { r.State = "draining" }},
		{name: "wrong-uid", writeShard: true, mutate: func(r *hubv1.HubShardStorageRecord) { r.GameserverUid = "rebuilt-uid" }},
		{name: "wrong-last-verified", writeShard: true, mutate: func(r *hubv1.HubShardStorageRecord) { r.LastVerifiedGen++ }},
		{name: "quarantined-same-marker", writeShard: true, preseed: true,
			mutateAuth: func(r *hubv1.HubShardAuthStorageRecord) { r.Phase = hubv1.HubAuthPhase_HUB_AUTH_PHASE_QUARANTINED }},
		{name: "draining-same-marker", writeShard: true, preseed: true,
			mutate: func(r *hubv1.HubShardStorageRecord) { r.State = "draining" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, dsSigner, mr := newAdmissionService(t, nil)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			defer rdb.Close()
			svc.admissionChecker = data.NewRedisDSAdmissionChecker(rdb, 30*time.Second)

			dsCredential, err := dsSigner.SignHubCredential(pod, uid, 7, 11, "hub-credential-jti", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			nowMs := time.Now().UnixMilli()
			authRecord := &hubv1.HubShardAuthStorageRecord{
				PodName: pod, InstanceUid: uid, ProtocolEpoch: 7,
				Phase: hubv1.HubAuthPhase_HUB_AUTH_PHASE_ACTIVE,
				Active: &hubv1.HubDSCredential{
					Gen: 11, Jti: "hub-credential-jti", ExpMs: uint64(dsCredential.ExpMs), Kid: dsCredential.Kid,
					InstanceUid: uid, ProtocolEpoch: 7, TokenSha256: dsCredential.TokenSHA256,
					WriterEpoch: auth.DSAuthWriterEpochV2,
				},
				HighWaterGen: 11, RequiredWriterEpoch: auth.DSAuthWriterEpochV2,
				LastActiveHeartbeatMs: nowMs,
			}
			if tc.mutateAuth != nil {
				tc.mutateAuth(authRecord)
			}
			authRaw, _ := proto.Marshal(authRecord)
			mr.Set("pandora:hub:auth:{hub-1}", string(authRaw))
			if tc.writeShard {
				shard := &hubv1.HubShardStorageRecord{
					HubPodName: pod, State: "ready", GameserverUid: uid, AuthEpoch: 7,
					LastVerifiedGen: 11, LastVerifiedJti: "hub-credential-jti",
					LastVerifiedWriterEpoch: auth.DSAuthWriterEpochV2, LastHeartbeatMs: nowMs,
				}
				if tc.mutate != nil {
					tc.mutate(shard)
				}
				shardRaw, _ := proto.Marshal(shard)
				mr.Set("pandora:hub:shard:{hub-1}", string(shardRaw))
			}

			entrySigner, _ := auth.NewSigner(auth.Config{Secret: []byte("pandora-player-ticket-test-secret-32!!")})
			ticket, _, err := entrySigner.SignBoundHubDSTicket(1001, 0, 0, 0, entryJTI, auth.DSTicketBinding{
				DSPodName: pod, DSInstanceUID: uid, ProtocolEpoch: 7, CredentialGen: 11,
				CredentialJTI: "hub-credential-jti", HubAssignmentID: "assignment-1",
				WriterEpoch: auth.DSAuthWriterEpochV2,
			})
			if err != nil {
				t.Fatal(err)
			}
			markerBefore := ""
			if tc.preseed {
				binding := data.DSAdmissionBinding{
					DSType: auth.DSTypeHub, PodName: pod, InstanceUID: uid, ProtocolEpoch: 7,
					CredentialGen: 11, CredentialJTI: "hub-credential-jti", ExpMs: dsCredential.ExpMs,
					Kid: dsCredential.Kid, TokenSHA256: dsCredential.TokenSHA256,
					WriterEpoch: auth.DSAuthWriterEpochV2,
				}
				attempt, _ := binding.AdmissionAttemptOwner(admissionID)
				credentialHash, _ := binding.AcceptedCredentialHash()
				markerRepo := data.NewRedisTicketJTIRepo(rdb)
				if _, err := markerRepo.MarkUsedByAdmission(context.Background(), entryJTI, attempt, credentialHash, 5*time.Minute); err != nil {
					t.Fatal(err)
				}
				markerBefore, _ = mr.Get("pandora:ticket:" + entryJTI)
			}
			res, rpcErr := svc.VerifyDSTicket(admissionServiceContext(dsCredential.Token), &loginv1.VerifyDSTicketRequest{
				Ticket: ticket, DsPodName: pod, AdmissionId: admissionID,
			})
			if rpcErr != nil || res.GetCode() != commonv1.ErrCode_ERR_UNAUTHORIZED {
				t.Fatalf("code=%v rpcErr=%v", res.GetCode(), rpcErr)
			}
			if tc.preseed {
				markerAfter, _ := mr.Get("pandora:ticket:" + entryJTI)
				if markerAfter != markerBefore {
					t.Fatal("rejected same-attempt call mutated existing marker")
				}
			} else if mr.Exists("pandora:ticket:" + entryJTI) {
				t.Fatal("rejected Hub projection consumed jti")
			}
		})
	}
}
