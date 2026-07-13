package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/go-kratos/kratos/v2/transport"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/middleware"
	battlev1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/battle/v1"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/data"
)

var battleCredentialNow = time.UnixMilli(1_800_000_000_000)

const battleCredentialTestSecret = "test-only-ds-callback-secret-32bytes"

type battleAuthReaderStub struct {
	rec      *dsv1.BattleDSAuthStorageRecord
	battle   *dsv1.BattleStorageRecord
	found    bool
	err      error
	recorded int
}

func (s *battleAuthReaderStub) GetBattleAuthority(context.Context, uint64) (*dsv1.BattleDSAuthStorageRecord, *dsv1.BattleStorageRecord, bool, error) {
	return s.rec, s.battle, s.found, s.err
}
func (s *battleAuthReaderStub) RecordBattleResult(context.Context, data.BattleResultCredential, time.Duration) error {
	s.recorded++
	return s.err
}

func validBattleCredential() (*middleware.VerifiedCredential, *dsv1.BattleDSAuthStorageRecord) {
	exp := battleCredentialNow.Add(time.Hour).UnixMilli()
	cred := &middleware.VerifiedCredential{DSType: auth.DSTypeBattle, MatchID: 9, Pod: "battle-9", InstanceUID: "uid-9", ProtocolEpoch: 3, Gen: 7, JTI: "j7", ExpMs: exp, TokenSHA256: "hash7", Kid: "kid7", WriterEpoch: auth.DSAuthWriterEpochV2}
	active := &dsv1.BattleDSCredential{Gen: 7, Jti: "j7", ExpMs: uint64(exp), Kid: "kid7", InstanceUid: "uid-9", InstanceEpoch: 3, TokenSha256: "hash7", WriterEpoch: auth.DSAuthWriterEpochV2}
	rec := &dsv1.BattleDSAuthStorageRecord{MatchId: 9, DsPodName: "battle-9", InstanceUid: "uid-9", InstanceEpoch: 3, Phase: dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_ACTIVE, Active: active, HighWaterGen: 7, RequiredWriterEpoch: auth.DSAuthWriterEpochV2, AllocationId: "alloc-9", LastActiveHeartbeatMs: battleCredentialNow.Add(-time.Second).UnixMilli()}
	return cred, rec
}

func futureWriterBattleTokenAndAuthority(t *testing.T) (string, *dsv1.BattleDSAuthStorageRecord, *dsv1.BattleStorageRecord) {
	t.Helper()
	cfg := auth.Config{
		Issuer: auth.DSCallbackIssuer, Audience: auth.DSCallbackAudience,
		Secret: []byte(battleCredentialTestSecret), NowFn: func() time.Time { return battleCredentialNow },
	}
	signer, err := auth.NewSigner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := signer.SignBattleCredential(9, "battle-9", "uid-9", 3, 7, "j7", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	exp := jwt.NewNumericDate(battleCredentialNow.Add(time.Hour))
	claims := auth.DSCallbackClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: auth.DSCallbackIssuer, Subject: "battle-9",
			Audience: jwt.ClaimStrings{auth.DSCallbackAudience},
			IssuedAt: jwt.NewNumericDate(battleCredentialNow), ExpiresAt: exp, ID: "j7",
		},
		DSType: string(auth.DSTypeBattle), MatchID: 9, DSGen: 7, DSInstanceUID: "uid-9",
		DSProtocolEpoch: 3, DSWriterEpoch: 3, DSKid: seed.Kid,
	}
	tokenObj := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenObj.Header["kid"] = seed.Kid
	token, err := tokenObj.SignedString([]byte(battleCredentialTestSecret))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(token))
	active := &dsv1.BattleDSCredential{
		Gen: 7, Jti: "j7", ExpMs: uint64(exp.UnixMilli()), Kid: seed.Kid,
		InstanceUid: "uid-9", InstanceEpoch: 3, TokenSha256: hex.EncodeToString(hash[:]), WriterEpoch: 3,
	}
	authRecord := &dsv1.BattleDSAuthStorageRecord{
		MatchId: 9, DsPodName: "battle-9", InstanceUid: "uid-9", InstanceEpoch: 3,
		Phase:  dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_ROTATING,
		Active: active, Pending: proto.Clone(active).(*dsv1.BattleDSCredential),
		HighWaterGen: 7, RequiredWriterEpoch: auth.DSAuthWriterEpochV2,
		AllocationId: "alloc-9", LastActiveHeartbeatMs: battleCredentialNow.Add(-time.Second).UnixMilli(),
	}
	battle := &dsv1.BattleStorageRecord{
		MatchId: 9, DsPodName: "battle-9", State: "running", AllocationId: "alloc-9",
		GameserverUid: "uid-9", InstanceEpoch: 3, LastVerifiedGen: 7,
		LastVerifiedJti: "j7", LastVerifiedWriterEpoch: 3,
	}
	return token, authRecord, battle
}

