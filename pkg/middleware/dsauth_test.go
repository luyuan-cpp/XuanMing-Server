// dsauth_test.go — DSCallbackGuard 单测(off/permissive/enforce × 网关标记 × 令牌范围)。
package middleware

import (
	"context"
	"testing"
	"time"

	"github.com/go-kratos/kratos/v2/transport"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
)

// ── 假 transport(注入 header)──────────────────────────────────────────────────

type fakeHeader map[string][]string

func (h fakeHeader) Get(key string) string {
	if vs := h[key]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}
func (h fakeHeader) Set(key, value string) { h[key] = []string{value} }
func (h fakeHeader) Add(key, value string) { h[key] = append(h[key], value) }
func (h fakeHeader) Keys() []string {
	ks := make([]string, 0, len(h))
	for k := range h {
		ks = append(ks, k)
	}
	return ks
}
func (h fakeHeader) Values(key string) []string { return h[key] }

type fakeTransport struct{ req fakeHeader }

func (t *fakeTransport) Kind() transport.Kind            { return transport.KindGRPC }
func (t *fakeTransport) Endpoint() string                { return "" }
func (t *fakeTransport) Operation() string               { return "/test/Op" }
func (t *fakeTransport) RequestHeader() transport.Header { return t.req }
func (t *fakeTransport) ReplyHeader() transport.Header   { return fakeHeader{} }

// ctxWith 构造带指定 header 的 server transport ctx。
func ctxWith(headers map[string]string) context.Context {
	h := fakeHeader{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return transport.NewServerContext(context.Background(), &fakeTransport{req: h})
}

// ── 测试基建 ─────────────────────────────────────────────────────────────────────

func newTestGuard(t *testing.T, mode DSAuthMode) (*DSCallbackGuard, *auth.Signer) {
	t.Helper()
	cfg := auth.Config{
		Issuer:   auth.DSCallbackIssuer,
		Audience: auth.DSCallbackAudience,
		Secret:   []byte("pandora-dev-shared-secret-32bytes!!"),
	}
	signer, err := auth.NewSigner(cfg)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	verifier, err := auth.NewVerifier(cfg)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	g, err := NewDSCallbackGuard(verifier, mode)
	if err != nil {
		t.Fatalf("NewDSCallbackGuard: %v", err)
	}
	return g, signer
}

func battleToken(t *testing.T, s *auth.Signer, matchID uint64) string {
	t.Helper()
	tok, _, err := s.SignDSCallback(auth.DSTypeBattle, "", matchID, time.Hour)
	if err != nil {
		t.Fatalf("SignDSCallback: %v", err)
	}
	return tok
}

func hubToken(t *testing.T, s *auth.Signer, pod string) string {
	t.Helper()
	tok, _, err := s.SignDSCallback(auth.DSTypeHub, pod, 0, time.Hour)
	if err != nil {
		t.Fatalf("SignDSCallback: %v", err)
	}
	return tok
}

func wantCode(t *testing.T, err error, code errcode.Code) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := errcode.As(err); got != code {
		t.Fatalf("errcode: got %d want %d (err=%v)", got, code, err)
	}
}

// ── 用例 ────────────────────────────────────────────────────────────────────────

func TestParseDSAuthMode(t *testing.T) {
	for _, c := range []struct {
		in   string
		want DSAuthMode
	}{{"", DSAuthOff}, {"off", DSAuthOff}, {"OFF", DSAuthOff}, {"permissive", DSAuthPermissive}, {"Enforce", DSAuthEnforce}} {
		got, err := ParseDSAuthMode(c.in)
		if err != nil || got != c.want {
			t.Fatalf("ParseDSAuthMode(%q) = %v,%v want %v", c.in, got, err, c.want)
		}
	}
	if _, err := ParseDSAuthMode("bogus"); err == nil {
		t.Fatal("expected error for bogus mode")
	}
}

func TestGuardNilAndOffAllowAll(t *testing.T) {
	var nilGuard *DSCallbackGuard
	if err := nilGuard.Check(ctxWith(map[string]string{MetadataKeyDSGateway: "1"}), DSScope{Type: auth.DSTypeBattle, MatchID: 1}); err != nil {
		t.Fatalf("nil guard must allow: %v", err)
	}
	g, _ := newTestGuard(t, DSAuthOff)
	if err := g.Check(ctxWith(map[string]string{MetadataKeyDSGateway: "1"}), DSScope{Type: auth.DSTypeBattle, MatchID: 1}); err != nil {
		t.Fatalf("off guard must allow: %v", err)
	}
}