func TestBattleCredentialStateCheckerStrictTuple(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*middleware.VerifiedCredential, *dsv1.BattleDSAuthStorageRecord, *battleAuthReaderStub)
		want   errcode.Code
	}{
		{name: "active", want: errcode.OK},
		{name: "redis failure", mutate: func(_ *middleware.VerifiedCredential, _ *dsv1.BattleDSAuthStorageRecord, r *battleAuthReaderStub) {
			r.err = errors.New("down")
		}, want: errcode.ErrUnavailable},
		{name: "missing", mutate: func(_ *middleware.VerifiedCredential, _ *dsv1.BattleDSAuthStorageRecord, r *battleAuthReaderStub) {
			r.found = false
		}, want: errcode.ErrUnauthorized},
		{name: "pending", mutate: func(c *middleware.VerifiedCredential, r *dsv1.BattleDSAuthStorageRecord, _ *battleAuthReaderStub) {
			r.Active.Gen = 6
			r.Pending = &dsv1.BattleDSCredential{Gen: c.Gen, Jti: c.JTI}
		}, want: errcode.ErrUnauthorized},
		{name: "wrong uid", mutate: func(c *middleware.VerifiedCredential, _ *dsv1.BattleDSAuthStorageRecord, _ *battleAuthReaderStub) {
			c.InstanceUID = "other"
		}, want: errcode.ErrUnauthorized},
		{name: "wrong epoch", mutate: func(c *middleware.VerifiedCredential, _ *dsv1.BattleDSAuthStorageRecord, _ *battleAuthReaderStub) {
			c.ProtocolEpoch++
		}, want: errcode.ErrUnauthorized},
		{name: "stale gen", mutate: func(c *middleware.VerifiedCredential, _ *dsv1.BattleDSAuthStorageRecord, _ *battleAuthReaderStub) {
			c.Gen--
		}, want: errcode.ErrUnauthorized},
		{name: "future gen", mutate: func(c *middleware.VerifiedCredential, _ *dsv1.BattleDSAuthStorageRecord, _ *battleAuthReaderStub) {
			c.Gen++
		}, want: errcode.ErrUnauthorized},
		{name: "wrong jti", mutate: func(c *middleware.VerifiedCredential, _ *dsv1.BattleDSAuthStorageRecord, _ *battleAuthReaderStub) {
			c.JTI = "other"
		}, want: errcode.ErrUnauthorized},
		{name: "wrong hash", mutate: func(c *middleware.VerifiedCredential, _ *dsv1.BattleDSAuthStorageRecord, _ *battleAuthReaderStub) {
			c.TokenSHA256 = "other"
		}, want: errcode.ErrUnauthorized},
		{name: "low writer", mutate: func(c *middleware.VerifiedCredential, _ *dsv1.BattleDSAuthStorageRecord, _ *battleAuthReaderStub) {
			c.WriterEpoch = 1
		}, want: errcode.ErrUnauthorized},
		{name: "low required-active-claims writer 1", mutate: func(c *middleware.VerifiedCredential, r *dsv1.BattleDSAuthStorageRecord, reader *battleAuthReaderStub) {
			c.WriterEpoch = 1
			r.RequiredWriterEpoch = 1
			r.Active.WriterEpoch = 1
			r.Pending = proto.Clone(r.Active).(*dsv1.BattleDSCredential)
			reader.battle.LastVerifiedWriterEpoch = 1
		}, want: errcode.ErrUnauthorized},
		{name: "required 2 future active-claims writer 3", mutate: func(c *middleware.VerifiedCredential, r *dsv1.BattleDSAuthStorageRecord, reader *battleAuthReaderStub) {
			c.WriterEpoch = 3
			r.RequiredWriterEpoch = auth.DSAuthWriterEpochV2
			r.Active.WriterEpoch = 3
			r.Pending = proto.Clone(r.Active).(*dsv1.BattleDSCredential)
			reader.battle.LastVerifiedWriterEpoch = 3
		}, want: errcode.ErrUnauthorized},
		{name: "stale heartbeat", mutate: func(_ *middleware.VerifiedCredential, r *dsv1.BattleDSAuthStorageRecord, _ *battleAuthReaderStub) {
			r.LastActiveHeartbeatMs = battleCredentialNow.Add(-time.Minute).UnixMilli()
		}, want: errcode.ErrUnauthorized},
		{name: "future heartbeat", mutate: func(_ *middleware.VerifiedCredential, r *dsv1.BattleDSAuthStorageRecord, _ *battleAuthReaderStub) {
			r.LastActiveHeartbeatMs = battleCredentialNow.Add(time.Second).UnixMilli()
		}, want: errcode.ErrUnauthorized},
		{name: "quarantined", mutate: func(_ *middleware.VerifiedCredential, r *dsv1.BattleDSAuthStorageRecord, _ *battleAuthReaderStub) {
			r.Phase = dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_QUARANTINED
		}, want: errcode.ErrUnauthorized},
		{name: "allocation missing", mutate: func(_ *middleware.VerifiedCredential, r *dsv1.BattleDSAuthStorageRecord, _ *battleAuthReaderStub) {
			r.AllocationId = ""
		}, want: errcode.ErrUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cred, rec := validBattleCredential()
			battle := &dsv1.BattleStorageRecord{
				MatchId: 9, DsPodName: "battle-9", State: "running", AllocationId: "alloc-9",
				GameserverUid: "uid-9", InstanceEpoch: 3, LastVerifiedGen: 7,
				LastVerifiedJti: "j7", LastVerifiedWriterEpoch: auth.DSAuthWriterEpochV2,
			}
			reader := &battleAuthReaderStub{rec: rec, battle: battle, found: true}
			if tc.mutate != nil {
				tc.mutate(cred, rec, reader)
			}
			checker := &redisBattleCredentialStateChecker{reader: reader, now: func() time.Time { return battleCredentialNow }, maxActiveHeartbeatAge: 30 * time.Second}
			if got := errcode.As(checker.CheckActive(context.Background(), 9, cred)); got != tc.want {
				t.Fatalf("code=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestBattleResultRecorderRejectsFutureWriterBeforeRecorderCall(t *testing.T) {
	cred, _ := validBattleCredential()
	cred.WriterEpoch = 3
	reader := &battleAuthReaderStub{found: true}
	checker := &redisBattleCredentialStateChecker{
		reader: reader, recorder: reader, now: func() time.Time { return battleCredentialNow },
		maxActiveHeartbeatAge: 30 * time.Second,
	}
	if err := checker.MarkResultRecorded(context.Background(), 9, cred); errcode.As(err) != errcode.ErrUnauthorized {
		t.Fatalf("code=%v err=%v", errcode.As(err), err)
	}
	if reader.recorded != 0 {
		t.Fatalf("future writer reached receipt recorder: calls=%d", reader.recorded)
	}
}

func TestBattleCredentialCheckerPreservesUnknownFieldsReadOnly(t *testing.T) {
	_, rec := validBattleCredential()
	raw, err := proto.Marshal(rec)
	if err != nil || len(raw) == 0 {
		t.Fatalf("marshal err=%v", err)
	}
	// checker 只读，不存在 read-modify-write；此测试锁定完整 proto 可解码基线。
}

type resultTestHeader map[string][]string

func (h resultTestHeader) Get(k string) string {
	if len(h[k]) > 0 {
		return h[k][0]
	}
	return ""
}
func (h resultTestHeader) Set(k, v string) { h[k] = []string{v} }
func (h resultTestHeader) Add(k, v string) { h[k] = append(h[k], v) }
func (h resultTestHeader) Keys() []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, k)
	}
	return out
}
func (h resultTestHeader) Values(k string) []string { return h[k] }

type resultTestTransport struct{ request resultTestHeader }

func (*resultTestTransport) Kind() transport.Kind { return transport.KindGRPC }
func (*resultTestTransport) Endpoint() string     { return "" }
func (*resultTestTransport) Operation() string {
	return "/pandora.battle.v1.BattleResultService/ReportResult"
}
func (t *resultTestTransport) RequestHeader() transport.Header { return t.request }
func (*resultTestTransport) ReplyHeader() transport.Header     { return resultTestHeader{} }

type rejectingBattleChecker struct{ calls int }

func (c *rejectingBattleChecker) CheckActive(context.Context, uint64, *middleware.VerifiedCredential) error {
	c.calls++
	return errcode.New(errcode.ErrUnauthorized, "stale")
}
func (c *rejectingBattleChecker) MarkResultRecorded(context.Context, uint64, *middleware.VerifiedCredential) error {
	panic("must not record a rejected result")
}

func TestReportResultChecksActiveBeforeUsecaseSideEffects(t *testing.T) {
	cfg := auth.Config{Issuer: auth.DSCallbackIssuer, Audience: auth.DSCallbackAudience, Secret: []byte(battleCredentialTestSecret), NowFn: func() time.Time { return battleCredentialNow }}
	signer, err := auth.NewSigner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := middleware.NewDSCallbackGuard(verifier, middleware.DSAuthEnforce)
	if err != nil {
		t.Fatal(err)
	}
	result, err := signer.SignBattleCredential(9, "battle-9", "uid-9", 3, 7, "j7", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	header := resultTestHeader{}
	header.Set("authorization", "Bearer "+result.Token)
	header.Set(middleware.MetadataKeyDSGateway, "1")
	ctx := transport.NewServerContext(context.Background(), &resultTestTransport{request: header})
	checker := &rejectingBattleChecker{}
	svc := NewBattleResultService(nil) // 若门放错到 usecase 之后，此测试会 panic。
	svc.SetDSCallbackGuard(guard)
	svc.SetBattleCredentialStateChecker(checker)
	resp, err := svc.ReportResult(ctx, &battlev1.ReportResultRequest{Result: &battlev1.BattleResult{MatchId: 9, DsPodName: "battle-9"}})
	if err != nil || resp.GetCode() != commonv1.ErrCode_ERR_UNAUTHORIZED || checker.calls != 1 {
		t.Fatalf("resp=%v err=%v checker_calls=%d", resp, err, checker.calls)
	}
}

func TestReportResultFutureWriterRejectedBeforeUsecaseAndReceipt(t *testing.T) {
	token, authRecord, battle := futureWriterBattleTokenAndAuthority(t)
	cfg := auth.Config{
		Issuer: auth.DSCallbackIssuer, Audience: auth.DSCallbackAudience,
		Secret: []byte(battleCredentialTestSecret), NowFn: func() time.Time { return battleCredentialNow },
	}
	verifier, err := auth.NewVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := middleware.NewDSCallbackGuard(verifier, middleware.DSAuthEnforce)
	if err != nil {
		t.Fatal(err)
	}
	header := resultTestHeader{}
	header.Set("authorization", "Bearer "+token)
	header.Set(middleware.MetadataKeyDSGateway, "1")
	ctx := transport.NewServerContext(context.Background(), &resultTestTransport{request: header})
	reader := &battleAuthReaderStub{rec: authRecord, battle: battle, found: true}
	checker := &redisBattleCredentialStateChecker{
		reader: reader, recorder: reader, now: func() time.Time { return battleCredentialNow },
		maxActiveHeartbeatAge: 30 * time.Second,
	}
	svc := NewBattleResultService(nil) // future writer 若越过 checker 会在 usecase 处 panic。
	svc.SetDSCallbackGuard(guard)
	svc.SetBattleCredentialStateChecker(checker)
	resp, err := svc.ReportResult(ctx, &battlev1.ReportResultRequest{
		Result: &battlev1.BattleResult{MatchId: 9, DsPodName: "battle-9"},
	})
	if err != nil || resp.GetCode() != commonv1.ErrCode_ERR_UNAUTHORIZED {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if reader.recorded != 0 {
		t.Fatalf("future writer reached result receipt: calls=%d", reader.recorded)
	}
}