func TestGuardEnforceGatewayRequiresToken(t *testing.T) {
	g, s := newTestGuard(t, DSAuthEnforce)
	scope := DSScope{Type: auth.DSTypeBattle, MatchID: 9001}

	// 经网关无令牌 → 401
	wantCode(t, g.Check(ctxWith(map[string]string{MetadataKeyDSGateway: "1"}), scope), errcode.ErrUnauthorized)
	// 经网关坏令牌 → 401
	wantCode(t, g.Check(ctxWith(map[string]string{
		MetadataKeyDSGateway: "1", authorizationHeader: "Bearer not-a-jwt",
	}), scope), errcode.ErrUnauthorized)
	// 经网关正确令牌 → 放行
	if err := g.Check(ctxWith(map[string]string{
		MetadataKeyDSGateway: "1", authorizationHeader: "Bearer " + battleToken(t, s, 9001),
	}), scope); err != nil {
		t.Fatalf("valid token must pass: %v", err)
	}
	// 内部直连(无标记头)无令牌 → 放行(东西向不受影响)
	if err := g.Check(ctxWith(nil), scope); err != nil {
		t.Fatalf("internal caller must pass: %v", err)
	}
}

func TestGuardEnforceScopeBinding(t *testing.T) {
	g, s := newTestGuard(t, DSAuthEnforce)
	gw := func(tok string) context.Context {
		return ctxWith(map[string]string{MetadataKeyDSGateway: "1", authorizationHeader: "Bearer " + tok})
	}

	// match_id 不匹配 → 403(拿 A 局令牌伪造 B 局上报)
	wantCode(t, g.Check(gw(battleToken(t, s, 9001)), DSScope{Type: auth.DSTypeBattle, MatchID: 9002}), errcode.ErrPermissionDeny)
	// ds_type 不匹配 → 403(hub 令牌调 battle 回调)
	wantCode(t, g.Check(gw(hubToken(t, s, "hub-1")), DSScope{Type: auth.DSTypeBattle, MatchID: 9001}), errcode.ErrPermissionDeny)
	// pod 不匹配 → 403(A 分片令牌冒充 B 分片心跳)
	wantCode(t, g.Check(gw(hubToken(t, s, "hub-1")), DSScope{Type: auth.DSTypeHub, Pod: "hub-2"}), errcode.ErrPermissionDeny)
	// pod 匹配 → 放行
	if err := g.Check(gw(hubToken(t, s, "hub-1")), DSScope{Type: auth.DSTypeHub, Pod: "hub-1"}); err != nil {
		t.Fatalf("matching hub scope must pass: %v", err)
	}
	// 内部直连但带了不匹配令牌 → 仍 403(带 DS 令牌即视为 DS)
	wantCode(t, g.Check(ctxWith(map[string]string{authorizationHeader: "Bearer " + battleToken(t, s, 9001)}),
		DSScope{Type: auth.DSTypeBattle, MatchID: 9002}), errcode.ErrPermissionDeny)
}

func TestGuardEnforceDenyDS(t *testing.T) {
	g, s := newTestGuard(t, DSAuthEnforce)
	scope := DSScope{DenyDS: true} // 例:SetLocation state=MATCHING 不允许来自 DS

	// 经网关 → 403(无论有无令牌)
	wantCode(t, g.Check(ctxWith(map[string]string{MetadataKeyDSGateway: "1"}), scope), errcode.ErrPermissionDeny)
	wantCode(t, g.Check(ctxWith(map[string]string{
		MetadataKeyDSGateway: "1", authorizationHeader: "Bearer " + hubToken(t, s, "hub-1"),
	}), scope), errcode.ErrPermissionDeny)
	// 内部直连无令牌 → 放行(matchmaker 写 MATCHING/BATTLE)
	if err := g.Check(ctxWith(nil), scope); err != nil {
		t.Fatalf("internal caller must pass: %v", err)
	}
}

func TestGuardEnforceRequireToken(t *testing.T) {
	g, s := newTestGuard(t, DSAuthEnforce)
	// 纯 DS 回调(Heartbeat / ReportResult / GM):无合法的东西向无令牌调用者。
	scope := DSScope{Type: auth.DSTypeBattle, MatchID: 7001, RequireToken: true}

	// 内部直连(无标记头)+ 无令牌 → 401(堵住绕过 Envoy 直连的无令牌旁路)
	wantCode(t, g.Check(ctxWith(nil), scope), errcode.ErrUnauthorized)
	// 内部直连 + 正确令牌 → 放行(DS 令牌合法即可,不强制经网关)
	if err := g.Check(ctxWith(map[string]string{
		authorizationHeader: "Bearer " + battleToken(t, s, 7001),
	}), scope); err != nil {
		t.Fatalf("valid token must pass even without gateway marker: %v", err)
	}
	// 经网关 + 正确令牌 → 放行
	if err := g.Check(ctxWith(map[string]string{
		MetadataKeyDSGateway: "1", authorizationHeader: "Bearer " + battleToken(t, s, 7001),
	}), scope); err != nil {
		t.Fatalf("valid token via gateway must pass: %v", err)
	}
	// permissive 下无令牌直连只告警放行(不破坏灰度)
	gp, _ := newTestGuard(t, DSAuthPermissive)
	if err := gp.Check(ctxWith(nil), scope); err != nil {
		t.Fatalf("permissive require-token must not reject: %v", err)
	}
}

// CheckHubCredential:Model B 令牌(带 uid/epoch/gen/jti)→ 返回非空 VerifiedCredential;
// legacy 令牌(无 uid)→ credential 为 nil(service 层据此回退代际门路径)。
func TestCheckHubCredential(t *testing.T) {
	g, s := newTestGuard(t, DSAuthEnforce)
	scope := DSScope{Type: auth.DSTypeHub, Pod: "hub-1", RequireToken: true}

	// Model B 令牌 → 非空 credential,身份四元组齐全。
	res, err := s.SignHubCredential("hub-1", "uid-A", 2, 7, "j7", time.Hour)
	if err != nil {
		t.Fatalf("SignHubCredential: %v", err)
	}
	ctxMB := ctxWith(map[string]string{MetadataKeyDSGateway: "1", authorizationHeader: "Bearer " + res.Token})
	claims, cred, err := g.CheckHubCredential(ctxMB, scope)
	if err != nil {
		t.Fatalf("CheckHubCredential model b: %v", err)
	}
	if claims == nil {
		t.Fatal("want claims for model b token")
	}
	if cred == nil {
		t.Fatal("want non-nil credential for model b token")
	}
	if cred.Pod != "hub-1" || cred.InstanceUID != "uid-A" || cred.ProtocolEpoch != 2 || cred.Gen != 7 || cred.JTI != "j7" ||
		cred.Kid == "" || cred.WriterEpoch != auth.DSAuthWriterEpochV2 {
		t.Fatalf("credential mismatch: %+v", cred)
	}
	if cred.TokenSHA256 == "" {
		t.Fatal("want token sha256 bound")
	}

	// legacy 令牌(仅 ds_gen,无 uid)→ credential 为 nil,claims 仍非空(回退代际门)。
	legacyTok, _, err := s.SignDSCallbackWithGen(auth.DSTypeHub, "hub-1", 0, 5, time.Hour)
	if err != nil {
		t.Fatalf("SignDSCallbackWithGen: %v", err)
	}
	ctxLegacy := ctxWith(map[string]string{MetadataKeyDSGateway: "1", authorizationHeader: "Bearer " + legacyTok})
	claims2, cred2, err := g.CheckHubCredential(ctxLegacy, scope)
	if err != nil {
		t.Fatalf("CheckHubCredential legacy: %v", err)
	}
	if claims2 == nil {
		t.Fatal("want legacy claims")
	}
	if cred2 != nil {
		t.Fatalf("legacy token must yield nil credential, got %+v", cred2)
	}
}

func TestCheckBattleCredential(t *testing.T) {
	g, s := newTestGuard(t, DSAuthEnforce)
	res, err := s.SignBattleCredential(7001, "battle-1", "uid-B", 3, 11, "battle-jti", time.Hour)
	if err != nil {
		t.Fatalf("SignBattleCredential: %v", err)
	}
	ctx := ctxWith(map[string]string{MetadataKeyDSGateway: "1", authorizationHeader: "Bearer " + res.Token})
	claims, cred, err := g.CheckBattleCredential(ctx, DSScope{
		Type: auth.DSTypeBattle, MatchID: 7001, Pod: "battle-1", RequireToken: true,
	})
	if err != nil || claims == nil || cred == nil {
		t.Fatalf("CheckBattleCredential: claims=%+v cred=%+v err=%v", claims, cred, err)
	}
	if cred.DSType != auth.DSTypeBattle || cred.MatchID != 7001 || cred.Pod != "battle-1" ||
		cred.InstanceUID != "uid-B" || cred.ProtocolEpoch != 3 || cred.Gen != 11 ||
		cred.JTI != "battle-jti" || cred.Kid != res.Kid || cred.WriterEpoch != auth.DSAuthWriterEpochV2 ||
		cred.TokenSHA256 != res.TokenSHA256 {
		t.Fatalf("battle credential mismatch: %+v", cred)
	}
}

func TestCheckCredentialAcceptsEitherTypeButAlwaysScopesPod(t *testing.T) {
	g, s := newTestGuard(t, DSAuthEnforce)
	for _, tc := range []struct {
		name  string
		issue func() (auth.HubCredentialResult, error)
		want  auth.DSType
	}{
		{"hub", func() (auth.HubCredentialResult, error) {
			return s.SignHubCredential("ds-1", "uid-1", 3, 7, "jti-h", time.Hour)
		}, auth.DSTypeHub},
		{"battle", func() (auth.HubCredentialResult, error) {
			return s.SignBattleCredential(99, "ds-1", "uid-1", 3, 7, "jti-b", time.Hour)
		}, auth.DSTypeBattle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.issue()
			if err != nil {
				t.Fatal(err)
			}
			ctx := ctxWith(map[string]string{MetadataKeyDSGateway: "1", authorizationHeader: "Bearer " + result.Token})
			_, credential, err := g.CheckCredential(ctx, DSScope{Pod: "ds-1", RequireToken: true})
			if err != nil || credential == nil || credential.DSType != tc.want {
				t.Fatalf("credential=%+v err=%v", credential, err)
			}
			if _, _, err := g.CheckCredential(ctx, DSScope{Pod: "other", RequireToken: true}); errcode.As(err) != errcode.ErrPermissionDeny {
				t.Fatalf("wrong pod code=%v err=%v", errcode.As(err), err)
			}
		})
	}
}

func TestGuardPermissiveNeverRejects(t *testing.T) {
	g, s := newTestGuard(t, DSAuthPermissive)
	// 网关无令牌 / 坏令牌 / 范围不匹配,全部放行(只记日志)
	if err := g.Check(ctxWith(map[string]string{MetadataKeyDSGateway: "1"}), DSScope{Type: auth.DSTypeBattle, MatchID: 1}); err != nil {
		t.Fatalf("permissive must allow missing token: %v", err)
	}
	if err := g.Check(ctxWith(map[string]string{
		MetadataKeyDSGateway: "1", authorizationHeader: "Bearer bad",
	}), DSScope{Type: auth.DSTypeBattle, MatchID: 1}); err != nil {
		t.Fatalf("permissive must allow bad token: %v", err)
	}
	if err := g.Check(ctxWith(map[string]string{
		MetadataKeyDSGateway: "1", authorizationHeader: "Bearer " + battleToken(t, s, 1),
	}), DSScope{Type: auth.DSTypeBattle, MatchID: 2}); err != nil {
		t.Fatalf("permissive must allow scope mismatch: %v", err)
	}
}

func TestNewDSCallbackGuardRequiresVerifier(t *testing.T) {
	if _, err := NewDSCallbackGuard(nil, DSAuthEnforce); err == nil {
		t.Fatal("enforce without verifier must fail")
	}
	if g, err := NewDSCallbackGuard(nil, DSAuthOff); err != nil || g == nil {
		t.Fatalf("off without verifier must be ok: %v", err)
	}
}
